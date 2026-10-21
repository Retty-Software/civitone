package srcapp

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/audio"
)

//go:embed page.html
var pageFS embed.FS

// App runs every configured source and serves the setup page.
type App struct {
	Store *Store
	ctx   context.Context

	mu      sync.Mutex
	runners map[string]*runner

	devMu   sync.Mutex
	devAt   time.Time
	inputs  []string
	outputs []string
}

func New(ctx context.Context, store *Store) *App {
	a := &App{Store: store, ctx: ctx, runners: map[string]*runner{}}
	for _, s := range store.Get().Sources {
		a.startRunner(s.ID)
	}
	return a
}

func (a *App) startRunner(id string) {
	r := newRunner(id, a.Store)
	a.mu.Lock()
	a.runners[id] = r
	a.mu.Unlock()
	r.start(a.ctx)
}

func (a *App) pokeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.runners {
		r.poke()
	}
}

func (a *App) runner(id string) *runner {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runners[id]
}

// Summary is one line per source for the tray: "RadioDJ: live".
func (a *App) Summary() []string {
	var out []string
	for _, s := range a.state().Sources {
		name := s.Name
		if name == "" {
			name = s.Label
		}
		if name == "" {
			name = s.Kind
		}
		label := map[string]string{StateLive: "live", StatePending: "waiting for approval", StateConnecting: "connecting",
			StateNeedsSetup: "needs setup", StateRejected: "rejected", StateError: "can't connect", StatePaused: "paused"}[s.State]
		if label == "" {
			label = "starting"
		}
		if s.Input != "" {
			label += " (audio problem)"
		}
		out = append(out, name+": "+label)
	}
	return out
}

// Kinds this build can offer.
func kinds() []map[string]string {
	k := []map[string]string{}
	if runtime.GOOS == "windows" {
		k = append(k,
			map[string]string{"id": KindSystem, "label": "This computer's sound", "help": "Whatever plays on an output device: RadioDJ, Spotify, a browser. No virtual cable needed."},
			map[string]string{"id": KindInput, "label": "Audio input", "help": "A microphone, line in, or USB audio interface."})
	}
	k = append(k,
		map[string]string{"id": KindPlaylist, "label": "Playlist", "help": "Every music file in a folder (MP3, FLAC, Ogg, WAV), shuffled, on repeat. Songs added to the folder join the next round."},
		map[string]string{"id": KindRadio, "label": "Internet radio", "help": "An MP3 or Ogg stream URL (Icecast, Shoutcast). Not the .pls/.m3u playlist file; the stream link inside it."},
		map[string]string{"id": KindTone, "label": "Test chime", "help": "A bell arpeggio on a loop, for checking speakers."})
	return k
}

type pageState struct {
	Version string              `json:"version"`
	OS      string              `json:"os"`
	Server  string              `json:"server"`
	Sources []Status            `json:"sources"`
	Kinds   []map[string]string `json:"kinds"`
	Inputs  []string            `json:"inputs"`
	Outputs []string            `json:"outputs"`
	Music   string              `json:"music_dir"` // suggested playlist folder
}

func (a *App) state() pageState {
	cfg := a.Store.Get()
	st := pageState{Version: Version, OS: runtime.GOOS, Server: cfg.Server, Sources: []Status{}, Kinds: kinds()}
	for _, s := range cfg.Sources {
		if r := a.runner(s.ID); r != nil {
			st.Sources = append(st.Sources, r.status())
		}
	}
	st.Inputs, st.Outputs = a.devices()
	if home, err := os.UserHomeDir(); err == nil {
		st.Music = filepath.Join(home, "Music")
	}
	return st
}

// devices lists recording and playback devices, re-enumerated at most every
// 5 s (the page polls twice a second).
func (a *App) devices() (inputs, outputs []string) {
	a.devMu.Lock()
	defer a.devMu.Unlock()
	if time.Since(a.devAt) > 5*time.Second {
		a.inputs, _ = audio.Devices(false)
		a.outputs, _ = audio.Devices(true)
		if a.inputs == nil {
			a.inputs = []string{}
		}
		if a.outputs == nil {
			a.outputs = []string{}
		}
		a.devAt = time.Now()
	}
	return a.inputs, a.outputs
}

// sameOrigin blocks other web pages in the person's browser from driving
// this local API: a write must come from this page.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return r.Header.Get("Sec-Fetch-Site") == "" || r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	switch {
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		b, _ := pageFS.ReadFile("page.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	case r.URL.Path == "/api/state" && r.Method == http.MethodGet:
		writeJSONResp(w, a.state())
	case r.URL.Path == "/api/server" && r.Method == http.MethodPost:
		var body struct{ Server string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		server := strings.TrimSpace(body.Server)
		if _, err := EndpointURL(server, "/source"); err != nil {
			http.Error(w, "Enter a server address, like civitone.example.com", http.StatusBadRequest)
			return
		}
		a.Store.Update(func(c *Config) {
			if c.Server != server {
				// Tokens belong to the old server.
				for i := range c.Sources {
					c.Sources[i].Token, c.Sources[i].Name = "", ""
				}
			}
			c.Server = server
		})
		a.pokeAll()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/api/sources" && r.Method == http.MethodPost:
		var body struct{ Kind, Device, Label, Code string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ok := false
		for _, k := range kinds() {
			ok = ok || k["id"] == body.Kind
		}
		if !ok {
			http.Error(w, "this computer can't offer that kind of source", http.StatusBadRequest)
			return
		}
		s := SourceCfg{ID: NewID(), Kind: body.Kind, Device: body.Device, Label: strings.TrimSpace(body.Label), AdoptCode: strings.TrimSpace(body.Code)}
		if err := a.Store.Update(func(c *Config) { c.Sources = append(c.Sources, s) }); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.startRunner(s.ID)
		writeJSONResp(w, map[string]string{"id": s.ID})
	case strings.HasPrefix(r.URL.Path, "/api/sources/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/sources/")
		rn := a.runner(id)
		if rn == nil {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			var body struct {
				Device *string
				Label  *string
				Code   *string
				Paused *bool
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			a.Store.UpdateSource(id, func(s *SourceCfg) {
				if body.Device != nil {
					s.Device = *body.Device
				}
				if body.Label != nil {
					s.Label = strings.TrimSpace(*body.Label)
				}
				if body.Code != nil {
					s.AdoptCode = strings.TrimSpace(*body.Code)
					s.Token, s.Name = "", "" // a new code is a new adoption
				}
				if body.Paused != nil {
					s.Paused = *body.Paused
				}
			})
			rn.poke()
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			rn.stop()
			a.mu.Lock()
			delete(a.runners, id)
			a.mu.Unlock()
			a.Store.Update(func(c *Config) {
				keep := c.Sources[:0]
				for _, s := range c.Sources {
					if s.ID != id {
						keep = append(keep, s)
					}
				}
				c.Sources = keep
			})
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Println(err)
	}
}
