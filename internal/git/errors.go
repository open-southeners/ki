package git

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Failure reasons, mirroring the wire RepositoryError reasons.
const (
	// ReasonInvalidURL means the repository URL was rejected before any
	// subprocess ran.
	ReasonInvalidURL = "invalid_url"
	// ReasonUnreachable means the host could not be contacted.
	ReasonUnreachable = "unreachable"
	// ReasonAuthFailed means the remote refused the credentials (or none
	// were given).
	ReasonAuthFailed = "auth_failed"
	// ReasonNotFound means the repository does not exist (or is hidden).
	ReasonNotFound = "not_found"
	// ReasonRefNotFound means the requested branch does not exist.
	ReasonRefNotFound = "ref_not_found"
	// ReasonComposeNotFound means no compose file was found in the checkout.
	ReasonComposeNotFound = "compose_not_found"
	// ReasonFailed is any other git failure.
	ReasonFailed = "failed"
)

// maxDetail is the longest Error.Detail, in bytes.
const maxDetail = 300

// Error is a classified git failure. Detail is human readable and contains no
// credentials.
type Error struct {
	Reason, Detail string
}

// Error implements error.
func (e *Error) Error() string {
	if e.Detail == "" {
		return "git: " + e.Reason
	}
	return "git: " + e.Reason + ": " + e.Detail
}

var classifiers = []struct {
	reason string
	re     *regexp.Regexp
}{
	{ReasonAuthFailed, regexp.MustCompile(`Authentication failed|Permission denied \(publickey|could not read Username|HTTP 40[13]|error: 40[13]`)},
	{ReasonNotFound, regexp.MustCompile(`Repository not found|does not appear to be a git repository|HTTP 404|error: 404|repository '.*' not found`)},
	{ReasonUnreachable, regexp.MustCompile(`Could not resolve host|Connection refused|timed out|Network is unreachable|No route to host|Connection reset`)},
	{ReasonRefNotFound, regexp.MustCompile(`Remote branch .* not found|couldn't find remote ref`)},
}

var (
	authHeaderRe = regexp.MustCompile(`(?i)authorization.*`)
	userinfoRe   = regexp.MustCompile(`(//)[^/@\s]+@`)
)

// classify turns git's stderr (LC_ALL=C) into an *Error. secrets are literal
// strings (tokens, encoded headers) that must not appear in the detail. The
// detail is the stderr line that matched the classification, or the last
// meaningful line when nothing matched.
func classify(stderr string, secrets ...string) *Error {
	lines := strings.Split(stderr, "\n")
	reason, line := ReasonFailed, ""
	for _, c := range classifiers {
		if l, ok := firstMatch(lines, c.re); ok {
			reason, line = c.reason, l
			break
		}
	}
	if line == "" {
		line = lastLine(lines)
	}
	return &Error{Reason: reason, Detail: redact(line, secrets)}
}

// firstMatch returns the first trimmed line matching re.
func firstMatch(lines []string, re *regexp.Regexp) (string, bool) {
	for _, l := range lines {
		if s := strings.TrimSpace(l); re.MatchString(s) {
			return s, true
		}
	}
	return "", false
}

// boilerplateRe matches git's generic trailer lines, which say nothing about
// the cause.
var boilerplateRe = regexp.MustCompile(`^(Please make sure you have the correct access rights|and the repository exists\.?)$`)

// lastLine returns the last non-empty line that is not boilerplate, falling
// back to the last non-empty line.
func lastLine(lines []string) string {
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" {
			continue
		}
		if !boilerplateRe.MatchString(s) {
			return s
		}
		if last == "" {
			last = s
		}
	}
	return last
}

// redact removes secrets and credentials from line and caps it at maxDetail.
func redact(line string, secrets []string) string {
	for _, s := range secrets {
		if s != "" {
			line = strings.ReplaceAll(line, s, "[redacted]")
		}
	}
	line = authHeaderRe.ReplaceAllString(line, "[redacted]")
	line = userinfoRe.ReplaceAllString(line, "${1}[redacted]@")
	if len(line) > maxDetail {
		line = line[:maxDetail]
		for !utf8.ValidString(line) {
			line = line[:len(line)-1]
		}
	}
	return line
}
