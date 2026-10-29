package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/dest"
	"github.com/bmcreations/loupe/internal/t3"
)

// A flag can go to a T3 Code thread rather than a loupe mcp session: an
// existing thread, or a new one in a configured project. T3 threads may run
// any provider and may never have touched loupe, so the flag goes as a
// message that stands alone: the note, the box, and the paths of the
// screenshots and UI tree written to disk.

// t3ListTTL is how long a listing of T3 threads is served before it is
// refreshed. Each project costs about 0.7s of CLI time, which the flag card
// must not wait on every time it opens.
const t3ListTTL = 10 * time.Second

// t3PerProject caps the threads listed per project, newest first.
const t3PerProject = 10

// t3Timeout bounds one CLI call.
const t3Timeout = 20 * time.Second

// keptFlagDirs is how many flags' files stay on disk, matching the flags the
// server keeps in memory.
const keptFlagDirs = 32

// FlagDelivery is how a flag sent to T3, a command or a webhook fared.
type FlagDelivery struct {
	// State is "sending", "sent" or "failed".
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Thread is the T3 thread it went to, a new one's id included.
	Thread string `json:"thread,omitempty"`
	// Dir holds the flag's screenshots and UI tree as sent.
	Dir string `json:"dir,omitempty"`
	// Output is the end of what a command wrote, or a webhook replied.
	Output string `json:"output,omitempty"`
}

// T3ThreadInfo is one thread a flag can be sent to.
type T3ThreadInfo struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// UpdatedAt is unix milliseconds.
	UpdatedAt int64 `json:"updatedAt"`
}

// T3ProjectInfo is a configured project and its open threads.
type T3ProjectInfo struct {
	Path    string         `json:"path"`
	Name    string         `json:"name"`
	Threads []T3ThreadInfo `json:"threads"`
	Error   string         `json:"error,omitempty"`
}

// FlagDestinationsResponse is GET /api/flags/destinations: where a flag can
// go besides a loupe mcp session.
type FlagDestinationsResponse struct {
	// T3 is absent when no t3Projects are configured.
	T3 *T3Destinations `json:"t3,omitempty"`
	// Commands and Webhooks are destinations.json's, by name only: what
	// they run or where they post never leaves the server.
	Commands []NamedDestination `json:"commands,omitempty"`
	Webhooks []NamedDestination `json:"webhooks,omitempty"`
	// Error says why destinations.json could not be read.
	Error string `json:"error,omitempty"`
}

// NamedDestination is a command or webhook a flag can be sent to.
type NamedDestination struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// T3Destinations lists T3 Code's projects and threads.
type T3Destinations struct {
	// Error says why T3 could not be asked, the CLI missing for one.
	Error    string          `json:"error,omitempty"`
	Projects []T3ProjectInfo `json:"projects"`
}

type t3Cache struct {
	mu         sync.Mutex
	key        string
	at         time.Time
	val        *T3Destinations
	refreshing bool
}

// IsT3 reports whether r is a T3 thread or project rather than a session.
func (r *FlagRecipient) IsT3() bool {
	return r != nil && (r.T3Thread != "" || r.T3Project != "")
}

// IsExternal reports whether the server delivers to r itself, rather than
// a loupe mcp session reading it.
func (r *FlagRecipient) IsExternal() bool {
	return r.IsT3() || r != nil && (r.Command != "" || r.Webhook != "")
}

// External names r for an agent that was not sent the flag.
func (r *FlagRecipient) External() string {
	switch {
	case r.IsT3():
		return "a T3 Code thread"
	case r.Command != "":
		return "a program on their Mac"
	}
	return "a webhook"
}

// SetDestinationsPath names destinations.json, read on each use so an edit
// to it needs no restart.
func (s *Server) SetDestinationsPath(path string) { s.destPath = path }

func (s *Server) destinations() (dest.File, error) {
	if s.destPath == "" {
		return dest.File{}, nil
	}
	return dest.Load(s.destPath)
}

// t3Projects is the configured project list.
func (s *Server) t3Projects() []string {
	if s.cfg == nil {
		return nil
	}
	return s.cfg.Current().T3ProjectList()
}

func (s *Server) t3CLI() (t3.CLI, error) {
	if s.findT3 != nil {
		return s.findT3()
	}
	return t3.Find()
}

// recipient is to checked: a session's names as headers are, a T3 thread by
// its id's shape, a T3 project against the configured list. False when it is
// none of those, or more than one.
func (s *Server) recipient(to *FlagRecipient) (*FlagRecipient, bool) {
	if to == nil {
		return nil, true
	}
	set := 0
	for _, v := range []string{to.T3Thread, to.T3Project, to.Command, to.Webhook, to.Agent + to.Session} {
		if v != "" {
			set++
		}
	}
	if set > 1 {
		return nil, false
	}
	switch {
	case to.Command != "" || to.Webhook != "":
		f, err := s.destinations()
		if err != nil {
			return nil, false
		}
		if _, ok := f.Command(to.Command); to.Command != "" && ok {
			return &FlagRecipient{Command: to.Command}, true
		}
		if _, ok := f.Webhook(to.Webhook); to.Webhook != "" && ok {
			return &FlagRecipient{Webhook: to.Webhook}, true
		}
		return nil, false
	case to.T3Thread != "":
		if !t3.ValidThread(to.T3Thread) {
			return nil, false
		}
		return &FlagRecipient{T3Thread: to.T3Thread}, true
	case to.T3Project != "":
		p := filepath.Clean(to.T3Project)
		for _, q := range s.t3Projects() {
			if q == p {
				return &FlagRecipient{T3Project: p}, true
			}
		}
		return nil, false
	}
	r := FlagRecipient{Agent: agentName(to.Agent), Session: agentName(to.Session)}
	if callerKey(r.Agent, r.Session) == "" {
		return nil, false
	}
	return &r, true
}

func (s *Server) handleFlagDestinations(w http.ResponseWriter, r *http.Request) {
	var resp FlagDestinationsResponse
	if projects := s.t3Projects(); len(projects) > 0 {
		resp.T3 = s.t3Destinations(r.Context(), projects)
	}
	f, err := s.destinations()
	if err != nil {
		resp.Error = err.Error()
	}
	for _, c := range f.Commands {
		resp.Commands = append(resp.Commands, NamedDestination{c.Name, dest.Title(c.Label, c.Name)})
	}
	for _, h := range f.Webhooks {
		resp.Webhooks = append(resp.Webhooks, NamedDestination{h.Name, dest.Title(h.Label, h.Name)})
	}
	writeJSON(w, http.StatusOK, resp)
}

// t3Destinations lists projects' threads, from the cache while it is fresh.
// A stale listing is served as is while a new one is fetched behind it.
func (s *Server) t3Destinations(ctx context.Context, projects []string) *T3Destinations {
	key := strings.Join(projects, "\x00")
	c := &s.t3cache
	c.mu.Lock()
	if c.val != nil && c.key == key {
		v := c.val
		if time.Since(c.at) > t3ListTTL && !c.refreshing {
			c.refreshing = true
			go func() {
				v := s.listT3(context.Background(), projects)
				c.mu.Lock()
				c.val, c.at, c.key, c.refreshing = v, time.Now(), key, false
				c.mu.Unlock()
			}()
		}
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()
	v := s.listT3(ctx, projects)
	c.mu.Lock()
	c.val, c.at, c.key = v, time.Now(), key
	c.mu.Unlock()
	return v
}

func (s *Server) listT3(ctx context.Context, projects []string) *T3Destinations {
	cli, err := s.t3CLI()
	if err != nil {
		return &T3Destinations{Error: err.Error(), Projects: []T3ProjectInfo{}}
	}
	out := make([]T3ProjectInfo, len(projects))
	var wg sync.WaitGroup
	for i, p := range projects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info := T3ProjectInfo{Path: p, Name: filepath.Base(p), Threads: []T3ThreadInfo{}}
			cctx, cancel := context.WithTimeout(ctx, t3Timeout)
			defer cancel()
			ts, err := cli.Threads(cctx, p)
			if err != nil {
				info.Error = err.Error()
			}
			// Settled threads stay: a finished thread takes a message as
			// its next turn, and is often where the fix belongs.
			for _, t := range ts {
				info.Threads = append(info.Threads, T3ThreadInfo{
					ID: t.ID, Title: t.Title, Status: t.Status,
					Provider: t.Provider, Model: t.Model, UpdatedAt: t.UpdatedAt.UnixMilli(),
				})
			}
			sort.SliceStable(info.Threads, func(a, b int) bool { return info.Threads[a].UpdatedAt > info.Threads[b].UpdatedAt })
			if len(info.Threads) > t3PerProject {
				info.Threads = info.Threads[:t3PerProject]
			}
			out[i] = info
		}()
	}
	wg.Wait()
	return &T3Destinations{Projects: out}
}

// deliverFlag sends flag id to the T3 thread or project, command or webhook
// it names, once the flag is complete: at once for a plain flag, when its
// watch ends otherwise.
// It runs off the request; the outcome lands on the flag as Delivery.
func (s *Server) deliverFlag(id uint64) {
	s.flags.mu.Lock()
	f := s.flags.get(id)
	if f == nil || !f.To.IsExternal() || f.Delivery != nil {
		s.flags.mu.Unlock()
		return
	}
	f.Delivery = &FlagDelivery{State: "sending"}
	s.flags.bumpLocked(f)
	snap := f.Flag
	shots := f.shots
	s.flags.mu.Unlock()

	go func() {
		var d FlagDelivery
		switch {
		case snap.To.IsT3():
			d = s.sendToT3(snap, shots)
		case snap.To.Command != "":
			d = s.sendToCommand(snap, shots)
		default:
			d = s.sendToWebhook(snap, shots)
		}
		s.flags.mu.Lock()
		if f := s.flags.get(id); f != nil {
			f.Delivery = &d
			s.flags.bumpLocked(f)
		}
		s.flags.mu.Unlock()
	}()
}

func (s *Server) sendToT3(f Flag, shots [2][]byte) FlagDelivery {
	dir, files, err := s.writeFlagFiles(f, shots)
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error()}
	}
	cli, err := s.t3CLI()
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error(), Dir: dir}
	}
	msg := flagMessage(f, files, true)
	ctx, cancel := context.WithTimeout(context.Background(), t3Timeout)
	defer cancel()
	if f.To.T3Thread != "" {
		if err := cli.Send(ctx, f.To.T3Thread, msg); err != nil {
			return FlagDelivery{State: "failed", Error: err.Error(), Thread: f.To.T3Thread, Dir: dir}
		}
		return FlagDelivery{State: "sent", Thread: f.To.T3Thread, Dir: dir}
	}
	id, err := cli.Start(ctx, f.To.T3Project, t3Title(f), msg)
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error(), Dir: dir}
	}
	// A new thread shows up in the next listing, not one that is up to 10s old.
	s.t3cache.mu.Lock()
	s.t3cache.at = time.Time{}
	s.t3cache.mu.Unlock()
	return FlagDelivery{State: "sent", Thread: id, Dir: dir}
}

// flagFiles are the paths a T3 message points at; empty when not written.
type flagFiles struct {
	start, end, ui, events string
}

// writeFlagFiles writes f's screenshots, UI tree and recorded input to a
// directory of its own, and keeps only the newest keptFlagDirs of those.
func (s *Server) writeFlagFiles(f Flag, shots [2][]byte) (string, flagFiles, error) {
	root := s.flagDir
	if root == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", flagFiles{}, fmt.Errorf("locate cache dir: %w", err)
		}
		root = filepath.Join(cache, "loupe", "flags")
	}
	// The start time keeps names unique across restarts, which reuse ids.
	dir := filepath.Join(root, fmt.Sprintf("%d-%d", f.Start.T, f.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", flagFiles{}, err
	}
	var files flagFiles
	write := func(name string, b []byte) (string, error) {
		p := filepath.Join(dir, name)
		return p, os.WriteFile(p, b, 0o600)
	}
	var err error
	if shots[0] != nil {
		if files.start, err = write("start.png", shots[0]); err != nil {
			return "", files, err
		}
	}
	if shots[1] != nil {
		if files.end, err = write("end.png", shots[1]); err != nil {
			return "", files, err
		}
	}
	ui := f.Start.UI
	if f.End != nil && f.End.UI != nil {
		ui = f.End.UI
	}
	if ui != nil {
		b, _ := json.MarshalIndent(ui, "", "  ")
		if files.ui, err = write("ui.json", b); err != nil {
			return "", files, err
		}
	}
	if len(f.Events) > 0 {
		b, _ := json.MarshalIndent(f.Events, "", "  ")
		if files.events, err = write("events.json", b); err != nil {
			return "", files, err
		}
	}
	pruneFlagDirs(root, keptFlagDirs)
	return dir, files, nil
}

// pruneFlagDirs removes all but the newest keep directories under root.
// Names start with the flag's start time, so they sort oldest first.
func pruneFlagDirs(root string, keep int) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Slice(dirs, func(a, b int) bool { return flagDirTime(dirs[a]) < flagDirTime(dirs[b]) })
	for len(dirs) > keep {
		os.RemoveAll(filepath.Join(root, dirs[0]))
		dirs = dirs[1:]
	}
}

func flagDirTime(name string) int64 {
	var t, id int64
	fmt.Sscanf(name, "%d-%d", &t, &id)
	return t
}

// t3Title names a new thread for f: its note's first line, or what it is.
func t3Title(f Flag) string {
	note := strings.TrimSpace(f.Note)
	if i := strings.IndexByte(note, '\n'); i >= 0 {
		note = note[:i]
	}
	if note == "" {
		return "Loupe flag on " + f.Target
	}
	if r := []rune(note); len(r) > 80 {
		note = string(r[:80]) + "…"
	}
	return "Loupe: " + note
}

// flagMessage is the flag as a message an agent can act on without loupe:
// every fact it needs is in the text or in a file it names. inChat asks the
// agent to show the screenshot in its reply, for a chat that cannot show one
// a message carries.
func flagMessage(f Flag, files flagFiles, inChat bool) string {
	var b strings.Builder
	// The note first, as the person wrote it: it is what they will scan for
	// in the thread.
	if note := strings.TrimSpace(f.Note); note != "" {
		for _, line := range strings.Split(note, "\n") {
			b.WriteString("> " + strings.TrimRight(line, " \r") + "\n")
		}
		b.WriteString("\n")
	}
	if f.Kind == FlagInterrupt {
		fmt.Fprintf(&b, "The person stopped the agent driving %s in Loupe to raise this with you.", f.Target)
	} else {
		fmt.Fprintf(&b, "The person flagged something that looks off on %s in Loupe.", f.Target)
	}
	if strings.TrimSpace(f.Note) == "" {
		b.WriteString(" They left no note; the screenshot shows what they marked.")
	}
	if r := f.Region; r != nil {
		fmt.Fprintf(&b, " Marked area: %d,%d, %dx%d device pixels", r.X, r.Y, r.Width, r.Height)
		if f.Width > 0 {
			fmt.Fprintf(&b, ", on a %dx%d screen", f.Width, f.Height)
		}
		b.WriteString(".\n\n")
	} else {
		b.WriteString(" Marked area: the whole screen.\n\n")
	}
	// T3 shows no image a CLI message carries, but renders one in the
	// agent's reply, so the agent is asked to put it there.
	if inChat && files.start != "" {
		fmt.Fprintf(&b, "Start your reply with the screenshot as an image, ![Flag %d](<%s>), so the person sees what they flagged here.\n\n", f.ID, files.start)
	}
	link := func(label, path, after string) {
		if path != "" {
			fmt.Fprintf(&b, "- [%s](<%s>)%s\n", label, path, after)
		}
	}
	link("Screen when flagged", files.start, ", the area outlined in red")
	link("Screen when they sent it", files.end, "")
	link("UI tree", files.ui, ", the interactive elements in device pixels")
	if files.events != "" {
		link(fmt.Sprintf("Their %d inputs while reproducing it", len(f.Events)), files.events, "")
	}
	fmt.Fprintf(&b, "\nLoupe flag %d. With loupe's MCP server, the flag tool returns it in full, and the device can be driven as target %s.", f.ID, f.Target)
	return b.String()
}

// sendToCommand runs the named command with the message on stdin, in the
// flag's own directory, where message.md holds the same text.
func (s *Server) sendToCommand(f Flag, shots [2][]byte) FlagDelivery {
	dir, files, err := s.writeFlagFiles(f, shots)
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error()}
	}
	df, err := s.destinations()
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error(), Dir: dir}
	}
	c, ok := df.Command(f.To.Command)
	if !ok {
		return FlagDelivery{State: "failed", Error: "no command " + f.To.Command + " in destinations.json now", Dir: dir}
	}
	msg := flagMessage(f, files, false)
	msgPath := filepath.Join(dir, "message.md")
	if err := os.WriteFile(msgPath, []byte(msg), 0o600); err != nil {
		return FlagDelivery{State: "failed", Error: err.Error(), Dir: dir}
	}
	vars := map[string]string{
		"id": fmt.Sprint(f.ID), "target": f.Target, "dir": dir, "message": msgPath, "text": msg,
		"start": files.start, "end": files.end, "ui": files.ui, "events": files.events,
	}
	out, err := c.Run(context.Background(), vars, dir, msg)
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error(), Dir: dir, Output: out}
	}
	return FlagDelivery{State: "sent", Dir: dir, Output: out}
}

// WebhookFlag is the body POSTed to a webhook. The screenshots travel in it
// because the receiver usually cannot reach this server.
type WebhookFlag struct {
	Flag Flag `json:"flag"`
	// Message is the flag as an agent would be sent it.
	Message string `json:"message"`
	// Start and End are PNGs, base64 in JSON; End only once a watch ended.
	Start []byte `json:"start,omitempty"`
	End   []byte `json:"end,omitempty"`
}

func (s *Server) sendToWebhook(f Flag, shots [2][]byte) FlagDelivery {
	df, err := s.destinations()
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error()}
	}
	h, ok := df.Webhook(f.To.Webhook)
	if !ok {
		return FlagDelivery{State: "failed", Error: "no webhook " + f.To.Webhook + " in destinations.json now"}
	}
	body, err := json.Marshal(WebhookFlag{Flag: f, Message: flagMessage(f, flagFiles{}, false), Start: shots[0], End: shots[1]})
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error()}
	}
	_, out, err := h.Post(context.Background(), body)
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error(), Output: out}
	}
	return FlagDelivery{State: "sent", Output: out}
}
