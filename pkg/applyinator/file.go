package applyinator

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/sirupsen/logrus"
)

const defaultDirectoryPermissions os.FileMode = 0755
const defaultFilePermissions os.FileMode = 0600

// resolvePermissions returns def when perm is empty.
// Otherwise, it parses perm as an octal file mode.
func resolvePermissions(perm string, def os.FileMode) (os.FileMode, error) {
	if perm == "" {
		return def, nil
	}
	return parsePerm(perm)
}

// writeBase64ContentToFile decodes base64 content and writes it to disk.
// It uses the file's Permissions, UID and GID.
func writeBase64ContentToFile(file planapi.File) error {
	content, err := base64.StdEncoding.DecodeString(file.Content)
	if err != nil {
		return err
	}
	if file.Permissions == "" {
		logrus.Debugf("[applyinator] requested file permission for %s was %s, defaulting to %d", file.Path, file.Permissions, defaultFilePermissions)
	}
	fileMode, err := resolvePermissions(file.Permissions, defaultFilePermissions)
	if err != nil {
		return err
	}
	return writeContentToFile(file.Path, file.UID, file.GID, fileMode, content)
}

// writeContentToFile writes content to path if it differs from existing content.
// It creates containing directories and sets permissions and ownership.
func writeContentToFile(path string, uid int, gid int, perm os.FileMode, content []byte) error {
	if path == "" {
		return fmt.Errorf("path was empty")
	}

	existing, err := os.ReadFile(path)
	if err == nil && bytes.Equal(existing, content) {
		logrus.Debugf("[applyinator] file %s does not need to be written", path)
	} else {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, defaultDirectoryPermissions); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, perm); err != nil {
			return err
		}
	}
	return reconcileFilePermissions(path, uid, gid, perm)
}

// createDirectory creates a directory described by file.
// It applies requested permissions and ownership.
func createDirectory(file planapi.File) error {
	if !file.Directory {
		return fmt.Errorf("%s was not a directory", file.Path)
	}
	if file.Permissions == "" {
		logrus.Debugf("[applyinator] requested file permission for %s was %s, defaulting to %d", file.Path, file.Permissions, defaultDirectoryPermissions)
	}
	fileMode, err := resolvePermissions(file.Permissions, defaultDirectoryPermissions)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(file.Path, fileMode); err != nil {
		return err
	}

	return reconcileFilePermissions(file.Path, file.UID, file.GID, fileMode)
}

// removeFile removes a file or directory described by file.
// It tolerates missing paths.
func removeFile(file planapi.File) error {
	if file.Directory {
		logrus.Debugf("[applyinator] removing directory %s", file.Path)
		if err := os.RemoveAll(file.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		logrus.Debugf("[applyinator] removing file %s", file.Path)
		if err := os.Remove(file.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// parsePerm parses a string as an octal file mode.
// It returns defaultFilePermissions on error.
func parsePerm(perm string) (os.FileMode, error) {
	parsedPerm, err := strconv.ParseInt(perm, 8, 32)
	if err != nil {
		return defaultFilePermissions, err
	}
	return os.FileMode(parsedPerm), nil
}
