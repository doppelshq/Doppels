// Package version holds the CLI build identity.
// GoReleaser and task build inject Version via -ldflags.
package version

// Version is the semver string for this binary (tag without leading v, or "dev").
var Version = "dev"
