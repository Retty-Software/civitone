package srcapp

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Retty-Software/civitone/controller/internal/audio"
	"github.com/Retty-Software/civitone/controller/internal/player"
)

// Version is set at build time (-ldflags "-X .../srcapp.Version=...").
var Version = "dev"

// Close codes from the controller (internal/nodes).
const (
	closeTokenRejected websocket.StatusCode = 4001
	closeCodeRejected  websocket.StatusCode = 4003
)

// States a source can be in, for the page.
const (
	StatePaused     = "paused"
	StateNeedsSetup = "needs-setup" // no server or no adopt code / token
	StateConnecting = "connecting"
	StatePending    = "pending" // waiting for approval in the console
	StateLive       = "live"
	StateRejected   = "rejected"
	StateError      = "error"
)

// Status is one source as the page shows it.
type Status struct {
	SourceCfg
	Token   string      `json:"-"`
	Adopted bool        `json:"adopted"`
	State   string      `json:"state"`
	Detail  string      `json:"detail"`
	Input   string      `json:"input"`       // capture problem, if any
	Playing string      `json:"now_playing"` // playlist / radio: what's on
	Level   audio.Level `json:"level"`
}

// runner keeps one source captured and connected until stopped.
type runner struct {
	id       string
	store    *Store
	cancel   context.CancelFunc
	wakeCap  chan struct{} // config changed: reopen the device
	wakeConn chan struct{} // config changed: reconnect
	meter    *audio.Meter
	live     atomic.Bool
	pcm      chan []byte // mono s16le, only filled while live

	mu     sync.Mutex
	state  string
	detail string
	input  string
	player *player.Player // playlist / radio, for "now playing"
}

func newRunner(id string, store *Store) *runner {
	return &runner{id: id, store: store, wakeCap: make(chan struct{}, 1), wakeConn: make(chan struct{}, 1), meter: audio.NewMeter(), pcm: make(chan []byte, 50)}
}

func (r *runner) set(state, detail string) {
	r.mu.Lock()
	r.state, r.detail = state, detail
	r.mu.Unlock()
}

func (r *runner) setInput(problem string) {
	r.mu.Lock()
	r.input = problem
	r.mu.Unlock()
}

func (r *runner) poke() {
	for _, ch := range []chan struct{}{r.wakeCap, r.wakeConn} {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (r *runner) status() Status {
	cfg, _ := r.store.Source(r.id)
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{SourceCfg: cfg, Adopted: cfg.Token != "", State: r.state, Detail: r.detail, Input: r.input, Level: r.meter.Level()}
	if r.player != nil {
		now, problem := r.player.NowPlaying()
		st.Playing = now
		if problem != "" && st.Input == "" {
			st.Input = problem
		}
	}
	return st
}

func (r *runner) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	go r.captureLoop(ctx)
	go r.connectLoop(ctx)
}

func (r *runner) stop() {
	if r.cancel != nil {
		r.cancel()
	}
}

// sleep waits d, or less if the config changes (wake).
func sleep(ctx context.Context, wake <-chan struct{}, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-wake:
	case <-time.After(d):
	}
}

func openKind(cfg SourceCfg) (audio.Capture, error) {
	switch cfg.Kind {
	case KindSystem:
		return audio.Open(cfg.Device, true)
	case KindInput:
		if cfg.Device == "" {
			return nil, errors.New("pick an input device")
		}
		return audio.Open(cfg.Device, false)
	case KindTone:
		return audio.Loop(audio.Chime(), 2*time.Second), nil
	case KindPlaylist:
		if cfg.Device == "" {
			return nil, errors.New("pick a music folder")
		}
		return player.Playlist(cfg.Device), nil
	case KindRadio:
		if cfg.Device == "" {
			return nil, errors.New("enter the stream URL")
		}
		return player.Radio(cfg.Device), nil
	}
	return nil, fmt.Errorf("unknown source kind %q", cfg.Kind)
}

// captureLoop keeps the device open, meters it, and hands mono PCM to the
// connection while it is live.
func (r *runner) captureLoop(ctx context.Context) {
	for ctx.Err() == nil {
		cfg, ok := r.store.Source(r.id)
		if !ok {
			return
		}
		if cfg.Paused {
			r.setInput("")
			sleep(ctx, r.wakeCap, time.Hour)
			continue
		}
		capt, err := openKind(cfg)
		if err != nil {
			r.setInput(err.Error())
			sleep(ctx, r.wakeCap, 3*time.Second)
			continue
		}
		r.setInput("")
		p, _ := capt.(*player.Player)
		r.mu.Lock()
		r.player = p
		r.mu.Unlock()
		r.pump(ctx, capt)
		capt.Close()
		r.mu.Lock()
		r.player = nil
		r.mu.Unlock()
		if err := capt.Err(); err != nil {
			r.setInput(err.Error())
		}
		sleep(ctx, r.wakeCap, time.Second)
	}
}

func (r *runner) pump(ctx context.Context, capt audio.Capture) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wakeCap: // config changed (device, pause): reopen
			return
		case chunk, ok := <-capt.Chunks():
			if !ok {
				return
			}
			// Mono: the poles carry one mix on both channels.
			mono := make([]byte, 0, len(chunk))
			for i := 0; i+1 < len(chunk); i += 2 {
				m := int16((int32(chunk[i]) + int32(chunk[i+1])) / 2)
				chunk[i], chunk[i+1] = m, m
				mono = binary.LittleEndian.AppendUint16(mono, uint16(m))
			}
			r.meter.Observe(chunk)
			if r.live.Load() {
				select {
				case r.pcm <- mono:
				default: // connection stalled; drop rather than build latency
				}
			}
		}
	}
}

// connectLoop keeps the source connected to the server and adopted.
func (r *runner) connectLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		cfg, ok := r.store.Source(r.id)
		if !ok {
			return
		}
		server := r.store.Get().Server
		switch {
		case cfg.Paused:
			r.set(StatePaused, "")
			sleep(ctx, r.wakeConn, time.Hour)
			continue
		case server == "":
			r.set(StateNeedsSetup, "enter the server address")
			sleep(ctx, r.wakeConn, time.Hour)
			continue
		case cfg.Token == "" && cfg.AdoptCode == "":
			r.set(StateNeedsSetup, "enter an adopt code from the console")
			sleep(ctx, r.wakeConn, time.Hour)
			continue
		}
		r.set(StateConnecting, server)
		err := r.session(ctx, server, cfg)
		r.live.Store(false)
		if ctx.Err() != nil {
			return
		}
		var hold *holdError
		if errors.As(err, &hold) {
			r.set(hold.state, hold.msg)
			sleep(ctx, r.wakeConn, time.Hour) // until the person changes something
			backoff = time.Second
			continue
		}
		if err != nil {
			r.set(StateError, err.Error())
		}
		sleep(ctx, r.wakeConn, backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if err == nil {
			backoff = time.Second
		}
	}
}

// holdError stops retrying until the config changes.
type holdError struct{ state, msg string }

func (e *holdError) Error() string { return e.msg }

func (r *runner) session(ctx context.Context, server string, cfg SourceCfg) error {
	url, err := EndpointURL(server, "/source")
	if err != nil {
		return &holdError{StateNeedsSetup, err.Error()}
	}
	h := http.Header{}
	if cfg.Token != "" {
		h.Set("Authorization", "Bearer "+cfg.Token)
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("server answered %s", resp.Status)
		}
		return fmt.Errorf("can't reach %s: %v", url, err)
	}
	defer c.CloseNow()
	c.SetReadLimit(64 * 1024)

	hello := map[string]any{
		"t": "hello", "device_id": r.id, "role": "source", "kind": cfg.Kind,
		"fw": Version, "board": "civitone-source/" + runtime.GOOS, "rate": audio.SampleRate, "channels": 1,
		"label": cfg.Label,
	}
	if cfg.Token == "" {
		hello["adopt_code"] = cfg.AdoptCode
	}
	if err := writeJSON(dctx, c, hello); err != nil {
		return err
	}

	msg, err := readMsg(dctx, c)
	if err != nil {
		return r.closed(err)
	}
	if msg["t"] != "welcome" {
		return fmt.Errorf("unexpected first frame %v", msg["t"])
	}
	if msg["state"] != "adopted" {
		return r.pending(ctx, c)
	}
	return r.stream(ctx, c)
}

// closed turns a controller close into a state.
func (r *runner) closed(err error) error {
	switch websocket.CloseStatus(err) {
	case closeCodeRejected:
		r.store.UpdateSource(r.id, func(s *SourceCfg) { s.AdoptCode = "" })
		reason := "adopt code wrong or expired"
		var ce websocket.CloseError
		if errors.As(err, &ce) && ce.Reason != "" {
			reason = ce.Reason
		}
		return &holdError{StateRejected, reason + ". Enter a new adopt code."}
	case closeTokenRejected:
		r.store.UpdateSource(r.id, func(s *SourceCfg) { s.Token = ""; s.Name = "" })
		return &holdError{StateRejected, "the server no longer knows this source (forgotten). Enter a new adopt code."}
	}
	return fmt.Errorf("connection lost: %v", err)
}

// pending waits for approval; the adopt command carries our token.
func (r *runner) pending(ctx context.Context, c *websocket.Conn) error {
	r.set(StatePending, "waiting for approval in the console")
	// Heartbeats while waiting, or the console shows the source as degraded.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				writeJSON(hbCtx, c, map[string]any{"t": "hb"})
			}
		}
	}()
	for {
		msg, err := readMsg(ctx, c)
		if err != nil {
			return r.closed(err)
		}
		if msg["t"] != "cmd" {
			continue
		}
		id := msg["id"]
		if msg["cmd"] == "adopt" {
			token, _ := msg["token"].(string)
			name, _ := msg["name"].(string)
			if len(token) < 16 {
				writeJSON(ctx, c, map[string]any{"t": "ack", "id": id, "ok": false, "err": "bad token"})
				continue
			}
			if err := r.store.UpdateSource(r.id, func(s *SourceCfg) { s.Token, s.Name, s.AdoptCode = token, name, "" }); err != nil {
				writeJSON(ctx, c, map[string]any{"t": "ack", "id": id, "ok": false, "err": "saving token failed"})
				return err
			}
			writeJSON(ctx, c, map[string]any{"t": "ack", "id": id, "ok": true})
			c.Close(websocket.StatusNormalClosure, "reconnecting with token")
			return nil // reconnect at once, adopted
		}
		writeJSON(ctx, c, map[string]any{"t": "ack", "id": id, "ok": msg["cmd"] == "identify"})
	}
}

// stream sends audio until the connection ends.
func (r *runner) stream(ctx context.Context, c *websocket.Conn) error {
	r.set(StateLive, "")
	// Drop audio queued from before this session.
	for len(r.pcm) > 0 {
		<-r.pcm
	}
	r.live.Store(true)
	defer r.live.Store(false)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readErr := make(chan error, 1)
	go func() {
		for {
			msg, err := readMsg(sctx, c)
			if err != nil {
				readErr <- err
				return
			}
			if msg["t"] != "cmd" {
				continue
			}
			ack := map[string]any{"t": "ack", "id": msg["id"], "ok": true}
			switch msg["cmd"] {
			case "identify":
			case "forget", "reset":
				r.store.UpdateSource(r.id, func(s *SourceCfg) { s.Token, s.Name = "", "" })
				writeJSON(sctx, c, ack)
				readErr <- &holdError{StateRejected, "forgotten by the server. Enter a new adopt code."}
				return
			default:
				ack["ok"], ack["err"] = false, "unknown cmd"
			}
			writeJSON(sctx, c, ack)
		}
	}()

	hb := time.NewTicker(5 * time.Second)
	defer hb.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusNormalClosure, "stopping")
			return nil
		case err := <-readErr:
			var hold *holdError
			if errors.As(err, &hold) {
				return err
			}
			return r.closed(err)
		case <-r.wakeConn:
			c.Close(websocket.StatusNormalClosure, "settings changed")
			return nil
		case <-hb.C:
			l := r.meter.Level()
			writeJSON(sctx, c, map[string]any{"t": "hb", "uptime": int(time.Since(start).Seconds()), "peak_dbfs": l.PeakDBFS})
		case pcm := <-r.pcm:
			wctx, wcancel := context.WithTimeout(sctx, 2*time.Second)
			err := c.Write(wctx, websocket.MessageBinary, pcm)
			wcancel()
			if err != nil {
				return fmt.Errorf("sending audio: %v", err)
			}
		}
	}
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

func readMsg(ctx context.Context, c *websocket.Conn) (map[string]any, error) {
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return nil, err
		}
		if typ != websocket.MessageText {
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			return m, nil
		}
	}
}
