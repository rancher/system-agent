package version

import "fmt"

// Build-time version variables. Set via ldflags in CI and releases.
var (
	Version   = "dev"
	GitCommit = "HEAD"
)

func FriendlyVersion() string {
	return fmt.Sprintf("%s (%s)", Version, GitCommit)
}
