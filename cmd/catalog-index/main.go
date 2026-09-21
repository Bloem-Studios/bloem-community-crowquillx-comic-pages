// catalog-index builds the Silo repository index from the exact release binaries.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	publicmanifest "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginsdk/manifest"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: catalog-index <manifest.json> <asset-dir> <output.json>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(manifestPath, assets, output string) error {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	manifest, err := publicmanifest.Load(raw)
	if err != nil {
		return err
	}
	const repo = "https://github.com/Bloem-Studios/bloem-community-crowquillx-comic-pages"
	if err := publicmanifest.ValidateCatalogPresentation(manifest, repo); err != nil {
		return err
	}
	manifest.Checksum = ""
	binaries := make(map[string]map[string]string)
	var checksums strings.Builder
	for _, platform := range manifest.GetSupportedPlatforms() {
		name := "plugin-" + platform.GetOs() + "-" + platform.GetArch()
		data, err := os.ReadFile(filepath.Join(assets, name))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		checksum := hex.EncodeToString(sum[:])
		binaries[platform.GetOs()+"/"+platform.GetArch()] = map[string]string{
			"url":      repo + "/releases/download/v" + manifest.Version + "/" + name,
			"checksum": checksum,
		}
		fmt.Fprintf(&checksums, "%s  %s\n", checksum, name)
	}
	index := map[string]any{"plugins": []any{map[string]any{
		"manifest": manifest, "repo_url": repo, "binaries": binaries,
	}}}
	encoded, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(output, append(encoded, '\n'), 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(assets, "checksums.txt"), []byte(checksums.String()), 0644)
}
