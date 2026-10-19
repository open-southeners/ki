package git

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultTokenUser is the basic-auth user used when Auth.TokenUser is empty.
const DefaultTokenUser = "x-access-token"

// maxOutput bounds the captured stdout/stderr of a git call (the tail is kept).
const maxOutput = 64 << 10

// Auth is the credential for one call. The zero value means anonymous.
type Auth struct {
	// SSHPrivateKey is an OpenSSH PEM private key; it is written to a 0600
	// temporary file for the duration of the call.
	SSHPrivateKey []byte
	// Token is an https access token, sent as a scoped basic-auth header.
	Token string
	// TokenUser is the basic-auth user for Token (default DefaultTokenUser).
	TokenUser string
}

// Options configures a Client.
type Options struct {
	// DataDir holds git/home, git/known_hosts and tmp.
	DataDir string
	// Binary is the git executable (default "git").
	Binary string
	// Protocols are the allowed transports (default "https", "ssh"); tests
	// add "file".
	Protocols []string
}

// Client runs git subprocesses with a minimal, fixed environment.
type Client struct {
	bin       string
	protocols []string
	home      string
	known     string
	tmp       string
}

// New creates the Client and the directories it needs under DataDir.
func New(opts Options) (*Client, error) {
	if opts.DataDir == "" {
		return nil, errors.New("git: data dir is required")
	}
	c := &Client{
		bin:       opts.Binary,
		protocols: opts.Protocols,
		home:      filepath.Join(opts.DataDir, "git", "home"),
		known:     filepath.Join(opts.DataDir, "git", "known_hosts"),
		tmp:       filepath.Join(opts.DataDir, "tmp"),
	}
	if c.bin == "" {
		c.bin = "git"
	}
	if len(c.protocols) == 0 {
		c.protocols = []string{"https", "ssh"}
	}
	for _, p := range c.protocols {
		if p == "" || strings.ContainsAny(p, ": \t\n") {
			return nil, fmt.Errorf("git: invalid protocol %q", p)
		}
	}
	for _, d := range []string{c.home, c.tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("git: create %s: %w", d, err)
		}
	}
	return c, nil
}

// DefaultBranch returns the remote's default branch (ls-remote --symref HEAD).
func (c *Client) DefaultBranch(ctx context.Context, rawURL string, auth Auth) (string, error) {
	u, err := ParseURL(rawURL, c.protocols)
	if err != nil {
		return "", err
	}
	out, err := c.run(ctx, u, auth, "", "ls-remote", "--symref", "--", u.Raw, "HEAD")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			if name, _, ok := strings.Cut(ref, "\t"); ok && name != "" {
				return name, nil
			}
		}
	}
	return "", &Error{Reason: ReasonRefNotFound, Detail: "remote has no default branch"}
}

// Sync makes dir a checkout of branch's head and returns the commit sha. If
// dir does not exist the branch is cloned; otherwise it is fetched and
// checked out detached, forcing tracked files and keeping untracked ones.
func (c *Client) Sync(ctx context.Context, rawURL string, auth Auth, branch, dir string) (string, error) {
	u, err := ParseURL(rawURL, c.protocols)
	if err != nil {
		return "", err
	}
	if !validBranch(branch) {
		return "", &Error{Reason: ReasonRefNotFound, Detail: "invalid branch name"}
	}
	if dir == "" {
		return "", errors.New("git: checkout directory is required")
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if _, err := c.run(ctx, u, auth, "", "clone", "--single-branch", "--no-tags", "--branch="+branch, "--", u.Raw, dir); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", fmt.Errorf("git: stat checkout: %w", err)
	} else {
		if _, err := c.run(ctx, u, auth, dir, "fetch", "--no-tags", "--", u.Raw, "refs/heads/"+branch); err != nil {
			return "", err
		}
		if _, err := c.run(ctx, u, auth, dir, "checkout", "--force", "--detach", "FETCH_HEAD"); err != nil {
			return "", err
		}
	}
	out, err := c.run(ctx, u, auth, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.Contains(b, "..") {
		return false
	}
	for _, r := range b {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return true
}

// run executes git with args in workDir (empty = inherit) and returns stdout.
func (c *Client) run(ctx context.Context, u URL, auth Auth, workDir string, args ...string) (string, error) {
	keyFile := ""
	if len(auth.SSHPrivateKey) > 0 && u.Scheme == "ssh" {
		f, err := c.writeKey(auth.SSHPrivateKey)
		if err != nil {
			return "", err
		}
		keyFile = f
		defer os.Remove(keyFile)
	}

	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = workDir
	cmd.Env = c.env(u, auth, keyFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr tailBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctxErr)
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return "", classify(stderr.String(), secretsOf(auth)...)
	}
	return stdout.String(), nil
}

func (c *Client) writeKey(pemKey []byte) (string, error) {
	f, err := os.CreateTemp(c.tmp, "key-*")
	if err != nil {
		return "", fmt.Errorf("git: create key file: %w", err)
	}
	name := f.Name()
	if !bytes.HasSuffix(pemKey, []byte("\n")) {
		pemKey = append(slices.Clone(pemKey), '\n')
	}
	_, werr := f.Write(pemKey)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("git: write key file: %w", err)
	}
	return name, nil
}

// env builds the complete subprocess environment; nothing is inherited.
func (c *Client) env(u URL, auth Auth, keyFile string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	allow := slices.Clone(c.protocols)
	if slices.Contains(allow, "https") && !slices.Contains(allow, "http") {
		allow = append(allow, "http") // loopback-only, enforced by ParseURL
	}
	ssh := "ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=" + shellQuote(c.known)
	if keyFile != "" {
		ssh += " -i " + shellQuote(keyFile)
	}

	filePolicy := "never"
	if slices.Contains(c.protocols, "file") {
		filePolicy = "always"
	}
	cfg := [][2]string{
		{"core.hooksPath", "/dev/null"},
		{"init.templateDir", ""},
		{"protocol.file.allow", filePolicy},
	}
	if auth.Token != "" && (u.Scheme == "https" || u.Scheme == "http") {
		cfg = append(cfg, [2]string{
			"http." + u.Scheme + "://" + u.Host + "/.extraHeader",
			"Authorization: " + basicAuth(auth),
		})
	}

	env := []string{
		"PATH=" + path,
		"HOME=" + c.home,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ALLOW_PROTOCOL=" + strings.Join(allow, ":"),
		"GIT_SSH_COMMAND=" + ssh,
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(cfg)),
	}
	for i, kv := range cfg {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+kv[0], "GIT_CONFIG_VALUE_"+n+"="+kv[1])
	}
	return env
}

func basicAuth(a Auth) string {
	user := a.TokenUser
	if user == "" {
		user = DefaultTokenUser
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+a.Token))
}

// secretsOf lists the strings that must be scrubbed from error output.
func secretsOf(a Auth) []string {
	if a.Token == "" {
		return nil
	}
	enc := strings.TrimPrefix(basicAuth(a), "Basic ")
	return []string{a.Token, enc}
}

// shellQuote quotes s for git's shell-split GIT_SSH_COMMAND when needed.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=,@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tailBuffer keeps the last maxOutput bytes written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > maxOutput {
		t.b = append([]byte(nil), t.b[len(t.b)-maxOutput:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
