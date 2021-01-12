package applyinator

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"os"
	"path/filepath"
)

func writeFile(path string, base64Content string) error {
	if path == "" {
		return fmt.Errorf("path was empty")
	}

	content, err := base64.StdEncoding.DecodeString(base64Content)
	if err != nil {
		return err
	}

	existing, err := ioutil.ReadFile(path)
	if err == nil && bytes.Equal(existing, content) {
		logrus.Debugf("file %s does not need to be written", path)
		return nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := ioutil.WriteFile(path, content, 0600); err != nil {
		return err
	}
	return nil
}