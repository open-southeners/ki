// Package version holds build-time identification of the ki-agent binary.
package version

// Version is the release version, set at build time via
// -ldflags "-X github.com/open-southeners/ki-agent/internal/version.Version=...".
var Version = "dev"

// Commit is the git commit the binary was built from, set at build time via
// -ldflags "-X github.com/open-southeners/ki-agent/internal/version.Commit=...".
var Commit = "dev"
