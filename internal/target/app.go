package target

import (
	"context"
)

// AppInfo is implemented by targets that can report what is on screen right
// now. Synthetic cannot, so this is deliberately not part of Target.
type AppInfo interface {
	ForegroundApp(ctx context.Context) (*App, error)
}

// App describes the foreground activity. Label and an icon are left out of
// v1 -- the spec records the icon as a follow-up and the client shows a
// letter tile instead. VersionName, VersionCode, PID and APKPath are filled
// on a best-effort basis: only the resumed activity is required to build an
// App at all.
type App struct {
	Package     string `json:"package"`
	Activity    string `json:"activity"`
	VersionName string `json:"versionName,omitempty"`
	VersionCode int    `json:"versionCode,omitempty"`
	PID         int    `json:"pid,omitempty"`
	APKPath     string `json:"apkPath,omitempty"`
}
