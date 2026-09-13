package silo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type upstreamClient struct {
	base    *url.URL
	http    *http.Client
	limit   int64
	timeout time.Duration
}

type upstreamFile struct {
	Size         int64
	ETag         string
	LastModified string
}

type catalogResponse struct {
	Versions []struct {
		FileID json.RawMessage `json:"file_id"`
	} `json:"versions"`
}

type upstreamDownload struct {
	Body         io.ReadCloser
	Size         int64
	ETag         string
	LastModified string
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *cancelReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}

func newUpstreamClient(baseURL string, archiveLimit int64) (*upstreamClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url")
	}
	return &upstreamClient{
		base: parsed,
		http: &http.Client{
			// Silo file URLs are same-origin and must never redirect to a
			// caller-controlled host while carrying a bearer token.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{Proxy: nil},
		},
		limit:   archiveLimit,
		timeout: UpstreamTimeout,
	}, nil
}

func (c *upstreamClient) endpoint(version, path string) (*url.URL, error) {
	if version != "v1" && version != "v2" {
		return nil, apiError(422, "invalid_api_version")
	}
	if path == "" || strings.HasPrefix(path, "/") == false {
		return nil, apiError(422, "invalid_upstream_path")
	}
	copyURL := *c.base
	copyURL.Path = "/api/" + version + path
	copyURL.RawPath = ""
	copyURL.RawQuery = ""
	copyURL.Fragment = ""
	return &copyURL, nil
}

func (c *upstreamClient) do(ctx context.Context, method, version, path, token, profileID, profileToken string) (*http.Response, error) {
	return c.doWithTimeout(ctx, c.timeout, method, version, path, token, profileID, profileToken)
}

func (c *upstreamClient) doWithTimeout(ctx context.Context, timeout time.Duration, method, version, path, token, profileID, profileToken string) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint, err := c.endpoint(version, path)
	if err != nil {
		return nil, err
	}
	requestContext, cancel := context.WithCancel(ctx)
	if timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, timeout)
	}
	req, err := http.NewRequestWithContext(requestContext, method, endpoint.String(), nil)
	if err != nil {
		cancel()
		return nil, apiError(503, "upstream_unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if profileID != "" {
		req.Header.Set("X-Profile-Id", profileID)
	}
	if profileToken != "" {
		req.Header.Set("X-Profile-Token", profileToken)
	}
	response, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, apiError(503, "upstream_unavailable")
	}
	response.Body = &cancelReadCloser{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func (c *upstreamClient) authenticate(ctx context.Context, version, token string) (string, error) {
	path := "/auth/me"
	if version == "v2" {
		path = "/account/me"
	}
	response, err := c.do(ctx, http.MethodGet, version, path, token, "", "")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return "", apiError(http.StatusUnauthorized, "unauthorized")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", apiError(503, "upstream_unavailable")
	}
	body, err := readBounded(response.Body, 1<<20)
	if err != nil {
		return "", apiError(503, "upstream_unavailable")
	}
	var value struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return "", apiError(503, "upstream_unavailable")
	}
	userID, err := jsonUserID(value.ID)
	if err != nil {
		return "", apiError(503, "upstream_unavailable")
	}
	return userID, nil
}

func jsonUserID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("missing id")
	}
	var text string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", err
		}
	} else {
		text = string(raw)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("empty id")
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value <= 0 {
		return "", fmt.Errorf("invalid id")
	}
	return strconv.FormatInt(value, 10), nil
}

func (c *upstreamClient) authorizeItem(ctx context.Context, version, token, profileID, profileToken, contentID string, fileID int64) error {
	path := "/catalog/items/" + contentID
	response, err := c.do(ctx, http.MethodGet, version, path, token, profileID, profileToken)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return apiError(http.StatusUnauthorized, "unauthorized")
	}
	if response.StatusCode == http.StatusForbidden {
		return apiError(http.StatusForbidden, "forbidden")
	}
	if response.StatusCode == http.StatusNotFound {
		return apiError(http.StatusNotFound, "not_found")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return apiError(503, "upstream_unavailable")
	}
	body, err := readBounded(response.Body, 4<<20)
	if err != nil {
		return apiError(503, "upstream_unavailable")
	}
	var item catalogResponse
	if err := json.Unmarshal(body, &item); err != nil {
		return apiError(503, "upstream_unavailable")
	}
	for _, version := range item.Versions {
		catalogFileID, parseErr := parseJSONInt64(version.FileID)
		if parseErr == nil && catalogFileID == fileID {
			return nil
		}
	}
	return apiError(http.StatusNotFound, "not_found")
}

func (c *upstreamClient) headFile(ctx context.Context, version, token, profileID, profileToken, contentID string, fileID int64) (upstreamFile, error) {
	path := "/ebooks/" + contentID + "/files/" + strconv.FormatInt(fileID, 10) + "/read"
	response, err := c.do(ctx, http.MethodHead, version, path, token, profileID, profileToken)
	if err != nil {
		return upstreamFile{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return upstreamFile{}, apiError(http.StatusUnauthorized, "unauthorized")
	}
	if response.StatusCode == http.StatusForbidden {
		return upstreamFile{}, apiError(http.StatusForbidden, "forbidden")
	}
	if response.StatusCode == http.StatusNotFound {
		return upstreamFile{}, apiError(http.StatusNotFound, "not_found")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return upstreamFile{}, apiError(503, "upstream_unavailable")
	}
	if response.ContentLength < 0 {
		return upstreamFile{}, apiError(503, "upstream_unavailable")
	}
	if response.ContentLength > c.limit {
		return upstreamFile{}, apiError(413, "archive_too_large")
	}
	return upstreamFile{
		Size:         response.ContentLength,
		ETag:         strings.TrimSpace(response.Header.Get("ETag")),
		LastModified: strings.TrimSpace(response.Header.Get("Last-Modified")),
	}, nil
}

func (c *upstreamClient) download(ctx context.Context, version, token, profileID, profileToken, contentID string, fileID int64) (upstreamDownload, error) {
	path := "/ebooks/" + contentID + "/files/" + strconv.FormatInt(fileID, 10) + "/read"
	response, err := c.doWithTimeout(ctx, 0, http.MethodGet, version, path, token, profileID, profileToken)
	if err != nil {
		return upstreamDownload{}, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		return upstreamDownload{}, apiError(http.StatusUnauthorized, "unauthorized")
	}
	if response.StatusCode == http.StatusForbidden {
		response.Body.Close()
		return upstreamDownload{}, apiError(http.StatusForbidden, "forbidden")
	}
	if response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		return upstreamDownload{}, apiError(http.StatusNotFound, "not_found")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return upstreamDownload{}, apiError(503, "upstream_unavailable")
	}
	if response.ContentLength > c.limit {
		response.Body.Close()
		return upstreamDownload{}, apiError(413, "archive_too_large")
	}
	return upstreamDownload{
		Body:         response.Body,
		Size:         response.ContentLength,
		ETag:         strings.TrimSpace(response.Header.Get("ETag")),
		LastModified: strings.TrimSpace(response.Header.Get("Last-Modified")),
	}, nil
}

func readBounded(reader io.Reader, max int64) ([]byte, error) {
	if max < 1 {
		return nil, fmt.Errorf("invalid response bound")
	}
	data, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("response too large")
	}
	return data, nil
}
