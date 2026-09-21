package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	pluginv1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestReleaseBinaryThroughSiloGRPC(t *testing.T) {
	binary := os.Getenv("COMIC_PAGES_TEST_BINARY")
	if binary == "" {
		binary = filepath.Join(t.TempDir(), "plugin")
		build := exec.Command("go", "build", "-trimpath", "-o", binary, "..")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build plugin: %v\n%s", err, output)
		}
	}
	if info, err := os.Stat(binary); err == nil {
		t.Logf("test binary: %d bytes", info.Size())
	}
	raw, err := exec.Command(binary, "manifest").CombinedOutput()
	if err != nil {
		t.Fatalf("manifest command: %v\n%s", err, raw)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	var expectedManifest map[string]any
	if err := json.Unmarshal(read(t, "../manifest.json"), &expectedManifest); err != nil {
		t.Fatal(err)
	}
	if manifest["plugin_id"] != "dev.crowquillx.comic-pages" || manifest["version"] != expectedManifest["version"] {
		t.Fatalf("unexpected manifest identity: %v %v", manifest["plugin_id"], manifest["version"])
	}
	binaryBytes := read(t, binary)
	sum := sha256.Sum256(binaryBytes)
	if manifest["checksum"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("manifest checksum does not match binary")
	}
	for _, name := range []string{"rar40-normal.cbr", "rar40-solid.cbr", "rar50-normal.cbr", "rar50-solid.cbr"} {
		t.Run(name, func(t *testing.T) {
			archive := read(t, filepath.Join("..", "testdata", "fixtures", name))
			expected := [][]byte{
				read(t, "../testdata/fixtures/page1.png"),
				read(t, "../testdata/fixtures/page2.png"),
				read(t, "../testdata/fixtures/page10.png"),
			}
			exercise(t, binary, archive, expected, "v1")
		})
	}
	t.Run("v2-account-and-string-ids", func(t *testing.T) {
		exercise(t, binary, read(t, "../testdata/fixtures/rar50-solid.cbr"), [][]byte{
			read(t, "../testdata/fixtures/page1.png"),
			read(t, "../testdata/fixtures/page2.png"),
			read(t, "../testdata/fixtures/page10.png"),
		}, "v2")
	})
	t.Run("large-image-chunks", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
		for y := 0; y < 1024; y++ {
			for x := 0; x < 1024; x++ {
				img.SetRGBA(x, y, color.RGBA{R: byte(x), G: byte(y), B: byte(x ^ y), A: 255})
			}
		}
		var pngBytes bytes.Buffer
		encoder := png.Encoder{CompressionLevel: png.NoCompression}
		if err := encoder.Encode(&pngBytes, img); err != nil {
			t.Fatal(err)
		}
		if pngBytes.Len() <= 3*1024*1024 {
			t.Fatal("fixture must exceed three chunks")
		}
		var archive bytes.Buffer
		writer := zip.NewWriter(&archive)
		entry, err := writer.Create("page1.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(pngBytes.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		exercise(t, binary, archive.Bytes(), [][]byte{pngBytes.Bytes()}, "v1")
	})
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func exercise(t *testing.T, binary string, archive []byte, expected [][]byte, apiVersion string) {
	t.Helper()
	var downloads atomic.Int32
	var revoked atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer reader-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		prefix := "/api/" + apiVersion
		accountPath := prefix + "/auth/me"
		if apiVersion == "v2" {
			accountPath = prefix + "/account/me"
		}
		if r.URL.Path == accountPath {
			w.Header().Set("Content-Type", "application/json")
			if apiVersion == "v2" {
				fmt.Fprint(w, `{"id":"7","username":"reader","role":"user"}`)
			} else {
				fmt.Fprint(w, `{"id":7,"username":"reader","role":"user"}`)
			}
			return
		}
		if revoked.Load() || r.Header.Get("X-Profile-Id") != "p1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case prefix + "/profiles":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"profiles":[{"id":"p1","name":"Reader","is_primary":true,"has_pin":false}]}`)
		case prefix + "/catalog/items/1":
			w.Header().Set("Content-Type", "application/json")
			fileID := "2"
			if apiVersion == "v2" {
				fileID = `"2"`
			}
			fmt.Fprintf(w, `{"content_id":"1","type":"ebook","versions":[{"file_id":%s,"container":"cbr","file_size":%d}]}`, fileID, len(archive))
		case prefix + "/ebooks/1/files/2/read":
			if r.Method == http.MethodGet {
				downloads.Add(1)
			}
			w.Header().Set("ETag", `"fixture-1"`)
			http.ServeContent(w, r, "comic.cbr", time.Unix(1700000000, 0), bytes.NewReader(archive))
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	command := exec.Command(binary)
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: sdkruntime.HandshakeConfig(),
		Plugins:         sdkruntime.DefaultPluginSet(sdkruntime.CapabilityServers{}),
		Cmd:             command, AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger: hclog.NewNullLogger(),
	})
	defer client.Kill()
	connection, err := client.Client()
	if err != nil {
		t.Fatal(err)
	}
	dispensed, err := connection.Dispense(sdkruntime.PluginSetName)
	if err != nil {
		t.Fatal(err)
	}
	rpc := dispensed.(*sdkruntime.Client)
	cacheDir := t.TempDir()
	config, err := structpb.NewStruct(map[string]any{"base_url": upstream.URL, "cache_dir": cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.Runtime().Configure(context.Background(), &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{Key: "connection", Value: config}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, health := range []struct {
		user   string
		status int32
	}{{"7", 200}, {"", 401}} {
		response, err := rpc.HttpRoutes().Handle(context.Background(), &pluginv1.HandleHTTPRequest{
			Method: "GET", Path: "/v1/health", Headers: map[string]string{"X-Silo-User-Id": health.user},
		})
		if err != nil || response.GetStatusCode() != health.status {
			t.Fatalf("bodyless health for user %q: response=%v error=%v", health.user, response, err)
		}
	}
	call := func(path string, body map[string]any, user string) *pluginv1.HandleHTTPResponse {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		response, err := rpc.HttpRoutes().Handle(ctx, &pluginv1.HandleHTTPRequest{
			Method: "POST", Path: path, Body: encoded,
			Headers: map[string]string{"X-Silo-User-Id": user, "Content-Type": "application/json"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	body := map[string]any{"token": "reader-token", "profile_id": "p1", "content_id": "1", "file_id": "2", "api_version": apiVersion, "offset": 0}
	denied := call("/v1/pages", body, "8")
	if denied.StatusCode != 403 || downloads.Load() != 0 {
		t.Fatalf("mismatched host identity: HTTP %d, downloads %d", denied.StatusCode, downloads.Load())
	}
	start := time.Now()
	var listed struct {
		CacheKey   string `json:"cache_key"`
		ChunkBytes int    `json:"chunk_bytes"`
		Pages      []struct {
			Name string `json:"name"`
			Size int    `json:"size"`
		} `json:"pages"`
	}
	for tries := 0; ; tries++ {
		response := call("/v1/pages", body, "7")
		if response.StatusCode == 202 && tries < 45 {
			continue
		}
		if response.StatusCode != 200 {
			t.Fatalf("page list: HTTP %d %s", response.StatusCode, response.Body)
		}
		if err := json.Unmarshal(response.Body, &listed); err != nil {
			t.Fatal(err)
		}
		break
	}
	t.Logf("prepare %d archive bytes: %s", len(archive), time.Since(start))
	if listed.ChunkBytes != 1048576 || len(listed.Pages) != len(expected) || len(listed.CacheKey) != 64 {
		t.Fatalf("unexpected page list: %+v", listed)
	}
	body["cache_key"] = listed.CacheKey
	for pageIndex, page := range listed.Pages {
		var assembled []byte
		for offset := 0; offset < page.Size; offset += listed.ChunkBytes {
			body["offset"] = offset
			response := call(fmt.Sprintf("/v1/page/%d", pageIndex), body, "7")
			if response.StatusCode != 200 || len(response.Body) > listed.ChunkBytes {
				t.Fatalf("page %d offset %d size %d: HTTP %d, response %s", pageIndex, offset, page.Size, response.StatusCode, response.Body)
			}
			assembled = append(assembled, response.Body...)
		}
		if !bytes.Equal(assembled, expected[pageIndex]) {
			t.Fatalf("page %d did not preserve original image bytes", pageIndex)
		}
	}
	if downloads.Load() != 1 {
		t.Fatalf("archive downloaded %d times", downloads.Load())
	}
	items, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.HasPrefix(item.Name(), ".job-") {
			t.Fatalf("completed job left temporary files: %s", item.Name())
		}
	}
	var cachedBytes int64
	if err := filepath.WalkDir(cacheDir, func(path string, item fs.DirEntry, err error) error {
		if err != nil || item.IsDir() || item.Name() == ".lock" || item.Name() == ".silo-comic-pages-owner" {
			return err
		}
		info, err := item.Info()
		if err == nil {
			cachedBytes += info.Size()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var expectedBytes int64
	for _, page := range expected {
		expectedBytes += int64(len(page))
	}
	if cachedBytes != expectedBytes {
		t.Fatalf("cache held %d bytes, expected only %d image bytes", cachedBytes, expectedBytes)
	}
	t.Logf("cache after preparation: %d image bytes; no temporary archive", cachedBytes)
	body["offset"] = 0
	body["cache_key"] = strings.Repeat("0", 64)
	if response := call("/v1/page/0", body, "7"); response.StatusCode != 409 {
		t.Fatalf("wrong cache revision: HTTP %d", response.StatusCode)
	}
	body["cache_key"] = listed.CacheKey
	revoked.Store(true)
	if response := call("/v1/page/0", body, "7"); response.StatusCode != 403 {
		t.Fatalf("cached image after authorization revoked: HTTP %d", response.StatusCode)
	}
	if apiVersion == "v2" {
		if err := command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !client.Exited() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !client.Exited() {
			t.Fatal("plugin did not exit after SIGTERM")
		}
	} else {
		client.Kill()
	}
	items, err = os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Name() != ".lock" && item.Name() != ".silo-comic-pages-owner" {
			t.Fatalf("graceful plugin shutdown left cached data: %s", item.Name())
		}
	}
}
