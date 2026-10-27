package record

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"
)

func writeFinished(t *testing.T, dir, id string, started time.Time, mp4Size int) {
	t.Helper()
	ended := started.Add(time.Minute)
	m := Meta{ID: id, Target: "t", Started: started, Ended: &ended, Reason: "stopped", BitrateChanges: []BitrateChange{}}
	b, err := json.Marshal(m)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, id+".json"), b, 0o644))
	must(t, os.WriteFile(filepath.Join(dir, id+".mp4"), make([]byte, mp4Size), 0o644))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestStartupDeletesUnclosedAndExpiredRecordings(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "aaaaaaaaaaaa.mp4"), []byte("no moov"), 0o644))
	writeFinished(t, dir, "bbbbbbbbbbbb", time.Now().Add(-8*24*time.Hour-time.Minute), 10)
	writeFinished(t, dir, "cccccccccccc", time.Now().Add(-time.Hour), 10)
	must(t, os.WriteFile(filepath.Join(dir, ".dddddddddddd.json.tmp"), []byte("{"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o644))

	s, err := NewStore(dir)
	must(t, err)
	for _, name := range []string{"aaaaaaaaaaaa.mp4", "bbbbbbbbbbbb.mp4", "bbbbbbbbbbbb.json", ".dddddddddddd.json.tmp"} {
		if exists(filepath.Join(dir, name)) {
			t.Errorf("%s kept", name)
		}
	}
	for _, name := range []string{"cccccccccccc.mp4", "cccccccccccc.json", "notes.txt"} {
		if !exists(filepath.Join(dir, name)) {
			t.Errorf("%s deleted", name)
		}
	}
	if l := s.List(); len(l) != 1 || l[0].ID != "cccccccccccc" {
		t.Fatalf("list %+v", l)
	}
}

func TestStartupCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "recordings")
	if _, err := NewStore(dir); err != nil {
		t.Fatal(err)
	}
	if !exists(dir) {
		t.Fatal("dir not created")
	}
}

func TestCapDeletesTheOldestFinishedRecordings(t *testing.T) {
	s := newStore(t)
	s.cap = 100
	now := time.Now()
	writeFinished(t, s.dir, "000000000001", now.Add(-3*time.Hour), 60)
	writeFinished(t, s.dir, "000000000002", now.Add(-2*time.Hour), 60)
	writeFinished(t, s.dir, "000000000003", now.Add(-1*time.Hour), 60)
	s.enforce()
	for id, want := range map[string]bool{"000000000001": false, "000000000002": false, "000000000003": true} {
		if got := exists(filepath.Join(s.dir, id+".mp4")); got != want {
			t.Errorf("%s exists %v, want %v", id, got, want)
		}
		if got := exists(filepath.Join(s.dir, id+".json")); got != want {
			t.Errorf("%s.json exists %v, want %v", id, got, want)
		}
	}

	// An in-progress recording past the cap on its own pushes out the last
	// finished one and is never touched itself. The recorder takes the cap
	// as its own limit at Start, so it starts under a large one and the
	// small cap returns for the check (TestCapEndsARecordingAsLimit covers
	// a recording reaching it).
	s.cap = 1 << 30
	src := newFakeSource()
	m, _ := start(t, s, src, "t1")
	s.cap = 100
	src.send(cfgFrame(sps320x240))
	src.send(keyFrame(0))
	for i := 1; i <= 100; i++ {
		src.send(interFrame(time.Duration(i) * 16 * time.Millisecond))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if a, _ := s.ActiveFor("t1"); a.Bytes >= 500 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("active recording never reached 500 bytes")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.enforce()
	if exists(filepath.Join(s.dir, "000000000003.mp4")) {
		t.Error("finished recording kept past the cap")
	}
	if !exists(filepath.Join(s.dir, m.ID+".mp4")) {
		t.Error("in-progress recording deleted")
	}
}

func TestOneRecordingPerTarget(t *testing.T) {
	s := newStore(t)
	start(t, s, newFakeSource(), "t1")
	if _, err := s.Start(newFakeSource(), "t1", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Start = %v, want ErrBusy", err)
	}
	start(t, s, newFakeSource(), "t2")
	if m, ok := s.ActiveFor("t1"); !ok || !m.InProgress || m.Target != "t1" {
		t.Fatalf("ActiveFor = %+v, %v", m, ok)
	}
}

func TestStopTwiceReturnsTheSameMeta(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	m, _ := start(t, s, src, "t1")
	src.send(cfgFrame(sps320x240))
	src.send(keyFrame(0))
	first, err := s.Stop(m.ID)
	must(t, err)
	second, err := s.Stop(m.ID)
	must(t, err)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("first %+v\nsecond %+v", first, second)
	}
	got, err := s.Get(m.ID)
	must(t, err)
	if !reflect.DeepEqual(first, got) {
		t.Fatalf("Get %+v", got)
	}
	// A new recording on the same target can start once the first ended.
	start(t, s, newFakeSource(), "t1")
}

func TestPathAndDeleteRefuseAnInProgressRecording(t *testing.T) {
	s := newStore(t)
	m, _ := start(t, s, newFakeSource(), "t1")
	if _, err := s.Path(m.ID); !errors.Is(err, ErrInProgress) {
		t.Fatalf("Path = %v, want ErrInProgress", err)
	}
	if err := s.Delete(m.ID); !errors.Is(err, ErrInProgress) {
		t.Fatalf("Delete = %v, want ErrInProgress", err)
	}
	_, err := s.Stop(m.ID)
	must(t, err)
	path, err := s.Path(m.ID)
	must(t, err)
	if path != filepath.Join(s.dir, m.ID+".mp4") {
		t.Fatalf("path %s", path)
	}
	must(t, s.Delete(m.ID))
	if exists(path) || exists(filepath.Join(s.dir, m.ID+".json")) {
		t.Fatal("files kept after Delete")
	}
	if _, err := s.Get(m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestWaitReturnsOnceRecordersClose(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	start(t, s, src, "t1")
	if s.Wait(20 * time.Millisecond) {
		t.Fatal("Wait returned true with a recorder running")
	}
	close(src.done)
	if !s.Wait(5 * time.Second) {
		t.Fatal("Wait timed out after the session ended")
	}
}

func TestIDsAreTwelveHex(t *testing.T) {
	s := newStore(t)
	re := regexp.MustCompile(`^[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		m, err := s.Start(newFakeSource(), "t1", nil)
		must(t, err)
		if !re.MatchString(m.ID) || seen[m.ID] {
			t.Fatalf("id %q", m.ID)
		}
		seen[m.ID] = true
		_, err = s.Stop(m.ID)
		must(t, err)
	}
	for _, bad := range []string{"../../etc/passwd", "ABCDEFABCDEF", "abc", ""} {
		if _, err := s.Get(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", bad, err)
		}
		if _, err := s.Path(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Path(%q) = %v, want ErrNotFound", bad, err)
		}
		if err := s.Delete(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%q) = %v, want ErrNotFound", bad, err)
		}
		if _, err := s.Stop(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stop(%q) = %v, want ErrNotFound", bad, err)
		}
	}
}

func TestListIsNewestFirstWithTheActiveOne(t *testing.T) {
	s := newStore(t)
	writeFinished(t, s.dir, "000000000001", time.Now().Add(-2*time.Hour), 10)
	writeFinished(t, s.dir, "000000000002", time.Now().Add(-1*time.Hour), 10)
	m, _ := start(t, s, newFakeSource(), "t1")
	l := s.List()
	if len(l) != 3 || l[0].ID != m.ID || !l[0].InProgress || l[1].ID != "000000000002" || l[2].ID != "000000000001" {
		t.Fatalf("list %+v", l)
	}
}

func TestCreateFailureLeavesNothingBehind(t *testing.T) {
	s := newStore(t)
	s.create = func(string) (sampleWriter, error) { return nil, errors.New("no space") }
	src := newFakeSource()
	if _, err := s.Start(src, "t1", nil); err == nil {
		t.Fatal("Start succeeded")
	}
	if src.tap != nil {
		t.Fatal("tap added for a recording that never started")
	}
	if _, ok := s.ActiveFor("t1"); ok {
		t.Fatal("target left busy")
	}
}

func TestStartAfterWaitIsRefused(t *testing.T) {
	s := newStore(t)
	if !s.Wait(time.Second) {
		t.Fatal("Wait timed out with nothing running")
	}
	if _, err := s.Start(newFakeSource(), "t1", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Wait: %v, want ErrClosed", err)
	}
}
