package deploys

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/open-southeners/ki-agent/internal/git"
	"github.com/open-southeners/ki-agent/internal/store"
)

// TriggerError is a synchronous trigger failure (wire code -32012).
type TriggerError struct {
	// Reason is project_not_ready, ref_not_allowed, ref_not_found or
	// no_rollback_target.
	Reason, Detail string
}

// Error implements error.
func (e *TriggerError) Error() string {
	if e.Detail == "" {
		return "deploy: " + e.Reason
	}
	return "deploy: " + e.Reason + ": " + e.Detail
}

// target is what a deploy will check out.
type target struct {
	// ref is the normalized requested ref recorded on the deploy.
	ref string
	// fetchRef is fetched from the remote; pin, when set, is the commit that
	// must be reachable from it and gets checked out.
	fetchRef, pin string
}

const (
	headsPrefix = "refs/heads/"
	tagsPrefix  = "refs/tags/"
)

func notAllowed(format string, args ...any) error {
	return &TriggerError{Reason: ReasonRefNotAllowed, Detail: fmt.Sprintf(format, args...)}
}

// resolve validates the requested ref against the project's trigger and
// resolves it. Remote failures that are not a missing ref are returned as the
// *git.Error they are.
func (m *Manager) resolve(ctx context.Context, p *store.Project, ref, commit string) (target, error) {
	auth, err := m.opts.Projects.Auth(p)
	if err != nil {
		return target{}, err
	}
	ref = strings.TrimSpace(ref)
	commit = strings.ToLower(strings.TrimSpace(commit))
	if commit != "" && !isSHA(commit) {
		return target{}, &TriggerError{Reason: ReasonRefNotFound, Detail: "invalid commit sha"}
	}
	if p.Branch != "" {
		return m.resolveBranch(ctx, p, auth, ref, commit)
	}
	return m.resolveTag(ctx, p, auth, ref, commit)
}

func (m *Manager) resolveBranch(ctx context.Context, p *store.Project, auth git.Auth, ref, commit string) (target, error) {
	want := headsPrefix + p.Branch
	switch {
	case ref == "" || ref == p.Branch || ref == want:
		// The trigger branch itself; existence is verified by the fetch.
		if _, err := m.opts.Git.ResolveRef(ctx, p.RepoURL, auth, want); err != nil {
			return target{}, refErr(err)
		}
		return target{ref: want, fetchRef: want, pin: commit}, nil
	case isSHA(ref):
		sha := strings.ToLower(ref)
		if commit != "" && commit != sha {
			return target{}, notAllowed("ref and commit disagree")
		}
		// Reachability from the branch is enforced by the checkout.
		return target{ref: sha, fetchRef: want, pin: sha}, nil
	case strings.HasPrefix(ref, "refs/"):
		return target{}, notAllowed("only branch %s can be deployed", p.Branch)
	}
	// A bare name that is not the trigger branch: tell a missing ref from a
	// forbidden one.
	if _, err := m.opts.Git.ResolveRef(ctx, p.RepoURL, auth, ref); err != nil {
		return target{}, refErr(err)
	}
	return target{}, notAllowed("only branch %s can be deployed", p.Branch)
}

func (m *Manager) resolveTag(ctx context.Context, p *store.Project, auth git.Auth, ref, commit string) (target, error) {
	matches := func(name string) bool {
		ok, err := path.Match(p.TagPattern, strings.TrimPrefix(name, tagsPrefix))
		return err == nil && ok
	}
	byName := func(full string) (target, error) {
		if !matches(full) {
			return target{}, notAllowed("tag does not match %q", p.TagPattern)
		}
		r, err := m.opts.Git.ResolveRef(ctx, p.RepoURL, auth, full)
		if err != nil {
			return target{}, refErr(err)
		}
		// The tag is authoritative: a supplied commit that is not the tag's
		// (an annotated tag object, a moved tag) is replaced by what it
		// points at now.
		return target{ref: full, fetchRef: full, pin: r.Commit}, nil
	}
	switch {
	case ref == "":
		tags, err := m.opts.Git.ListTags(ctx, p.RepoURL, auth)
		if err != nil {
			return target{}, refErr(err)
		}
		for _, t := range tags {
			if matches(t.Name) {
				return target{ref: t.Name, fetchRef: t.Name, pin: t.Commit}, nil
			}
		}
		return target{}, &TriggerError{Reason: ReasonRefNotFound, Detail: "no tag matches " + p.TagPattern}
	case isSHA(ref):
		sha := strings.ToLower(ref)
		tags, err := m.opts.Git.ListTags(ctx, p.RepoURL, auth)
		if err != nil {
			return target{}, refErr(err)
		}
		for _, t := range tags {
			if t.Commit == sha && matches(t.Name) {
				return target{ref: sha, fetchRef: t.Name, pin: sha}, nil
			}
		}
		return target{}, notAllowed("commit is not pointed at by a tag matching %q", p.TagPattern)
	case strings.HasPrefix(ref, tagsPrefix):
		return byName(ref)
	case strings.HasPrefix(ref, "refs/"):
		return target{}, notAllowed("only tags matching %q can be deployed", p.TagPattern)
	}
	if matches(ref) {
		return byName(tagsPrefix + ref)
	}
	if _, err := m.opts.Git.ResolveRef(ctx, p.RepoURL, auth, ref); err != nil {
		return target{}, refErr(err)
	}
	return target{}, notAllowed("only tags matching %q can be deployed", p.TagPattern)
}

// refErr maps a git "ref not found" to the trigger error; everything else is
// returned unchanged.
func refErr(err error) error {
	var ge *git.Error
	if errors.As(err, &ge) && ge.Reason == git.ReasonRefNotFound {
		return &TriggerError{Reason: ReasonRefNotFound, Detail: ge.Detail}
	}
	return err
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
