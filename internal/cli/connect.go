package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/open-southeners/ki-agent/internal/client"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/rpc"
)

// resolveSocket picks the agent socket: the --socket flag, then
// KI_SOCKET_PATH, then socket.path from the config file when it is readable
// (read and parse errors are ignored), then the built-in default.
func resolveSocket(f *flags) string {
	if f.socket != "" {
		return f.socket
	}
	if v := os.Getenv("KI_SOCKET_PATH"); v != "" {
		return v
	}
	if p := socketFromConfig(f.configPath); p != "" {
		return p
	}
	return config.Default().Socket.Path
}

// socketFromConfig returns socket.path from the YAML file at path, or "" if
// the file cannot be read or has none.
func socketFromConfig(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		Socket struct {
			Path string `yaml:"path"`
		} `yaml:"socket"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return ""
	}
	return doc.Socket.Path
}

// maxSocketPath is the longest Unix socket path the kernel accepts
// (sizeof(sun_path) minus the NUL terminator).
const maxSocketPath = 107

// connect dials the agent for a command, translating connection failures into
// actionable messages.
func connect(cmd *cobra.Command, f *flags) (*client.Client, error) {
	path := resolveSocket(f)
	c, err := client.Dial(cmd.Context(), path)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrPermission):
			return nil, fmt.Errorf("permission denied on %s: add your user to the ki-agent group or use sudo", path)
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
			return nil, fmt.Errorf("cannot reach the agent at %s: is ki-agent running? (use --socket to point elsewhere)", path)
		case errors.Is(err, syscall.EINVAL) && len(path) > maxSocketPath:
			return nil, fmt.Errorf("socket path is too long (%d bytes, max %d): %s", len(path), maxSocketPath, path)
		}
		return nil, explainError(err)
	}
	return c, nil
}

// callOnce connects, performs one call and disconnects. result may be nil.
func callOnce(cmd *cobra.Command, f *flags, method string, params, result any) error {
	c, err := connect(cmd, f)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Call(cmd.Context(), method, params, result); err != nil {
		return explainError(err)
	}
	return nil
}

// repositoryHints maps a RepositoryError reason to a one-line suggestion.
var repositoryHints = map[string]string{
	"invalid_url":       "use https://host/org/repo.git or git@host:org/repo.git",
	"unreachable":       "the agent host cannot reach the Git server; check DNS and network access",
	"auth_failed":       "for https pass a token with --token-stdin or --token-file; for ssh add the deploy key, then run `ki-agent projects verify`",
	"not_found":         "check the URL and that the credentials can read the repository",
	"ref_not_found":     "the branch or tag does not exist; check --branch or --tags",
	"compose_not_found": "pass the Compose file path with --compose PATH",
}

// enrollmentHints maps an EnrollmentError reason to a one-line suggestion.
var enrollmentHints = map[string]string{
	"unreachable":      "the agent host cannot reach the enrollment server; check the URL, DNS and network access",
	"invalid_response": "the server does not speak the ki-agent enrollment protocol; check the URL",
	"denied":           "the request was denied on the control plane; run `ki-agent enroll` again to retry",
	"expired":          "the code was not approved in time; run `ki-agent enroll` again",
}

// explainError turns JSON-RPC errors into user-facing messages.
func explainError(err error) error {
	var re *rpc.Error
	if !errors.As(err, &re) {
		if errors.Is(err, rpc.ErrClosed) {
			return errors.New("connection to the agent was closed")
		}
		return err
	}
	switch re.Code {
	case rpc.CodeMethodNotFound:
		return fmt.Errorf("%s (does the agent version support this command?)", re.Message)
	case rpc.CodeEnrollmentError:
		var d struct{ Reason, Detail string }
		_ = json.Unmarshal(re.Data, &d)
		msg := re.Message
		if h := enrollmentHints[d.Reason]; h != "" {
			msg += "\nhint: " + h
		}
		return errors.New(msg)
	case rpc.CodeRepositoryError:
		var d struct{ Reason, Detail string }
		_ = json.Unmarshal(re.Data, &d)
		msg := re.Message
		if d.Detail != "" && !strings.Contains(msg, d.Detail) {
			msg += ": " + d.Detail
		}
		if h := repositoryHints[d.Reason]; h != "" {
			msg += "\nhint: " + h
		}
		return errors.New(msg)
	case rpc.CodeDeployError:
		var d struct{ Reason, Detail string }
		_ = json.Unmarshal(re.Data, &d)
		msg := re.Message
		if d.Detail != "" && !strings.Contains(msg, d.Detail) {
			msg += ": " + d.Detail
		}
		if h := deployHints[d.Reason]; h != "" {
			msg += "\nhint: " + h
		}
		return errors.New(msg)
	}
	return errors.New(re.Message)
}

// deployHints maps a DeployError reason to a one-line suggestion.
var deployHints = map[string]string{
	"project_not_ready": "grant the agent access to the repository, then run `ki-agent projects verify <slug>`",
	"ref_not_allowed":   "only the project's trigger branch or matching tags can be deployed; see `ki-agent projects get <slug>`",
	"ref_not_found":     "the branch, tag or commit does not exist on the remote",
}

// addJSONFlag registers --json on cmd.
func addJSONFlag(cmd *cobra.Command, dst *bool) {
	cmd.Flags().BoolVar(dst, "json", false, "print the raw result as JSON")
}

// printJSON pretty-prints raw JSON followed by a newline.
func printJSON(w io.Writer, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}
