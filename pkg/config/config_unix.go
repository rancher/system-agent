// +build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
)

func pathOwnedByRoot(fi os.FileInfo, path string) error {
	if fi.Sys().(*syscall.Stat_t).Uid != 0 || fi.Sys().(*syscall.Stat_t).Gid != 0 {
		return fmt.Errorf("file %s had was not owned by root:root", path)
	}
	return nil
}
