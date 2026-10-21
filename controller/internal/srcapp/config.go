// Package srcapp is Civitone Source: the app on a computer that feeds audio
// to a Civitone server. Each configured source (this computer's sound, an
// audio input, a test chime, ...) is adopted on the server like a node and
// streams mono PCM over its own WebSocket. A local web page configures it.
package srcapp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Source kinds.
const (
	KindSystem   = "system"   // loopback of a playback device: whatever this computer plays
	KindInput    = "input"    // a recording device: mic, line in, USB interface
	KindTone     = "tone"     // the test chime on a loop
	KindPlaylist = "playlist" // every music file in a folder, shuffled
	KindRadio    = "radio"    // an MP3 internet stream
)

// SourceCfg is one configured source.
type SourceCfg struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Device    string `json:"device,omitempty"` // device name (empty = default output), folder (playlist) or URL (radio)
	Label     string `json:"label,omitempty"`  // shown until the server names it
	Name      string `json:"name,omitempty"`   // name the server gave it at approval
	Token     string `json:"token,omitempty"`
	AdoptCode string `json:"adopt_code,omitempty"`
	Paused    bool   `json:"paused,omitempty"`
}

// Config is everything Civitone Source keeps on disk.
type Config struct {
	Server  string      `json:"server"`
	Sources []SourceCfg `json:"sources"`
}

// Store is the config file, safe for concurrent use.
type Store struct {
	path string
	mu   sync.Mutex
	c    Config
}

// DefaultPath is %APPDATA%\Civitone\source.json on Windows,
// ~/Library/Application Support/Civitone/source.json on macOS.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "Civitone", "source.json")
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, &s.c)
}

// Get returns a copy of the config.
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.c
	c.Sources = append([]SourceCfg(nil), s.c.Sources...)
	return c
}

// Update changes the config under the lock and saves it.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.c)
	b, err := json.MarshalIndent(s.c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Source returns one source's config.
func (s *Store) Source(id string) (SourceCfg, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.c.Sources {
		if x.ID == id {
			return x, true
		}
	}
	return SourceCfg{}, false
}

// UpdateSource changes one source and saves.
func (s *Store) UpdateSource(id string, fn func(*SourceCfg)) error {
	return s.Update(func(c *Config) {
		for i := range c.Sources {
			if c.Sources[i].ID == id {
				fn(&c.Sources[i])
			}
		}
	})
}

// NewID makes a source device id: "src-" and 8 hex digits.
func NewID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "src-" + hex.EncodeToString(b)
}
