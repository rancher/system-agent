package applyinator

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"

	"github.com/rancher/system-agent/pkg/types"
	"github.com/sirupsen/logrus"
)

const defaultDirectoryPermissions os.FileMode = 0755
const defaultFilePermissions os.FileMode = 0600

func writeBase64ContentToFile(file types.File) error {
	content, err := base64.StdEncoding.DecodeString(file.Content)
	if err != nil {
		return err
	}
	var fileMode os.FileMode
	if file.Permissions == "" {
		logrus.Debugf("requested file permission for %s was %s, defaulting to %d", file.Path, file.Permissions, defaultFilePermissions)
		fileMode = defaultFilePermissions
	} else {
		if parsedPerm, err := parsePerm(file.Permissions); err != nil {
			return err
		} else {
			fileMode = parsedPerm
		}
	}
	return writeContentToFile(file.Path, file.UID, file.GID, fileMode, content)
}

func writeContentToFile(path string, uid int, gid int, perm os.FileMode, content []byte) error {
	if path == "" {
		return fmt.Errorf("path was empty")
	}

	existing, err := ioutil.ReadFile(path)
	if err == nil && bytes.Equal(existing, content) {
		logrus.Debugf("file %s does not need to be written", path)
	} else {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, defaultDirectoryPermissions); err != nil {
			return err
		}
		if err := ioutil.WriteFile(path, content, perm); err != nil {
			return err
		}
	}
	return reconcileFilePermissions(path, uid, gid, perm)
}

func createDirectory(file types.File) error {
	if !file.Directory {
		return fmt.Errorf("%s was not a directory", file.Path)
	}
	var fileMode os.FileMode
	if file.Permissions == "" {
		logrus.Debugf("requested file permission for %s was %s, defaulting to %d", file.Path, file.Permissions, defaultDirectoryPermissions)
		fileMode = defaultDirectoryPermissions
	} else {
		if parsedPerm, err := parsePerm(file.Permissions); err != nil {
			return err
		} else {
			fileMode = parsedPerm
		}
	}

	if err := os.MkdirAll(file.Path, fileMode); err != nil {
		return err
	}

	return reconcileFilePermissions(file.Path, file.UID, file.GID, fileMode)
}

func parsePerm(perm string) (os.FileMode, error) {
	if parsedPerm, err := strconv.ParseInt(perm, 8, 32); err != nil {
		return defaultFilePermissions, err
	} else {
		return os.FileMode(parsedPerm), nil
	}
}

func reconcileFilePermissions(path string, uid int, gid int, perm os.FileMode) error {
	logrus.Debugf("reconciling file permissions for %s to %d:%d %d", path, uid, gid, perm)
	if err := os.Chmod(path, perm); err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return nil
}
