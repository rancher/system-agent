package version

import "testing"

func TestFriendlyVersion(t *testing.T) {
	origVersion, origCommit := Version, GitCommit
	t.Cleanup(func() { Version, GitCommit = origVersion, origCommit })

	tests := []struct {
		name      string
		version   string
		gitCommit string
		want      string
	}{
		{name: "dev defaults", version: "dev", gitCommit: "HEAD", want: "dev (HEAD)"},
		{name: "released version", version: "v0.14.0", gitCommit: "abc1234", want: "v0.14.0 (abc1234)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Version, GitCommit = tt.version, tt.gitCommit
			if got := FriendlyVersion(); got != tt.want {
				t.Errorf("FriendlyVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}
