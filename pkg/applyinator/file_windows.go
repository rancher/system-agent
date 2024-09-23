//go:build windows

package applyinator

import (
	"github.com/rancher/permissions/pkg/acl"
	"github.com/sirupsen/logrus"

	"os"
)

// reconcileFilePermissions abstracts out the file permissions checks and are a no op on Windows
func reconcileFilePermissions(path string, uid int, gid int, perm os.FileMode) error {
	logrus.Debugf("[Applyinator] Reconciling file permissions for %s to %d", path, perm)
	if uid != 0 || gid != 0 {
		// note: although acl.Chown is implemented, adding support for Windows UID and GIDs (which are strings)
		// would require adding new fields to the File struct.
		logrus.Debugf("windows file permissions do not support custom uid and guid (%d:%d) for %s", uid, gid, path)
	}

	return acl.Chmod(path, perm)
}

func getPermissions(path string) (os.FileMode, error) {
	logrus.Debugf("getting windows file permissions for %s is not implemented", path)
	return 0000, nil
}
