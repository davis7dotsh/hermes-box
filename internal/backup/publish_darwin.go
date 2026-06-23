//go:build darwin

package backup

import "golang.org/x/sys/unix"

func publishDirectoryExclusive(source, destination string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_EXCL)
}
