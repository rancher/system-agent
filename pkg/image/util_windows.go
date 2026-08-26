//go:build windows
// +build windows

package image

import (
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/rancher/wharfie/pkg/extract"
)

// extractFiles extracts image files into dir on Windows.
// Place files in a subdirectory because Windows has no scratch container concept.
func extractFiles(img v1.Image, dir string) error {
	extractPaths := map[string]string{
		"/Files/bin": dir,
	}
	return extract.ExtractDirs(img, extractPaths)
}
