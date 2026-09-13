//go:build !linux

package silo

import (
	"errors"
	"os/exec"
)

func configureExtractorCommand(*exec.Cmd) {}

// Extraction is release-supported only on Linux, where the child receives an
// address-space ceiling before archive decoding starts.
func ApplyExtractionResourceLimits() error {
	return errors.New("extraction resource limits are unsupported on this platform")
}
