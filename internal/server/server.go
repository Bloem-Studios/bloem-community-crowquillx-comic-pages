package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	pluginv1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Bloem-Studios/bloem-community-crowquillx-comic-pages/internal/silo"
)

const maxRequestBody = 16 << 10

// Server adapts the SDK HTTP capability RPC to the comic-pages backend.
type Server struct {
	pluginv1.UnimplementedHttpRoutesServer
	backend *silo.Backend
}

func New(backend *silo.Backend) *Server {
	return &Server{backend: backend}
}

func (s *Server) Handle(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if req == nil {
		return responseForError(siloError(422, "invalid_request")), nil
	}
	method := strings.ToUpper(strings.TrimSpace(req.GetMethod()))
	path := req.GetPath()
	if question := strings.IndexByte(path, '?'); question >= 0 {
		path = path[:question]
	}
	if method == http.MethodGet && path == "/v1/health" {
		return s.handleHealth(req)
	}
	if method != http.MethodPost {
		return responseForError(siloError(http.StatusNotFound, "not_found")), nil
	}
	request, err := decodeRequest(req.GetBody())
	if err != nil {
		return responseForError(err), nil
	}
	trustedUserID := headerValue(req.GetHeaders(), "X-Silo-User-Id")
	if s == nil || s.backend == nil {
		return responseForError(siloError(503, "not_configured")), nil
	}
	if path == "/v1/pages" {
		result, backendErr := s.backend.Pages(ctx, request, trustedUserID)
		return responseForResult(result, backendErr), nil
	}
	const pagePrefix = "/v1/page/"
	if strings.HasPrefix(path, pagePrefix) {
		indexText := strings.TrimPrefix(path, pagePrefix)
		if indexText == "" || strings.Contains(indexText, "/") {
			return responseForError(siloError(404, "not_found")), nil
		}
		index, parseErr := strconv.ParseInt(indexText, 10, 64)
		if parseErr != nil || index < 0 {
			return responseForError(siloError(422, "invalid_page")), nil
		}
		result, backendErr := s.backend.Page(ctx, request, trustedUserID, index)
		return responseForResult(result, backendErr), nil
	}
	return responseForError(siloError(404, "not_found")), nil
}

func (s *Server) handleHealth(req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if s == nil || s.backend == nil {
		return responseForError(siloError(503, "not_configured")), nil
	}
	err := s.backend.Health(headerValue(req.GetHeaders(), "X-Silo-User-Id"))
	if err != nil {
		return responseForError(err), nil
	}
	return jsonResponse(http.StatusOK, map[string]string{"status": "ok"}), nil
}

func decodeRequest(body []byte) (silo.Request, error) {
	var request silo.Request
	if err := decodeJSON(body, &request); err != nil {
		return silo.Request{}, err
	}
	return request, nil
}

func decodeJSON(body []byte, destination any) error {
	if len(body) > maxRequestBody {
		return siloError(413, "request_too_large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return siloError(422, "invalid_request")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return siloError(422, "invalid_request")
	}
	return nil
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func responseForResult(result silo.Result, err error) *pluginv1.HandleHTTPResponse {
	if err != nil {
		return responseForError(err)
	}
	if result.Body != nil {
		return &pluginv1.HandleHTTPResponse{
			StatusCode: int32(result.Status),
			Headers: map[string]string{
				"Content-Type":   "application/octet-stream",
				"Cache-Control":  "no-store",
				"Content-Length": strconv.Itoa(len(result.Body)),
			},
			Body: result.Body,
		}
	}
	if result.StatusJSON != "" {
		return &pluginv1.HandleHTTPResponse{
			StatusCode: int32(result.Status),
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(result.StatusJSON),
		}
	}
	return jsonResponse(result.Status, map[string]any{
		"cache_key":   result.CacheKey,
		"pages":       result.Pages,
		"chunk_bytes": silo.ChunkBytes,
	})
}

func responseForError(err error) *pluginv1.HandleHTTPResponse {
	status := http.StatusServiceUnavailable
	code := "unavailable"
	var apiErr *silo.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		status = apiErr.Status
		code = apiErr.Code
	}
	if status == 202 {
		return jsonResponse(status, map[string]string{"status": "preparing"})
	}
	return jsonResponse(status, map[string]string{"error": code})
}

func jsonResponse(status int, value any) *pluginv1.HandleHTTPResponse {
	body, err := json.Marshal(value)
	if err != nil {
		body = []byte(`{"error":"unavailable"}`)
		status = http.StatusServiceUnavailable
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers: map[string]string{
			"Content-Type":   "application/json",
			"Content-Length": strconv.Itoa(len(body)),
		},
		Body: body,
	}
}

func siloError(status int, code string) error {
	return &silo.APIError{Status: status, Code: code}
}
