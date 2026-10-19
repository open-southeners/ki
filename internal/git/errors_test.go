package git

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name, stderr, reason, detail string
	}{
		{"auth failed", "remote: x\nfatal: Authentication failed for 'https://h/a/b/'\n", ReasonAuthFailed, "fatal: Authentication failed for 'https://h/a/b/'"},
		{"publickey", "git@h: Permission denied (publickey).\nfatal: Could not read from remote repository.\n", ReasonAuthFailed, "git@h: Permission denied (publickey)."},
		{"ssh auth multi-line", "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n", ReasonAuthFailed, "git@github.com: Permission denied (publickey)."},
		{"https auth multi-line", "Cloning into '/tmp/x'...\nremote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/a/b.git/'\n", ReasonAuthFailed, "fatal: Authentication failed for 'https://github.com/a/b.git/'"},
		{"not found multi-line", "Cloning into '/tmp/x'...\nremote: Repository not found.\nfatal: repository 'https://github.com/a/b.git/' not found\n", ReasonNotFound, "remote: Repository not found."},
		{"unreachable multi-line", "Cloning into '/tmp/x'...\nssh: connect to host github.com port 22: Connection timed out\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n", ReasonUnreachable, "ssh: connect to host github.com port 22: Connection timed out"},
		{"boilerplate only falls back", "fatal: oops\nPlease make sure you have the correct access rights\nand the repository exists.\n", ReasonFailed, "fatal: oops"},
		{"no username", "fatal: could not read Username for 'https://h': terminal prompts disabled\n", ReasonAuthFailed, ""},
		{"http 403", "fatal: unable to access 'https://h/a/b/': The requested URL returned error: 403\n", ReasonAuthFailed, ""},
		{"http 401", "fatal: HTTP 401\n", ReasonAuthFailed, ""},
		{"repo not found", "remote: Repository not found.\nfatal: repository 'https://h/a/b/' not found\n", ReasonNotFound, ""},
		{"not a repo", "fatal: '/x' does not appear to be a git repository\nfatal: Could not read from remote repository.\n", ReasonNotFound, ""},
		{"http 404", "fatal: unable to access 'https://h/a/b/': The requested URL returned error: 404\n", ReasonNotFound, ""},
		{"dns", "ssh: Could not resolve hostname h: Name or service not known\nfatal: unable\nCould not resolve host: h\n", ReasonUnreachable, ""},
		{"refused", "ssh: connect to host h port 22: Connection refused\n", ReasonUnreachable, ""},
		{"timeout", "ssh: connect to host h port 22: Connection timed out\n", ReasonUnreachable, ""},
		{"network", "connect: Network is unreachable\n", ReasonUnreachable, ""},
		{"branch", "warning: Could not find remote branch nope to clone.\nfatal: Remote branch nope not found in upstream origin\n", ReasonRefNotFound, ""},
		{"ref", "fatal: couldn't find remote ref refs/heads/nope\n", ReasonRefNotFound, ""},
		{"other", "fatal: something odd\n\n  \n", ReasonFailed, "fatal: something odd"},
		{"empty", "", ReasonFailed, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := classify(tc.stderr)
			if e.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", e.Reason, tc.reason)
			}
			if tc.detail != "" && e.Detail != tc.detail {
				t.Errorf("detail = %q, want %q", e.Detail, tc.detail)
			}
		})
	}
}

func TestClassifyRedactsAndTruncates(t *testing.T) {
	tests := []struct {
		name, stderr string
		secrets      []string
		absent       string
	}{
		{"header", "fatal: bad header Authorization: Basic c2VjcmV0\n", nil, "c2VjcmV0"},
		{"lowercase header", "> authorization: basic c2VjcmV0\n", nil, "c2VjcmV0"},
		{"userinfo", "fatal: unable to access 'https://user:hunter2@h/a/b/'\n", nil, "hunter2"},
		{"literal token", "fatal: remote said tok-123 is bad\n", []string{"tok-123"}, "tok-123"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := classify(tc.stderr, tc.secrets...)
			if strings.Contains(e.Detail, tc.absent) || !strings.Contains(e.Detail, "[redacted]") {
				t.Errorf("detail not redacted: %q", e.Detail)
			}
		})
	}

	e := classify("fatal: " + strings.Repeat("é", 400))
	if len(e.Detail) > maxDetail || strings.ContainsRune(e.Detail, '�') {
		t.Errorf("detail len %d / invalid utf-8", len(e.Detail))
	}
}

func TestErrorString(t *testing.T) {
	if got := (&Error{Reason: ReasonFailed}).Error(); got != "git: failed" {
		t.Errorf("got %q", got)
	}
	if got := (&Error{Reason: ReasonNotFound, Detail: "x"}).Error(); got != "git: not_found: x" {
		t.Errorf("got %q", got)
	}
}
