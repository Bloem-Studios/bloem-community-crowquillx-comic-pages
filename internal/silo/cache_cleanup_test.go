package silo

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bloem-Studios/bloem-community-crowquillx-comic-pages/internal/archive"
)

func cleanupTestCache(t *testing.T, maxBytes int64) *pageCache {
	t.Helper()
	cache, err := newPageCache(t.TempDir(), maxBytes, CacheFallbackTTL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.close)
	return cache
}

func cleanupTestEntry(t *testing.T, cache *pageCache, key, validator string, now time.Time) *cacheEntry {
	t.Helper()
	reservation, err := cache.reserve(8, now)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.release()
	jobDir, err := os.MkdirTemp(cache.root, ".job-")
	if err != nil {
		t.Fatal(err)
	}
	reservation.jobDir = jobDir
	outputDir := filepath.Join(jobDir, "pages")
	if err := os.Mkdir(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(outputDir, "page-000001")
	if err := os.WriteFile(page, []byte("page"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := reservation.commit(hashFingerprint(key), validator, outputDir,
		[]archive.Page{{Name: "page.png", File: page, Size: 4}}, now)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func requireRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("path was not removed: %s: %v", path, err)
	}
}

func requireCacheOnlyControlFiles(t *testing.T, root string) {
	t.Helper()
	items, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Name() != cacheOwnerMarker && item.Name() != ".lock" {
			t.Fatalf("cache artifact remained: %s", item.Name())
		}
	}
}

func TestDefaultCacheIsTwoGiBWithFourGiBOverride(t *testing.T) {
	config := Config{BaseURL: "http://127.0.0.1", CacheDir: t.TempDir()}
	normalized, err := normalizeConfig(config)
	if err != nil || normalized.CacheSize != 2<<30 {
		t.Fatalf("default cache size = %d, %v", normalized.CacheSize, err)
	}
	config.CacheSize = 4 << 30
	if _, err := normalizeConfig(config); err != nil {
		t.Fatalf("4 GiB override rejected: %v", err)
	}
	config.CacheSize++
	if _, err := normalizeConfig(config); err == nil {
		t.Fatal("cache limit above 4 GiB accepted")
	}
}

func TestCacheIdleSweepPreservesActiveHandlesAndRefreshesOnLookup(t *testing.T) {
	cache := cleanupTestCache(t, 64)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entry := cleanupTestEntry(t, cache, "idle", "etag", now)
	active := cleanupTestEntry(t, cache, "active", "etag", now)
	handle, ok := cache.lookup(active.key, now)
	if !ok {
		t.Fatal("missing active page")
	}
	defer handle.release()
	refreshed, ok := cache.lookup(entry.key, now.Add(20*time.Minute))
	if !ok {
		t.Fatal("missing recently used page")
	}
	refreshed.release()
	cache.sweep(now.Add(30 * time.Minute))
	if data, err := os.ReadFile(handle.entry.pages[0].file); err != nil || string(data) != "page" {
		t.Fatalf("idle sweep unlinked an active handle: %q, %v", data, err)
	}
	if _, err := os.Stat(entry.root); err != nil {
		t.Fatalf("idle sweep ignored the last successful lookup: %v", err)
	}
	if stale, ok := cache.lookup(active.key, now.Add(30*time.Minute)); ok {
		stale.release()
		t.Fatal("expired entry accepted another handle")
	}
	handle.release()
	cache.sweep(now.Add(30 * time.Minute))
	requireRemoved(t, active.root)
	cache.sweep(now.Add(50 * time.Minute))
	requireRemoved(t, entry.root)
	if cache.usedBytes != 0 {
		t.Fatalf("deleted pages remain charged: %d", cache.usedBytes)
	}
}

func TestCacheUnvalidatedFreshnessExpiryDoesNotSlide(t *testing.T) {
	cache := cleanupTestCache(t, 64)
	now := time.Now()
	entry := cleanupTestEntry(t, cache, "unvalidated", "", now)
	handle, ok := cache.lookup(entry.key, now.Add(9*time.Minute))
	if !ok {
		t.Fatal("fresh entry was missing")
	}
	handle.release()
	cache.sweep(now.Add(10 * time.Minute))
	requireRemoved(t, entry.root)
}

func TestCacheFailedEvictionKeepsBudgetAndSweepRetries(t *testing.T) {
	cache := cleanupTestCache(t, 8)
	now := time.Now()
	entry := cleanupTestEntry(t, cache, "failed-delete", "etag", now)
	cache.removeAll = func(path string) error {
		// Partial removal must also retain the entire charge until the directory
		// is gone, even when no readable page is left.
		_ = os.Remove(entry.pages[0].file)
		return os.ErrPermission
	}
	if reservation, err := cache.reserve(8, now); err == nil {
		reservation.release()
		t.Fatal("failed deletion made disk budget available")
	}
	if cache.usedBytes != 4 || cache.entries[entry.key] == nil {
		t.Fatal("failed deletion was discounted from the cache")
	}
	if handle, ok := cache.lookup(entry.key, now); ok {
		handle.release()
		t.Fatal("partially deleted entry was served")
	}
	cache.removeAll = os.RemoveAll
	cache.sweep(now)
	requireRemoved(t, entry.root)
	if cache.usedBytes != 0 {
		t.Fatalf("successful retry retained charge: %d", cache.usedBytes)
	}
	reservation, err := cache.reserve(8, now)
	if err != nil {
		t.Fatalf("successful retry did not free budget: %v", err)
	}
	reservation.release()
}

func TestJobCleanupKeepsPeakChargedUntilDeletion(t *testing.T) {
	for _, outcome := range []string{"success", "extract-failure", "commit-failure"} {
		for _, failDelete := range []bool{false, true} {
			name := outcome
			if failDelete {
				name += "/delete-failure"
			}
			t.Run(name, func(t *testing.T) {
				backend, runtime, _ := newTestBackend(t, &testUpstream{}, []byte("page"), func(cfg *Config) {
					cfg.CacheSize = 128
					cfg.Limits = archive.Limits{MaxArchiveBytes: 64, MaxTotalBytes: 64, MaxPageBytes: 32}
				})
				if outcome != "success" {
					runtime.extractor = func(_ context.Context, _, outputDir string, _ archive.Limits) ([]archive.Page, error) {
						if err := os.Mkdir(outputDir, 0o700); err != nil {
							return nil, err
						}
						path := filepath.Join(outputDir, "partial")
						if err := os.WriteFile(path, []byte("page"), 0o600); err != nil {
							return nil, err
						}
						if outcome == "extract-failure" {
							return nil, errors.New("invalid archive")
						}
						return []archive.Page{{Name: "page.png", File: path + "-missing", Size: 4}}, nil
					}
				}
				cache := runtime.cache
				type deletion struct {
					path    string
					charged int64
					input   bool
				}
				deleted := make(chan deletion, 1)
				var deny atomic.Bool
				deny.Store(failDelete)
				cache.mu.Lock()
				cache.removeAll = func(path string) error {
					if strings.HasPrefix(filepath.Base(path), ".job-") {
						_, err := os.Stat(filepath.Join(path, "archive"))
						select {
						case deleted <- deletion{path, cache.usedBytes + cache.reserved, err == nil}:
						default:
						}
						if deny.Load() {
							return os.ErrPermission
						}
					}
					return os.RemoveAll(path)
				}
				cache.mu.Unlock()
				_, err := backend.Pages(t.Context(), testRequest("", 0), "7")
				switch outcome {
				case "success":
					if err != nil {
						t.Fatal(err)
					}
				case "extract-failure":
					apiStatus(t, err, http.StatusUnprocessableEntity)
				case "commit-failure":
					apiStatus(t, err, http.StatusServiceUnavailable)
				}
				var attempt deletion
				select {
				case attempt = <-deleted:
				default:
					t.Fatal("job did not attempt temporary cleanup")
				}
				if !attempt.input || attempt.charged != 128 {
					t.Fatalf("input was not covered through cleanup: %+v", attempt)
				}
				cache.mu.Lock()
				charged, pending := cache.usedBytes+cache.reserved, len(cache.pending)
				cache.mu.Unlock()
				if failDelete {
					if charged != 128 || pending != 1 {
						t.Fatalf("failed job deletion lost budget: charged=%d pending=%d", charged, pending)
					}
					deny.Store(false)
					cache.sweep(time.Now())
				}
				requireRemoved(t, attempt.path)
				cache.mu.Lock()
				reserved, pending := cache.reserved, len(cache.pending)
				cache.mu.Unlock()
				if reserved != 0 || pending != 0 {
					t.Fatalf("cleaned job remains charged: reserved=%d pending=%d", reserved, pending)
				}
				backend.Close()
				requireCacheOnlyControlFiles(t, cache.root)
			})
		}
	}
}

func TestRuntimeSweepsWithoutRequestsAndDrainsBeforeLeaseClose(t *testing.T) {
	cache := cleanupTestCache(t, 64)
	entry := cleanupTestEntry(t, cache, "idle-runtime", "etag", time.Now().Add(-CacheIdleTTL))
	client, err := newUpstreamClient("http://127.0.0.1", MaxArchiveBytes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	runtime := &runtime{cache: cache, client: client, ctx: ctx, cancel: cancel, closed: make(chan struct{})}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); cancel(); runtime.wg.Wait() })
	cache.removeAll = func(path string) error {
		close(started)
		<-release
		return os.RemoveAll(path)
	}
	runtime.startCacheSweeper(5 * time.Millisecond)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("idle cache was not swept without requests")
	}
	go runtime.close()
	select {
	case <-runtime.closed:
		t.Fatal("runtime closed while its sweeper was deleting files")
	case <-time.After(20 * time.Millisecond):
	}
	if lease, err := lockCacheRoot(cache.root); err == nil {
		_ = lease.Close()
		t.Fatal("runtime released its cache lease before draining the sweeper")
	}
	once.Do(func() { close(release) })
	select {
	case <-runtime.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not drain its sweeper")
	}
	requireRemoved(t, entry.root)
	requireCacheOnlyControlFiles(t, cache.root)
}

func TestBackendCloseCleansCancelledPartialJob(t *testing.T) {
	backend, runtime, _ := newTestBackend(t, &testUpstream{}, []byte("page"), nil)
	started := make(chan struct{})
	runtime.extractor = func(ctx context.Context, _, outputDir string, _ archive.Limits) ([]archive.Page, error) {
		if err := os.Mkdir(outputDir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(outputDir, "partial"), []byte("partial page"), 0o600); err != nil {
			return nil, err
		}
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := backend.Pages(t.Context(), testRequest("", 0), "7")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("partial extraction did not start")
	}
	backend.Close()
	if err := <-done; err == nil {
		t.Fatal("cancelled extraction succeeded")
	}
	requireCacheOnlyControlFiles(t, runtime.cache.root)
	if runtime.cache.reserved != 0 || len(runtime.cache.pending) != 0 {
		t.Fatal("cancelled job retained disk budget after cleanup")
	}
}

func TestPendingCleanupNeverDeletesOutsideOwnedJobDirectories(t *testing.T) {
	cache := cleanupTestCache(t, 64)
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "keep"), []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	reservation, err := cache.reserve(8, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reservation.jobDir = foreign
	reservation.release()
	cache.sweep(time.Now())
	cache.close()
	if data, err := os.ReadFile(filepath.Join(foreign, "keep")); err != nil || string(data) != "unrelated" {
		t.Fatalf("cleanup affected an unowned directory: %q, %v", data, err)
	}
	if cache.reserved != 8 {
		t.Fatal("refusing an unsafe cleanup path incorrectly freed its budget")
	}
}

func TestPendingCleanupMetadataIsBounded(t *testing.T) {
	cache := cleanupTestCache(t, 1024)
	cache.removeAll = func(string) error { return os.ErrPermission }
	for i := 0; i < maxPendingCleanup; i++ {
		reservation, err := cache.reserve(1, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		reservation.jobDir, err = os.MkdirTemp(cache.root, ".job-")
		if err != nil {
			t.Fatal(err)
		}
		reservation.release()
	}
	if reservation, err := cache.reserve(1, time.Now()); err == nil {
		reservation.release()
		t.Fatal("cleanup failures accumulated unbounded metadata")
	}
	cache.removeAll = os.RemoveAll
	cache.sweep(time.Now())
	if cache.reserved != 0 || len(cache.pending) != 0 {
		t.Fatal("pending directories were not retried")
	}
}
