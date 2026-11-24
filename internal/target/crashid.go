package target

import (
	"regexp"
)

// DropboxIDRE is an Android crash id: the entry's tag and the epoch millis
// its file is named by, so a fetch knows which tag to read.
var DropboxIDRE = regexp.MustCompile(`^(data_app|system_app|system_server)_(crash|native_crash|anr)-\d{10,13}$`)
