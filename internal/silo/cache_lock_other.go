//go:build !linux

package silo

import (
	"errors"
	"os"
)

func lockCacheRoot(string) (*os.File, error) {
	return nil, errors.New("cache locking is supported on Linux only")
}
