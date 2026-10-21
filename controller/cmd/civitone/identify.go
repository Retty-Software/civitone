package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/audio"
	"github.com/Retty-Software/civitone/controller/internal/snap"
)

// chimeStream is the hidden snapserver stream identify plays on
// (snapserver.conf: tcp://127.0.0.1:4955?name=Chime).
const (
	chimeStream = "Chime"
	chimeAddr   = "127.0.0.1:4955"
)

// chimer plays the identify chime on one speaker: it moves that speaker's
// snapserver group to the Chime stream, unmuted, plays the chime, and puts
// the group back. One at a time, so two identifies don't fight over restore.
type chimer struct {
	mu     sync.Mutex
	sc     *snap.Link
	buffer func() time.Duration // snapserver buffer: audio plays this long after it's written
}

// chime returns once the speaker is back on its stream. mac is the node's
// MAC from its hello.
func (c *chimer) chime(ctx context.Context, mac string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, err := c.sc.GetStatus(ctx)
	if err != nil {
		return err
	}
	var group *snap.Group
	var client *snap.Client
	for gi := range st.Server.Groups {
		g := &st.Server.Groups[gi]
		for ci := range g.Clients {
			cl := &g.Clients[ci]
			// The client id is the MAC too, unless snapclient ran with --hostID.
			if (strings.EqualFold(cl.Host.MAC, mac) || strings.EqualFold(cl.ID, mac)) && cl.Connected {
				group, client = g, cl
			}
		}
	}
	if group == nil {
		return fmt.Errorf("not playing from this server's snapserver")
	}
	hasChime := false
	for _, s := range st.Server.Streams {
		hasChime = hasChime || s.ID == chimeStream
	}
	if !hasChime {
		return fmt.Errorf("snapserver has no %s stream", chimeStream)
	}

	prevStream := group.StreamID
	vol, muted := client.Config.Volume.Percent, client.Config.Volume.Muted
	restore := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.sc.SetGroupStream(rctx, group.ID, prevStream)
		if muted {
			c.sc.SetVolume(rctx, client.ID, vol, true)
		}
	}
	if err := c.sc.SetGroupStream(ctx, group.ID, chimeStream); err != nil {
		return err
	}
	defer restore()
	if muted {
		c.sc.SetVolume(ctx, client.ID, vol, false)
	}
	if err := audio.PlayTo(ctx, chimeAddr, audio.Chime()); err != nil {
		return err
	}
	// The last chunk reaches the speaker one buffer later.
	select {
	case <-ctx.Done():
	case <-time.After(c.buffer() + 300*time.Millisecond):
	}
	return nil
}
