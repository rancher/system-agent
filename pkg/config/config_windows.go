// +build windows

package config

import (
	"os"
)

func pathOwnedByRoot(fi os.FileInfo, path string) error {
	return nil
}
