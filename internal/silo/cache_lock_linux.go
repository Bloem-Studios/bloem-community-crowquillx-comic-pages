//go:build linux

package silo

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func lockCacheRoot(root string) (*os.File, error) {
	fd, err := syscall.Open(filepath.Join(root, ".lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open cache lock")
	}
	file := os.NewFile(uintptr(fd), "cache-lock")
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("cache directory is already in use")
	}
	return file, nil
}
