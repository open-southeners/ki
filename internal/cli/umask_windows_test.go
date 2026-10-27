//go:build windows

package cli

func syscallUmask(m int) int { return 0 }
