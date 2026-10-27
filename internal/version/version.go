// Package version holds build-time identification of the ki-agent binary.
package version

// Version is the release version, set at build time via
// -ldflags "-X github.com/open-southeners/ki/internal/version.Version=...".
var Version = "dev"

// Commit is the git commit the binary was built from, set at build time via
// -ldflags "-X github.com/open-southeners/ki/internal/version.Commit=...".
var Commit = "dev"

// EnrollURL is the default control-plane base URL used by `ki agent enroll`
// when none is given. Empty in source builds; official release builds set it via
// -ldflags "-X github.com/open-southeners/ki/internal/version.EnrollURL=...".
var EnrollURL = ""
