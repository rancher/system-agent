package image

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/rancher/wharfie/pkg/extract"
	"github.com/rancher/wharfie/pkg/registries"
	"github.com/rancher/wharfie/pkg/tarfile"
	"github.com/sirupsen/logrus"
)

const imagesDir string = "/var/lib/rancher/agent/images"
const cacheDir string = "/var/lib/rancher/agent/cache"

func Stage(destDir string, imgString string, dockerConfigJson []byte) error {
	var img v1.Image
	image, err := name.ParseReference(imgString)
	if err != nil {
		return err
	}

	imagesDir, err := filepath.Abs(imagesDir)
	if err != nil {
		return err
	}

	i, err := tarfile.FindImage(imagesDir, image)
	if err != nil && !errors.Is(err, tarfile.NotFoundError) {
		return err
	}
	img = i

	if img == nil {
		registry, err := registries.GetPrivateRegistries("")

		if err != nil {
			return err
		}

		multiKeychain := authn.NewMultiKeychain(registry, authn.DefaultKeychain)
		logrus.Infof("Pulling image %s", image.Name())
		img, err = remote.Image(registry.Rewrite(image), remote.WithAuthFromKeychain(multiKeychain), remote.WithTransport(registry))
		if err != nil {
			return fmt.Errorf("%v: failed to get image %s", err, image.Name())
		}
	}

	return extract.Extract(img, destDir)
}
