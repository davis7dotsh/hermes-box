//go:build !darwin && !linux

package backup

import (
	"errors"
	"os"
)

func publishDirectoryExclusive(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, destination)
}
