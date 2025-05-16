//go:build !arm64

package applyinator

import (
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestWriteContentToFile(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	getFile := func(path string, permissions string, content string) File {
		return File{
			Content:     content,
			UID:         -1,
			GID:         -1,
			Path:        filepath.Join(tempDir, path),
			Permissions: permissions,
		}
	}

	testCases := []struct {
		Path        string
		Permissions string
		Content     string

		Base64Encode bool

		ExpectedPermissions os.FileMode
		ExpectedErr         bool
	}{
		{
			Path:    "test-no-perms",
			Content: "hello world",

			ExpectedPermissions: defaultFilePermissions,
			ExpectedErr:         false,
		},
		{
			Path:        "test-perms",
			Permissions: "0666",
			Content:     "hello world 2",

			ExpectedPermissions: 0666,
			ExpectedErr:         false,
		},
		{
			Path:    "test-invalid-base64",
			Content: "not base64 content",

			Base64Encode: true,

			ExpectedErr: true,
		},
		{
			Path:    "test-no-perms-base64",
			Content: "aGVsbG8gd29ybGQ=",

			Base64Encode: true,

			ExpectedPermissions: defaultFilePermissions,
			ExpectedErr:         false,
		},
		{
			Path:        "test-perms-base64",
			Permissions: "0666",
			Content:     "aGVsbG8gd29ybGQ=",

			Base64Encode: true,

			ExpectedPermissions: 0666,
			ExpectedErr:         false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Path, func(t *testing.T) {
			f := getFile(tc.Path, tc.Permissions, tc.Content)
			t.Run("Create File", func(t *testing.T) {
				var err error
				if tc.Base64Encode {
					err = writeBase64ContentToFile(f)
				} else {
					var perms os.FileMode
					perms, err = parsePerm(f.Permissions)
					if err != nil && f.Permissions != "" {
						t.Fatalf("invalid permissions provided: %s", f.Permissions)
					}
					err = writeContentToFile(f.Path, f.UID, f.GID, perms, []byte(f.Content))
				}
				if tc.ExpectedErr {
					if err == nil {
						t.Error("expected error, returned successfully")
					}
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
			})
			if tc.ExpectedErr {
				// no need to run any further tests if file was never created
				return
			}
			t.Run("Read File", func(t *testing.T) {
				content, err := os.ReadFile(f.Path)
				if err != nil {
					t.Error(err)
					return
				}
				decoded := []byte(tc.Content)
				if tc.Base64Encode {
					decoded, err = base64.StdEncoding.DecodeString(tc.Content)
					if err != nil {
						t.Error(err)
						return
					}
				}

				if !reflect.DeepEqual(content, decoded) {
					t.Errorf("expected %s, found %s", tc.Content, content)
					return
				}
			})
			t.Run("Check Permissions", func(t *testing.T) {
				if runtime.GOOS == "windows" {
					t.Skip("cannot get permissions on Windows")
				}
				permissions, err := getPermissions(f.Path)
				if err != nil {
					t.Error(err)
				}
				if permissions != tc.ExpectedPermissions {
					t.Errorf("expected permissions %v, found %v", tc.ExpectedPermissions, permissions)
				}
			})
		})
	}
}

func TestCreateDirectory(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	getFile := func(path string, permissions string) File {
		return File{
			Directory:   true,
			UID:         -1,
			GID:         -1,
			Path:        filepath.Join(tempDir, path),
			Permissions: permissions,
		}
	}

	testCases := []struct {
		Path        string
		Permissions string

		ExpectedPermissions os.FileMode
		ExpectedErr         bool
	}{
		{
			Path: "test-no-perms",

			ExpectedPermissions: fs.ModeDir | defaultDirectoryPermissions,
			ExpectedErr:         false,
		},
		{
			Path:        "test-perms",
			Permissions: "0777",

			ExpectedPermissions: fs.ModeDir | 0777,
			ExpectedErr:         false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Path, func(t *testing.T) {
			f := getFile(tc.Path, tc.Permissions)
			t.Run("Create Directory", func(t *testing.T) {
				err := createDirectory(f)
				if tc.ExpectedErr {
					if err == nil {
						t.Error("expected error, returned successfully")
					}
					return
				}
				if err != nil {
					t.Error(err)
				}
			})
			t.Run("Check Permissions", func(t *testing.T) {
				if runtime.GOOS == "windows" {
					t.Skip("cannot get permissions on Windows")
				}
				permissions, err := getPermissions(f.Path)
				if err != nil {
					t.Error(err)
				}
				if permissions != tc.ExpectedPermissions {
					t.Errorf("expected permissions %v, found %v", tc.ExpectedPermissions, permissions)
				}
			})
		})
	}
}

func TestParsePerm(t *testing.T) {
	testCases := []struct {
		Permissions string

		ExpectedPermissions os.FileMode
		ExpectedErr         bool
	}{
		{
			Permissions: "0777",

			ExpectedPermissions: 0777,
			ExpectedErr:         false,
		},
		{
			Permissions: "0007",

			ExpectedPermissions: 0007,
			ExpectedErr:         false,
		},
		{
			Permissions: "0070",

			ExpectedPermissions: 0070,
			ExpectedErr:         false,
		},
		{
			Permissions: "0700",

			ExpectedPermissions: 0700,
			ExpectedErr:         false,
		},
		{
			Permissions: "0333",

			ExpectedPermissions: 0333,
			ExpectedErr:         false,
		},
		{
			Permissions: "0003",

			ExpectedPermissions: 0003,
			ExpectedErr:         false,
		},
		{
			Permissions: "0030",

			ExpectedPermissions: 0030,
			ExpectedErr:         false,
		},
		{
			Permissions: "0300",

			ExpectedPermissions: 0300,
			ExpectedErr:         false,
		},
		{
			Permissions: "",
			ExpectedErr: true,
		},
	}

	for _, tc := range testCases {
		testName := tc.Permissions
		if len(testName) == 0 {
			testName = "Empty String"
		}
		t.Run(testName, func(t *testing.T) {
			fileMode, err := parsePerm(tc.Permissions)
			if tc.ExpectedErr {
				if err == nil {
					t.Error("expected error, returned successfully")
				}
				return
			}
			if err != nil {
				t.Error(err)
			}
			if fileMode != tc.ExpectedPermissions {
				t.Errorf("expected filemode %v, found %v", tc.ExpectedPermissions, fileMode)
			}
		})
	}
}
