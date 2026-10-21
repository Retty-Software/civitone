// Package ws fans state out to console browsers over WebSocket. Later it also
// carries the node control channel (heartbeats in, commands out).
package ws

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Hub struct {
	mu    sync.Mutex
	conns map[*websocket.Conn]struct{}
	last  []byte // last state frame, sent to new connections

	sending sync.Map // *websocket.Conn -> true while a live frame is being written
}

func NewHub() *Hub { return &Hub{conns: map[*websocket.Conn]struct{}{}} }

// Broadcast sends v as JSON to every connected console.
func (h *Hub) Broadcast(v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("ws: marshal: %v", err)
		return
	}
	h.mu.Lock()
	h.last = b
	conns := make([]*websocket.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			h.drop(c)
		}
		cancel()
	}
}

// Clients reports how many consoles are connected.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// BroadcastLive sends v like Broadcast but doesn't keep it for new
// connections: frequent, partial frames (meter levels) that the next full
// state frame supersedes. A console still busy with the previous one skips
// this one, so a slow link falls behind on meters, not on state.
func (h *Hub) BroadcastLive(v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		if _, busy := h.sending.LoadOrStore(c, true); busy {
			continue
		}
		go func(c *websocket.Conn) {
			defer h.sending.Delete(c)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := c.Write(ctx, websocket.MessageText, b); err != nil {
				h.drop(c)
			}
		}(c)
	}
}

func (h *Hub) drop(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
	c.Close(websocket.StatusNormalClosure, "")
}

// ServeHTTP upgrades a console browser connection.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	last := h.last
	h.mu.Unlock()
	if last != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		_ = c.Write(ctx, websocket.MessageText, last)
		cancel()
	}
	// Read loop only to notice the browser going away.
	for {
		if _, _, err := c.Read(r.Context()); err != nil {
			h.drop(c)
			return
		}
	}
}
