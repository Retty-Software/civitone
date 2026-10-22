// Package nodes is the controller end of the node control channel
// (docs/control-protocol.md): nodes dial /node, say hello, send a heartbeat
// every 5 s and answer commands. The manager tracks health per node and lets
// the rest of the controller send commands and wait for the ack.
//
// Adoption: an operator makes an adopt code in the console and types it into
// the node's setup page. The node says hello with that code; the controller
// holds it as pending until an operator approves it (or at once, if the code
// auto-approves), then sends "adopt" with a device token. From then on the
// node sends "Authorization: Bearer <token>" and no code.
//
// Sources (role "source": source boards, Civitone Source on a computer) use
// the same channel and the same adoption; once adopted they also send mono
// s16le PCM at 48 kHz in binary frames, handed to OnAudio.
package nodes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Health per system design rule 5.
const (
	Healthy  = "healthy"
	Degraded = "degraded"
	Offline  = "offline"

	HeartbeatEvery = 5 * time.Second
	degradedAfter  = 2 * HeartbeatEvery
	offlineAfter   = 6 * HeartbeatEvery
)

// Close codes the firmware acts on.
const (
	// CloseTokenRejected: the token is unknown here. The node drops it and
	// goes back to setup.
	CloseTokenRejected websocket.StatusCode = 4001
	// CloseCodeRejected: the adopt code is wrong, expired, or an operator
	// rejected the node. The node shows that on its setup page.
	CloseCodeRejected websocket.StatusCode = 4003
)

// Guessing adopt codes: 10 wrong codes from one address per 10 minutes.
const (
	badCodeLimit  = 10
	badCodeWindow = 10 * time.Minute
)

// Roles.
const (
	RoleNode   = "node"
	RoleSource = "source"
)

// Hello is the first frame a node or source sends.
type Hello struct {
	DeviceID  string `json:"device_id"`
	Role      string `json:"role,omitempty"` // node (default) | source
	MAC       string `json:"mac,omitempty"`
	FW        string `json:"fw"`
	Board     string `json:"board"`
	Kind      string `json:"kind,omitempty"`  // sources: what feeds it (system, input, playlist, radio, board)
	Label     string `json:"label,omitempty"` // the name typed on the device, offered at approval
	Rate      int    `json:"rate,omitempty"`
	Channels  int    `json:"channels,omitempty"`
	AdoptCode string `json:"adopt_code,omitempty"`
}

// State is what the console and API see for one node. Hello.AdoptCode is
// always empty here.
type State struct {
	Hello
	Name      string          `json:"name"`
	Adopted   bool            `json:"adopted"`
	Health    string          `json:"health"`
	Connected bool            `json:"connected"`
	LastSeen  time.Time       `json:"last_seen"`
	Heartbeat json.RawMessage `json:"hb,omitempty"`
}

// Events are reported to the controller (event log, console refresh).
type Event struct {
	DeviceID string
	Level    string // info | warn | bad | ops
	Text     string
}

type frame struct {
	T     string `json:"t"`
	ID    uint64 `json:"id,omitempty"`
	OK    bool   `json:"ok,omitempty"`
	Err   string `json:"err,omitempty"`
	Down  bool   `json:"down,omitempty"` // t=ptt
	Hello        // inlined for t=hello
}

type session struct {
	conn    *websocket.Conn
	host    string   // Host the node dialled, without port: where it finds snapserver too
	pending sync.Map // cmd id -> chan frame
}

type node struct {
	state State
	sess  *session
}

// Store keeps adoptions and adopt codes. The registry implements it.
type Store interface {
	// Adoption returns the token hash and name for an adopted device.
	Adoption(ctx context.Context, deviceID string) (tokenHash, name string, ok bool, err error)
	SaveAdoption(ctx context.Context, deviceID, role, name, tokenHash string) error
	Forget(ctx context.Context, deviceID string) error
	// UseCode checks a normalized adopt code and counts the use.
	UseCode(ctx context.Context, code string) (ok, autoApprove bool, err error)
}

var (
	ErrNotConnected = errors.New("node not connected")
	ErrNotAdopted   = errors.New("node is not adopted")
	ErrNotPending   = errors.New("node is not waiting for approval")
)

// Manager holds every node that has connected since start.
type Manager struct {
	mu      sync.Mutex
	nodes   map[string]*node
	badCode map[string][]time.Time // remote address -> recent wrong codes
	nextID  atomic.Uint64
	now     func() time.Time
	store   Store

	// SnapHost and SnapPort are sent to nodes at adoption. An empty SnapHost
	// means "the host the node dialled", which is right whenever snapserver
	// runs beside the controller.
	SnapHost string
	SnapPort int

	// OnEvent, if set, is called for connects, disconnects and health changes.
	// It must not block.
	OnEvent func(Event)

	// OnConnect / OnDisconnect, if set, see every session start and end.
	OnConnect    func(State)
	OnDisconnect func(State)
	// OnAudio gets each binary frame from an adopted source. It must not block.
	OnAudio func(deviceID string, pcm []byte)
	// OnPTT gets an adopted source's push-to-talk presses and releases.
	OnPTT func(deviceID string, down bool)
	// BeforeAdopt, if set, can refuse an approval (a plan's pole limit).
	BeforeAdopt func(ctx context.Context, role string) error
}

func NewManager(store Store) *Manager {
	return &Manager{nodes: map[string]*node{}, badCode: map[string][]time.Time{}, now: time.Now, store: store, SnapPort: 1704}
}

// HashToken is how tokens are stored: never in the clear.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NormalizeCode makes "7qk m2x", "7QKM2X" and "7QK-M2X" the same code.
func NormalizeCode(c string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(c) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NewCode makes a 6-character adopt code from an alphabet without 0/O/1/I.
func NewCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 32: no modulo bias
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// FormatCode shows a normalized code as "7QK-M2X".
func FormatCode(c string) string {
	if len(c) == 6 {
		return c[:3] + "-" + c[3:]
	}
	return c
}

func (m *Manager) emit(e Event) {
	log.Printf("node %s: %s", e.DeviceID, e.Text)
	if m.OnEvent != nil {
		m.OnEvent(e)
	}
}

// clientAddr is the node's address for rate limiting. Behind nginx the
// socket peer is localhost, so from there trust the proxy's header.
func clientAddr(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host == "127.0.0.1" || host == "::1" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return host
}

func (m *Manager) tooManyBadCodes(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cut := m.now().Add(-badCodeWindow)
	keep := m.badCode[addr][:0]
	for _, t := range m.badCode[addr] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	m.badCode[addr] = keep
	return len(keep) >= badCodeLimit
}

func (m *Manager) noteBadCode(addr string) {
	m.mu.Lock()
	m.badCode[addr] = append(m.badCode[addr], m.now())
	m.mu.Unlock()
}

// ServeHTTP accepts one node connection on /node.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(64 * 1024)
	ctx := r.Context()

	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var hello frame
	err = readJSON(hctx, c, &hello)
	cancel()
	if err != nil || hello.T != "hello" || hello.DeviceID == "" {
		c.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	switch hello.Role {
	case "":
		hello.Role = RoleNode
	case RoleNode:
	case RoleSource:
		if hello.Rate != 48000 || hello.Channels != 1 {
			c.Close(websocket.StatusPolicyViolation, "sources send 48000 Hz mono s16le")
			return
		}
	default:
		c.Close(websocket.StatusPolicyViolation, "unknown role")
		return
	}

	adopted, autoApprove, name := false, false, ""
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && tok != "" {
		hash, n, found, err := m.store.Adoption(ctx, hello.DeviceID)
		if err != nil {
			c.Close(websocket.StatusInternalError, "registry unavailable")
			return
		}
		if !found || subtle.ConstantTimeCompare([]byte(hash), []byte(HashToken(tok))) != 1 {
			m.emit(Event{hello.DeviceID, "warn", "rejected: unknown device token (node goes back to setup)"})
			c.Close(CloseTokenRejected, "token rejected")
			return
		}
		adopted, name = true, n
	} else {
		addr := clientAddr(r)
		if m.tooManyBadCodes(addr) {
			c.Close(CloseCodeRejected, "too many wrong adopt codes; wait 10 minutes")
			return
		}
		ok, auto := false, false
		if code := NormalizeCode(hello.AdoptCode); code != "" {
			if ok, auto, err = m.store.UseCode(ctx, code); err != nil {
				c.Close(websocket.StatusInternalError, "registry unavailable")
				return
			}
		}
		if !ok {
			m.noteBadCode(addr)
			c.Close(CloseCodeRejected, "adopt code wrong or expired")
			return
		}
		autoApprove = auto
	}

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	s := &session{conn: c, host: host}
	m.attach(hello.Hello, s, adopted, name)
	defer m.detach(hello.DeviceID, s)

	state := "pending"
	if adopted {
		state = "adopted"
	}
	wm := map[string]string{"t": "welcome", "state": state}
	if adopted && name != "" {
		wm["name"] = name // the node shows it on its setup page
	}
	welcome, _ := json.Marshal(wm)
	if err := c.Write(ctx, websocket.MessageText, welcome); err != nil {
		return
	}
	if autoApprove {
		go func() {
			actx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := m.Approve(actx, hello.DeviceID, ""); err != nil {
				m.emit(Event{hello.DeviceID, "warn", "auto-approve failed: " + err.Error()})
			}
		}()
	}

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if adopted && hello.Role == RoleSource && m.OnAudio != nil {
				m.OnAudio(hello.DeviceID, data)
			}
			continue
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.T {
		case "hb":
			m.heartbeat(hello.DeviceID, data)
		case "ptt":
			if adopted && hello.Role == RoleSource && m.OnPTT != nil {
				m.OnPTT(hello.DeviceID, f.Down)
			}
		case "ack":
			if ch, ok := s.pending.LoadAndDelete(f.ID); ok {
				ch.(chan frame) <- f
			}
		default:
			log.Printf("node %s: ignoring frame %q", hello.DeviceID, f.T)
		}
	}
}

func (m *Manager) attach(h Hello, s *session, adopted bool, name string) {
	h.AdoptCode = ""
	m.mu.Lock()
	n, ok := m.nodes[h.DeviceID]
	if !ok {
		n = &node{}
		m.nodes[h.DeviceID] = n
	}
	old := n.sess
	n.sess = s
	n.state.Hello = h
	n.state.Adopted = adopted
	n.state.Name = name
	n.state.Connected = true
	n.state.Health = Healthy
	n.state.LastSeen = m.now()
	st := n.state
	m.mu.Unlock()
	if old != nil {
		// Close waits for the peer's close frame; a half-dead old session
		// never sends one, so don't hold up the new session's welcome.
		go old.conn.Close(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}
	if m.OnConnect != nil {
		m.OnConnect(st)
	}
	if adopted {
		m.emit(Event{h.DeviceID, "info", fmt.Sprintf("connected (fw %s)", h.FW)})
	} else {
		m.emit(Event{h.DeviceID, "info", fmt.Sprintf("waiting for approval (fw %s)", h.FW)})
	}
}

func (m *Manager) detach(id string, s *session) {
	m.mu.Lock()
	n := m.nodes[id]
	current := n != nil && n.sess == s
	pending := current && !n.state.Adopted
	var st State
	if current {
		st = n.state
		n.sess = nil
		n.state.Connected = false
		n.state.Health = Offline
		if pending {
			delete(m.nodes, id) // a pending node only exists while connected
		}
	}
	m.mu.Unlock()
	s.conn.CloseNow()
	if current && m.OnDisconnect != nil {
		st.Connected = false
		m.OnDisconnect(st)
	}
	if current && !pending {
		m.emit(Event{id, "bad", "disconnected"})
	}
}

func (m *Manager) heartbeat(id string, raw []byte) {
	m.mu.Lock()
	n := m.nodes[id]
	if n == nil {
		m.mu.Unlock()
		return
	}
	was := n.state.Health
	n.state.Heartbeat = append(json.RawMessage(nil), raw...)
	n.state.LastSeen = m.now()
	n.state.Health = Healthy
	m.mu.Unlock()
	if was == Degraded {
		m.emit(Event{id, "info", "healthy again"})
	}
}

// Sweep recomputes health from heartbeat age. Call it every few seconds.
func (m *Manager) Sweep() {
	now := m.now()
	var events []Event
	m.mu.Lock()
	for id, n := range m.nodes {
		if !n.state.Connected {
			continue
		}
		age := now.Sub(n.state.LastSeen)
		next := Healthy
		switch {
		case age >= offlineAfter:
			next = Offline
		case age >= degradedAfter:
			next = Degraded
		}
		if next != n.state.Health {
			n.state.Health = next
			lvl := "warn"
			if next == Offline {
				lvl = "bad"
			}
			events = append(events, Event{id, lvl, fmt.Sprintf("%s: no heartbeat for %s", next, age.Round(time.Second))})
		}
	}
	m.mu.Unlock()
	for _, e := range events {
		m.emit(e)
	}
}

// Run sweeps health until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Sweep()
		}
	}
}

// States returns every known node, sorted by device id.
func (m *Manager) States() []State {
	m.mu.Lock()
	out := make([]State, 0, len(m.nodes))
	for _, n := range m.nodes {
		out = append(out, n.state)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}

// Send issues a command and waits for the node's ack. args are merged into
// the frame next to "t", "id" and "cmd". A pending node only takes identify.
func (m *Manager) Send(ctx context.Context, deviceID, cmd string, args map[string]any) error {
	return m.send(ctx, deviceID, cmd, args, true)
}

func (m *Manager) send(ctx context.Context, deviceID, cmd string, args map[string]any, checkAdopted bool) error {
	m.mu.Lock()
	n := m.nodes[deviceID]
	var s *session
	adopted := false
	if n != nil {
		s, adopted = n.sess, n.state.Adopted
	}
	m.mu.Unlock()
	if s == nil {
		return ErrNotConnected
	}
	if checkAdopted && !adopted && cmd != "identify" {
		return ErrNotAdopted
	}
	return m.sendOn(ctx, s, deviceID, cmd, args)
}

func (m *Manager) sendOn(ctx context.Context, s *session, deviceID, cmd string, args map[string]any) error {
	id := m.nextID.Add(1)
	msg := map[string]any{}
	for k, v := range args {
		msg[k] = v
	}
	msg["t"], msg["id"], msg["cmd"] = "cmd", id, cmd
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ch := make(chan frame, 1)
	s.pending.Store(id, ch)
	defer s.pending.Delete(id)

	if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
		return err
	}
	select {
	case f := <-ch:
		if !f.OK {
			return fmt.Errorf("%s %s: %s", deviceID, cmd, f.Err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s %s: no ack: %w", deviceID, cmd, ctx.Err())
	}
}

func readJSON(ctx context.Context, c *websocket.Conn, v any) error {
	_, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Approve issues a pending node its device token and tells it where
// snapserver is. The node stores both, restarts and reconnects adopted.
func (m *Manager) Approve(ctx context.Context, deviceID, name string) error {
	m.mu.Lock()
	n := m.nodes[deviceID]
	var s *session
	role := ""
	if n != nil && n.sess != nil && !n.state.Adopted {
		s, role = n.sess, n.state.Role
	}
	m.mu.Unlock()
	if s == nil {
		return ErrNotPending
	}
	if m.BeforeAdopt != nil {
		if err := m.BeforeAdopt(ctx, role); err != nil {
			return err
		}
	}
	if name == "" {
		name = n.state.Label
	}
	if name == "" {
		name = deviceID
	}
	snapHost := m.SnapHost
	if snapHost == "" {
		snapHost = s.host
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err := m.store.SaveAdoption(ctx, deviceID, role, name, HashToken(token)); err != nil {
		return err
	}
	args := map[string]any{"token": token, "name": name}
	if role == RoleNode {
		args["snap_host"], args["snap_port"] = snapHost, m.SnapPort
	}
	err := m.sendOn(ctx, s, deviceID, "adopt", args)
	if err != nil {
		m.store.Forget(context.Background(), deviceID)
		return err
	}
	m.emit(Event{deviceID, "ops", fmt.Sprintf("adopted as %q", name)})
	return nil
}

// Reject turns a pending node away; its setup page says so.
func (m *Manager) Reject(deviceID string) error {
	m.mu.Lock()
	n := m.nodes[deviceID]
	var s *session
	if n != nil && n.sess != nil && !n.state.Adopted {
		s = n.sess
	}
	m.mu.Unlock()
	if s == nil {
		return ErrNotPending
	}
	s.conn.Close(CloseCodeRejected, "rejected by an operator")
	m.emit(Event{deviceID, "ops", "rejected"})
	return nil
}

// Forget revokes a node's token and tells it, if connected, to go back to
// setup. It keeps its Wi-Fi networks.
func (m *Manager) Forget(ctx context.Context, deviceID string) error {
	return m.drop(ctx, deviceID, "forget", "forgotten")
}

// Reset is Forget plus a wipe of the node itself: Wi-Fi networks, server
// address, everything. It reboots into the setup network. The node must be
// connected; an offline node can only be forgotten.
func (m *Manager) Reset(ctx context.Context, deviceID string) error {
	m.mu.Lock()
	n := m.nodes[deviceID]
	connected := n != nil && n.sess != nil && n.state.Adopted
	m.mu.Unlock()
	if !connected {
		return ErrNotConnected
	}
	return m.drop(ctx, deviceID, "reset", "reset to factory settings")
}

func (m *Manager) drop(ctx context.Context, deviceID, cmd, what string) error {
	if err := m.store.Forget(ctx, deviceID); err != nil {
		return err
	}
	m.mu.Lock()
	var s *session
	if n := m.nodes[deviceID]; n != nil {
		s = n.sess
	}
	delete(m.nodes, deviceID)
	m.mu.Unlock()
	if s != nil {
		// Ack or not, the token is already dead: the node gets a 4001 on its
		// next connect and resets itself.
		m.sendOn(ctx, s, deviceID, cmd, nil)
	}
	m.emit(Event{deviceID, "ops", what})
	return nil
}

// Rename updates the display name of a connected node.
func (m *Manager) Rename(deviceID, name string) {
	m.mu.Lock()
	if n := m.nodes[deviceID]; n != nil {
		n.state.Name = name
	}
	m.mu.Unlock()
}
