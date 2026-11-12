package httpapi

import (
	"sync"
	"time"
)

// lifeViewerWindow is how long after a person's shutdown or reset their
// viewer id still speaks for them on that target.
const lifeViewerWindow = 5 * time.Minute

// lifeViewers remembers the viewer id behind the last shutdown or reset of
// each target. A restart from a viewer is a shutdown then a boot, and the
// shutdown ends the stream the viewer was attached to, so by the boot its id
// is attached nowhere and would read as an agent that an agent's claim
// refuses, leaving the device off. Only the id that did the shutdown counts,
// and only on that target, for lifeViewerWindow.
type lifeViewers struct {
	mu  sync.Mutex
	ids map[string]lifeViewer
	now func() time.Time
}

type lifeViewer struct {
	id string
	at time.Time
}

func (l *lifeViewers) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// note records that viewer id shut down or reset target.
func (l *lifeViewers) note(target, id string) {
	if id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ids == nil {
		l.ids = map[string]lifeViewer{}
	}
	l.ids[target] = lifeViewer{id: id, at: l.clock()}
}

// recent reports whether viewer id shut down or reset target within the window.
func (l *lifeViewers) recent(target, id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.ids[target]
	return ok && v.id == id && l.clock().Sub(v.at) < lifeViewerWindow
}
