//go:build !darwin && !linux

package app

import (
	"errors"
	"runtime"
)

func renameNoReplace(_, _ string) error {
	return errors.New("atomic no-replace publication is unsupported on " + runtime.GOOS)
}
