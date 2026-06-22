//go:build darwin

package app

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	darwinRenameAtXNP = 488
	darwinAtFDCWD     = -2
	darwinRenameExcl  = 0x4
)

func renameNoReplace(oldPath, newPath string) error {
	oldPointer, err := syscall.BytePtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPointer, err := syscall.BytePtrFromString(newPath)
	if err != nil {
		return err
	}
	directory := darwinAtFDCWD
	_, _, errno := syscall.Syscall6(
		darwinRenameAtXNP,
		uintptr(directory),
		uintptr(unsafe.Pointer(oldPointer)),
		uintptr(directory),
		uintptr(unsafe.Pointer(newPointer)),
		darwinRenameExcl,
		0,
	)
	if errno != 0 {
		return &os.LinkError{Op: "renameatx_np", Old: oldPath, New: newPath, Err: errno}
	}
	return nil
}
