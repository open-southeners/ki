//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// dataOwner returns the owner of dir, or -1, -1 when it cannot be told.
func dataOwner(dir string) (uid, gid int) {
	st, err := os.Stat(dir)
	if err != nil {
		return -1, -1
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid), int(sys.Gid)
	}
	return -1, -1
}
