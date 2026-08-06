package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		v    string
		def  string
		want string
	}{
		{name: "empty value uses default", v: "", def: "default", want: "default"},
		{name: "non-empty value is used as-is", v: "configured", def: "default", want: "configured"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := firstNonEmpty(tt.v, tt.def); got != tt.want {
				t.Errorf("firstNonEmpty(%q, %q) = %q, want %q", tt.v, tt.def, got, tt.want)
			}
		})
	}
}

func TestNewUtilityAppliesDefaults(t *testing.T) {
	t.Parallel()

	u := NewUtility("", "", "", "")
	if u.imagesDir != defaultImagesDir {
		t.Errorf("imagesDir = %q, want default %q", u.imagesDir, defaultImagesDir)
	}
	if u.imageCredentialProviderConfig != defaultImageCredentialProviderConfig {
		t.Errorf("imageCredentialProviderConfig = %q, want default %q", u.imageCredentialProviderConfig, defaultImageCredentialProviderConfig)
	}
	if u.imageCredentialProviderBinDir != defaultImageCredentialProviderBinDir {
		t.Errorf("imageCredentialProviderBinDir = %q, want default %q", u.imageCredentialProviderBinDir, defaultImageCredentialProviderBinDir)
	}
	if u.agentRegistriesFile != defaultAgentRegistriesFile {
		t.Errorf("agentRegistriesFile = %q, want default %q", u.agentRegistriesFile, defaultAgentRegistriesFile)
	}
}

func TestNewUtilityKeepsConfiguredValues(t *testing.T) {
	t.Parallel()

	u := NewUtility("/custom/images", "/custom/cred-config.yaml", "/custom/cred-bin", "/custom/registries.yaml")
	if u.imagesDir != "/custom/images" {
		t.Errorf("imagesDir = %q, want %q", u.imagesDir, "/custom/images")
	}
	if u.imageCredentialProviderConfig != "/custom/cred-config.yaml" {
		t.Errorf("imageCredentialProviderConfig = %q, want %q", u.imageCredentialProviderConfig, "/custom/cred-config.yaml")
	}
	if u.imageCredentialProviderBinDir != "/custom/cred-bin" {
		t.Errorf("imageCredentialProviderBinDir = %q, want %q", u.imageCredentialProviderBinDir, "/custom/cred-bin")
	}
	if u.agentRegistriesFile != "/custom/registries.yaml" {
		t.Errorf("agentRegistriesFile = %q, want %q", u.agentRegistriesFile, "/custom/registries.yaml")
	}
}

func TestFindFirstExisting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.yaml")
	if err := os.WriteFile(existing, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	missingA := filepath.Join(dir, "missing-a.yaml")
	missingB := filepath.Join(dir, "missing-b.yaml")

	tests := []struct {
		name       string
		candidates []string
		want       string
	}{
		{name: "first candidate exists", candidates: []string{existing, missingA}, want: existing},
		{name: "later candidate exists", candidates: []string{missingA, existing, missingB}, want: existing},
		{name: "no candidates exist", candidates: []string{missingA, missingB}, want: ""},
		{name: "no candidates given", candidates: nil, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := findFirstExisting(tt.candidates...); got != tt.want {
				t.Errorf("findFirstExisting(%v) = %q, want %q", tt.candidates, got, tt.want)
			}
		})
	}
}

func TestFindRegistriesYamlPrefersAgentRegistriesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	agentFile := filepath.Join(dir, "agent-registries.yaml")
	if err := os.WriteFile(agentFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	u := &Utility{agentRegistriesFile: agentFile}
	if got := u.findRegistriesYaml(); got != agentFile {
		t.Errorf("findRegistriesYaml() = %q, want %q", got, agentFile)
	}
}

func TestFindRegistriesYamlReturnsEmptyWhenNoneExist(t *testing.T) {
	t.Parallel()

	u := &Utility{agentRegistriesFile: filepath.Join(t.TempDir(), "does-not-exist.yaml")}
	if got := u.findRegistriesYaml(); got != "" {
		t.Errorf("findRegistriesYaml() = %q, want empty string (rke2/k3s registries files are not expected to exist on a test host)", got)
	}
}
