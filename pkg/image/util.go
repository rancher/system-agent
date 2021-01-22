package image

import (
	"archive/tar"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	errors2 "github.com/pkg/errors"
	"github.com/rancher/wrangler/pkg/merr"
	"github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

// imagesDir returns the path to dataDir/agent/images.
func imagesDir(dataDir string) string {
	return filepath.Join(dataDir, "agent", "images")
}

// Stage extracts everything contained within the specified "image" to the specified destDir.
// @TODO: This needs to support private registry credentials
func Stage(destDir string, image string) error {
	var img v1.Image
	ref, err := name.ParseReference(image)
	if err != nil {
		return err
	}

	img, err = preloadBootstrapImage("", ref.String())
	if err != nil {
		return err
	}

	// If we didn't find the requested image in a tarball, pull it from the remote registry.
	// Note that this will fail (potentially after a long delay) if the registry cannot be reached.
	if img == nil {
		logrus.Infof("Pulling runtime image %q", ref)
		img, err = remote.Image(ref)
		if err != nil {
			return errors2.Wrapf(err, "Failed to pull runtime image %q", ref)
		}
	}

	// Extract binaries
	if err := extractToDir(destDir, "/", img, ref.String()); err != nil {
		return err
	}
	if err := os.Chmod(destDir, 0755); err != nil {
		return err
	}

	return nil
}

// extract extracts image content to targetDir all content from reader where the filename is prefixed with prefix.
// The imageName argument is used solely for logging.
// The destination directory is expected to be nonexistent or empty.
func extract(imageName string, targetDir string, prefix string, reader io.Reader) error {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	t := tar.NewReader(reader)
	for {
		h, err := t.Next()
		if err == io.EOF {
			logrus.Infof("Done extracting %q", imageName)
			return nil
		} else if err != nil {
			return err
		}

		if h.FileInfo().IsDir() {
			continue
		}

		n := filepath.Join("/", h.Name)
		if !strings.HasPrefix(n, prefix) {
			continue
		}

		logrus.Infof("Extracting file %q", h.Name)

		targetName := filepath.Join(targetDir, filepath.Base(n))
		//mode := h.FileInfo().Mode() & 0755
		mode := os.ModePerm
		f, err := os.OpenFile(targetName, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}

		if _, err = io.Copy(f, t); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}

// extractToDir extracts to targetDir all content from img where the filename is prefixed with prefix.
// The imageName argument is used solely for logging.
// Extracted content is staged through a temporary directory and moved into place, overwriting any existing files.
func extractToDir(dir, prefix string, img v1.Image, imageName string) error {
	logrus.Infof("Extracting %q %q to %q", imageName, prefix, dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return err
	}

	tempDir, err := ioutil.TempDir(filepath.Split(dir))
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	imageReader := mutate.Extract(img)
	defer imageReader.Close()

	// Extract content to temporary directory.
	if err := extract(imageName, tempDir, prefix, imageReader); err != nil {
		return err
	}

	// Try to rename the temp dir into its target location.
	if err := os.Rename(tempDir, dir); err == nil {
		// Successfully renamed into place, nothing else to do.
		return nil
	} else if !os.IsExist(err) {
		// Failed to rename, but not because the destination already exists.
		return err
	}

	// Target directory already exists (got ErrExist above), fall back list/rename files into place.
	files, err := ioutil.ReadDir(tempDir)
	if err != nil {
		return err
	}

	var errs []error
	for _, file := range files {
		src := filepath.Join(tempDir, file.Name())
		dst := filepath.Join(dir, file.Name())
		if err := os.Rename(src, dst); os.IsExist(err) {
			// Can't rename because dst already exists, remove it...
			if err = os.RemoveAll(dst); err != nil {
				errs = append(errs, errors2.Wrapf(err, "failed to remove %q", dst))
				continue
			}
			// ...then try renaming again
			if err = os.Rename(src, dst); err != nil {
				errs = append(errs, errors2.Wrapf(err, "failed to rename %q to %q", src, dst))
			}
		} else if err != nil {
			// Other error while renaming src to dst.
			errs = append(errs, errors2.Wrapf(err, "failed to rename %q to %q", src, dst))
		}
	}
	return merr.NewErrors(errs...)
}

// preloadBootstrapImage attempts return an image named imageName from a tarball
// within imagesDir.
func preloadBootstrapImage(dataDir string, imageName string) (v1.Image, error) {
	imagesDir := imagesDir(dataDir)
	if _, err := os.Stat(imagesDir); err != nil {
		if os.IsNotExist(err) {
			logrus.Debugf("No local image available for %q: dir %q does not exist", imageName, imagesDir)
			return nil, nil
		}
		return nil, err
	}

	// Walk the images dir to get a list of tar files
	files := map[string]os.FileInfo{}
	if err := filepath.Walk(imagesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".tar") {
			files[path] = info
		}
		return nil
	}); err != nil {
		return nil, err
	}

	imageTag, err := name.NewTag(imageName, name.WeakValidation)
	if err != nil {
		return nil, err
	}

	// Try to find the requested tag in each file, moving on to the next if there's an error
	for fileName := range files {
		img, err := tarball.ImageFromPath(fileName, &imageTag)
		if err != nil {
			logrus.Debugf("Did not find %q in %q: %s", imageName, fileName, err)
			continue
		}
		logrus.Debugf("Found %q in %q", imageName, fileName)
		return img, nil
	}
	logrus.Debugf("No local image available for %q: not found in any file in %q", imageName, imagesDir)
	return nil, nil
}
