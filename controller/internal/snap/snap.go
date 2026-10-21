// Package snap talks to a Snapcast server over its JSON-RPC TCP port (1705)
// and optionally runs the server as a child process.
package snap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Status is the subset of Server.GetStatus the controller cares about.
type Status struct {
	Server struct {
		Groups  []Group  `json:"groups"`
		Streams []Stream `json:"streams"`
	} `json:"server"`
}

type Group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	StreamID string   `json:"stream_id"`
	Muted    bool     `json:"muted"`
	Clients  []Client `json:"clients"`
}

type Client struct {
	ID        string `json:"id"`
	Connected bool   `json:"connected"`
	Host      struct {
		Name string `json:"name"`
		IP   string `json:"ip"`
		MAC  string `json:"mac"`
	} `json:"host"`
	Config struct {
		Name    string `json:"name"`
		Latency int    `json:"latency"`
		Volume  struct {
			Muted   bool `json:"muted"`
			Percent int  `json:"percent"`
		} `json:"volume"`
	} `json:"config"`
	Snapclient struct {
		Version string `json:"version"`
	} `json:"snapclient"`
	LastSeen struct {
		Sec int64 `json:"sec"`
	} `json:"lastSeen"`
}

type Stream struct {
	ID     string `json:"id"`
	Status string `json:"status"` // idle | playing | disabled
}

// Notification is a server-pushed event (Client.OnConnect, Group.OnStreamChanged, ...).
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      uint64      `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *uint64         `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Conn is a live JSON-RPC connection to snapserver.
type Conn struct {
	addr    string
	conn    net.Conn
	mu      sync.Mutex // guards writes
	nextID  uint64
	pending sync.Map // id -> chan rpcResponse
	Events  chan Notification
}

// Dial connects to the JSON-RPC port, retrying until ctx is done.
func Dial(ctx context.Context, addr string) (*Conn, error) {
	var d net.Dialer
	for {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			sc := &Conn{addr: addr, conn: c, Events: make(chan Notification, 64)}
			go sc.readLoop()
			return sc, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (c *Conn) Close() error { return c.conn.Close() }

func (c *Conn) readLoop() {
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var r rpcResponse
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			log.Printf("snap: bad frame: %v", err)
			continue
		}
		if r.ID == nil {
			select {
			case c.Events <- Notification{Method: r.Method, Params: r.Params}:
			default:
				log.Printf("snap: dropping event %s (consumer slow)", r.Method)
			}
			continue
		}
		if ch, ok := c.pending.LoadAndDelete(*r.ID); ok {
			ch.(chan rpcResponse) <- r
		}
	}
	close(c.Events)
}

// Call sends one request and decodes the result into out (may be nil).
func (c *Conn) Call(ctx context.Context, method string, params interface{}, out interface{}) error {
	id := atomic.AddUint64(&c.nextID, 1)
	ch := make(chan rpcResponse, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)

	b, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	c.mu.Lock()
	_, err = c.conn.Write(append(b, '\n'))
	c.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("snap %s: %d %s", method, r.Error.Code, r.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Conn) GetStatus(ctx context.Context) (*Status, error) {
	var s Status
	if err := c.Call(ctx, "Server.GetStatus", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SetVolume sets a client's DAC volume (0-100) and mute.
func (c *Conn) SetVolume(ctx context.Context, clientID string, percent int, muted bool) error {
	return c.Call(ctx, "Client.SetVolume", map[string]interface{}{
		"id":     clientID,
		"volume": map[string]interface{}{"percent": percent, "muted": muted},
	}, nil)
}

func (c *Conn) SetClientName(ctx context.Context, clientID, name string) error {
	return c.Call(ctx, "Client.SetName", map[string]interface{}{"id": clientID, "name": name}, nil)
}

// SetGroupMute mutes or unmutes a whole group.
func (c *Conn) SetGroupMute(ctx context.Context, groupID string, muted bool) error {
	return c.Call(ctx, "Group.SetMute", map[string]interface{}{"id": groupID, "mute": muted}, nil)
}

// SetGroupStream routes a group to a stream. Clients in one group are sample-locked.
func (c *Conn) SetGroupStream(ctx context.Context, groupID, streamID string) error {
	return c.Call(ctx, "Group.SetStream", map[string]interface{}{"id": groupID, "stream_id": streamID}, nil)
}

// AddStream creates a stream at runtime from a source URI
// (tcp://127.0.0.1:4960?name=...&mode=server). Runtime streams do not
// survive a snapserver restart; the caller re-adds them.
func (c *Conn) AddStream(ctx context.Context, uri string) error {
	return c.Call(ctx, "Stream.AddStream", map[string]interface{}{"streamUri": uri}, nil)
}

// RemoveStream deletes a stream by id.
func (c *Conn) RemoveStream(ctx context.Context, id string) error {
	return c.Call(ctx, "Stream.RemoveStream", map[string]interface{}{"id": id}, nil)
}

// DeleteClient drops a (disconnected) client from snapserver's state.
func (c *Conn) DeleteClient(ctx context.Context, clientID string) error {
	return c.Call(ctx, "Server.DeleteClient", map[string]interface{}{"id": clientID}, nil)
}

// SetGroupClients moves the given clients into a group (the "link" operation).
func (c *Conn) SetGroupClients(ctx context.Context, groupID string, clientIDs []string) error {
	return c.Call(ctx, "Group.SetClients", map[string]interface{}{"id": groupID, "clients": clientIDs}, nil)
}

// Supervisor runs snapserver as a child process, restarts it if it dies,
// and restarts it on purpose when a setting in its config changes (the
// buffer). Each start writes Runtime from the Base config with the current
// settings applied, so Base can stay read-only.
type Supervisor struct {
	Binary   string
	Base     string     // config template (the repo's snapserver.conf, or /etc/snapserver.conf)
	Runtime  string     // where the config actually used is written
	BufferMs func() int // current [stream] buffer; 0 keeps Base's

	mu      sync.Mutex
	cmd     *exec.Cmd
	restart bool
}

// Run supervises until ctx is done.
func (s *Supervisor) Run(ctx context.Context) error {
	if _, err := exec.LookPath(s.Binary); err != nil {
		return errors.New("snapserver not found: brew install snapcast (Mac), apt install snapserver (Debian)")
	}
	for ctx.Err() == nil {
		if err := s.writeConfig(); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, s.Binary, "-c", s.Runtime)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		log.Printf("snap: started %s (pid %d) with %s", s.Binary, cmd.Process.Pid, s.Runtime)
		s.mu.Lock()
		s.cmd, s.restart = cmd, false
		s.mu.Unlock()
		err := cmd.Wait()
		s.mu.Lock()
		planned := s.restart
		s.mu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		if !planned {
			log.Printf("snap: snapserver exited (%v); restarting in 1 s", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

// Restart stops snapserver; Run starts it again with a fresh config.
func (s *Supervisor) Restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		s.restart = true
		s.cmd.Process.Signal(os.Interrupt)
		go func(c *exec.Cmd) { // snapserver normally exits within a second
			time.Sleep(5 * time.Second)
			c.Process.Kill()
		}(s.cmd)
	}
}

var bufferLine = regexp.MustCompile(`(?m)^\s*buffer\s*=.*$`)

func (s *Supervisor) writeConfig() error {
	b, err := os.ReadFile(s.Base)
	if err != nil {
		return fmt.Errorf("snapserver config: %w", err)
	}
	conf := string(b)
	if s.BufferMs != nil {
		if ms := s.BufferMs(); ms > 0 {
			line := fmt.Sprintf("buffer = %d", ms)
			if bufferLine.MatchString(conf) {
				conf = bufferLine.ReplaceAllString(conf, line)
			} else {
				conf = strings.Replace(conf, "[stream]", "[stream]\n"+line, 1)
			}
		}
	}
	if s.Runtime == s.Base {
		return nil
	}
	return os.WriteFile(s.Runtime, []byte(conf), 0o644)
}

// ErrNotConnected is returned by Link calls while snapserver is unreachable.
var ErrNotConnected = errors.New("snapserver not connected")

// Link keeps a JSON-RPC connection to snapserver up, redialling after a drop
// (snapserver restarts, the WSL VM restarting). Events from every connection
// are forwarded on one channel; a synthetic "Link.Connected" event marks each
// (re)connect so consumers can refresh.
type Link struct {
	addr   string
	cur    atomic.Pointer[Conn]
	Events chan Notification
}

func NewLink(addr string) *Link {
	return &Link{addr: addr, Events: make(chan Notification, 64)}
}

// Run blocks until ctx is done.
func (l *Link) Run(ctx context.Context) {
	for ctx.Err() == nil {
		c, err := Dial(ctx, l.addr)
		if err != nil {
			return // ctx done
		}
		l.cur.Store(c)
		log.Printf("snap: connected to %s", l.addr)
		l.forward(Notification{Method: "Link.Connected"})
		for ev := range c.Events {
			l.forward(ev)
		}
		l.cur.Store(nil)
		c.Close()
		if ctx.Err() == nil {
			log.Printf("snap: connection to %s lost, redialling", l.addr)
		}
	}
}

func (l *Link) forward(ev Notification) {
	select {
	case l.Events <- ev:
	default:
		log.Printf("snap: dropping event %s (consumer slow)", ev.Method)
	}
}

// Conn returns the live connection or ErrNotConnected.
func (l *Link) Conn() (*Conn, error) {
	if c := l.cur.Load(); c != nil {
		return c, nil
	}
	return nil, ErrNotConnected
}

func (l *Link) GetStatus(ctx context.Context) (*Status, error) {
	c, err := l.Conn()
	if err != nil {
		return nil, err
	}
	return c.GetStatus(ctx)
}

func (l *Link) SetVolume(ctx context.Context, clientID string, percent int, muted bool) error {
	c, err := l.Conn()
	if err != nil {
		return err
	}
	return c.SetVolume(ctx, clientID, percent, muted)
}

func (l *Link) AddStream(ctx context.Context, uri string) error {
	c, err := l.Conn()
	if err != nil {
		return err
	}
	return c.AddStream(ctx, uri)
}

func (l *Link) RemoveStream(ctx context.Context, id string) error {
	c, err := l.Conn()
	if err != nil {
		return err
	}
	return c.RemoveStream(ctx, id)
}

func (l *Link) DeleteClient(ctx context.Context, clientID string) error {
	c, err := l.Conn()
	if err != nil {
		return err
	}
	return c.DeleteClient(ctx, clientID)
}

func (l *Link) SetGroupStream(ctx context.Context, groupID, streamID string) error {
	c, err := l.Conn()
	if err != nil {
		return err
	}
	return c.SetGroupStream(ctx, groupID, streamID)
}

func (l *Link) SetGroupMute(ctx context.Context, groupID string, muted bool) error {
	c, err := l.Conn()
	if err != nil {
		return err
	}
	return c.SetGroupMute(ctx, groupID, muted)
}
