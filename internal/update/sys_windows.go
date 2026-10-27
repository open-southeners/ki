//go:build windows

package update

import "os"

// Windows has no O_NOFOLLOW or O_NONBLOCK; the updater only runs on Linux.
const (
	oNoFollow = 0
	oNonBlock = 0
)

func fileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
