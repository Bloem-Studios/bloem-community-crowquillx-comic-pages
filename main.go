package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimedefault"
	"github.com/crowquillx/silo-comic-pages/internal/archive"
	"github.com/crowquillx/silo-comic-pages/internal/server"
	"github.com/crowquillx/silo-comic-pages/internal/silo"
	"google.golang.org/protobuf/encoding/protojson"
)

var version string

//go:embed manifest.json
var manifestJSON []byte

type runtimeServer struct {
	runtimedefault.Server
	manifest *pluginv1.PluginManifest
	backend  *silo.Backend
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

func (s *runtimeServer) Configure(ctx context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("configuration is required")
	}
	cfg, err := silo.ConfigFromEntries(req.GetConfig())
	if err != nil {
		return nil, err
	}
	if err := s.backend.Configure(ctx, cfg); err != nil {
		return nil, err
	}
	return &pluginv1.ConfigureResponse{}, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "extract" {
		os.Exit(runExtractCommand(os.Args[2:]))
	}

	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load embedded manifest: %v\n", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "manifest" {
		data, err := (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(manifest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode manifest: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}
	backend := silo.NewBackend()
	defer backend.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		<-signals
		backend.Close()
		os.Exit(0)
	}()
	runtimeServer := &runtimeServer{manifest: manifest, backend: backend}
	sdkruntime.Serve(sdkruntime.ServeConfig{
		Servers: sdkruntime.CapabilityServers{
			Runtime:    runtimeServer,
			HttpRoutes: server.New(backend),
		},
	})
}

func runExtractCommand(args []string) int {
	if err := silo.ApplyExtractionResourceLimits(); err != nil {
		fmt.Fprintln(os.Stderr, "extract resource setup failed")
		return 1
	}
	flags := flag.NewFlagSet("extract", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	inputPath := flags.String("input", "", "")
	outputDir := flags.String("output", "", "")
	maxEntries := flags.Int("max-entries", 0, "")
	maxPageBytes := flags.Int64("max-page-bytes", 0, "")
	maxTotalBytes := flags.Int64("max-total-bytes", 0, "")
	maxArchiveBytes := flags.Int64("max-archive-bytes", 0, "")
	maxDictionaryBytes := flags.Int64("max-dictionary-bytes", 0, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *inputPath == "" || *outputDir == "" {
		return 2
	}
	pages, err := archive.Extract(context.Background(), *inputPath, *outputDir, archive.Limits{
		MaxEntries:         *maxEntries,
		MaxPageBytes:       *maxPageBytes,
		MaxTotalBytes:      *maxTotalBytes,
		MaxArchiveBytes:    *maxArchiveBytes,
		MaxDictionaryBytes: *maxDictionaryBytes,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "extract failed")
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(pages); err != nil {
		return 1
	}
	return 0
}
