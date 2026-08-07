package image

import (
	"os"
	"reflect"
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
	rke2File := filepath.Join(dir, "rke2-registries.yaml")
	for _, f := range []string{agentFile, rke2File} {
		if err := os.WriteFile(f, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// Both exist, so the agent file must win.
	u := &Utility{agentRegistriesFile: agentFile, fallbackRegistriesFiles: []string{rke2File}}
	if got := u.findRegistriesYaml(); got != agentFile {
		t.Errorf("findRegistriesYaml() = %q, want %q", got, agentFile)
	}
}

func TestFindRegistriesYamlFallsBackInOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rke2File := filepath.Join(dir, "rke2-registries.yaml")
	k3sFile := filepath.Join(dir, "k3s-registries.yaml")
	for _, f := range []string{rke2File, k3sFile} {
		if err := os.WriteFile(f, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	u := &Utility{
		agentRegistriesFile:     filepath.Join(dir, "does-not-exist.yaml"),
		fallbackRegistriesFiles: []string{rke2File, k3sFile},
	}
	if got := u.findRegistriesYaml(); got != rke2File {
		t.Errorf("findRegistriesYaml() = %q, want the first existing fallback %q", got, rke2File)
	}
}

func TestFindRegistriesYamlReturnsEmptyWhenNoneExist(t *testing.T) {
	t.Parallel()

	// The fallback paths are injected rather than read from the package constants, so this does
	// not depend on whether the host happens to have rke2 or k3s installed.
	dir := t.TempDir()
	u := &Utility{
		agentRegistriesFile:     filepath.Join(dir, "does-not-exist.yaml"),
		fallbackRegistriesFiles: []string{filepath.Join(dir, "no-rke2.yaml"), filepath.Join(dir, "no-k3s.yaml")},
	}
	if got := u.findRegistriesYaml(); got != "" {
		t.Errorf("findRegistriesYaml() = %q, want empty string", got)
	}
}

func TestNewUtilityWiresDistroRegistriesFallbacks(t *testing.T) {
	t.Parallel()

	u := NewUtility("", "", "", "")
	want := []string{rke2RegistriesFile, k3sRegistriesFile}
	if !reflect.DeepEqual(u.fallbackRegistriesFiles, want) {
		t.Errorf("expected NewUtility to wire the distro registry fallbacks in order %v, got %v", want, u.fallbackRegistriesFiles)
	}
}
