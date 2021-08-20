// +build !windows

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func pathOwnedByRoot(fi os.FileInfo, path string) error {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("failed type assertion for *syscall.Stat_t")
	}
	if stat.Uid != 0 || stat.Gid != 0 {
		return fmt.Errorf("file %s had was not owned by root:root", path)
	}
	return nil
}
