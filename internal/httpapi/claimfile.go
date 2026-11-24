package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// claimFile is one claims file and what this server last read of it. The
// machine-wide one is read by every loupe server on the machine, so a server
// reads it again whenever another has changed it, and every change to it is
// made with the lock file beside it held: lock, read, change, write, unlock.
// The lock is a file of its own because a write renames a new file over the
// claims file, and a lock on the old one would end with it.
type claimFile struct {
	path string
	// stamp is the file as this server last read or wrote it, so a read that
	// finds it unchanged costs an open and a stat. Nil when it was missing.
	stamp os.FileInfo
	// failed is the last error reading or locking the file, so one that
	// lasts is traced once and not on every call.
	failed string
}

// claimData is a claims file's contents.
type claimData struct {
	Claims []savedClaim `json:"claims"`
	// Lapsed is who held a target whose claim ran out, so the holder hears
	// it lapsed whichever server it calls next.
	Lapsed []savedLapse `json:"lapsed,omitempty"`
}

// savedClaim is a claim as the file holds it.
type savedClaim struct {
	Target  string    `json:"target"`
	Agent   string    `json:"agent,omitempty"`
	Session string    `json:"session,omitempty"`
	Link    string    `json:"link,omitempty"`
	Since   time.Time `json:"since"`
	Seen    time.Time `json:"seen"`
	// HoldUntil is set while a request of the holder's keeps the claim alive
	// on the server running it, such as a boot waiting on ?wait=. It is that
	// request's latest end, so a server that dies mid-boot holds the device
	// no longer than that.
	HoldUntil time.Time `json:"holdUntil,omitzero"`
}

// savedLapse is a lapse marker as the file holds it.
type savedLapse struct {
	Target  string    `json:"target"`
	Agent   string    `json:"agent,omitempty"`
	Session string    `json:"session,omitempty"`
	At      time.Time `json:"at"`
}

// read returns the file's contents and changed true when it differs from
// what this server last read or wrote, and changed false otherwise. A missing
// file is empty. A file that does not parse is reported once and then read as
// unchanged, so the claims from it stay as they were until a change writes a
// good one over it.
func (f *claimFile) read() (d claimData, changed bool, err error) {
	fh, err := os.Open(f.path)
	if os.IsNotExist(err) {
		changed = f.stamp != nil
		f.stamp = nil
		return claimData{}, changed, nil
	}
	if err != nil {
		return claimData{}, false, err
	}
	defer fh.Close()
	// The stamp comes from the open file, not the path, so it is the stamp
	// of what is read even when another server renames a new file in.
	fi, err := fh.Stat()
	if err != nil {
		return claimData{}, false, err
	}
	if f.stamp != nil && os.SameFile(f.stamp, fi) && f.stamp.ModTime().Equal(fi.ModTime()) && f.stamp.Size() == fi.Size() {
		return claimData{}, false, nil
	}
	f.stamp = fi
	raw, err := io.ReadAll(fh)
	if err != nil {
		return claimData{}, false, err
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return claimData{}, false, fmt.Errorf("%s: %w", f.path, err)
	}
	return d, true, nil
}

// write replaces the file with d. Called with the lock held, so the stamp it
// records is of this write and nobody else's.
func (f *claimFile) write(d claimData) error {
	if err := writeJSONFile(f.path, d); err != nil {
		return err
	}
	fi, err := os.Stat(f.path)
	if err != nil {
		return err
	}
	f.stamp = fi
	return nil
}

// claimLockWait bounds how long a change waits for another server to let go
// of the lock. A server holds it for a read and a write of a small file, so
// running out means that server is stopped mid-change, and the change goes
// ahead without the lock rather than stall every agent call on the machine.
const claimLockWait = 5 * time.Second

// lock takes the lock beside the file and returns the func that lets go of
// it. The system lets go of it too when the process holding it exits.
func (f *claimFile) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return nil, err
	}
	fh, err := os.OpenFile(f.path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(claimLockWait)
	for {
		ok, err := tryLockFile(fh)
		if err != nil {
			fh.Close()
			return nil, fmt.Errorf("lock %s: %w", fh.Name(), err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			fh.Close()
			return nil, fmt.Errorf("lock %s: still held after %s", fh.Name(), claimLockWait)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return func() {
		_ = unlockFile(fh)
		_ = fh.Close()
	}, nil
}
