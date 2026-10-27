//go:build !windows

package cli

import "syscall"

func syscallUmask(m int) int { return syscall.Umask(m) }
