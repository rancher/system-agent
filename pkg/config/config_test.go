package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type testResult struct {
	// Parse decodes YAML via sigs.k8s.io/yaml, which converts YAML to JSON and honors JSON tags
	// only -- so a YAML tag here would be inert and misleading.
	Foo string `json:"foo"`
}

func writeConfigFile(t *testing.T, name string, content []byte, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, perm); err != nil {
		t.Fatal(err)
	}
	// os.WriteFile passes perm through open(2), where the kernel applies the process umask -- so
	// under umask 077 a requested 0644 lands as 0600 and silently inverts the negative permission
	// tests. chmod(2) ignores the umask.
	if err := os.Chmod(path, perm); err != nil {
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
	err := Parse(path, &testResult{})
	if err == nil {
		t.Fatal("expected an error for a file with non-0600 permissions, got nil")
	}
	// Assert on the cause: otherwise this passes for any unrelated failure (ownership, decode).
	if !strings.Contains(err.Error(), "was not expected 0600") {
		t.Errorf("expected a permissions error, got %v", err)
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
	// anywhere, not by checking the true file extension. This is existing behavior (see the
	// switch in Parse), characterized here rather than "fixed."
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
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "was not expected 0600") {
				t.Errorf("expected the error to name the expected mode, got %v", err)
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

// TestParseRejectsFileOwnedByAnotherUser covers the ownership branch of Parse.
// Chowning to another user requires root, so this is skipped otherwise.
func TestParseRejectsFileOwnedByAnotherUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pathOwnedByCurrentUser is a no-op on windows")
	}
	if os.Getuid() != 0 {
		t.Skip("requires root to chown a file to another user")
	}
	t.Parallel()

	const nobody = 65534
	path := writeConfigFile(t, "config.json", []byte(`{"foo":"bar"}`), 0600)
	if err := os.Chown(path, nobody, nobody); err != nil {
		t.Fatal(err)
	}

	err := Parse(path, &testResult{})
	if err == nil {
		t.Fatal("expected an error for a file owned by another user, got nil")
	}
	if !strings.Contains(err.Error(), "was not owned by") {
		t.Errorf("expected an ownership error, got %v", err)
	}
}

func TestPathOwnedByCurrentUserRejectsOtherOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pathOwnedByCurrentUser is a no-op on windows")
	}
	if os.Getuid() != 0 {
		t.Skip("requires root to chown a file to another user")
	}
	t.Parallel()

	const nobody = 65534
	path := writeConfigFile(t, "owner-test", []byte("{}"), 0600)
	if err := os.Chown(path, nobody, nobody); err != nil {
		t.Fatal(err)
	}
	if err := pathOwnedByCurrentUser(path); err == nil {
		t.Error("expected an error for a file owned by another user, got nil")
	}
}
