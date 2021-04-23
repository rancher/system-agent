package applyinator

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
)

func writeBase64ContentToFile(path string, base64EncodedContent string) error {
	content, err := base64.StdEncoding.DecodeString(base64EncodedContent)
	if err != nil {
		return err
	}
	if err := writeContentToFile(path, content); err != nil {
		return err
	}
	return nil
}

func writeContentToFile(path string, content []byte) error {
	if path == "" {
		return fmt.Errorf("path was empty")
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
