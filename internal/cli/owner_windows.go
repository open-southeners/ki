//go:build windows

package cli

// dataOwner reports an unknown owner: Windows has no numeric uid/gid.
func dataOwner(string) (uid, gid int) { return -1, -1 }
