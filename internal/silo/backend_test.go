package silo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowquillx/silo-comic-pages/internal/archive"
)

type testUpstream struct {
	mu            sync.Mutex
	authStatus    int
	authID        string
	catalogStatus int
	headStatus    int
	etag          string
	getETag       string
	lastModified  string
	body          []byte
	authBlock     bool
	releaseAuth   chan struct{}
	downloads     atomic.Int32
	unexpected    atomic.Int32
}

func (u *testUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	authStatus, authID, catalogStatus, headStatus := u.authStatus, u.authID, u.catalogStatus, u.headStatus
	etag, getETag, lastModified, body := u.etag, u.getETag, u.lastModified, append([]byte(nil), u.body...)
	authBlock, releaseAuth := u.authBlock, u.releaseAuth
	u.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/api/v1/auth/me" {
		if authBlock {
			select {
			case <-r.Context().Done():
				return
			case <-releaseAuth:
			}
		}
		if authStatus != 0 {
			w.WriteHeader(authStatus)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%q}`, authID)
		return
	}
	if r.Header.Get("X-Profile-Id") != "p1" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/api/v1/catalog/items/content":
		if catalogStatus != 0 {
			w.WriteHeader(catalogStatus)
			return
		}
		_, _ = fmt.Fprint(w, `{"versions":[{"file_id":"2"}]}`)
		return
	case "/api/v1/ebooks/content/files/2/read":
		if r.Method == http.MethodHead {
			if headStatus != 0 {
				w.WriteHeader(headStatus)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("ETag", etag)
			w.Header().Set("Last-Modified", lastModified)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet {
			u.downloads.Add(1)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("ETag", getETag)
			w.Header().Set("Last-Modified", lastModified)
			_, _ = w.Write(body)
			return
		}
	}
	u.unexpected.Add(1)
	http.NotFound(w, r)
}

func newTestBackend(t *testing.T, upstream *testUpstream, page []byte, configure func(*Config)) (*Backend, *runtime, *atomic.Int32) {
	t.Helper()
	if upstream.body == nil {
		upstream.body = []byte("archive")
	}
	if upstream.authID == "" {
		upstream.authID = "7"
	}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	cfg := Config{BaseURL: server.URL, CacheDir: t.TempDir()}
	if configure != nil {
		configure(&cfg)
	}
	backend := NewBackend()
	if err := backend.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	backend.mu.RLock()
	runtime := backend.current
	backend.mu.RUnlock()
	var extracted atomic.Int32
	runtime.extractor = func(ctx context.Context, _, outputDir string, _ archive.Limits) ([]archive.Page, error) {
		extracted.Add(1)
		select {
		case <-time.After(25 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := os.MkdirAll(outputDir, 0o700); err != nil {
			return nil, err
		}
		path := filepath.Join(outputDir, "page-000001")
		if err := os.WriteFile(path, page, 0o600); err != nil {
			return nil, err
		}
		return []archive.Page{{Name: "page.png", File: path, Size: int64(len(page)), MediaType: "image/png"}}, nil
	}
	t.Cleanup(backend.Close)
	return backend, runtime, &extracted
}

func testRequest(cacheKey string, offset uint64) Request {
	return Request{
		Token:      "token",
		ProfileID:  "p1",
		ContentID:  "content",
		FileID:     2,
		APIVersion: "v1",
		CacheKey:   cacheKey,
		Offset:     offset,
	}
}

func apiStatus(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != status {
		t.Fatalf("error = %v, want API status %d", err, status)
	}
}

func TestBackendRejectsIdentityMismatchBeforeItemAuthorization(t *testing.T) {
	upstream := &testUpstream{}
	upstream.authID = "8"
	backend, _, extracted := newTestBackend(t, upstream, []byte("page"), nil)
	_, err := backend.Pages(context.Background(), testRequest("", 0), "7")
	apiStatus(t, err, http.StatusForbidden)
	if extracted.Load() != 0 || upstream.downloads.Load() != 0 || upstream.unexpected.Load() != 0 {
		t.Fatalf("identity mismatch reached later stages: extracted=%d downloads=%d unexpected=%d", extracted.Load(), upstream.downloads.Load(), upstream.unexpected.Load())
	}
}

func TestBackendReauthorizesCachedPageBeforeServing(t *testing.T) {
	upstream := &testUpstream{}
	backend, _, _ := newTestBackend(t, upstream, []byte("page"), nil)
	listed, err := backend.Pages(context.Background(), testRequest("", 0), "7")
	if err != nil {
		t.Fatal(err)
	}
	upstream.mu.Lock()
	upstream.catalogStatus = http.StatusForbidden
	upstream.mu.Unlock()
	_, err = backend.Page(context.Background(), testRequest(listed.CacheKey, 0), "7", 0)
	apiStatus(t, err, http.StatusForbidden)

	upstream.mu.Lock()
	upstream.catalogStatus = 0
	upstream.headStatus = http.StatusForbidden
	upstream.mu.Unlock()
	_, err = backend.Page(context.Background(), testRequest(listed.CacheKey, 0), "7", 0)
	apiStatus(t, err, http.StatusForbidden)
}

func TestBackendDeduplicatesConcurrentPreparation(t *testing.T) {
	upstream := &testUpstream{}
	backend, _, extracted := newTestBackend(t, upstream, []byte("page"), nil)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := backend.Pages(context.Background(), testRequest("", 0), "7")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if extracted.Load() != 1 || upstream.downloads.Load() != 1 {
		t.Fatalf("preparation was not deduplicated: extracted=%d downloads=%d", extracted.Load(), upstream.downloads.Load())
	}
}

func TestBackendCacheLimitRejectsBeforePublishing(t *testing.T) {
	upstream := &testUpstream{}
	backend, _, extracted := newTestBackend(t, upstream, []byte("page"), func(cfg *Config) {
		cfg.CacheSize = 3
	})
	_, err := backend.Pages(context.Background(), testRequest("", 0), "7")
	apiStatus(t, err, http.StatusServiceUnavailable)
	if extracted.Load() != 0 || upstream.downloads.Load() != 0 {
		t.Fatalf("cache reservation did not precede download: extracted=%d downloads=%d", extracted.Load(), upstream.downloads.Load())
	}
}

func TestBackendRejectsSecondExtractionWhileOneIsRunning(t *testing.T) {
	upstream := &testUpstream{}
	_, runtime, _ := newTestBackend(t, upstream, []byte("page"), nil)
	firstKey := strings.Repeat("a", 64)
	secondKey := strings.Repeat("b", 64)
	first, err := runtime.getOrStartJob(authorizedRequest{
		request:  Request{Token: "token", ProfileID: "p1", ContentID: "content", FileID: 2, APIVersion: "v1"},
		file:     upstreamFile{Size: int64(len(upstream.body))},
		cacheKey: firstKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.getOrStartJob(authorizedRequest{
		request:  Request{Token: "token", ProfileID: "p1", ContentID: "content-2", FileID: 2, APIVersion: "v1"},
		file:     upstreamFile{Size: int64(len(upstream.body))},
		cacheKey: secondKey,
	}); err == nil {
		t.Fatal("second extraction was admitted while the first was running")
	} else {
		apiStatus(t, err, http.StatusServiceUnavailable)
	}
	<-first.done
}

func TestBackendReconfigureWaitsBeforeCleaningOldCacheJobs(t *testing.T) {
	upstream := &testUpstream{}
	backend, runtime, _ := newTestBackend(t, upstream, []byte("page"), nil)
	started := make(chan struct{})
	release := make(chan struct{})
	runtime.extractor = func(context.Context, string, string, archive.Limits) ([]archive.Page, error) {
		close(started)
		<-release
		return nil, errors.New("stop after lifecycle check")
	}
	preparing := make(chan error, 1)
	go func() {
		_, err := backend.Pages(context.Background(), testRequest("", 0), "7")
		preparing <- err
	}()
	<-started

	reconfigured := make(chan error, 1)
	go func() {
		reconfigured <- backend.Configure(context.Background(), Config{
			BaseURL:  runtime.client.base.String(),
			CacheDir: runtime.cfg.CacheDir,
		})
	}()
	select {
	case <-reconfigured:
		t.Fatal("reconfigure cleaned the old job before it stopped")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-reconfigured; err != nil {
		t.Fatal(err)
	}
	if err := <-preparing; err == nil {
		t.Fatal("old preparation unexpectedly succeeded")
	}
	items, err := os.ReadDir(runtime.cfg.CacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Name() != cacheOwnerMarker && item.Name() != ".lock" {
			t.Fatalf("old cache artifact remained after reconfigure: %s", item.Name())
		}
	}
}

func TestBackendTimeoutDoesNotStartExtraction(t *testing.T) {
	upstream := &testUpstream{authBlock: true, releaseAuth: make(chan struct{})}
	backend, runtime, extracted := newTestBackend(t, upstream, []byte("page"), nil)
	runtime.client.timeout = 20 * time.Millisecond
	_, err := backend.Pages(context.Background(), testRequest("", 0), "7")
	apiStatus(t, err, http.StatusServiceUnavailable)
	if extracted.Load() != 0 || upstream.downloads.Load() != 0 {
		t.Fatalf("timeout started work: extracted=%d downloads=%d", extracted.Load(), upstream.downloads.Load())
	}
}

func TestBackendReassemblesUnchangedImageChunks(t *testing.T) {
	page := bytes.Repeat([]byte("0123456789abcdef"), (2*int(ChunkBytes)+123)/16+1)
	page = page[:2*int(ChunkBytes)+123]
	upstream := &testUpstream{}
	backend, _, _ := newTestBackend(t, upstream, page, nil)
	listed, err := backend.Pages(context.Background(), testRequest("", 0), "7")
	if err != nil {
		t.Fatal(err)
	}
	var assembled []byte
	for offset := 0; offset < len(page); offset += int(ChunkBytes) {
		result, err := backend.Page(context.Background(), testRequest(listed.CacheKey, uint64(offset)), "7", 0)
		if err != nil {
			t.Fatal(err)
		}
		assembled = append(assembled, result.Body...)
	}
	if !bytes.Equal(assembled, page) {
		t.Fatal("chunk response changed image bytes")
	}
}

func TestBackendRejectsChangedCacheKeyAndSourceValidators(t *testing.T) {
	upstream := &testUpstream{etag: `"v1"`, getETag: `"v1"`, lastModified: "Mon, 01 Jan 2024 00:00:00 GMT"}
	backend, _, extracted := newTestBackend(t, upstream, []byte("page"), nil)
	listed, err := backend.Pages(context.Background(), testRequest("", 0), "7")
	if err != nil {
		t.Fatal(err)
	}
	upstream.mu.Lock()
	upstream.etag = `"v2"`
	upstream.getETag = `"v2"`
	upstream.mu.Unlock()
	_, err = backend.Page(context.Background(), testRequest(listed.CacheKey, 0), "7", 0)
	apiStatus(t, err, http.StatusConflict)
	if extracted.Load() != 1 {
		t.Fatalf("changed cache key triggered extraction: %d", extracted.Load())
	}

	upstream.mu.Lock()
	upstream.etag = `"v3"`
	upstream.getETag = `"v4"`
	upstream.mu.Unlock()
	_, err = backend.Pages(context.Background(), testRequest("", 0), "7")
	apiStatus(t, err, http.StatusConflict)
	if extracted.Load() != 1 {
		t.Fatalf("changed source triggered extraction: %d", extracted.Load())
	}
}
