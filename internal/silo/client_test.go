package silo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type delayedResponseBody struct {
	ctx     context.Context
	started chan struct{}
	release chan struct{}
	once    sync.Once
	data    []byte
	done    bool
}

func (b *delayedResponseBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	select {
	case <-b.release:
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	return copy(p, b.data), nil
}

func (b *delayedResponseBody) Close() error { return nil }

func TestUpstreamAuthorizationUsesFixedSameOriginPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("authorization = %q", got)
		}
		if r.URL.Path == "/api/v1/auth/me" || r.URL.Path == "/api/v2/account/me" {
			_, _ = w.Write([]byte(`{"id":"7"}`))
			return
		}
		if r.URL.Path == "/api/v1/catalog/items/content" {
			_, _ = w.Write([]byte(`{"versions":[{"file_id":"2"}]}`))
			return
		}
		if r.URL.Path == "/api/v1/ebooks/content/files/2/read" && r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "3")
			w.Header().Set("ETag", `"e"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := newUpstreamClient(server.URL, MaxArchiveBytes)
	if err != nil {
		t.Fatal(err)
	}
	userID, err := client.authenticate(t.Context(), "v1", "token")
	if err != nil || userID != "7" {
		t.Fatalf("authenticate = %q, %v", userID, err)
	}
	v2UserID, err := client.authenticate(t.Context(), "v2", "token")
	if err != nil || v2UserID != "7" {
		t.Fatalf("v2 authenticate = %q, %v", v2UserID, err)
	}
	if err := client.authorizeItem(t.Context(), "v1", "token", "p1", "profile-token", "content", 2); err != nil {
		t.Fatal(err)
	}
	file, err := client.headFile(t.Context(), "v1", "token", "p1", "profile-token", "content", 2)
	if err != nil {
		t.Fatal(err)
	}
	if file.Size != 3 || file.ETag != `"e"` {
		t.Fatalf("head file = %+v", file)
	}
}

func TestRequestAcceptsStringFileID(t *testing.T) {
	var request Request
	if err := request.UnmarshalJSON(bytes.NewBufferString(`{"token":"t","profile_id":"p","content_id":"c","file_id":"2","api_version":"v1"}`).Bytes()); err != nil {
		t.Fatal(err)
	}
	if request.FileID != 2 {
		t.Fatalf("file id = %d", request.FileID)
	}
}

func TestCatalogFileIDAcceptsNumberAndString(t *testing.T) {
	for _, raw := range []string{`2`, `"2"`} {
		var response catalogResponse
		if err := json.Unmarshal([]byte(`{"versions":[{"file_id":`+raw+`}]}`), &response); err != nil {
			t.Fatal(err)
		}
		fileID, err := parseJSONInt64(response.Versions[0].FileID)
		if err != nil || fileID != 2 {
			t.Fatalf("file id %s = %d, %v", raw, fileID, err)
		}
	}
}

func TestResponseBodyContextStaysLiveUntilBodyClose(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var responseBody *delayedResponseBody
	client, err := newUpstreamClient("http://silo.test", MaxArchiveBytes)
	if err != nil {
		t.Fatal(err)
	}
	client.timeout = 0
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		responseBody = &delayedResponseBody{
			ctx:     request.Context(),
			started: started,
			release: release,
			data:    []byte(`{"id":7}`),
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          responseBody,
			ContentLength: 8,
			Request:       request,
		}, nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := client.authenticate(context.Background(), "v1", "token")
		done <- err
	}()
	<-started
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamClientDoesNotFollowCrossOriginRedirect(t *testing.T) {
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	client, err := newUpstreamClient(redirect.URL, MaxArchiveBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.authenticate(context.Background(), "v1", "token"); err == nil {
		t.Fatal("redirect unexpectedly authenticated")
	}
	if targetCalls != 0 {
		t.Fatalf("redirect target calls = %d, want zero", targetCalls)
	}
}
