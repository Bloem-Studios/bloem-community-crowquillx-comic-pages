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
	cache *pageCache
	bytes int64
	used  bool
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
	}
	if err := cache.removeOwnedDirectories(); err != nil {
		cache.close()
		return nil, err
	}
	return cache, nil
}

func (c *pageCache) close() {
	if c.lease != nil {
		_ = c.lease.Close()
		c.lease = nil
	}
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
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
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
	c.evictUntilLocked(bytes, now)
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
	if r.bytes > 0 {
		r.cache.reserved -= r.bytes
	}
	r.used = true
	r.cache.mu.Unlock()
}

func (r *cacheReservation) commit(key, fingerprint, outputDir string, pages []archive.Page, now time.Time) (*cacheEntry, error) {
	if r == nil || r.cache == nil || r.used {
		return nil, errCacheFull
	}
	c := r.cache
	if len(pages) == 0 {
		r.release()
		return nil, fmt.Errorf("no pages extracted")
	}
	converted := make([]cachedPage, 0, len(pages))
	var total int64
	for _, page := range pages {
		if !validPageFile(outputDir, page.File) || page.Size < 1 || page.Size > MaxPageBytes {
			r.release()
			return nil, fmt.Errorf("invalid extracted page")
		}
		linkInfo, err := os.Lstat(page.File)
		if err != nil || !linkInfo.Mode().IsRegular() {
			r.release()
			return nil, fmt.Errorf("invalid extracted page")
		}
		info, err := os.Stat(page.File)
		if err != nil || !info.Mode().IsRegular() || info.Size() != page.Size {
			r.release()
			return nil, fmt.Errorf("invalid extracted page")
		}
		if total > MaxTotalBytes-page.Size {
			r.release()
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
		r.release()
		return nil, errCacheFull
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.entries[key]; existing != nil {
		if existing.active > 0 {
			c.reserved -= r.bytes
			r.used = true
			return nil, errCacheFull
		}
		c.removeLocked(existing)
	}
	entryRoot := filepath.Join(c.root, ".entry-"+key)
	if !c.ownedPath(entryRoot) || !c.ownedPath(outputDir) {
		c.reserved -= r.bytes
		r.used = true
		return nil, errCacheFull
	}
	if err := os.Rename(outputDir, entryRoot); err != nil {
		c.reserved -= r.bytes
		r.used = true
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
	c.reserved -= r.bytes
	r.used = true
	c.entries[key] = entry
	return entry, nil
}

func fallbackExpiry(fingerprint string, now time.Time, ttl time.Duration) time.Time {
	if fingerprint == "" {
		return now.Add(ttl)
	}
	return time.Time{}
}

func (c *pageCache) evictUntilLocked(incoming int64, now time.Time) {
	for c.usedBytes+c.reserved+incoming > c.maxBytes {
		var oldest *cacheEntry
		for _, entry := range c.entries {
			if entry.active > 0 || (!entry.expiresAt.IsZero() && !now.Before(entry.expiresAt)) {
				if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) && entry.active == 0 {
					oldest = entry
					break
				}
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

func (c *pageCache) removeLocked(entry *cacheEntry) {
	if entry == nil || entry.active > 0 {
		return
	}
	delete(c.entries, entry.key)
	if entry.total <= c.usedBytes {
		c.usedBytes -= entry.total
	} else {
		c.usedBytes = 0
	}
	if c.ownedPath(entry.root) {
		_ = os.RemoveAll(entry.root)
	}
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
