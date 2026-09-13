package silo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crowquillx/silo-comic-pages/internal/archive"
)

var errCacheFull = errors.New("cache is full")

const cacheOwnerMarker = ".silo-comic-pages-owner"

const cacheOwnerContents = "silo-comic-pages-cache-v1\n"

// Only one job is admitted at a time. Stop admitting jobs if repeated cleanup
// failures accumulate this many directories, even when their byte limits are tiny.
const maxPendingCleanup = 128

type cachedPage struct {
	name      string
	file      string
	size      int64
	mediaType string
}

type cacheEntry struct {
	key       string
	root      string
	pages     []cachedPage
	total     int64
	expiresAt time.Time
	lastUsed  time.Time
	active    int
	retired   bool
}

type pageHandle struct {
	entry *cacheEntry
	cache *pageCache
}

func (h *pageHandle) release() {
	if h == nil || h.entry == nil || h.cache == nil {
		return
	}
	h.cache.mu.Lock()
	if h.entry.active > 0 {
		h.entry.active--
	}
	h.cache.mu.Unlock()
	h.entry = nil
	h.cache = nil
}

type cacheReservation struct {
	cache     *pageCache
	bytes     int64
	jobDir    string
	committed bool
	used      bool
}

type pageCache struct {
	mu          sync.Mutex
	root        string
	maxBytes    int64
	fallbackTTL time.Duration
	entries     map[string]*cacheEntry
	usedBytes   int64
	reserved    int64
	lease       *os.File
	pending     map[string]int64
	removeAll   func(string) error
	closed      bool
}

func newPageCache(root string, maxBytes int64, fallbackTTL time.Duration) (*pageCache, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory")
	}
	lstat, err := os.Lstat(root)
	if err != nil || lstat.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("cache directory must not be a symlink")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("cache directory is not a directory")
	}
	if err := ensureOwnedRoot(root); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("protect cache directory")
	}
	lease, err := lockCacheRoot(root)
	if err != nil {
		return nil, err
	}
	cache := &pageCache{
		root:        root,
		maxBytes:    maxBytes,
		fallbackTTL: fallbackTTL,
		entries:     make(map[string]*cacheEntry),
		lease:       lease,
		pending:     make(map[string]int64),
		removeAll:   os.RemoveAll,
	}
	if err := cache.removeOwnedDirectories(); err != nil {
		cache.close()
		return nil, err
	}
	return cache, nil
}

func (c *pageCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	// The runtime drains requests, jobs and the sweeper before releasing its lease.
	for _, entry := range c.entries {
		c.removeLocked(entry)
	}
	c.retryPendingLocked()
	if c.lease != nil {
		_ = c.lease.Close()
		c.lease = nil
	}
	c.closed = true
}

func ensureOwnedRoot(root string) error {
	marker := filepath.Join(root, cacheOwnerMarker)
	info, err := os.Lstat(marker)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() > int64(len(cacheOwnerContents)) {
			return fmt.Errorf("cache ownership marker is invalid")
		}
		file, openErr := os.Open(marker)
		if openErr != nil {
			return fmt.Errorf("read cache ownership marker")
		}
		contents, readErr := io.ReadAll(io.LimitReader(file, int64(len(cacheOwnerContents)+1)))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || string(contents) != cacheOwnerContents {
			return fmt.Errorf("cache ownership marker is invalid")
		}
		if err := os.Chmod(marker, 0o600); err != nil {
			return fmt.Errorf("protect cache ownership marker")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect cache ownership marker")
	}
	items, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("inspect cache directory")
	}
	if len(items) != 0 {
		return fmt.Errorf("cache directory is not plugin-owned")
	}
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return ensureOwnedRoot(root)
		}
		return fmt.Errorf("create cache ownership marker")
	}
	if _, err := io.WriteString(file, cacheOwnerContents); err != nil {
		_ = file.Close()
		_ = os.Remove(marker)
		return fmt.Errorf("write cache ownership marker")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(marker)
		return fmt.Errorf("close cache ownership marker")
	}
	return nil
}

func (c *pageCache) removeOwnedDirectories() error {
	items, err := os.ReadDir(c.root)
	if err != nil {
		return err
	}
	for _, item := range items {
		name := item.Name()
		if !item.IsDir() || (!strings.HasPrefix(name, ".entry-") && !strings.HasPrefix(name, ".job-")) {
			continue
		}
		path := filepath.Join(c.root, name)
		if c.ownedPath(path) {
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove expired cache directory")
			}
		}
	}
	return nil
}

func (c *pageCache) ownedPath(path string) bool {
	rel, err := filepath.Rel(c.root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return filepath.Base(rel) != ""
}

func (c *pageCache) lookup(key string, now time.Time) (*pageHandle, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || c.closed {
		return nil, false
	}
	if entry.retired || entry.expired(now) {
		c.removeLocked(entry)
		return nil, false
	}
	for _, page := range entry.pages {
		info, err := os.Stat(page.file)
		if err != nil || !info.Mode().IsRegular() || info.Size() != page.size {
			c.removeLocked(entry)
			return nil, false
		}
	}
	entry.active++
	entry.lastUsed = now
	return &pageHandle{entry: entry, cache: c}, true
}

func (c *pageCache) reserve(bytes int64, now time.Time) (*cacheReservation, error) {
	if bytes < 1 || bytes > c.maxBytes {
		return nil, errCacheFull
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errCacheFull
	}
	c.sweepLocked(now)
	if len(c.pending) >= maxPendingCleanup {
		return nil, errCacheFull
	}
	c.evictUntilLocked(bytes)
	if c.usedBytes+c.reserved+bytes > c.maxBytes {
		return nil, errCacheFull
	}
	c.reserved += bytes
	return &cacheReservation{cache: c, bytes: bytes}, nil
}

func (r *cacheReservation) release() {
	if r == nil || r.cache == nil || r.used {
		return
	}
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	if r.jobDir != "" && (!r.cache.ownedDirectory(r.jobDir, ".job-") || r.cache.removeAll(r.jobDir) != nil) {
		// Keep the full remaining reservation charged until a sweep can delete
		// the directory. Pending records contain owned paths and byte counts only.
		r.cache.pending[r.jobDir] = r.bytes
	} else {
		r.cache.reserved -= r.bytes
	}
	r.used = true
}

func (r *cacheReservation) commit(key, fingerprint, outputDir string, pages []archive.Page, now time.Time) (*cacheEntry, error) {
	if r == nil || r.cache == nil || r.used || r.committed {
		return nil, errCacheFull
	}
	c := r.cache
	if len(pages) == 0 {
		return nil, fmt.Errorf("no pages extracted")
	}
	converted := make([]cachedPage, 0, len(pages))
	var total int64
	for _, page := range pages {
		if !validPageFile(outputDir, page.File) || page.Size < 1 || page.Size > MaxPageBytes {
			return nil, fmt.Errorf("invalid extracted page")
		}
		linkInfo, err := os.Lstat(page.File)
		if err != nil || !linkInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("invalid extracted page")
		}
		info, err := os.Stat(page.File)
		if err != nil || !info.Mode().IsRegular() || info.Size() != page.Size {
			return nil, fmt.Errorf("invalid extracted page")
		}
		if total > MaxTotalBytes-page.Size {
			return nil, fmt.Errorf("extracted pages exceed total limit")
		}
		total += page.Size
		converted = append(converted, cachedPage{
			name:      page.Name,
			file:      filepath.Join(outputDir, filepath.Base(page.File)),
			size:      page.Size,
			mediaType: page.MediaType,
		})
	}
	if total > r.bytes {
		return nil, errCacheFull
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || normalizeKey(key) == "" {
		return nil, errCacheFull
	}
	if existing := c.entries[key]; existing != nil {
		if existing.active > 0 || !c.removeLocked(existing) {
			return nil, errCacheFull
		}
	}
	entryRoot := filepath.Join(c.root, ".entry-"+key)
	if !c.ownedPath(entryRoot) || !c.ownedPath(outputDir) {
		return nil, errCacheFull
	}
	if err := os.Rename(outputDir, entryRoot); err != nil {
		return nil, fmt.Errorf("publish cache entry")
	}
	for i := range converted {
		converted[i].file = filepath.Join(entryRoot, filepath.Base(converted[i].file))
	}
	entry := &cacheEntry{
		key:       key,
		root:      entryRoot,
		pages:     converted,
		total:     total,
		lastUsed:  now,
		expiresAt: fallbackExpiry(fingerprint, now, c.fallbackTTL),
	}
	if total > 0 {
		c.usedBytes += total
	}
	// The input archive and any other job files are still covered by the
	// remaining reservation. Only release it after job-directory cleanup.
	c.reserved -= total
	r.bytes -= total
	r.committed = true
	c.entries[key] = entry
	return entry, nil
}

func fallbackExpiry(fingerprint string, now time.Time, ttl time.Duration) time.Time {
	if fingerprint == "" {
		return now.Add(ttl)
	}
	return time.Time{}
}

func (c *pageCache) evictUntilLocked(incoming int64) {
	for c.usedBytes+c.reserved+incoming > c.maxBytes {
		var oldest *cacheEntry
		for _, entry := range c.entries {
			if entry.active > 0 || entry.retired {
				continue
			}
			if oldest == nil || entry.lastUsed.Before(oldest.lastUsed) {
				oldest = entry
			}
		}
		if oldest == nil {
			return
		}
		c.removeLocked(oldest)
	}
}

func (c *pageCache) removeLocked(entry *cacheEntry) bool {
	if entry == nil {
		return true
	}
	entry.retired = true
	if entry.active > 0 || !c.ownedDirectory(entry.root, ".entry-") {
		return false
	}
	if err := c.removeAll(entry.root); err != nil {
		return false
	}
	delete(c.entries, entry.key)
	c.usedBytes -= entry.total
	return true
}

func (entry *cacheEntry) expired(now time.Time) bool {
	return !now.Before(entry.lastUsed.Add(CacheIdleTTL)) ||
		(!entry.expiresAt.IsZero() && !now.Before(entry.expiresAt))
}

// sweep is also called without request traffic by the runtime's timer.
func (c *pageCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.sweepLocked(now)
	}
}

func (c *pageCache) sweepLocked(now time.Time) {
	c.retryPendingLocked()
	for _, entry := range c.entries {
		if entry.retired || entry.expired(now) {
			c.removeLocked(entry)
		}
	}
}

func (c *pageCache) retryPendingLocked() {
	for path, bytes := range c.pending {
		if c.ownedDirectory(path, ".job-") && c.removeAll(path) == nil {
			delete(c.pending, path)
			c.reserved -= bytes
		}
	}
}

// Cleanup can delete only a direct, plugin-named child of the leased cache
// root. It never deletes the configured root, marker, lock or caller paths.
func (c *pageCache) ownedDirectory(path, prefix string) bool {
	return filepath.Dir(path) == c.root && strings.HasPrefix(filepath.Base(path), prefix) &&
		len(filepath.Base(path)) > len(prefix)
}

func validPageFile(outputDir, pagePath string) bool {
	if outputDir == "" || pagePath == "" {
		return false
	}
	rel, err := filepath.Rel(outputDir, pagePath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return filepath.Dir(rel) == "." && filepath.Base(rel) == rel
}
