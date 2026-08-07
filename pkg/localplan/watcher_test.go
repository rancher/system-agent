package localplan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/applyinator"
)

func TestPositionFileName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		planPath string
		want     string
	}{
		{name: "plan suffix is replaced", planPath: "/plans/foo.plan", want: "/plans/foo.pos"},
		{name: "no plan suffix just appends", planPath: "/plans/foo", want: "/plans/foo.pos"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := positionFileName(tt.planPath); got != tt.want {
				t.Errorf("positionFileName(%q) = %q, want %q", tt.planPath, got, tt.want)
			}
		})
	}
}

func TestReadPositionFile(t *testing.T) {
	t.Parallel()

	t.Run("missing file returns empty, no error", func(t *testing.T) {
		t.Parallel()
		data, err := readPositionFile(filepath.Join(t.TempDir(), "does-not-exist.pos"))
		if err != nil {
			t.Fatalf("readPositionFile returned error: %v", err)
		}
		if len(data) != 0 {
			t.Errorf("expected empty data, got %q", data)
		}
	})

	t.Run("existing file returns its contents", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "existing.pos")
		if err := os.WriteFile(path, []byte(`{"appliedChecksum":"abc"}`), 0600); err != nil {
			t.Fatal(err)
		}
		data, err := readPositionFile(path)
		if err != nil {
			t.Fatalf("readPositionFile returned error: %v", err)
		}
		if string(data) != `{"appliedChecksum":"abc"}` {
			t.Errorf("readPositionFile() = %q, want the file's contents", data)
		}
	})
}

func TestParsePositionData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    []byte
		want    NodePlanPosition
		wantErr bool
	}{
		{name: "empty data returns zero value", data: nil, want: NodePlanPosition{}},
		{name: "empty slice returns zero value", data: []byte{}, want: NodePlanPosition{}},
		{
			name: "valid JSON is parsed",
			data: []byte(`{"appliedChecksum":"abc123"}`),
			want: NodePlanPosition{AppliedChecksum: "abc123"},
		},
		{name: "invalid JSON returns an error", data: []byte("not json"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePositionData(tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePositionData() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got.AppliedChecksum != tt.want.AppliedChecksum {
				t.Errorf("parsePositionData() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNeedsApplication(t *testing.T) {
	t.Parallel()

	w := &watcher{}
	probeStatus := map[string]planapi.ProbeStatus{"probe-a": {Healthy: true}}

	tests := []struct {
		name             string
		appliedChecksum  string
		computedChecksum string
		wantApplied      bool
	}{
		{name: "checksum matches: no application needed", appliedChecksum: "abc", computedChecksum: "abc", wantApplied: false},
		{name: "checksum differs: application needed", appliedChecksum: "abc", computedChecksum: "def", wantApplied: true},
		{name: "no prior checksum: application needed", appliedChecksum: "", computedChecksum: "def", wantApplied: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			planPosition := NodePlanPosition{AppliedChecksum: tt.appliedChecksum, ProbeStatus: probeStatus}
			cp := applyinator.CalculatedPlan{Checksum: tt.computedChecksum}

			gotApplied, gotProbeStatus, err := w.needsApplication(planPosition, cp)
			if err != nil {
				t.Fatalf("needsApplication returned error: %v", err)
			}
			if gotApplied != tt.wantApplied {
				t.Errorf("needsApplication() applied = %v, want %v", gotApplied, tt.wantApplied)
			}
			if gotProbeStatus["probe-a"] != probeStatus["probe-a"] {
				t.Errorf("expected the existing probe status to be passed through unchanged, got %+v", gotProbeStatus)
			}
		})
	}
}

func TestSkipFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileName string
		skips    map[string]bool
		want     bool
	}{
		{name: "hidden file is skipped", fileName: ".hidden.plan", skips: map[string]bool{}, want: true},
		{name: "explicitly skipped name is skipped", fileName: "foo.plan", skips: map[string]bool{"foo.plan": true}, want: true},
		{name: "plan file is not skipped", fileName: "foo.plan", skips: map[string]bool{}, want: false},
		{name: "non-plan file is skipped", fileName: "foo.txt", skips: map[string]bool{}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := skipFile(tt.fileName, tt.skips); got != tt.want {
				t.Errorf("skipFile(%q, %v) = %v, want %v", tt.fileName, tt.skips, got, tt.want)
			}
		})
	}
}

func TestListFilesAggregatesErrorsAcrossBases(t *testing.T) {
	t.Parallel()

	w := &watcher{
		applyinator: *applyinator.NewApplyinator(t.TempDir(), false, "", "", nil),
		bases:       []string{t.TempDir(), filepath.Join(t.TempDir(), "does-not-exist")},
	}

	err := w.listFiles(context.Background(), false)
	if err == nil {
		t.Fatal("expected an aggregated error when one base directory does not exist, got nil")
	}
}

func TestListFilesNoErrorsWhenAllBasesSucceed(t *testing.T) {
	t.Parallel()

	w := &watcher{
		applyinator: *applyinator.NewApplyinator(t.TempDir(), false, "", "", nil),
		bases:       []string{t.TempDir(), t.TempDir()},
	}

	if err := w.listFiles(context.Background(), false); err != nil {
		t.Fatalf("listFiles returned error: %v", err)
	}
}

func TestWatchFilesCreatesMissingPlanDirectories(t *testing.T) {
	t.Parallel()

	// Nothing else on the node creates the local plan directory, so WatchFiles must. Without it,
	// listFiles reports a filepath.Walk error on every 5s poll for the lifetime of the daemon.
	parent := t.TempDir()
	missing := filepath.Join(parent, "plans")
	nested := filepath.Join(parent, "nested", "plans")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	WatchFiles(ctx, *applyinator.NewApplyinator(t.TempDir(), false, "", "", nil), missing, nested)

	for _, dir := range []string{missing, nested} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("expected WatchFiles to create %s, stat failed: %v", dir, err)
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be a directory", dir)
		}
	}

	// The created directories must then walk cleanly, i.e. no recurring error.
	w := &watcher{
		applyinator: *applyinator.NewApplyinator(t.TempDir(), false, "", "", nil),
		bases:       []string{missing, nested},
	}
	if err := w.listFiles(ctx, false); err != nil {
		t.Errorf("expected no error walking the created directories, got %v", err)
	}
}
