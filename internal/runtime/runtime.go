// Package runtime defines the container-runtime abstraction ki-agent manages
// containers through. It is runtime-neutral: concrete engines live in
// sub-packages (see internal/runtime/docker).
package runtime

import (
	"context"
	"errors"
	"time"
)

// Labels ki puts on every container it creates. Only containers carrying
// LabelManaged=true are visible to a Runtime.
const (
	// LabelProject is the slug of the owning project.
	LabelProject = "ki.project"
	// LabelService is the compose service name (informational).
	LabelService = "ki.service"
	// LabelManaged is "true" on everything ki created.
	LabelManaged = "ki.managed"
)

var (
	// ErrNotFound is returned when the container does not exist.
	ErrNotFound = errors.New("container not found")
	// ErrNotManaged is returned when the container exists but is not managed
	// by ki (it lacks the LabelManaged=true label).
	ErrNotManaged = errors.New("container not managed")
	// ErrUnavailable is returned when the engine cannot be reached.
	ErrUnavailable = errors.New("container runtime unavailable")
)

// Port is a published or exposed container port.
type Port struct {
	PrivatePort uint16 `json:"private_port"`
	PublicPort  uint16 `json:"public_port,omitempty"`
	Type        string `json:"type"`
	IP          string `json:"ip,omitempty"`
}

// Container is the summary of a managed container.
type Container struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	State   string            `json:"state"`
	Status  string            `json:"status"`
	Project string            `json:"project,omitempty"`
	Service string            `json:"service,omitempty"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels,omitempty"`
	Ports   []Port            `json:"ports,omitempty"`
}

// Mount is a filesystem mount of a container.
type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only"`
}

// ContainerDetail is the full description of a managed container.
type ContainerDetail struct {
	Container
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	ExitCode     int       `json:"exit_code"`
	RestartCount int       `json:"restart_count"`
	// Health is "", "starting", "healthy" or "unhealthy".
	Health   string   `json:"health,omitempty"`
	Mounts   []Mount  `json:"mounts,omitempty"`
	Networks []string `json:"networks,omitempty"`
}

// LogOptions selects which log lines Logs delivers.
type LogOptions struct {
	// Follow keeps streaming until the container exits or ctx is cancelled.
	Follow bool
	// Tail limits output to the last N lines; -1 means all.
	Tail int
	// Since limits output to lines after this time; zero means no limit.
	Since time.Time
	// Timestamps asks the runtime for per-line timestamps (LogLine.Time).
	Timestamps bool
}

// LogLine is one line of container output.
type LogLine struct {
	// Stream is "stdout" or "stderr".
	Stream string    `json:"stream"`
	Time   time.Time `json:"time"`
	Text   string    `json:"text"`
}

// Info describes the runtime engine.
type Info struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}

// PruneReport counts the resources PruneProject removed.
type PruneReport struct {
	Containers int `json:"containers"`
	Networks   int `json:"networks"`
	Volumes    int `json:"volumes"`
}

// Runtime manages ki-managed containers. Every operation on an id first
// confirms the container carries LabelManaged=true and otherwise fails with
// ErrNotManaged; unmanaged containers are invisible and untouchable.
type Runtime interface {
	// Ping checks the engine is reachable and describes it.
	Ping(ctx context.Context) (Info, error)
	// List returns managed containers, of one project or (project == "") all.
	List(ctx context.Context, project string) ([]Container, error)
	Inspect(ctx context.Context, id string) (ContainerDetail, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string, timeout time.Duration) error
	Restart(ctx context.Context, id string, timeout time.Duration) error
	Remove(ctx context.Context, id string, force bool) error
	// Logs calls fn for every log line; it returns when the stream ends, fn
	// returns an error, or ctx is cancelled.
	Logs(ctx context.Context, id string, opts LogOptions, fn func(LogLine) error) error
	// PruneProject force-removes every container, then network, then (when
	// volumes is set) volume carrying both LabelManaged=true and
	// LabelProject=project. An empty project is rejected without touching the
	// engine. Resources that vanish mid-prune are ignored.
	PruneProject(ctx context.Context, project string, volumes bool) (PruneReport, error)
}
