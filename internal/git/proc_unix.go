//go:build !windows

package git

import (
	"os/exec"
	"syscall"
)

// isolate runs the command in its own process group and kills the whole group
// on cancellation so git's helpers (ssh, credential programs) do not linger.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
