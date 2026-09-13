package silo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowquillx/silo-comic-pages/internal/archive"
)

// Request is the authenticated plugin request body. Token is deliberately
// kept out of cache keys and persistent structures.
type Request struct {
	Token        string `json:"token"`
	ProfileID    string `json:"profile_id"`
	ProfileToken string `json:"profile_token,omitempty"`
	ContentID    string `json:"content_id"`
	FileID       int64  `json:"file_id"`
	APIVersion   string `json:"api_version"`
	CacheKey     string `json:"cache_key,omitempty"`
	Offset       uint64 `json:"offset"`
}

// UnmarshalJSON accepts the catalog's numeric file IDs in either JSON form
// used by Silo clients. Some API versions serialize IDs as strings.
func (r *Request) UnmarshalJSON(data []byte) error {
	var wire struct {
		Token        string          `json:"token"`
		ProfileID    string          `json:"profile_id"`
		ProfileToken string          `json:"profile_token,omitempty"`
		ContentID    string          `json:"content_id"`
		FileID       json.RawMessage `json:"file_id"`
		APIVersion   string          `json:"api_version"`
		CacheKey     string          `json:"cache_key,omitempty"`
		Offset       uint64          `json:"offset"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	fileID, err := parseJSONInt64(wire.FileID)
	if err != nil {
		return err
	}
	*r = Request{
		Token:        wire.Token,
		ProfileID:    wire.ProfileID,
		ProfileToken: wire.ProfileToken,
		ContentID:    wire.ContentID,
		FileID:       fileID,
		APIVersion:   wire.APIVersion,
		CacheKey:     wire.CacheKey,
		Offset:       wire.Offset,
	}
	return nil
}

func parseJSONInt64(raw json.RawMessage) (int64, error) {
	text := strings.TrimSpace(string(raw))
	if len(text) >= 2 && text[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, err
		}
	}
	value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("file_id must be a positive integer")
	}
	return value, nil
}

// PageInfo is the metadata returned by POST /v1/pages.
type PageInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Result is an HTTP-shaped backend response. Binary page responses use Body;
// JSON responses use Pages or StatusJSON.
type Result struct {
	Status     int
	Pages      []PageInfo
	CacheKey   string
	Body       []byte
	StatusJSON string
}

type runtime struct {
	cfg       Config
	client    *upstreamClient
	cache     *pageCache
	jobsMu    sync.Mutex
	jobs      map[string]*extractionJob
	extract   chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closed    chan struct{}
	stopping  bool
	extractor func(context.Context, string, string, archive.Limits) ([]archive.Page, error)
}

type extractionJob struct {
	done chan struct{}
	err  error
}

type credentials struct {
	token        string
	profileID    string
	profileToken string
}

type authorizedRequest struct {
	request     Request
	file        upstreamFile
	fingerprint string
	cacheKey    string
}

// Backend owns the currently configured Silo connection. It is safe for the
// Runtime and HTTP capability servers to use concurrently.
type Backend struct {
	mu          sync.RWMutex
	lifecycleMu sync.Mutex
	current     *runtime
}

func NewBackend() *Backend {
	return &Backend{}
}

// Configure validates a complete replacement configuration before swapping it
// in. The old runtime is cancelled and joined before its cache root is
// reopened, so active requests and jobs cannot access a replacement cache.
func (b *Backend) Configure(_ context.Context, cfg Config) error {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return apiError(422, "invalid_configuration")
	}
	client, err := newUpstreamClient(normalized.BaseURL, normalized.Limits.MaxArchiveBytes)
	if err != nil {
		return apiError(422, "invalid_configuration")
	}
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	b.mu.Lock()
	previous := b.current
	b.current = nil
	b.mu.Unlock()
	if previous != nil {
		previous.close()
	}
	cache, err := newPageCache(normalized.CacheDir, normalized.CacheSize, normalized.CacheTTL)
	if err != nil {
		return apiError(503, "cache_unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	next := &runtime{
		cfg:       normalized,
		client:    client,
		cache:     cache,
		jobs:      make(map[string]*extractionJob),
		extract:   make(chan struct{}, 1),
		ctx:       ctx,
		cancel:    cancel,
		closed:    make(chan struct{}),
		extractor: runExtractorChild,
	}
	b.mu.Lock()
	b.current = next
	b.mu.Unlock()
	return nil
}

func (b *Backend) getRuntime() (*runtime, error) {
	b.mu.RLock()
	current := b.current
	b.mu.RUnlock()
	if current == nil {
		return nil, ErrNotConfigured
	}
	return current, nil
}

func (b *Backend) Close() {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	b.mu.Lock()
	current := b.current
	b.current = nil
	b.mu.Unlock()
	if current != nil {
		current.close()
	}
}

func (r *runtime) close() {
	r.jobsMu.Lock()
	r.stopping = true
	r.cancel()
	r.jobsMu.Unlock()
	r.wg.Wait()
	r.client.http.CloseIdleConnections()
	r.cache.close()
	close(r.closed)
}

// acquireRequest pins both the upstream connection and its cache until the
// entire handler returns. The shared admission lock prevents WaitGroup.Add
// from racing with shutdown's Wait after it has stopped accepting work.
func (b *Backend) acquireRequest(ctx context.Context) (*runtime, context.Context, func(), error) {
	r, err := b.getRuntime()
	if err != nil {
		return nil, nil, nil, err
	}
	r.jobsMu.Lock()
	if r.stopping {
		r.jobsMu.Unlock()
		return nil, nil, nil, ErrNotConfigured
	}
	r.wg.Add(1)
	r.jobsMu.Unlock()
	requestContext, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	if r.ctx.Err() != nil {
		cancel()
	}
	release := func() {
		stop()
		cancel()
		r.wg.Done()
	}
	return r, requestContext, release, nil
}

func (b *Backend) Health(trustedUserID string) error {
	_, err := b.getRuntime()
	if err != nil {
		return err
	}
	if strings.TrimSpace(trustedUserID) == "" {
		return apiError(401, "unauthorized")
	}
	if _, err := jsonUserID([]byte(strconv.Quote(strings.TrimSpace(trustedUserID)))); err != nil {
		return apiError(403, "forbidden")
	}
	return nil
}

func (b *Backend) Pages(ctx context.Context, req Request, trustedUserID string) (Result, error) {
	r, ctx, release, err := b.acquireRequest(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err := validateRequest(req, false); err != nil {
		return Result{}, err
	}
	authorized, err := r.authorize(ctx, req, trustedUserID)
	if err != nil {
		return Result{}, err
	}
	if req.CacheKey != "" && normalizeKey(req.CacheKey) != authorized.cacheKey {
		return Result{}, ErrCacheStale
	}
	if handle, ok := r.cache.lookup(authorized.cacheKey, time.Now()); ok {
		defer handle.release()
		return Result{Status: 200, CacheKey: authorized.cacheKey, Pages: pageInfo(handle.entry.pages)}, nil
	}
	if req.CacheKey != "" {
		return Result{}, ErrCacheMissing
	}
	job, err := r.getOrStartJob(authorized)
	if err != nil {
		return Result{}, err
	}
	if err := waitForJob(ctx, job); err != nil {
		return Result{}, err
	}
	if job.err != nil {
		return Result{}, job.err
	}
	handle, ok := r.cache.lookup(authorized.cacheKey, time.Now())
	if !ok {
		return Result{}, ErrCacheMissing
	}
	defer handle.release()
	return Result{Status: 200, CacheKey: authorized.cacheKey, Pages: pageInfo(handle.entry.pages)}, nil
}

func (b *Backend) Page(ctx context.Context, req Request, trustedUserID string, index int64) (Result, error) {
	r, ctx, release, err := b.acquireRequest(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err := validateRequest(req, true); err != nil {
		return Result{}, err
	}
	if index < 0 || req.Offset%uint64(ChunkBytes) != 0 {
		return Result{}, apiError(422, "invalid_offset")
	}
	authorized, err := r.authorize(ctx, req, trustedUserID)
	if err != nil {
		return Result{}, err
	}
	if normalizeKey(req.CacheKey) != authorized.cacheKey {
		return Result{}, ErrCacheStale
	}
	handle, ok := r.cache.lookup(authorized.cacheKey, time.Now())
	if !ok {
		return Result{}, ErrCacheMissing
	}
	defer handle.release()
	if index >= int64(len(handle.entry.pages)) {
		return Result{}, apiError(422, "invalid_offset")
	}
	page := handle.entry.pages[index]
	if req.Offset >= uint64(page.size) {
		return Result{}, apiError(422, "invalid_offset")
	}
	remaining := uint64(page.size) - req.Offset
	chunkSize := uint64(ChunkBytes)
	if remaining < chunkSize {
		chunkSize = remaining
	}
	file, err := os.Open(page.file)
	if err != nil {
		return Result{}, ErrCacheMissing
	}
	defer file.Close()
	body := make([]byte, chunkSize)
	if _, err := io.ReadFull(io.NewSectionReader(file, int64(req.Offset), int64(chunkSize)), body); err != nil {
		return Result{}, ErrCacheMissing
	}
	return Result{Status: 200, CacheKey: authorized.cacheKey, Body: body}, nil
}

func validateRequest(req Request, requireCacheKey bool) error {
	if strings.TrimSpace(req.Token) == "" || len(req.Token) > 16<<10 {
		return apiError(401, "unauthorized")
	}
	if len(req.ProfileToken) > 16<<10 || !validHeaderValue(req.ProfileToken) {
		return apiError(422, "invalid_profile_token")
	}
	if strings.TrimSpace(req.ProfileID) == "" || !validPathID(req.ProfileID) {
		return apiError(422, "invalid_profile")
	}
	if strings.TrimSpace(req.ContentID) == "" || !validPathID(req.ContentID) {
		return apiError(422, "invalid_content")
	}
	if req.FileID < 1 {
		return apiError(422, "invalid_file")
	}
	if req.APIVersion != "v1" && req.APIVersion != "v2" {
		return apiError(422, "invalid_api_version")
	}
	if req.CacheKey != "" && normalizeKey(req.CacheKey) == "" {
		return apiError(422, "invalid_cache_key")
	}
	if requireCacheKey && req.CacheKey == "" {
		return apiError(422, "cache_key_required")
	}
	return nil
}

func validPathID(value string) bool {
	if len(value) > 512 {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == '/' || char == '\\' || char == '?' || char == '#' {
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func normalizeKey(value string) string {
	if len(value) != sha256.Size*2 {
		return ""
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return ""
	}
	return strings.ToLower(value)
}

func (r *runtime) authorize(ctx context.Context, req Request, trustedUserID string) (authorizedRequest, error) {
	trustedUserID = strings.TrimSpace(trustedUserID)
	if trustedUserID == "" {
		return authorizedRequest{}, apiError(401, "unauthorized")
	}
	userID, err := r.client.authenticate(ctx, req.APIVersion, req.Token)
	if err != nil {
		return authorizedRequest{}, err
	}
	trusted, err := jsonUserID([]byte(strconv.Quote(trustedUserID)))
	if err != nil || trusted != userID {
		return authorizedRequest{}, apiError(403, "forbidden")
	}
	if err := r.client.authorizeItem(ctx, req.APIVersion, req.Token, req.ProfileID, req.ProfileToken, req.ContentID, req.FileID); err != nil {
		return authorizedRequest{}, err
	}
	file, err := r.client.headFile(ctx, req.APIVersion, req.Token, req.ProfileID, req.ProfileToken, req.ContentID, req.FileID)
	if err != nil {
		return authorizedRequest{}, err
	}
	fingerprint := fingerprintFor(req, file)
	return authorizedRequest{
		request:     req,
		file:        file,
		fingerprint: fingerprint,
		cacheKey:    hashFingerprint(fingerprint),
	}, nil
}

func fingerprintFor(req Request, file upstreamFile) string {
	return strings.Join([]string{
		req.APIVersion,
		req.ContentID,
		strconv.FormatInt(req.FileID, 10),
		strconv.FormatInt(file.Size, 10),
		file.ETag,
		file.LastModified,
	}, "\x00")
}

func hashFingerprint(fingerprint string) string {
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:])
}

func pageInfo(pages []cachedPage) []PageInfo {
	result := make([]PageInfo, len(pages))
	for i, page := range pages {
		result[i] = PageInfo{Name: page.name, Size: page.size}
	}
	return result
}

func (r *runtime) getOrStartJob(authorized authorizedRequest) (*extractionJob, error) {
	r.jobsMu.Lock()
	if r.stopping {
		job := &extractionJob{done: make(chan struct{}), err: apiError(503, "runtime_stopped")}
		close(job.done)
		r.jobsMu.Unlock()
		return job, nil
	}
	if job := r.jobs[authorized.cacheKey]; job != nil {
		r.jobsMu.Unlock()
		return job, nil
	}
	select {
	case r.extract <- struct{}{}:
	default:
		r.jobsMu.Unlock()
		return nil, apiError(503, "extractor_busy")
	}
	// Reserve the full input bound, including unknown or stale HEAD lengths.
	peakBytes := r.cfg.Limits.MaxArchiveBytes + r.cfg.Limits.MaxTotalBytes
	reservation, err := r.cache.reserve(peakBytes, time.Now())
	if err != nil {
		<-r.extract
		r.jobsMu.Unlock()
		return nil, apiError(503, "cache_full")
	}
	job := &extractionJob{done: make(chan struct{})}
	r.jobs[authorized.cacheKey] = job
	r.wg.Add(1)
	r.jobsMu.Unlock()
	go r.runJob(job, authorized, reservation)
	return job, nil
}

func waitForJob(ctx context.Context, job *extractionJob) error {
	timer := time.NewTimer(PreparationWait)
	defer timer.Stop()
	select {
	case <-job.done:
		return nil
	case <-timer.C:
		return ErrPreparing
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return apiError(503, "request_timeout")
		}
		return apiError(503, "request_cancelled")
	}
}

func (r *runtime) runJob(job *extractionJob, authorized authorizedRequest, reservation *cacheReservation) {
	defer r.wg.Done()
	defer close(job.done)
	defer func() {
		r.jobsMu.Lock()
		delete(r.jobs, authorized.cacheKey)
		r.jobsMu.Unlock()
	}()
	job.err = r.extractJob(authorized, reservation)
	authorized.request.Token = ""
	authorized.request.ProfileToken = ""
}

func (r *runtime) extractJob(authorized authorizedRequest, reservation *cacheReservation) error {
	if reservation == nil {
		return apiError(503, "cache_full")
	}
	defer reservation.release()
	defer func() { <-r.extract }()
	if r.ctx.Err() != nil {
		return apiError(503, "runtime_stopped")
	}
	jobCtx, cancel := context.WithTimeout(r.ctx, JobTimeout)
	defer cancel()
	jobDir, err := os.MkdirTemp(r.cache.root, ".job-")
	if err != nil {
		return apiError(503, "cache_unavailable")
	}
	defer func() { removeOwnedDir(r.cache, jobDir, ".job-") }()
	inputPath := filepath.Join(jobDir, "archive")
	outputPath := filepath.Join(jobDir, "pages")
	creds := credentials{
		token:        authorized.request.Token,
		profileID:    authorized.request.ProfileID,
		profileToken: authorized.request.ProfileToken,
	}
	defer func() { creds = credentials{} }()
	download, err := r.client.download(jobCtx, authorized.request.APIVersion, creds.token, creds.profileID, creds.profileToken, authorized.request.ContentID, authorized.request.FileID)
	if err != nil {
		if jobCtx.Err() != nil {
			return apiError(503, "extraction_timeout")
		}
		return err
	}
	if download.Size > r.cfg.Limits.MaxArchiveBytes {
		download.Body.Close()
		return apiError(413, "archive_too_large")
	}
	actualSize, err := writeBoundedFile(inputPath, download.Body, r.cfg.Limits.MaxArchiveBytes)
	closeErr := download.Body.Close()
	if err != nil {
		if jobCtx.Err() != nil {
			return apiError(503, "extraction_timeout")
		}
		return mapDownloadError(err)
	}
	if closeErr != nil {
		return apiError(503, "download_failed")
	}
	if (authorized.file.Size >= 0 && actualSize != authorized.file.Size) ||
		(download.Size >= 0 && actualSize != download.Size) ||
		download.ETag != authorized.file.ETag ||
		download.LastModified != authorized.file.LastModified {
		return apiError(409, "source_changed")
	}
	if actualSize > r.cfg.Limits.MaxArchiveBytes {
		return apiError(413, "archive_too_large")
	}
	if err := os.Chmod(inputPath, 0o600); err != nil {
		return apiError(503, "cache_unavailable")
	}
	creds = credentials{}
	authorized.request.Token = ""
	authorized.request.ProfileToken = ""
	pages, err := r.extractor(jobCtx, inputPath, outputPath, r.cfg.Limits)
	if err != nil {
		return mapExtractError(jobCtx, err)
	}
	if len(pages) == 0 {
		return apiError(422, "no_image_pages")
	}
	var total int64
	for _, page := range pages {
		if page.Size < 1 || page.Size > r.cfg.Limits.MaxPageBytes || total > r.cfg.Limits.MaxTotalBytes-page.Size {
			return apiError(413, "pages_too_large")
		}
		total += page.Size
	}
	if _, err := reservation.commit(authorized.cacheKey, validatorFingerprint(authorized.file), outputPath, pages, time.Now()); err != nil {
		return apiError(503, "cache_publish_failed")
	}
	return nil
}

func validatorFingerprint(file upstreamFile) string {
	if file.ETag == "" && file.LastModified == "" {
		return ""
	}
	return file.ETag + "\x00" + file.LastModified
}

func writeBoundedFile(path string, reader io.Reader, max int64) (int64, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	written, copyErr := io.Copy(file, io.LimitReader(reader, max+1))
	if copyErr != nil {
		return written, copyErr
	}
	if written > max {
		return written, fmt.Errorf("archive exceeds limit")
	}
	return written, file.Sync()
}

func removeOwnedDir(cache *pageCache, path, prefix string) {
	if cache == nil || !cache.ownedPath(path) || !strings.HasPrefix(filepath.Base(path), prefix) {
		return
	}
	_ = os.RemoveAll(path)
}

func mapDownloadError(err error) error {
	if strings.Contains(err.Error(), "exceeds limit") {
		return apiError(413, "archive_too_large")
	}
	return apiError(503, "download_failed")
}

func mapExtractError(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return apiError(503, "extraction_timeout")
	}
	return apiError(422, "invalid_archive")
}
