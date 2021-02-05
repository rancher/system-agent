package regkeychain

import (
	"bytes"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/types"
	"github.com/google/go-containerregistry/pkg/name"
)

type ByteDataKeychain struct {
	DockerConfigJson []byte
}

// This is functionally identical to https://github.com/google/go-containerregistry/blob/2276eac05fbee1de1315d16ec4b9346f3350c804/pkg/authn/keychain.go#L59
// except for the fact that it loads the docker config json from a byte data blob.

func (bdk *ByteDataKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	cf, err := config.LoadFromReader(bytes.NewReader(bdk.DockerConfigJson))
	if err != nil {
		return nil, err
	}

	// See:
	// https://github.com/google/ko/issues/90
	// https://github.com/moby/moby/blob/fc01c2b481097a6057bec3cd1ab2d7b4488c50c4/registry/config.go#L397-L404
	key := target.RegistryStr()
	if key == name.DefaultRegistry {
		key = authn.DefaultAuthKey
	}

	cfg, err := cf.GetAuthConfig(key)
	if err != nil {
		return nil, err
	}

	empty := types.AuthConfig{}
	if cfg == empty {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(authn.AuthConfig{
		Username:      cfg.Username,
		Password:      cfg.Password,
		Auth:          cfg.Auth,
		IdentityToken: cfg.IdentityToken,
		RegistryToken: cfg.RegistryToken,
	}), nil
}
