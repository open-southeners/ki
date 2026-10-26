package deploys

import (
	"encoding/json"
	"time"

	"github.com/open-southeners/ki-agent/internal/policy"
	"github.com/open-southeners/ki-agent/internal/store"
)

// Deploy statuses.
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusSuperseded  = "superseded"
	StatusInterrupted = "interrupted"
)

// Pipeline stages.
const (
	StageFetch  = "fetch"
	StageConfig = "config"
	StagePolicy = "policy"
	StagePull   = "pull"
	StageBuild  = "build"
	StageUp     = "up"
	StageHealth = "health"
)

// Failure reasons recorded on a failed deploy (Deploy.Error.Reason).
const (
	ReasonFetchFailed        = "fetch_failed"
	ReasonComposeNotFound    = "compose_not_found"
	ReasonComposeInvalid     = "compose_invalid"
	ReasonPolicyViolation    = "policy_violation"
	ReasonDomainConflict     = "domain_conflict"
	ReasonPullFailed         = "pull_failed"
	ReasonBuildFailed        = "build_failed"
	ReasonUpFailed           = "up_failed"
	ReasonHealthFailed       = "health_failed"
	ReasonRuntimeUnavailable = "runtime_unavailable"
	ReasonInternal           = "internal"
)

// Reasons of a synchronous trigger failure (TriggerError.Reason).
const (
	ReasonProjectNotReady = "project_not_ready"
	ReasonRefNotAllowed   = "ref_not_allowed"
	ReasonRefNotFound     = "ref_not_found"
	// ReasonNoRollbackTarget: no deploy to roll back to (none succeeded before
	// the current one, or the given deploy is not a deployable target).
	ReasonNoRollbackTarget = "no_rollback_target"
)

// Deploy sources.
const (
	SourceWebhook = "webhook"
	SourceAPI     = "api"
	SourceCLI     = "cli"
)

// Deploy is the wire form of a deploy.
type Deploy struct {
	ID      int64  `json:"id"`
	Project string `json:"project"`
	Ref     string `json:"ref"`
	Commit  string `json:"commit"`
	Source  string `json:"source"`
	Status  string `json:"status"`
	Stage   string `json:"stage"`
	// RollbackOf is the deploy this one re-deploys (omitted for ordinary deploys).
	RollbackOf int64        `json:"rollback_of,omitempty"`
	Error      *DeployError `json:"error,omitempty"`
	CreatedAt  string       `json:"created_at"`
	StartedAt  *string      `json:"started_at"`
	FinishedAt *string      `json:"finished_at"`
}

// DeployError is the failure recorded on a failed deploy.
type DeployError struct {
	Reason     string             `json:"reason"`
	Detail     string             `json:"detail"`
	Violations []policy.Violation `json:"violations,omitempty"`
}

// View converts a stored deploy to its wire form.
func View(d *store.Deploy) Deploy {
	v := Deploy{
		ID: d.ID, Project: d.Project, Ref: d.Ref, Commit: d.Commit, Source: d.Source,
		Status: d.Status, Stage: d.Stage, RollbackOf: d.RollbackOf,
		CreatedAt:  d.CreatedAt.UTC().Format(time.RFC3339),
		StartedAt:  stamp(d.StartedAt),
		FinishedAt: stamp(d.FinishedAt),
	}
	if d.Status == StatusFailed {
		e := &DeployError{Reason: d.ErrorReason, Detail: d.ErrorDetail}
		if d.ErrorData != "" {
			var data struct {
				Violations []policy.Violation `json:"violations"`
			}
			if json.Unmarshal([]byte(d.ErrorData), &data) == nil {
				e.Violations = data.Violations
			}
		}
		v.Error = e
	}
	return v
}

func stamp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// Update is the payload of a deploys.update notification: a log line, or the
// final status.
type Update struct {
	Type string `json:"type"`
	// Seq is the stored line number of a log update (what since_seq refers
	// to); omitted for lines past the log cap, which are only streamed.
	Seq    int64   `json:"seq,omitempty"`
	Stream string  `json:"stream,omitempty"`
	Text   string  `json:"text,omitempty"`
	Deploy *Deploy `json:"deploy,omitempty"`
}
