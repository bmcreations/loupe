package t3

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeT3 writes a t3 that records its arguments and stdin into dir and
// prints out.
func fakeT3(t *testing.T, out string) (CLI, string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + dir + "/args\ncat > " + dir + "/stdin\ncat <<'EOF'\n" + out + "\nEOF\n"
	p := filepath.Join(dir, "t3")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return CLI{Path: p}, dir
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestThreadsParsesTheList(t *testing.T) {
	c, dir := fakeT3(t, `[{"threadId":"469bab44-1f43-4536-b0f6-3349d0c7092f","title":"Fix login","status":"idle","providerInstanceId":"codex","model":"gpt","settled":false,"updatedAt":"2026-10-29T01:49:16.135Z"}]`)
	ts, err := c.Threads(context.Background(), "/p")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].Title != "Fix login" || ts[0].Provider != "codex" || ts[0].UpdatedAt.IsZero() {
		t.Fatalf("threads = %+v", ts)
	}
	if got := read(t, dir+"/args"); got != "thread\nlist\n--json\n--project\n/p\n" {
		t.Fatalf("args = %q", got)
	}
}

func TestSendPutsTheMessageOnStdin(t *testing.T) {
	c, dir := fakeT3(t, "")
	msg := "it's off; $(rm -rf /) `x`"
	if err := c.Send(context.Background(), "mcp:9cfd27bc-1c9e-46f4-be88-3144e57d7707", msg); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir+"/args"); got != "thread\nsend\nmcp:9cfd27bc-1c9e-46f4-be88-3144e57d7707\n-\n" {
		t.Fatalf("args = %q", got)
	}
	if got := read(t, dir+"/stdin"); got != msg {
		t.Fatalf("stdin = %q", got)
	}
}

func TestSendRefusesWhatIsNotAThreadID(t *testing.T) {
	c, _ := fakeT3(t, "")
	if err := c.Send(context.Background(), "--wait", "x"); err == nil {
		t.Fatal("sent to a flag")
	}
}

func TestStartReturnsTheNewID(t *testing.T) {
	c, dir := fakeT3(t, `{"threadId":"d5b40970-48a1-4086-9552-eb534e09d3de"}`)
	id, err := c.Start(context.Background(), "/p", "Loupe flag", "look")
	if err != nil || id != "d5b40970-48a1-4086-9552-eb534e09d3de" {
		t.Fatalf("id = %q, %v", id, err)
	}
	if got := read(t, dir+"/args"); got != "thread\nstart\n--json\n--title\nLoupe flag\n/p\n-\n" {
		t.Fatalf("args = %q", got)
	}
}

func TestStartedIDTakesABareID(t *testing.T) {
	id, err := startedID([]byte("d5b40970-48a1-4086-9552-eb534e09d3de\n"))
	if err != nil || id != "d5b40970-48a1-4086-9552-eb534e09d3de" {
		t.Fatalf("id = %q, %v", id, err)
	}
	if _, err := startedID([]byte("Started.")); err == nil || !strings.Contains(err.Error(), "no thread id") {
		t.Fatalf("err = %v", err)
	}
}

func TestAFailureCarriesStderr(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t3")
	os.WriteFile(p, []byte("#!/bin/sh\necho 'No T3 Code project matches' >&2\nexit 1\n"), 0o755)
	_, err := CLI{Path: p}.Threads(context.Background(), "/nope")
	if err == nil || !strings.Contains(err.Error(), "No T3 Code project matches") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidThread(t *testing.T) {
	const u = "469bab44-1f43-4536-b0f6-3349d0c7092f"
	for _, id := range []string{u, "mcp:" + u, "import:claudeAgent:" + u} {
		if !ValidThread(id) {
			t.Errorf("%s refused", id)
		}
	}
	for _, id := range []string{"", "x", "a:b:c:" + u, "-x:" + u, u + " ", "mcp:" + u + ";rm"} {
		if ValidThread(id) {
			t.Errorf("%q accepted", id)
		}
	}
}
