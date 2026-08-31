//go:build !windows
// +build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
)

// pathOwnedByCurrentUser verifies the file is owned by the current user.
// It returns an error when the owner differs.
func pathOwnedByCurrentUser(path string) error {
	var stat syscall.Stat_t
	err := syscall.Stat(path, &stat)
	if err != nil {
		return fmt.Errorf("unable to determine ownership of file %s: %w", path, err)
	}

	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	if stat.Uid != uid || stat.Gid != gid {
		return fmt.Errorf("file %s was not owned by uid=%d gid=%d", path, uid, gid)
	}

	return nil
}

// permissionsCheck ensures the file mode equals 0600.
// It returns an error when the mode differs.
func permissionsCheck(fi os.FileInfo, path string) error {
	if fi.Mode().Perm() != 0600 {
		return fmt.Errorf("file %s had permission %#o which was not expected 0600", path, fi.Mode().Perm())
	}
	return nil
}
