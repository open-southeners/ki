package update

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// statusFile is the checker's cache inside the exchange directory.
const statusFile = "status.json"

// maxStateSize bounds how much of status.json is read, so a corrupt file
// cannot hurt.
const maxStateSize = 1 << 20

// Release is the latest release as shown to users.
type Release struct {
	Tag         string    `json:"tag"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	URL         string    `json:"url"`
	Notes       string    `json:"notes"`
	Prerelease  bool      `json:"prerelease"`
}

// Result is the outcome of an update attempt, as written by the root helper to
// the result file in the exchange directory.
type Result struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"` // succeeded | failed | rolled_back
	From       string    `json:"from"`
	To         string    `json:"to"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Error      string    `json:"error,omitempty"`
}

// UpdateStatus is the result of updates.status and updates.check.
type UpdateStatus struct {
	Current         string     `json:"current"`
	Channel         string     `json:"channel"`
	CheckEnabled    bool       `json:"check_enabled"`
	CheckedAt       *time.Time `json:"checked_at"`
	CheckError      *string    `json:"check_error"`
	Latest          *Release   `json:"latest"`
	UpdateAvailable bool       `json:"update_available"`
	ApplySupported  bool       `json:"apply_supported"`
	State           string     `json:"state"` // idle | requested
	LastResult      *Result    `json:"last_result"`
}

// Values of [UpdateStatus.State].
const (
	StateIdle      = "idle"
	StateRequested = "requested"
)

// cachedState is what the checker persists in status.json.
type cachedState struct {
	Channel    string     `json:"channel"`
	ETag       string     `json:"etag,omitempty"`
	CheckedAt  *time.Time `json:"checked_at"`
	CheckError string     `json:"check_error,omitempty"`
	Latest     *Release   `json:"latest"`
}

// loadState reads <dir>/status.json. A missing or unreadable file yields the
// zero state: the cache is only an optimisation.
func loadState(dir string) cachedState {
	f, err := os.Open(filepath.Join(dir, statusFile))
	if err != nil {
		return cachedState{}
	}
	defer f.Close()
	var st cachedState
	if err := json.NewDecoder(io.LimitReader(f, maxStateSize)).Decode(&st); err != nil {
		return cachedState{}
	}
	return st
}

// saveState writes <dir>/status.json atomically (temp file + rename, 0640).
func saveState(dir string, st cachedState) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create update dir: %w", err)
	}
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode update state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".status-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	name := tmp.Name()
	fail := func(what string, err error) error {
		tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("%s: %w", what, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write update state", err)
	}
	if err := tmp.Chmod(0o640); err != nil {
		return fail("chmod update state", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close update state: %w", err)
	}
	if err := os.Rename(name, filepath.Join(dir, statusFile)); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("replace update state: %w", err)
	}
	return nil
}
