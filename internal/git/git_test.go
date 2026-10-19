package git

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testEnv = []string{
	"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
	"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), testEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// seedRepo builds a bare repo with commits on main and dev and a tag, and
// returns its file:// URL plus the seeding work tree.
func seedRepo(t *testing.T) (repoURL, work string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	work = filepath.Join(root, "work")
	gitRun(t, root, "init", "--bare", "--initial-branch=main", bare)
	gitRun(t, root, "init", "--initial-branch=main", work)
	gitRun(t, work, "remote", "add", "origin", bare)
	commit(t, work, "README", "hello\n")
	gitRun(t, work, "tag", "v1")
	gitRun(t, work, "checkout", "-b", "dev")
	commit(t, work, "DEV", "dev\n")
	gitRun(t, work, "checkout", "main")
	gitRun(t, work, "push", "origin", "main", "dev", "v1")
	return "file://" + bare, work
}

func commit(t *testing.T, work, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", name)
	gitRun(t, work, "commit", "-m", "add "+name)
	return gitRun(t, work, "rev-parse", "HEAD")
}

func newClient(t *testing.T, mod func(*Options)) *Client {
	t.Helper()
	opts := Options{DataDir: t.TempDir(), Protocols: []string{"file"}}
	if mod != nil {
		mod(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v, want *Error", err)
	}
	return ge.Reason
}

func TestNewCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(Options{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"git/home", "tmp"} {
		if st, err := os.Stat(filepath.Join(dir, d)); err != nil || !st.IsDir() {
			t.Errorf("%s not created: %v", d, err)
		}
	}
	if _, err := New(Options{}); err == nil {
		t.Error("expected error without data dir")
	}
	if _, err := New(Options{DataDir: dir, Protocols: []string{"ext::x"}}); err == nil {
		t.Error("expected error for bad protocol")
	}
}

func TestDefaultBranch(t *testing.T) {
	url, work := seedRepo(t)
	c := newClient(t, nil)
	got, err := c.DefaultBranch(context.Background(), url, Auth{})
	if err != nil || got != "main" {
		t.Fatalf("got %q, %v", got, err)
	}
	// Retarget remote HEAD.
	gitRun(t, work, "--git-dir", strings.TrimPrefix(url, "file://"), "symbolic-ref", "HEAD", "refs/heads/dev")
	got, err = c.DefaultBranch(context.Background(), url, Auth{})
	if err != nil || got != "dev" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDefaultBranchErrors(t *testing.T) {
	c := newClient(t, nil)
	_, err := c.DefaultBranch(context.Background(), "file://"+filepath.Join(t.TempDir(), "nope.git"), Auth{})
	if r := reasonOf(t, err); r != ReasonNotFound {
		t.Errorf("reason = %q", r)
	}
	_, err = c.DefaultBranch(context.Background(), "ext::sh -c id", Auth{})
	if r := reasonOf(t, err); r != ReasonInvalidURL {
		t.Errorf("reason = %q", r)
	}
	// file:// is not allowed with default protocols.
	c2 := newClient(t, func(o *Options) { o.Protocols = nil })
	url, _ := seedRepo(t)
	_, err = c2.DefaultBranch(context.Background(), url, Auth{})
	if r := reasonOf(t, err); r != ReasonInvalidURL {
		t.Errorf("reason = %q", r)
	}
}

func TestSyncCloneAndFetch(t *testing.T) {
	url, work := seedRepo(t)
	c := newClient(t, nil)
	dir := filepath.Join(t.TempDir(), "checkout")
	ctx := context.Background()

	want := gitRun(t, work, "rev-parse", "main")
	got, err := c.Sync(ctx, url, Auth{}, "main", dir)
	if err != nil || got != want {
		t.Fatalf("clone: got %q, %v; want %q", got, err, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Errorf("README missing: %v", err)
	}

	// Untracked file must survive a later sync; .git/config holds the plain URL.
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("X=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "extraHeader") || strings.Contains(string(cfg), "hooksPath") {
		t.Errorf("unexpected config persisted:\n%s", cfg)
	}

	want = commit(t, work, "NEW", "new\n")
	gitRun(t, work, "push", "origin", "main")
	got, err = c.Sync(ctx, url, Auth{}, "main", dir)
	if err != nil || got != want {
		t.Fatalf("fetch: got %q, %v; want %q", got, err, want)
	}
	for _, f := range []string{"NEW", ".env"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s missing after fetch: %v", f, err)
		}
	}
	if head := gitRun(t, dir, "rev-parse", "HEAD"); head != want {
		t.Errorf("HEAD = %s", head)
	}

	// Switch to another branch in the same checkout; tracked state is forced.
	got, err = c.Sync(ctx, url, Auth{}, "dev", dir)
	if err != nil || got != gitRun(t, work, "rev-parse", "dev") {
		t.Fatalf("dev: got %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "DEV")); err != nil {
		t.Errorf("DEV missing: %v", err)
	}
}

func TestSyncErrors(t *testing.T) {
	url, _ := seedRepo(t)
	c := newClient(t, nil)
	ctx := context.Background()

	_, err := c.Sync(ctx, url, Auth{}, "nope", filepath.Join(t.TempDir(), "a"))
	if r := reasonOf(t, err); r != ReasonRefNotFound {
		t.Errorf("clone missing branch: reason = %q", r)
	}

	dir := filepath.Join(t.TempDir(), "b")
	if _, err := c.Sync(ctx, url, Auth{}, "main", dir); err != nil {
		t.Fatal(err)
	}
	_, err = c.Sync(ctx, url, Auth{}, "nope", dir)
	if r := reasonOf(t, err); r != ReasonRefNotFound {
		t.Errorf("fetch missing branch: reason = %q", r)
	}

	_, err = c.Sync(ctx, "file://"+filepath.Join(t.TempDir(), "gone.git"), Auth{}, "main", filepath.Join(t.TempDir(), "c"))
	if r := reasonOf(t, err); r != ReasonNotFound {
		t.Errorf("missing repo: reason = %q", r)
	}

	for _, b := range []string{"", "-x", "a..b", "a b", "a:b"} {
		_, err = c.Sync(ctx, url, Auth{}, b, filepath.Join(t.TempDir(), "d"))
		if r := reasonOf(t, err); r != ReasonRefNotFound {
			t.Errorf("branch %q: reason = %q", b, r)
		}
	}
}

func TestSyncContextCancel(t *testing.T) {
	url, _ := seedRepo(t)
	c := newClient(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Sync(ctx, url, Auth{}, "main", filepath.Join(t.TempDir(), "x"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestEnv(t *testing.T) {
	c := newClient(t, func(o *Options) { o.Protocols = []string{"https", "ssh"} })
	envMap := func(env []string) map[string]string {
		m := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}

	t.Run("token scoped to host", func(t *testing.T) {
		u, _ := ParseURL("https://git.example.com:8443/a/b.git", c.protocols)
		env := c.env(u, Auth{Token: "s3cret", TokenUser: "bob"}, "")
		m := envMap(env)
		want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("bob:s3cret"))
		found := false
		for i := 0; ; i++ {
			k, ok := m["GIT_CONFIG_KEY_"+string(rune('0'+i))]
			if !ok {
				break
			}
			if k == "http.https://git.example.com:8443/.extraHeader" {
				found = true
				if v := m["GIT_CONFIG_VALUE_"+string(rune('0'+i))]; v != want {
					t.Errorf("header = %q, want %q", v, want)
				}
			}
		}
		if !found {
			t.Errorf("scoped extraHeader missing: %v", env)
		}
		if m["GIT_CONFIG_COUNT"] != "4" {
			t.Errorf("count = %q", m["GIT_CONFIG_COUNT"])
		}
	})

	t.Run("default token user", func(t *testing.T) {
		u, _ := ParseURL("https://h.example/a/b", c.protocols)
		joined := strings.Join(c.env(u, Auth{Token: "t"}, ""), "\n")
		enc := base64.StdEncoding.EncodeToString([]byte("x-access-token:t"))
		if !strings.Contains(joined, enc) {
			t.Error("default token user not applied")
		}
	})

	t.Run("no token no header, no ssh token leak", func(t *testing.T) {
		u, _ := ParseURL("git@h.example:a/b.git", c.protocols)
		joined := strings.Join(c.env(u, Auth{Token: "t"}, ""), "\n")
		if strings.Contains(joined, "extraHeader") || strings.Contains(joined, "Authorization") {
			t.Error("token leaked to ssh remote")
		}
	})

	t.Run("fixed environment", func(t *testing.T) {
		t.Setenv("GIT_DIR", "/evil")
		t.Setenv("SSH_AUTH_SOCK", "/evil")
		u, _ := ParseURL("git@h.example:a/b.git", c.protocols)
		env := c.env(u, Auth{}, "/data/tmp/key-1")
		m := envMap(env)
		for _, k := range []string{"GIT_DIR", "SSH_AUTH_SOCK"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s inherited", k)
			}
		}
		checks := map[string]string{
			"HOME":                c.home,
			"LC_ALL":              "C",
			"GIT_TERMINAL_PROMPT": "0",
			"GIT_CONFIG_NOSYSTEM": "1",
			"GIT_CONFIG_GLOBAL":   "/dev/null",
			"GIT_ALLOW_PROTOCOL":  "https:ssh:http",
			"GIT_CONFIG_KEY_0":    "core.hooksPath",
			"GIT_CONFIG_VALUE_0":  "/dev/null",
			"GIT_CONFIG_KEY_2":    "protocol.file.allow",
			"GIT_CONFIG_VALUE_2":  "never",
			"GIT_CONFIG_COUNT":    "3",
		}
		for k, v := range checks {
			if m[k] != v {
				t.Errorf("%s = %q, want %q", k, m[k], v)
			}
		}
		ssh := m["GIT_SSH_COMMAND"]
		for _, s := range []string{"ssh -F /dev/null", "BatchMode=yes", "IdentitiesOnly=yes", "StrictHostKeyChecking=accept-new", "UserKnownHostsFile=" + c.known, "-i /data/tmp/key-1"} {
			if !strings.Contains(ssh, s) {
				t.Errorf("GIT_SSH_COMMAND %q missing %q", ssh, s)
			}
		}
	})

	t.Run("file allowed", func(t *testing.T) {
		fc := newClient(t, nil)
		u, _ := ParseURL("file:///x/y.git", fc.protocols)
		m := envMap(fc.env(u, Auth{}, ""))
		if m["GIT_ALLOW_PROTOCOL"] != "file" || m["GIT_CONFIG_VALUE_2"] != "always" {
			t.Errorf("env = %v", m)
		}
		if strings.Contains(m["GIT_SSH_COMMAND"], " -i ") {
			t.Error("unexpected -i")
		}
	})
}

// fakeGit writes a stand-in git binary that records argv and environment,
// shows the contents of the -i key file, and prints ls-remote output.
func fakeGit(t *testing.T, record string) string {
	t.Helper()
	script := `#!/bin/sh
{
  echo "ARGV:$*"
  env
  key=$(printf '%s' "$GIT_SSH_COMMAND" | sed -n 's/.* -i \([^ ]*\).*/\1/p')
  if [ -n "$key" ]; then
    echo "KEYFILE:$key"
    echo "KEYMODE:$(stat -c %a "$key")"
    echo "KEYDATA:$(cat "$key" | head -n 1)"
  fi
} > ` + record + `
printf 'ref: refs/heads/trunk\tHEAD\n'
`
	p := filepath.Join(t.TempDir(), "fakegit")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTokenNotInArgv(t *testing.T) {
	record := filepath.Join(t.TempDir(), "rec")
	c := newClient(t, func(o *Options) { o.Binary = fakeGit(t, record); o.Protocols = []string{"https"} })
	got, err := c.DefaultBranch(context.Background(), "https://git.example.com/a/b.git", Auth{Token: "s3cret-token"})
	if err != nil || got != "trunk" {
		t.Fatalf("got %q, %v", got, err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var argv string
	for _, l := range strings.Split(string(data), "\n") {
		if a, ok := strings.CutPrefix(l, "ARGV:"); ok {
			argv = a
		}
	}
	if argv != "ls-remote --symref -- https://git.example.com/a/b.git HEAD" {
		t.Errorf("argv = %q", argv)
	}
	enc := base64.StdEncoding.EncodeToString([]byte("x-access-token:s3cret-token"))
	if strings.Contains(argv, "s3cret-token") || strings.Contains(argv, enc) {
		t.Error("token in argv")
	}
	if !strings.Contains(string(data), "http.https://git.example.com/.extraHeader") || !strings.Contains(string(data), enc) {
		t.Errorf("header not passed via environment:\n%s", data)
	}
}

func TestKeyFileLifecycle(t *testing.T) {
	record := filepath.Join(t.TempDir(), "rec")
	c := newClient(t, func(o *Options) { o.Binary = fakeGit(t, record); o.Protocols = []string{"ssh"} })
	pemKey, _, err := GenerateDeployKey("t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DefaultBranch(context.Background(), "git@h.example:a/b.git", Auth{SSHPrivateKey: pemKey}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(record)
	rec := string(data)
	var keyFile string
	for _, l := range strings.Split(rec, "\n") {
		if f, ok := strings.CutPrefix(l, "KEYFILE:"); ok {
			keyFile = f
		}
	}
	if keyFile == "" {
		t.Fatalf("no key file passed:\n%s", rec)
	}
	if !strings.HasPrefix(keyFile, c.tmp+string(filepath.Separator)) {
		t.Errorf("key file %q outside tmp dir", keyFile)
	}
	if !strings.Contains(rec, "KEYMODE:600") || !strings.Contains(rec, "KEYDATA:-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("key file mode/content wrong:\n%s", rec)
	}
	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("key file still exists after call: %v", err)
	}
	for _, l := range strings.Split(rec, "\n") {
		if strings.HasPrefix(l, "ARGV:") && strings.Contains(l, "PRIVATE KEY") {
			t.Error("key material in argv")
		}
	}
}

func TestKeyFileRemovedOnFailure(t *testing.T) {
	c := newClient(t, func(o *Options) { o.Binary = "/bin/false"; o.Protocols = []string{"ssh"} })
	pemKey, _, _ := GenerateDeployKey("t")
	_, err := c.DefaultBranch(context.Background(), "git@h.example:a/b.git", Auth{SSHPrivateKey: pemKey})
	if r := reasonOf(t, err); r != ReasonFailed {
		t.Errorf("reason = %q", r)
	}
	entries, _ := os.ReadDir(c.tmp)
	if len(entries) != 0 {
		t.Errorf("leftover files in tmp: %v", entries)
	}
}

func TestCancelKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := filepath.Join(t.TempDir(), "slowgit")
	body := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, func(o *Options) { o.Binary = script })
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.DefaultBranch(ctx, "file:///x/y.git", Auth{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("cancel did not return promptly")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(raw))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + pid); err != nil {
			return
		}
		st, _ := os.ReadFile("/proc/" + pid + "/stat")
		if strings.Contains(string(st), ") Z ") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("child process survived cancel")
}
