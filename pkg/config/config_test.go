package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type testResult struct {
	Foo string `json:"foo" yaml:"foo"`
}

func writeConfigFile(t *testing.T, name string, content []byte, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseEmptyPath(t *testing.T) {
	t.Parallel()

	if err := Parse("", &testResult{}); err == nil {
		t.Fatal("expected an error for an empty path, got nil")
	}
}

func TestParseNonexistentFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	if err := Parse(path, &testResult{}); err == nil {
		t.Fatal("expected an error for a nonexistent file, got nil")
	}
}

func TestParseWrongPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissionsCheck is a no-op on windows")
	}
	t.Parallel()

	path := writeConfigFile(t, "config.json", []byte(`{"foo":"bar"}`), 0644)
	if err := Parse(path, &testResult{}); err == nil {
		t.Fatal("expected an error for a file with non-0600 permissions, got nil")
	}
}

func TestParseJSON(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, "config.json", []byte(`{"foo":"bar"}`), 0600)
	var result testResult
	if err := Parse(path, &result); err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if result.Foo != "bar" {
		t.Errorf("expected Foo %q, got %q", "bar", result.Foo)
	}
}

func TestParseYAML(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, "config.yaml", []byte("foo: bar\n"), 0600)
	var result testResult
	if err := Parse(path, &result); err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if result.Foo != "bar" {
		t.Errorf("expected Foo %q, got %q", "bar", result.Foo)
	}
}

func TestParseUnrecognizedExtension(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, "config.txt", []byte(`{"foo":"bar"}`), 0600)
	if err := Parse(path, &testResult{}); err == nil {
		t.Fatal("expected an error for a file that is neither JSON nor YAML, got nil")
	}
}

func TestParseMatchesExtensionAsSubstringNotSuffix(t *testing.T) {
	t.Parallel()

	// Parse chooses a decoder by checking whether the filename contains ".json"/".yaml"
	// anywhere, not by checking the true file extension — this is documented, existing
	// behavior (see CLAUDE.md), characterized here rather than "fixed."
	path := writeConfigFile(t, "config.json.bak", []byte(`{"foo":"bar"}`), 0600)
	var result testResult
	if err := Parse(path, &result); err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if result.Foo != "bar" {
		t.Errorf("expected Foo %q, got %q", "bar", result.Foo)
	}
}

func TestPermissionsCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissionsCheck is a no-op on windows")
	}
	t.Parallel()

	tests := []struct {
		name    string
		perm    os.FileMode
		wantErr bool
	}{
		{name: "0600 is allowed", perm: 0600, wantErr: false},
		{name: "0644 is rejected", perm: 0644, wantErr: true},
		{name: "0400 is rejected", perm: 0400, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeConfigFile(t, "perm-test", []byte("{}"), tt.perm)
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			err = permissionsCheck(fi, path)
			if (err != nil) != tt.wantErr {
				t.Errorf("permissionsCheck() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPathOwnedByCurrentUser(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, "owner-test", []byte("{}"), 0600)
	if err := pathOwnedByCurrentUser(path); err != nil {
		t.Errorf("expected a file created by this process to be owned by the current user, got: %v", err)
	}
}
