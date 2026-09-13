package silo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/crowquillx/silo-comic-pages/internal/archive"
)

// runExtractorChild invokes the plugin executable's private extract command.
// Paths are passed as argument values to exec.CommandContext, so HTTP input
// can never become shell syntax.
func runExtractorChild(ctx context.Context, inputPath, outputDir string, limits archive.Limits) ([]archive.Page, error) {
	command := exec.CommandContext(ctx, os.Args[0], "extract",
		"--input", inputPath,
		"--output", outputDir,
		"--max-entries", strconv.Itoa(limits.MaxEntries),
		"--max-page-bytes", strconv.FormatInt(limits.MaxPageBytes, 10),
		"--max-total-bytes", strconv.FormatInt(limits.MaxTotalBytes, 10),
		"--max-archive-bytes", strconv.FormatInt(limits.MaxArchiveBytes, 10),
		"--max-dictionary-bytes", strconv.FormatInt(limits.MaxDictionaryBytes, 10),
	)
	configureExtractorCommand(command)
	command.Stderr = io.Discard
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, 1<<20+1))
	tooLarge := len(output) > 1<<20
	if tooLarge {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if tooLarge {
		return nil, fmt.Errorf("extractor result is too large")
	}
	if waitErr != nil {
		return nil, waitErr
	}
	var pages []archive.Page
	if err := json.Unmarshal(output, &pages); err != nil {
		return nil, fmt.Errorf("decode extractor result")
	}
	return pages, nil
}
