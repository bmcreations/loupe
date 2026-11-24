//go:build !windows

package dest

import (
	"os"
	"syscall"
)

// ownedByOther reports the owner's uid when fi belongs to a different user.
func ownedByOther(fi os.FileInfo) (uid uint32, other bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return st.Uid, true
	}
	return 0, false
}
