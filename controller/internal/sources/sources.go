// Package sources runs the audio side of adopted sources. Each adopted
// source owns one snapserver stream (named after its device id, on its own
// TCP port from Base up) and one jitter buffer that feeds it. Streams are
// created at runtime, so snapserver.conf only carries the internal ones, and
// they are re-created whenever snapserver restarts (Sync).
package sources

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/audio"
	"github.com/Retty-Software/civitone/controller/internal/ingest"
	"github.com/Retty-Software/civitone/controller/internal/nodes"
	"github.com/Retty-Software/civitone/controller/internal/registry"
	"github.com/Retty-Software/civitone/controller/internal/snap"
)

// Info is one source as the console sees it.
type Info struct {
	DeviceID  string       `json:"device_id"`
	Name      string       `json:"name"`
	Stream    string       `json:"stream"`
	Kind      string       `json:"kind"`
	Connected bool         `json:"connected"`
	Buffer    ingest.Stats `json:"buffer"`
	GainDB    int          `json:"gain_db"`
	Paging    bool         `json:"paging"`
	Level     audio.Level  `json:"level"`
}

type source struct {
	node      registry.Node
	kind      string
	connected bool
	in        *ingest.Ingest
	meter     *audio.Meter
	cancel    context.CancelFunc
}

// Manager owns every adopted source's stream and buffer.
type Manager struct {
	SC        *snap.Link
	Reg       *registry.Registry
	Base      int     // first TCP port for source streams
	CeilingDB float64 // limiter ceiling
	OnEvent   func(deviceID, level, text string)

	ctx  context.Context
	mu   sync.Mutex
	srcs map[string]*source
}

func New(ctx context.Context, sc *snap.Link, reg *registry.Registry) *Manager {
	return &Manager{SC: sc, Reg: reg, Base: 4960, CeilingDB: -1, ctx: ctx, srcs: map[string]*source{}}
}

// Load starts every source in the registry. Call once at startup.
func (m *Manager) Load(ctx context.Context) error {
	all, err := m.Reg.ByRole(ctx, nodes.RoleSource)
	if err != nil {
		return err
	}
	for _, n := range all {
		if n.StreamID != "" {
			m.start(n)
		}
	}
	return nil
}

func streamURI(n registry.Node) string {
	q := url.Values{}
	q.Set("name", n.StreamID)
	q.Set("mode", "server")
	q.Set("sampleformat", "48000:16:2")
	// Snapserver calls a stream idle after 100 ms of digital silence and
	// stops it; every idle/playing flip made the speakers resync (a gap
	// between songs, a pause in a playlist). A connected source stays
	// playing through silence; it goes idle when it disconnects (no data).
	q.Set("idle_threshold", "60000")
	return fmt.Sprintf("tcp://127.0.0.1:%d?%s", n.Port, q.Encode())
}

// start runs the buffer for a source and makes sure its stream exists.
func (m *Manager) start(n registry.Node) {
	m.mu.Lock()
	if s, ok := m.srcs[n.DeviceID]; ok {
		s.node = n
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	s := &source{node: n, meter: audio.NewMeter(), cancel: cancel}
	s.in = ingest.New(n.DeviceID, fmt.Sprintf("127.0.0.1:%d", n.Port))
	s.in.Shaper = audio.NewShaper(true, float64(n.GainDB), m.CeilingDB)
	if n.BufferMs > 0 {
		s.in.SetBuffer(n.BufferMs)
	}
	s.in.Meter = s.meter
	s.in.OnEvent = m.OnEvent
	m.srcs[n.DeviceID] = s
	m.mu.Unlock()
	go s.in.Run(ctx)
	m.addStream(n)
}

func (m *Manager) addStream(n registry.Node) {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	err := m.SC.AddStream(ctx, streamURI(n))
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "exist") {
		log.Printf("source %s: add stream %s: %v", n.DeviceID, n.StreamID, err)
	}
}

// Sync re-creates every source stream; call it when snapserver (re)connects.
func (m *Manager) Sync() {
	m.mu.Lock()
	var all []registry.Node
	for _, s := range m.srcs {
		all = append(all, s.node)
	}
	m.mu.Unlock()
	for _, n := range all {
		m.addStream(n)
	}
}

// ensure gives an adopted source a stream and port if it has none yet.
func (m *Manager) ensure(ctx context.Context, id string) error {
	n, ok, err := m.Reg.Node(ctx, id)
	if err != nil || !ok {
		return fmt.Errorf("source %s not in registry: %v", id, err)
	}
	if n.StreamID == "" {
		port, err := m.Reg.FreePort(ctx, m.Base)
		if err != nil {
			return err
		}
		n.StreamID, n.Port = id, port
		if err := m.Reg.SetStream(ctx, id, n.StreamID, n.Port); err != nil {
			return err
		}
		log.Printf("source %s: stream %s on port %d", id, n.StreamID, n.Port)
	}
	m.start(n)
	return nil
}

// Connect is nodes.Manager.OnConnect for sources.
func (m *Manager) Connect(st nodes.State) {
	if st.Role != nodes.RoleSource || !st.Adopted {
		return
	}
	if err := m.ensure(m.ctx, st.DeviceID); err != nil {
		log.Printf("%v", err)
		return
	}
	m.mu.Lock()
	s := m.srcs[st.DeviceID]
	s.kind, s.connected = st.Kind, true
	s.node.Name = st.Name
	m.mu.Unlock()
	s.in.SetActive(true)
}

// Disconnect is nodes.Manager.OnDisconnect for sources.
func (m *Manager) Disconnect(st nodes.State) {
	m.mu.Lock()
	s := m.srcs[st.DeviceID]
	if s != nil {
		s.connected = false
	}
	m.mu.Unlock()
	if s != nil {
		s.in.SetActive(false)
	}
}

// Audio is nodes.Manager.OnAudio.
func (m *Manager) Audio(id string, pcm []byte) {
	m.mu.Lock()
	s := m.srcs[id]
	m.mu.Unlock()
	if s != nil {
		s.in.Push(pcm)
	}
}

// Remove stops a forgotten source and deletes its stream. Unknown ids are
// ignored, so it is safe to call for any forgotten device.
func (m *Manager) Remove(ctx context.Context, id string) {
	m.mu.Lock()
	s := m.srcs[id]
	delete(m.srcs, id)
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.cancel()
	if err := m.SC.RemoveStream(ctx, s.node.StreamID); err != nil {
		log.Printf("source %s: remove stream: %v", id, err)
	}
}

// SetBuffer changes a source's jitter buffer target, live, and keeps it.
func (m *Manager) SetBuffer(ctx context.Context, id string, ms int) error {
	m.mu.Lock()
	s := m.srcs[id]
	m.mu.Unlock()
	if s == nil {
		return fmt.Errorf("no source %s", id)
	}
	ms = max(ingest.MinBufferMs, min(ingest.MaxBufferMs, ms))
	if err := m.Reg.SetBuffer(ctx, id, ms); err != nil {
		return err
	}
	s.in.SetBuffer(ms)
	return nil
}

// Gain limits, dB. The limiter after the gain keeps boosted sources from
// clipping; past +30 dB a quiet source is mostly noise.
const (
	MinGainDB = -20
	MaxGainDB = 30
)

// SetGain changes a source's makeup gain, live, and keeps it.
func (m *Manager) SetGain(ctx context.Context, id string, db int) error {
	m.mu.Lock()
	s := m.srcs[id]
	m.mu.Unlock()
	if s == nil {
		return fmt.Errorf("no source %s", id)
	}
	db = max(MinGainDB, min(MaxGainDB, db))
	if err := m.Reg.SetGain(ctx, id, db); err != nil {
		return err
	}
	m.mu.Lock()
	s.node.GainDB = db
	m.mu.Unlock()
	s.in.Shaper.SetGain(float64(db))
	return nil
}

// SetPaging marks a source as the paging mic, or not, and keeps it.
func (m *Manager) SetPaging(ctx context.Context, id string, on bool) error {
	m.mu.Lock()
	s := m.srcs[id]
	m.mu.Unlock()
	if s == nil {
		return fmt.Errorf("no source %s", id)
	}
	if err := m.Reg.SetPaging(ctx, id, on); err != nil {
		return err
	}
	m.mu.Lock()
	s.node.Paging = on
	m.mu.Unlock()
	return nil
}

// Page is what the pager needs to know about one source.
type Page struct {
	Name     string
	Stream   string
	Paging   bool
	BufferMs int // jitter buffer target: how far the source's audio lags
}

// PageInfo describes a source for the pager; ok is false if it is unknown.
func (m *Manager) PageInfo(id string) (p Page, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.srcs[id]
	if s == nil {
		return Page{}, false
	}
	return Page{Name: s.node.Name, Stream: s.node.StreamID, Paging: s.node.Paging, BufferMs: s.in.Stats().TargetMs}, true
}

// Overlay plays mono pcm on a source's stream in place of its audio.
func (m *Manager) Overlay(id string, pcm []int16) {
	m.mu.Lock()
	s := m.srcs[id]
	m.mu.Unlock()
	if s != nil {
		s.in.Overlay(pcm)
	}
}

// Rename updates the display name.
func (m *Manager) Rename(id, name string) {
	m.mu.Lock()
	if s := m.srcs[id]; s != nil {
		s.node.Name = name
	}
	m.mu.Unlock()
}

// Infos lists every adopted source, oldest first.
// Peaks is each source's loudest sample since the last call, in dBFS, for
// the console's fast meters (Meter.TakePeak). Only one caller should use it.
func (m *Manager) Peaks() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(m.srcs))
	for id, s := range m.srcs {
		out[id] = s.meter.TakePeak()
	}
	return out
}

func (m *Manager) Infos() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.srcs))
	for id, s := range m.srcs {
		out = append(out, Info{
			DeviceID: id, Name: s.node.Name, Stream: s.node.StreamID, Kind: s.kind,
			Connected: s.connected, Buffer: s.in.Stats(), GainDB: s.node.GainDB, Paging: s.node.Paging, Level: s.meter.Level(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := m.srcs[out[i].DeviceID].node.AdoptedAt, m.srcs[out[j].DeviceID].node.AdoptedAt
		return a.Before(b) || a.Equal(b) && out[i].DeviceID < out[j].DeviceID
	})
	return out
}
