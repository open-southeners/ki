package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Ref is a resolved remote ref.
type Ref struct {
	// Name is the full ref name (refs/heads/... or refs/tags/...); empty when
	// the ref was given as a bare commit sha.
	Name string
	// Commit is the commit sha; annotated tags are peeled to their commit.
	Commit string
}

// ResolveRef resolves ref against the remote with ls-remote. ref may be a full
// refs/heads/ or refs/tags/ name, a bare name (branch first, then tag) or a
// full 40/64-hex sha, which is returned lowercased with an empty Name and not
// checked against the remote. A ref the remote does not have yields
// *Error{Reason: ReasonRefNotFound}.
func (c *Client) ResolveRef(ctx context.Context, rawURL string, auth Auth, ref string) (Ref, error) {
	u, err := ParseURL(rawURL, c.protocols)
	if err != nil {
		return Ref{}, err
	}
	if isSHA(ref) {
		return Ref{Commit: strings.ToLower(ref)}, nil
	}
	if !validBranch(ref) {
		return Ref{}, &Error{Reason: ReasonRefNotFound, Detail: "invalid ref name"}
	}
	candidates := []string{ref}
	if !strings.HasPrefix(ref, "refs/") {
		candidates = []string{"refs/heads/" + ref, "refs/tags/" + ref}
	} else if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return Ref{}, &Error{Reason: ReasonRefNotFound, Detail: "ref must be a branch or tag"}
	}
	args := []string{"ls-remote", "--", u.Raw}
	for _, cand := range candidates {
		args = append(args, cand, cand+"^{}")
	}
	out, err := c.run(ctx, u, auth, "", args...)
	if err != nil {
		return Ref{}, err
	}
	found := parseRefs(out)
	for _, cand := range candidates {
		if sha, ok := found[cand]; ok {
			return Ref{Name: cand, Commit: sha}, nil
		}
	}
	return Ref{}, &Error{Reason: ReasonRefNotFound, Detail: "ref not found on remote"}
}

// ListTags returns the remote's tags newest first by version sort, with
// annotated tags peeled to their commit.
func (c *Client) ListTags(ctx context.Context, rawURL string, auth Auth) ([]Ref, error) {
	u, err := ParseURL(rawURL, c.protocols)
	if err != nil {
		return nil, err
	}
	out, err := c.run(ctx, u, auth, "", "ls-remote", "--tags", "--sort=-v:refname", "--", u.Raw)
	if err != nil {
		return nil, err
	}
	peeled := parseRefs(out)
	var tags []Ref
	for line := range strings.SplitSeq(out, "\n") {
		_, name, ok := splitRefLine(line)
		if !ok || strings.HasSuffix(name, "^{}") {
			continue
		}
		tags = append(tags, Ref{Name: name, Commit: peeled[name]})
	}
	return tags, nil
}

// Checkout makes dir a detached checkout of fetchRef's head, or of commit when
// given, and returns the resulting HEAD sha. fetchRef is fetched into dir
// (created and initialised when missing); commit must be an ancestor of, or
// equal to, the fetched tip, else the error is ReasonRefNotFound. Tracked
// files are forced, untracked ones kept.
func (c *Client) Checkout(ctx context.Context, rawURL string, auth Auth, dir, fetchRef, commit string) (string, error) {
	u, err := ParseURL(rawURL, c.protocols)
	if err != nil {
		return "", err
	}
	if !validBranch(fetchRef) {
		return "", &Error{Reason: ReasonRefNotFound, Detail: "invalid ref name"}
	}
	if commit != "" && !isSHA(commit) {
		return "", &Error{Reason: ReasonRefNotFound, Detail: "invalid commit sha"}
	}
	commit = strings.ToLower(commit)
	if dir == "" {
		return "", errors.New("git: checkout directory is required")
	}
	created := false
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if _, err := c.run(ctx, u, auth, "", "init", "--quiet", "--", dir); err != nil {
			return "", err
		}
		created = true
	} else if err != nil {
		return "", fmt.Errorf("git: stat checkout: %w", err)
	}
	sha, err := c.fetchAndCheckout(ctx, u, auth, dir, fetchRef, commit)
	if err != nil && created {
		os.RemoveAll(dir)
	}
	return sha, err
}

func (c *Client) fetchAndCheckout(ctx context.Context, u URL, auth Auth, dir, fetchRef, commit string) (string, error) {
	if _, err := c.run(ctx, u, auth, dir, "fetch", "--no-tags", "--", u.Raw, fetchRef); err != nil {
		return "", err
	}
	target := "FETCH_HEAD"
	if commit != "" {
		if _, err := c.run(ctx, u, auth, dir, "merge-base", "--is-ancestor", commit, "FETCH_HEAD^{commit}"); err != nil {
			var ge *Error
			if errors.As(err, &ge) {
				return "", &Error{Reason: ReasonRefNotFound, Detail: "commit not on ref"}
			}
			return "", err
		}
		target = commit
	}
	if _, err := c.run(ctx, u, auth, dir, "checkout", "--force", "--detach", target); err != nil {
		return "", err
	}
	out, err := c.run(ctx, u, auth, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// IsTracked reports whether path (relative to the checkout dir) is tracked by
// the checkout's index.
func (c *Client) IsTracked(ctx context.Context, dir, path string) (bool, error) {
	out, err := c.run(ctx, URL{}, Auth{}, dir, "ls-files", "--", path)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// isSHA reports whether s is a full 40 or 64 digit hex object name.
func isSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f' || 'A' <= r && r <= 'F') {
			return false
		}
	}
	return true
}

// splitRefLine splits one "<sha>\t<name>" ls-remote line.
func splitRefLine(line string) (sha, name string, ok bool) {
	sha, name, ok = strings.Cut(strings.TrimSpace(line), "\t")
	return sha, name, ok && sha != "" && name != ""
}

// parseRefs maps ref name to commit sha from ls-remote output; a peeled
// (^{}) line overrides the tag object's own sha.
func parseRefs(out string) map[string]string {
	m := map[string]string{}
	peeled := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		sha, name, ok := splitRefLine(line)
		if !ok {
			continue
		}
		if base, ok := strings.CutSuffix(name, "^{}"); ok {
			peeled[base] = sha
		} else {
			m[name] = sha
		}
	}
	for name, sha := range peeled {
		if _, ok := m[name]; ok {
			m[name] = sha
		}
	}
	return m
}
