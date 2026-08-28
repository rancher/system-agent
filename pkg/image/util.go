package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/rancher/wharfie/pkg/credentialprovider/plugin"
	"github.com/rancher/wharfie/pkg/registries"
	"github.com/rancher/wharfie/pkg/tarfile"
	"github.com/sirupsen/logrus"
)

const (
	baseRancherDir                       string = "/var/lib/rancher/"
	defaultImagesDir                            = baseRancherDir + "agent/images"
	defaultImageCredentialProviderConfig        = baseRancherDir + "credentialprovider/config.yaml"
	defaultImageCredentialProviderBinDir        = baseRancherDir + "credentialprovider/bin"
	defaultAgentRegistriesFile           string = "/etc/rancher/agent/registries.yaml"
	rke2RegistriesFile                   string = "/etc/rancher/rke2/registries.yaml"
	k3sRegistriesFile                    string = "/etc/rancher/k3s/registries.yaml"
)

// Utility stages images and extracts them for instruction execution.
type Utility struct {
	imagesDir                     string
	imageCredentialProviderConfig string
	imageCredentialProviderBinDir string
	agentRegistriesFile           string
	// fallbackRegistriesFiles are distro registry configs consulted in order when
	// agentRegistriesFile does not exist. This field makes the function testable.
	fallbackRegistriesFiles []string
}

// NewUtility constructs a Utility and wires defaults.
func NewUtility(imagesDir, imageCredentialProviderConfig, imageCredentialProviderBinDir, agentRegistriesFile string) *Utility {
	u := Utility{
		imagesDir:                     firstNonEmpty(imagesDir, defaultImagesDir),
		imageCredentialProviderConfig: firstNonEmpty(imageCredentialProviderConfig, defaultImageCredentialProviderConfig),
		imageCredentialProviderBinDir: firstNonEmpty(imageCredentialProviderBinDir, defaultImageCredentialProviderBinDir),
		agentRegistriesFile:           firstNonEmpty(agentRegistriesFile, defaultAgentRegistriesFile),
		fallbackRegistriesFiles:       []string{rke2RegistriesFile, k3sRegistriesFile},
	}

	logrus.Debugf("[image] instantiated new image utility with imagesDir: %s, imageCredentialProviderConfig: %s, imageCredentialProviderBinDir: %s, agentRegistriesFile: %s", u.imagesDir, u.imageCredentialProviderConfig, u.imageCredentialProviderBinDir, u.agentRegistriesFile)

	return &u
}

// Stage ensures destDir exists and extracts imgString into it.
// It first searches local tar archives, then pulls from registries when needed.
func (u *Utility) Stage(destDir string, imgString string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	var img v1.Image
	image, err := name.ParseReference(imgString)
	if err != nil {
		return err
	}

	imagesDir, err := filepath.Abs(u.imagesDir)
	if err != nil {
		return err
	}

	i, err := tarfile.FindImage(imagesDir, image)
	if err != nil && !errors.Is(err, tarfile.ErrNotFound) {
		return err
	}
	img = i

	if img == nil {
		registry, err := registries.GetPrivateRegistries(u.findRegistriesYaml())
		if err != nil {
			return err
		}

		if _, err := os.Stat(u.imageCredentialProviderConfig); err == nil {
			logrus.Debugf("[image] image Credential Provider Configuration file %s existed, using plugins from directory %s", u.imageCredentialProviderConfig, u.imageCredentialProviderBinDir)
			plugins, err := plugin.RegisterCredentialProviderPlugins(u.imageCredentialProviderConfig, u.imageCredentialProviderBinDir)
			if err != nil {
				return err
			}
			registry.DefaultKeychain = plugins
		} else {
			// The kubelet image credential provider plugin also falls back to checking legacy Docker credentials, so only
			// explicitly set up the go-containerregistry DefaultKeychain if plugins are not configured.
			// DefaultKeychain tries to read config from the home dir, and will error if HOME isn't set, so also gate on that.
			if os.Getenv("HOME") != "" {
				registry.DefaultKeychain = authn.DefaultKeychain
			}
		}

		logrus.Infof("[image] pulling image %s", image.Name())
		img, err = registry.Image(image,
			remote.WithPlatform(v1.Platform{
				Architecture: runtime.GOARCH,
				OS:           runtime.GOOS,
			}),
		)
		if err != nil {
			return fmt.Errorf("failed to get image %s: %w", image.Name(), err)
		}
	}

	return extractFiles(img, destDir)
}

// findRegistriesYaml returns the first existing registries.yaml path or empty.
func (u *Utility) findRegistriesYaml() string {
	return findFirstExisting(append([]string{u.agentRegistriesFile}, u.fallbackRegistriesFiles...)...)
}

// findFirstExisting returns the first path that exists from candidates.
// It returns an empty string when none exist.
func findFirstExisting(candidates ...string) string {
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// firstNonEmpty returns v when non-empty, otherwise def.
func firstNonEmpty(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
