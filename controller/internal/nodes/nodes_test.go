package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeNode dials the manager like firmware would.
type fakeNode struct {
	t *testing.T
	c *websocket.Conn
}

func dial(t *testing.T, url string, hdr map[string][]string, hello map[string]any) *fakeNode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+"/node", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	n := &fakeNode{t: t, c: c}
	n.send(hello)
	return n
}

// dialCode connects unadopted, with an adopt code, and reads the welcome.
func dialCode(t *testing.T, url, id, code string) *fakeNode {
	t.Helper()
	n := dial(t, url, nil, map[string]any{"t": "hello", "device_id": id, "mac": "24:0A:C4:00:1A:2B", "fw": "test", "adopt_code": code})
	return n
}

// dialAdopted connects with a device token.
func dialAdopted(t *testing.T, url, id, token string) *fakeNode {
	t.Helper()
	return dial(t, url, map[string][]string{"Authorization": {"Bearer " + token}},
		map[string]any{"t": "hello", "device_id": id, "fw": "test"})
}

func (n *fakeNode) send(v any) {
	n.t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := n.c.Write(ctx, websocket.MessageText, b); err != nil {
		n.t.Fatalf("write: %v", err)
	}
}

func (n *fakeNode) read() map[string]any {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := n.c.Read(ctx)
	if err != nil {
		n.t.Fatalf("read: %v", err)
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	return m
}

// closeStatus waits for the controller to close the connection. Reading
// after a timeout is not possible (the library closes on timeout), so call
// it last.
func (n *fakeNode) closeStatus() websocket.StatusCode {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		_, _, err := n.c.Read(ctx)
		if err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// memStore is an in-memory Store.
type memStore struct {
	mu    sync.Mutex
	nodes map[string][2]string // id -> hash, name
	codes map[string]bool      // code -> auto-approve
}

func newMemStore() *memStore {
	return &memStore{nodes: map[string][2]string{}, codes: map[string]bool{"MANUAL": false, "AUTO22": true}}
}

func (s *memStore) Adoption(_ context.Context, id string) (string, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.nodes[id]
	return v[0], v[1], ok, nil
}

func (s *memStore) SaveAdoption(_ context.Context, id, role, name, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[id] = [2]string{hash, name}
	return nil
}

func (s *memStore) Forget(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
	return nil
}

func (s *memStore) UseCode(_ context.Context, code string) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	auto, ok := s.codes[code]
	return ok, auto, nil
}

func setup(t *testing.T) (*Manager, *httptest.Server) {
	m := NewManager(newMemStore())
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return m, srv
}

// adopted connects a node that the store already knows.
func adopted(t *testing.T, m *Manager, url, id string) *fakeNode {
	t.Helper()
	m.store.SaveAdoption(context.Background(), id, RoleNode, "Main", HashToken("tok-1234567890abcdef"))
	n := dialAdopted(t, url, id, "tok-1234567890abcdef")
	if w := n.read(); w["t"] != "welcome" || w["state"] != "adopted" {
		t.Fatalf("welcome = %v", w)
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHeartbeatIsKept(t *testing.T) {
	m, srv := setup(t)
	n := adopted(t, m, srv.URL, "civ-1a2b")
	n.send(map[string]any{"t": "hb", "rssi": -59})
	waitFor(t, "heartbeat", func() bool {
		s := m.States()
		return len(s) == 1 && s[0].Heartbeat != nil
	})
	s := m.States()[0]
	if s.DeviceID != "civ-1a2b" || !s.Connected || !s.Adopted || s.Health != Healthy || s.Name != "Main" {
		t.Fatalf("state = %+v", s)
	}
}

func TestSendWaitsForAck(t *testing.T) {
	m, srv := setup(t)
	n := adopted(t, m, srv.URL, "civ-1a2b")

	errc := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		errc <- m.Send(ctx, "civ-1a2b", "identify", map[string]any{"seconds": 5})
	}()
	cmd := n.read()
	if cmd["t"] != "cmd" || cmd["cmd"] != "identify" || cmd["seconds"].(float64) != 5 {
		t.Fatalf("got %v", cmd)
	}
	n.send(map[string]any{"t": "ack", "id": cmd["id"], "ok": true})
	if err := <-errc; err != nil {
		t.Fatalf("Send: %v", err)
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		errc <- m.Send(ctx, "civ-1a2b", "frobnicate", nil)
	}()
	cmd = n.read()
	n.send(map[string]any{"t": "ack", "id": cmd["id"], "ok": false, "err": "unknown cmd"})
	if err := <-errc; err == nil || !strings.Contains(err.Error(), "unknown cmd") {
		t.Fatalf("want unknown cmd error, got %v", err)
	}
}

func TestSendToUnknownNode(t *testing.T) {
	m, _ := setup(t)
	if err := m.Send(context.Background(), "nope", "reboot", nil); err != ErrNotConnected {
		t.Fatalf("got %v", err)
	}
}

func TestHealthDegradesThenGoesOffline(t *testing.T) {
	m, srv := setup(t)
	adopted(t, m, srv.URL, "civ-1a2b")

	base := time.Now()
	m.now = func() time.Time { return base.Add(11 * time.Second) }
	m.Sweep()
	if h := m.States()[0].Health; h != Degraded {
		t.Fatalf("after 11 s: %s", h)
	}
	m.now = func() time.Time { return base.Add(31 * time.Second) }
	m.Sweep()
	if h := m.States()[0].Health; h != Offline {
		t.Fatalf("after 31 s: %s", h)
	}
}

func TestDisconnectMarksOffline(t *testing.T) {
	m, srv := setup(t)
	n := adopted(t, m, srv.URL, "civ-1a2b")
	n.c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "offline", func() bool {
		s := m.States()[0]
		return !s.Connected && s.Health == Offline
	})
}

func TestReconnectReplacesOldSession(t *testing.T) {
	m, srv := setup(t)
	first := adopted(t, m, srv.URL, "civ-1a2b")
	adopted(t, m, srv.URL, "civ-1a2b")
	first.closeStatus() // the old session is closed
	time.Sleep(50 * time.Millisecond)
	if s := m.States()[0]; !s.Connected {
		t.Fatalf("node marked disconnected after reconnect: %+v", s)
	}
}

func TestRejectsMissingHello(t *testing.T) {
	m, srv := setup(t)
	n := dial(t, srv.URL, nil, map[string]any{"t": "hb"})
	if st := n.closeStatus(); st != websocket.StatusPolicyViolation {
		t.Fatalf("close = %v", st)
	}
	if len(m.States()) != 0 {
		t.Fatal("node registered without hello")
	}
}

func TestApprovalFlow(t *testing.T) {
	m, srv := setup(t)
	n := dialCode(t, srv.URL, "civ-1a2b", "man-ual") // case and dash don't matter
	if w := n.read(); w["t"] != "welcome" || w["state"] != "pending" {
		t.Fatalf("welcome = %v", w)
	}
	waitFor(t, "pending", func() bool { return len(m.States()) == 1 })
	b, _ := json.Marshal(m.States())
	if strings.Contains(string(b), "MANUAL") || strings.Contains(string(b), "man-ual") {
		t.Fatalf("adopt code in API output: %s", b)
	}
	if err := m.Send(context.Background(), "civ-1a2b", "reboot", nil); err != ErrNotAdopted {
		t.Fatalf("reboot to pending node: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- m.Approve(ctx, "civ-1a2b", "Main & 2nd")
	}()
	cmd := n.read()
	token, _ := cmd["token"].(string)
	if cmd["cmd"] != "adopt" || len(token) < 40 || cmd["name"] != "Main & 2nd" ||
		cmd["snap_host"] != "127.0.0.1" || cmd["snap_port"].(float64) != 1704 {
		t.Fatalf("adopt cmd = %v", cmd)
	}
	n.send(map[string]any{"t": "ack", "id": cmd["id"], "ok": true})
	if err := <-done; err != nil {
		t.Fatalf("Approve: %v", err)
	}
	n.c.Close(websocket.StatusNormalClosure, "restarting")

	r := dialAdopted(t, srv.URL, "civ-1a2b", token)
	if w := r.read(); w["state"] != "adopted" {
		t.Fatalf("after approval: %v", w)
	}
	waitFor(t, "adopted", func() bool { s := m.States(); return len(s) == 1 && s[0].Adopted && s[0].Connected })
	if s := m.States()[0]; s.Name != "Main & 2nd" {
		t.Fatalf("name = %q", s.Name)
	}
}

func TestAutoApproveCode(t *testing.T) {
	m, srv := setup(t)
	m.SnapHost = "snap.example.com"
	n := dialCode(t, srv.URL, "civ-1a2b", "AUTO22")
	n.read() // welcome
	cmd := n.read()
	if cmd["cmd"] != "adopt" || cmd["snap_host"] != "snap.example.com" {
		t.Fatalf("want adopt without an operator, got %v", cmd)
	}
	n.send(map[string]any{"t": "ack", "id": cmd["id"], "ok": true})
	waitFor(t, "stored", func() bool { _, _, ok, _ := m.store.Adoption(context.Background(), "civ-1a2b"); return ok })
}

func TestWrongCodeRejectedAndRateLimited(t *testing.T) {
	m, srv := setup(t)
	if st := dialCode(t, srv.URL, "civ-1a2b", "NOPE99").closeStatus(); st != CloseCodeRejected {
		t.Fatalf("wrong code: close %v", st)
	}
	if st := dial(t, srv.URL, nil, map[string]any{"t": "hello", "device_id": "civ-1a2b"}).closeStatus(); st != CloseCodeRejected {
		t.Fatalf("no code: close %v", st)
	}
	for i := 0; i < badCodeLimit; i++ {
		dialCode(t, srv.URL, "civ-1a2b", "NOPE99").closeStatus()
	}
	// Now even the right code is refused from this address.
	if st := dialCode(t, srv.URL, "civ-1a2b", "MANUAL").closeStatus(); st != CloseCodeRejected {
		t.Fatalf("after %d bad codes the right one still got in: %v", badCodeLimit, st)
	}
	if len(m.States()) != 0 {
		t.Fatal("rejected node registered")
	}
}

func TestRejectClosesPendingNode(t *testing.T) {
	m, srv := setup(t)
	n := dialCode(t, srv.URL, "civ-1a2b", "MANUAL")
	n.read()
	waitFor(t, "pending", func() bool { return len(m.States()) == 1 })
	if err := m.Reject("civ-1a2b"); err != nil {
		t.Fatal(err)
	}
	if st := n.closeStatus(); st != CloseCodeRejected {
		t.Fatalf("close = %v", st)
	}
	waitFor(t, "gone", func() bool { return len(m.States()) == 0 })
}

func TestUnknownTokenIsRejectedWith4001(t *testing.T) {
	_, srv := setup(t)
	if st := dialAdopted(t, srv.URL, "civ-1a2b", "not-a-real-token-123").closeStatus(); st != CloseTokenRejected {
		t.Fatalf("want close 4001, got %v", st)
	}
}

func TestForgetRevokesToken(t *testing.T) {
	m, srv := setup(t)
	n := adopted(t, m, srv.URL, "civ-1a2b")
	go func() {
		cmd := n.read()
		n.send(map[string]any{"t": "ack", "id": cmd["id"], "ok": true})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Forget(ctx, "civ-1a2b"); err != nil {
		t.Fatal(err)
	}
	if st := dialAdopted(t, srv.URL, "civ-1a2b", "tok-1234567890abcdef").closeStatus(); st != CloseTokenRejected {
		t.Fatalf("forgotten token still works: %v", st)
	}
}

func TestCodes(t *testing.T) {
	c := NewCode()
	if len(c) != 6 || strings.ContainsAny(c, "01IO") {
		t.Fatalf("code %q", c)
	}
	if NormalizeCode(" 7qk-m2x ") != "7QKM2X" || FormatCode("7QKM2X") != "7QK-M2X" {
		t.Fatal("normalize/format")
	}
}

func TestResetWipesConnectedNodeOnly(t *testing.T) {
	m, srv := setup(t)
	if err := m.Reset(context.Background(), "civ-1a2b"); err != ErrNotConnected {
		t.Fatalf("reset of unknown node: %v", err)
	}
	n := adopted(t, m, srv.URL, "civ-1a2b")
	got := make(chan string, 1)
	go func() {
		cmd := n.read()
		got <- cmd["cmd"].(string)
		n.send(map[string]any{"t": "ack", "id": cmd["id"], "ok": true})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Reset(ctx, "civ-1a2b"); err != nil {
		t.Fatal(err)
	}
	if c := <-got; c != "reset" {
		t.Fatalf("node got %q", c)
	}
	if _, _, ok, _ := m.store.Adoption(ctx, "civ-1a2b"); ok {
		t.Fatal("token survived reset")
	}
	if len(m.States()) != 0 {
		t.Fatal("reset node still listed")
	}
}

func TestSourceAdoptionAndAudio(t *testing.T) {
	m, srv := setup(t)
	var mu sync.Mutex
	var got []byte
	connects := make(chan State, 4)
	m.OnConnect = func(st State) { connects <- st }
	m.OnAudio = func(id string, pcm []byte) { mu.Lock(); got = append(got, pcm...); mu.Unlock() }

	// Wrong format is refused before anything else.
	bad := dial(t, srv.URL, nil, map[string]any{"t": "hello", "device_id": "src-a", "role": "source", "rate": 44100, "channels": 2, "adopt_code": "MANUAL"})
	if st := bad.closeStatus(); st != websocket.StatusPolicyViolation {
		t.Fatalf("wrong format: %v", st)
	}

	hello := map[string]any{"t": "hello", "device_id": "src-a", "role": "source", "kind": "system", "rate": 48000, "channels": 1, "adopt_code": "MANUAL"}
	n := dial(t, srv.URL, nil, hello)
	n.read() // welcome pending
	if st := <-connects; st.Role != RoleSource || st.Adopted {
		t.Fatalf("connect = %+v", st)
	}
	// Audio from a pending source is dropped.
	n.c.Write(context.Background(), websocket.MessageBinary, []byte{1, 2})

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- m.Approve(ctx, "src-a", "RadioDJ")
	}()
	cmd := n.read()
	if cmd["cmd"] != "adopt" || cmd["name"] != "RadioDJ" || cmd["snap_host"] != nil {
		t.Fatalf("source adopt cmd = %v", cmd)
	}
	n.send(map[string]any{"t": "ack", "id": cmd["id"], "ok": true})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	n.c.Close(websocket.StatusNormalClosure, "")

	delete(hello, "adopt_code")
	a := dial(t, srv.URL, map[string][]string{"Authorization": {"Bearer " + cmd["token"].(string)}}, hello)
	a.read() // welcome adopted
	if st := <-connects; !st.Adopted || st.Name != "RadioDJ" {
		t.Fatalf("adopted connect = %+v", st)
	}
	a.c.Write(context.Background(), websocket.MessageBinary, []byte{3, 4, 5, 6})
	waitFor(t, "audio", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 4 })
	if hash, name, ok, _ := m.store.Adoption(context.Background(), "src-a"); !ok || name != "RadioDJ" || hash == "" {
		t.Fatal("source adoption not stored")
	}
}

func TestBeforeAdoptCanRefuse(t *testing.T) {
	m, srv := setup(t)
	full := errors.New("pole limit reached: 8 of 8")
	m.BeforeAdopt = func(ctx context.Context, role string) error {
		if role == RoleNode {
			return full
		}
		return nil
	}
	n := dialCode(t, srv.URL, "civ-1a2b", "MANUAL")
	n.read() // welcome
	waitFor(t, "pending", func() bool { return len(m.States()) == 1 })
	if err := m.Approve(context.Background(), "civ-1a2b", ""); err != full {
		t.Fatalf("Approve = %v, want the limit error", err)
	}
	if _, _, ok, _ := m.store.Adoption(context.Background(), "civ-1a2b"); ok {
		t.Fatal("adopted past the limit")
	}
}
