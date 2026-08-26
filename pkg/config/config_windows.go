//go:build windows
// +build windows

package config

import (
	"os"
)

// pathOwnedByCurrentUser validates ownership on Unix. On Windows return nil.
func pathOwnedByCurrentUser(_ string) error {
	return nil
}

// permissionsCheck validates file mode on Unix. On Windows return nil.
func permissionsCheck(_ os.FileInfo, _ string) error {
	return nil
}
