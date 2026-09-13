//go:build linux

package silo

import (
	"os/exec"
	"syscall"
)

func configureExtractorCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// applyExtractionResourceLimits is called only by the private extraction
// child. Go's runtime has already initialized before this point, so the
// extraction process has a hard 2 GiB address-space ceiling after startup.
func ApplyExtractionResourceLimits() error {
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: 2 << 30, Max: 2 << 30}); err != nil {
		return err
	}
	return syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
}
