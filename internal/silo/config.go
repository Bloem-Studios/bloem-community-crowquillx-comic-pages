package silo

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	pluginv1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Bloem-Studios/bloem-community-crowquillx-comic-pages/internal/archive"
)

const (
	ChunkBytes         int64 = 1 << 20
	DefaultCacheBytes  int64 = 2 << 30
	MaxCacheBytes      int64 = 4 << 30
	MaxArchiveBytes    int64 = 512 << 20
	MaxPageBytes       int64 = 32 << 20
	MaxTotalBytes      int64 = 1 << 30
	MaxEntries               = 2048
	MaxDictionaryBytes int64 = 64 << 20
	CacheFallbackTTL         = 10 * time.Minute
	CacheIdleTTL             = 30 * time.Minute
	CacheSweepInterval       = time.Minute
	JobTimeout               = 120 * time.Second
	PreparationWait          = 3 * time.Second
	UpstreamTimeout          = 2 * time.Second
)

// Config is the plugin's immutable runtime configuration.
type Config struct {
	BaseURL   string
	CacheDir  string
	CacheSize int64
	CacheTTL  time.Duration
	Limits    archive.Limits
}

func defaultConfig() Config {
	return Config{
		CacheSize: DefaultCacheBytes,
		CacheTTL:  CacheFallbackTTL,
		Limits: archive.Limits{
			MaxEntries:         MaxEntries,
			MaxPageBytes:       MaxPageBytes,
			MaxTotalBytes:      MaxTotalBytes,
			MaxArchiveBytes:    MaxArchiveBytes,
			MaxDictionaryBytes: MaxDictionaryBytes,
		},
	}
}

func normalizeConfig(cfg Config) (Config, error) {
	defaults := defaultConfig()
	if cfg.CacheSize == 0 {
		cfg.CacheSize = defaults.CacheSize
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = defaults.CacheTTL
	}
	if cfg.Limits.MaxEntries == 0 {
		cfg.Limits.MaxEntries = defaults.Limits.MaxEntries
	}
	if cfg.Limits.MaxPageBytes == 0 {
		cfg.Limits.MaxPageBytes = defaults.Limits.MaxPageBytes
	}
	if cfg.Limits.MaxTotalBytes == 0 {
		cfg.Limits.MaxTotalBytes = defaults.Limits.MaxTotalBytes
	}
	if cfg.Limits.MaxArchiveBytes == 0 {
		cfg.Limits.MaxArchiveBytes = defaults.Limits.MaxArchiveBytes
	}
	if cfg.Limits.MaxDictionaryBytes == 0 {
		cfg.Limits.MaxDictionaryBytes = defaults.Limits.MaxDictionaryBytes
	}

	if strings.TrimSpace(cfg.BaseURL) == "" {
		return Config{}, fmt.Errorf("base_url is required")
	}
	base, err := validateBaseURL(cfg.BaseURL)
	if err != nil {
		return Config{}, err
	}
	cfg.BaseURL = base
	if strings.TrimSpace(cfg.CacheDir) == "" {
		return Config{}, fmt.Errorf("cache_dir is required")
	}
	cfg.CacheDir, err = filepath.Abs(filepath.Clean(cfg.CacheDir))
	if err != nil {
		return Config{}, fmt.Errorf("cache_dir is invalid")
	}
	if cfg.CacheDir == filepath.Dir(cfg.CacheDir) {
		return Config{}, fmt.Errorf("cache_dir must name a private directory")
	}
	if cfg.CacheSize < 1 || cfg.CacheSize > MaxCacheBytes {
		return Config{}, fmt.Errorf("cache_size is outside its limit")
	}
	if cfg.CacheTTL < time.Second || cfg.CacheTTL > 24*time.Hour {
		return Config{}, fmt.Errorf("cache_ttl is outside its limit")
	}
	if cfg.Limits.MaxEntries < 1 || cfg.Limits.MaxEntries > MaxEntries ||
		cfg.Limits.MaxPageBytes < 1 || cfg.Limits.MaxPageBytes > MaxPageBytes ||
		cfg.Limits.MaxTotalBytes < 1 || cfg.Limits.MaxTotalBytes > MaxTotalBytes ||
		cfg.Limits.MaxArchiveBytes < 1 || cfg.Limits.MaxArchiveBytes > MaxArchiveBytes ||
		cfg.Limits.MaxDictionaryBytes < 1 || cfg.Limits.MaxDictionaryBytes > MaxDictionaryBytes {
		return Config{}, fmt.Errorf("archive limits are outside their bounds")
	}
	return cfg, nil
}

func validateBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("base_url must be an absolute http or https URL without credentials")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base_url must use http or https")
	}
	if u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("base_url must contain only an origin")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("http base_url must use a loopback host")
	}
	u.Path = ""
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ConfigFromEntries decodes the native SDK Configure request. The plugin has
// one global connection entry; unknown entries are rejected so a misspelled
// admin setting cannot silently leave the runtime unconfigured.
func ConfigFromEntries(entries []*pluginv1.ConfigEntry) (Config, error) {
	var raw map[string]any
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if entry.GetKey() != "connection" {
			return Config{}, fmt.Errorf("unknown config key %q", entry.GetKey())
		}
		if entry.GetValue() == nil {
			return Config{}, fmt.Errorf("connection config is required")
		}
		if raw != nil {
			return Config{}, fmt.Errorf("connection config is duplicated")
		}
		raw = entry.GetValue().AsMap()
	}
	if raw == nil {
		return Config{}, fmt.Errorf("connection config is required")
	}
	getString := func(key string, required bool) (string, error) {
		value, ok := raw[key]
		if !ok {
			if required {
				return "", fmt.Errorf("%s is required", key)
			}
			return "", nil
		}
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("%s must be a non-empty string", key)
		}
		return strings.TrimSpace(text), nil
	}
	baseURL, err := getString("base_url", true)
	if err != nil {
		return Config{}, err
	}
	cacheDir, err := getString("cache_dir", true)
	if err != nil {
		return Config{}, err
	}
	cfg := defaultConfig()
	cfg.BaseURL = baseURL
	cfg.CacheDir = cacheDir
	for key := range raw {
		switch key {
		case "base_url", "cache_dir", "max_cache_bytes", "max_entries", "max_page_bytes", "max_total_bytes", "max_archive_bytes", "max_dictionary_bytes":
		default:
			return Config{}, fmt.Errorf("unknown connection field %q", key)
		}
	}
	readInt := func(key string, current int64) (int64, error) {
		value, ok := raw[key]
		if !ok {
			return current, nil
		}
		number, ok := value.(float64)
		if !ok || number != float64(int64(number)) {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return int64(number), nil
	}
	cacheSize, err := readInt("max_cache_bytes", cfg.CacheSize)
	if err != nil {
		return Config{}, err
	}
	cfg.CacheSize = cacheSize
	entriesValue, err := readInt("max_entries", int64(cfg.Limits.MaxEntries))
	if err != nil {
		return Config{}, err
	}
	cfg.Limits.MaxEntries = int(entriesValue)
	for key, destination := range map[string]*int64{
		"max_page_bytes":       &cfg.Limits.MaxPageBytes,
		"max_total_bytes":      &cfg.Limits.MaxTotalBytes,
		"max_archive_bytes":    &cfg.Limits.MaxArchiveBytes,
		"max_dictionary_bytes": &cfg.Limits.MaxDictionaryBytes,
	} {
		value, readErr := readInt(key, *destination)
		if readErr != nil {
			return Config{}, readErr
		}
		*destination = value
	}
	return normalizeConfig(cfg)
}
