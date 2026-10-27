package peer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Store persists the peer list. It holds tokens, so the file is 0600 and sits
// beside handsoff.json, where a spare server started with its own -config
// keeps its own list.
type Store struct{ path string }

// NewStore keeps the list at path. An empty path keeps nothing, which is what
// a test with no file wants.
func NewStore(path string) *Store { return &Store{path: path} }

// Load reads the list. A missing file is an empty list.
func (s *Store) Load() ([]Server, error) {
	if s == nil || s.path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Servers []Server `json:"servers"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return f.Servers, nil
}

// Save writes the list atomically.
func (s *Store) Save(servers []Server) error {
	if s == nil || s.path == "" {
		return nil
	}
	if servers == nil {
		servers = []Server{}
	}
	raw, err := json.MarshalIndent(map[string]any{"servers": servers}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
