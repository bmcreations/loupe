//go:build windows

package dest

import "os"

// ownedByOther does not check ownership on Windows: there is no uid.
func ownedByOther(fi os.FileInfo) (uid uint32, other bool) { return 0, false }
