//go:build linux

package app

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	linuxAtFDCWD         = -100
	linuxRenameNoReplace = 0x1
)

func renameNoReplace(oldPath, newPath string) error {
	trap, err := linuxRenameAt2Trap()
	if err != nil {
		return err
	}
	oldPointer, err := syscall.BytePtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPointer, err := syscall.BytePtrFromString(newPath)
	if err != nil {
		return err
	}
	directory := linuxAtFDCWD
	_, _, errno := syscall.Syscall6(
		trap,
		uintptr(directory),
		uintptr(unsafe.Pointer(oldPointer)),
		uintptr(directory),
		uintptr(unsafe.Pointer(newPointer)),
		linuxRenameNoReplace,
		0,
	)
	if errno != 0 {
		return &os.LinkError{Op: "renameat2", Old: oldPath, New: newPath, Err: errno}
	}
	return nil
}

func linuxRenameAt2Trap() (uintptr, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 316, nil
	case "arm64", "riscv64", "loong64":
		return 276, nil
	default:
		return 0, errors.New("atomic no-replace publication is unsupported on linux/" + runtime.GOARCH)
	}
}
