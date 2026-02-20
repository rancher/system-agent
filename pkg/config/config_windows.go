//go:build windows
// +build windows

package config

import (
	"os"
)

// pathOwnedByCurrentUser is abstracted out with Windows being a no op.
func pathOwnedByCurrentUser(_ string) error {
	return nil
}

// permissionsCheck is abstracted out with Windows being a no op.
func permissionsCheck(_ os.FileInfo, _ string) error {
	return nil
}
