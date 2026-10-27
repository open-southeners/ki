//go:build windows

package git

import "os/exec"

// isolate is a no-op on Windows: the default cancellation kills the process.
func isolate(cmd *exec.Cmd) {}
