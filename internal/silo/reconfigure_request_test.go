package silo

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowquillx/silo-comic-pages/internal/archive"
)

func TestReconfigureCancelsAndDrainsRequestsBeforeReplacingCache(t *testing.T) {
	for _, method := range []string{"Page", "Pages"} {
		t.Run(method, func(t *testing.T) {
			pageA, pageB := []byte("image-A"), []byte("image-B")
			upstreamA := &testUpstream{etag: `"same"`, getETag: `"same"`, body: []byte("archive-A")}
			backend, oldRuntime, _ := newTestBackend(t, upstreamA, pageA, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			t.Cleanup(cancel)
			listedA, err := backend.Pages(ctx, testRequest("", 0), "7")
			if err != nil {
				t.Fatal(err)
			}
			oldRuntime.cache.mu.Lock()
			oldPagePath := oldRuntime.cache.entries[listedA.CacheKey].pages[0].file
			oldRuntime.cache.mu.Unlock()

			// B deliberately has the same fingerprint and output size, but accepts
			// a different token and supplies different image bytes.
			upstreamB := &testUpstream{authID: "7", etag: `"same"`, getETag: `"same"`, body: []byte("archive-B")}
			var wrongTokenAtB atomic.Int32
			serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer token-B" {
					wrongTokenAtB.Add(1)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				forwarded := r.Clone(r.Context())
				forwarded.Header.Set("Authorization", "Bearer token")
				upstreamB.ServeHTTP(w, forwarded)
			}))
			t.Cleanup(serverB.Close)

			authStarted, authCancelled := make(chan struct{}), make(chan struct{})
			releaseAuth, allowUnwind := make(chan struct{}), make(chan struct{})
			var releaseOnce, unwindOnce sync.Once
			t.Cleanup(func() {
				cancel()
				releaseOnce.Do(func() { close(releaseAuth) })
				unwindOnce.Do(func() { close(allowUnwind) })
			})
			transport := oldRuntime.client.http.Transport
			// Make cancellation come from reconfiguration, not the metadata timer.
			oldRuntime.client.timeout = 30 * time.Second
			oldRuntime.client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if err != nil || r.URL.Path != "/api/v1/auth/me" {
					return response, err
				}
				close(authStarted)
				select {
				case <-r.Context().Done():
					close(authCancelled)
					// Keep the handler alive after cancellation to prove Configure
					// drains it before releasing the cache directory for reuse.
					select {
					case <-allowUnwind:
					case <-ctx.Done():
					}
					_ = response.Body.Close()
					return nil, r.Context().Err()
				case <-releaseAuth:
					return response, nil
				}
			})
			t.Cleanup(func() {
				if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
					closer.CloseIdleConnections()
				}
			})

			type outcome struct {
				result Result
				err    error
			}
			oldDone := make(chan outcome, 1)
			go func() {
				request := testRequest(listedA.CacheKey, 0)
				var result Result
				var err error
				if method == "Page" {
					result, err = backend.Page(ctx, request, "7", 0)
				} else {
					result, err = backend.Pages(ctx, request, "7")
				}
				oldDone <- outcome{result: result, err: err}
			}()
			select {
			case <-authStarted:
			case <-ctx.Done():
				t.Fatal("old request never reached account authorization")
			}

			configured := make(chan error, 1)
			go func() {
				configured <- backend.Configure(ctx, Config{BaseURL: serverB.URL, CacheDir: oldRuntime.cfg.CacheDir})
			}()
			select {
			case <-authCancelled:
			case err := <-configured:
				t.Fatalf("Configure completed before cancelling the paused old request: %v", err)
			case <-ctx.Done():
				t.Fatal("Configure did not cancel the old request")
			}
			select {
			case err := <-configured:
				t.Fatalf("Configure completed before the cancelled handler drained: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			if contents, err := os.ReadFile(oldPagePath); err != nil || !bytes.Equal(contents, pageA) {
				t.Fatalf("old cache was changed while its handler was still pinned: %q, %v", contents, err)
			}
			unwindOnce.Do(func() { close(allowUnwind) })
			var old outcome
			select {
			case old = <-oldDone:
			case <-ctx.Done():
				t.Fatal("cancelled old request did not drain")
			}
			apiStatus(t, old.err, http.StatusServiceUnavailable)
			if old.result.Status == http.StatusOK || len(old.result.Body) != 0 || len(old.result.Pages) != 0 {
				t.Fatalf("old request returned cache data after cancellation: %+v", old.result)
			}
			select {
			case err := <-configured:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("Configure did not complete after the old handler drained")
			}

			backend.mu.RLock()
			newRuntime := backend.current
			backend.mu.RUnlock()
			newRuntime.extractor = func(_ context.Context, _, outputDir string, _ archive.Limits) ([]archive.Page, error) {
				if err := os.MkdirAll(outputDir, 0o700); err != nil {
					return nil, err
				}
				path := filepath.Join(outputDir, "page-000001")
				if err := os.WriteFile(path, pageB, 0o600); err != nil {
					return nil, err
				}
				return []archive.Page{{Name: "page.png", File: path, Size: int64(len(pageB)), MediaType: "image/png"}}, nil
			}
			requestB := testRequest("", 0)
			requestB.Token = "token-B"
			listedB, err := backend.Pages(ctx, requestB, "7")
			if err != nil {
				t.Fatal(err)
			}
			if listedB.CacheKey != listedA.CacheKey {
				t.Fatal("fixture did not reproduce the matching cache keys")
			}
			if contents, err := os.ReadFile(oldPagePath); err != nil || !bytes.Equal(contents, pageB) {
				t.Fatalf("B did not replace the original cache path: %q, %v", contents, err)
			}
			// Releasing A's original auth gate cannot revive its completed handler.
			releaseOnce.Do(func() { close(releaseAuth) })
			requestB.CacheKey = listedB.CacheKey
			resultB, err := backend.Page(ctx, requestB, "7", 0)
			if err != nil || !bytes.Equal(resultB.Body, pageB) {
				t.Fatalf("authorized B page = %q, %v", resultB.Body, err)
			}
			if wrongTokenAtB.Load() != 0 || upstreamB.downloads.Load() != 1 {
				t.Fatalf("unexpected B access: wrong token=%d downloads=%d", wrongTokenAtB.Load(), upstreamB.downloads.Load())
			}
		})
	}
}
