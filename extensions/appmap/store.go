package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Map is one recorded walk through an app on one target.
type Map struct {
	ID     string `json:"id"`
	Target string `json:"target"`
	Name   string `json:"name,omitempty"`
	// Compose asks /api/ui for composable names on each step. Costs about
	// two seconds on the first call after an app starts (docs/agent-api.md).
	Compose bool      `json:"compose,omitempty"`
	Created time.Time `json:"created"`
	// Current is the screen the device was last seen on.
	Current string    `json:"current"`
	Root    string    `json:"root"`
	Screens []*Screen `json:"screens"`
	Edges   []*Edge   `json:"edges"`

	// steps serialises a map's steps: each one starts from where the last
	// one left the device.
	steps sync.Mutex
}

// Screen is one screen of the app, however often it was visited.
type Screen struct {
	ID          string      `json:"id"`
	Name        string      `json:"name,omitempty"`
	Fingerprint Fingerprint `json:"fingerprint"`
	// Aliases are the keys of screens merged into this one by hand, so a
	// later visit to either lands here.
	Aliases []string  `json:"aliases,omitempty"`
	Visits  int       `json:"visits"`
	First   time.Time `json:"first"`
	Width   int       `json:"width"`
	Height  int       `json:"height"`
}

func (s *Screen) matches(key string) bool {
	if s.Fingerprint.Key == key {
		return true
	}
	for _, a := range s.Aliases {
		if a == key {
			return true
		}
	}
	return false
}

// Edge is an action that led from one screen to another.
type Edge struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Action string   `json:"action"`
	Node   *NodeSel `json:"node,omitempty"`
	Note   string   `json:"note,omitempty"`
	Count  int      `json:"count"`
}

func (e *Edge) same(o *Edge) bool {
	if e.From != o.From || e.To != o.To || e.Action != o.Action {
		return false
	}
	if (e.Node == nil) != (o.Node == nil) {
		return false
	}
	return e.Node == nil || *e.Node == *o.Node
}

func (m *Map) screen(id string) *Screen {
	for _, s := range m.Screens {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (m *Map) find(key string) *Screen {
	for _, s := range m.Screens {
		if s.matches(key) {
			return s
		}
	}
	return nil
}

func (m *Map) addEdge(e *Edge) *Edge {
	for _, o := range m.Edges {
		if o.same(e) {
			o.Count += e.Count
			if e.Note != "" {
				o.Note = e.Note
			}
			return o
		}
	}
	m.Edges = append(m.Edges, e)
	return e
}

// merge folds screen from into screen into: its keys, its visits and its
// edges. Edges that end up from a screen to itself are dropped.
func (m *Map) merge(from, into *Screen) {
	into.Aliases = append(into.Aliases, from.Fingerprint.Key)
	into.Aliases = append(into.Aliases, from.Aliases...)
	into.Visits += from.Visits
	edges := m.Edges
	m.Edges = nil
	for _, e := range edges {
		if e.From == from.ID {
			e.From = into.ID
		}
		if e.To == from.ID {
			e.To = into.ID
		}
		if e.From != e.To {
			m.addEdge(e)
		}
	}
	for i, s := range m.Screens {
		if s == from {
			m.Screens = append(m.Screens[:i], m.Screens[i+1:]...)
			break
		}
	}
	if m.Current == from.ID {
		m.Current = into.ID
	}
	if m.Root == from.ID {
		m.Root = into.ID
	}
}

func (m *Map) nextScreenID() string {
	n := len(m.Screens) + 1
	for m.screen(fmt.Sprintf("s%d", n)) != nil {
		n++
	}
	return fmt.Sprintf("s%d", n)
}

func (s *Server) mapDir(id string) string { return filepath.Join(s.cfg.DataDir, id) }

// save writes the map's JSON. Callers hold s.mu.
func (s *Server) save(m *Map) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.mapDir(m.ID), "map.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.mapDir(m.ID), "map.json"))
}

// loadAll reads every map on disk, so a restart keeps them.
func (s *Server) loadAll() error {
	ents, err := os.ReadDir(s.cfg.DataDir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !e.IsDir() || !idPattern.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.mapDir(e.Name()), "map.json"))
		if err != nil {
			continue
		}
		var m Map
		if json.Unmarshal(b, &m) == nil && m.ID == e.Name() {
			s.maps[m.ID] = &m
		}
	}
	return nil
}
