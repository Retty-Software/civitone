// Package paging is the emergency page: while the push-to-talk on a source
// marked as the paging mic is held, every speaker group plays that source,
// unmuted, after a two-tone alert. Releasing waits until the last words have
// played out of the buffers, then puts every group and speaker back as they
// were.
//
// Pages win over everything, including speakers that join mid-page (the
// keeper re-applies every couple of seconds). One page at a time: a second
// paging mic pressed during a page is ignored.
package paging

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/audio"
	"github.com/Retty-Software/civitone/controller/internal/snap"
	"github.com/Retty-Software/civitone/controller/internal/sources"
)

const (
	// DefaultMaxPage ends a page whose push-to-talk never comes up (a stuck
	// switch must not hold every speaker forever).
	DefaultMaxPage = 5 * time.Minute
	tailMargin     = 500 * time.Millisecond
	rpcTimeout     = 5 * time.Second
)

// Snap is the part of snap.Link the pager drives.
type Snap interface {
	GetStatus(ctx context.Context) (*snap.Status, error)
	SetGroupStream(ctx context.Context, groupID, streamID string) error
	SetGroupMute(ctx context.Context, groupID string, muted bool) error
	SetVolume(ctx context.Context, clientID string, percent int, muted bool) error
}

// Sources is the part of sources.Manager the pager needs.
type Sources interface {
	PageInfo(id string) (sources.Page, bool)
	Overlay(id string, pcm []int16)
}

// State is what the console shows.
type State struct {
	Active bool      `json:"active"`
	Source string    `json:"source,omitempty"`
	Name   string    `json:"name,omitempty"`
	Since  time.Time `json:"since,omitempty"`
	Ending bool      `json:"ending,omitempty"` // released; playing out the buffers
}

type Pager struct {
	SC            Snap
	Src           Sources
	SpeakerBuffer func() time.Duration // how long audio takes to reach the speakers
	OnEvent       func(deviceID, level, text string)
	MaxPage       time.Duration
	KeepEvery     time.Duration // how often a page re-takes every group

	mu  sync.Mutex
	cur *page
}

type page struct {
	src, name, stream string
	since             time.Time
	ending            bool
	endTimer          *time.Timer
	stop              chan struct{}

	mu        sync.Mutex // serializes apply and restore
	done      bool
	streams   map[string]string // group -> stream before the page
	mutedGrp  map[string]bool   // groups we unmuted
	mutedCl   map[string]int    // clients we unmuted -> their volume
	applyErrs int
}

func New(sc Snap, src Sources) *Pager {
	return &Pager{SC: sc, Src: src, MaxPage: DefaultMaxPage, KeepEvery: 2 * time.Second, SpeakerBuffer: func() time.Duration { return time.Second }}
}

func (p *Pager) emit(id, level, text string) {
	log.Printf("paging: %s", text)
	if p.OnEvent != nil {
		p.OnEvent(id, level, text)
	}
}

// PTT is nodes.Manager.OnPTT.
func (p *Pager) PTT(id string, down bool) {
	if down {
		p.Start(id)
	} else {
		p.Release(id)
	}
}

// Start begins a page from source id, if it is the paging mic.
func (p *Pager) Start(id string) {
	info, ok := p.Src.PageInfo(id)
	if !ok || !info.Paging || info.Stream == "" {
		return
	}
	p.mu.Lock()
	if c := p.cur; c != nil {
		if c.src == id && c.ending && c.endTimer.Stop() {
			c.ending = false // pressed again while the last page played out
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		if c.src != id {
			p.emit(id, "warn", fmt.Sprintf("%s pressed talk during a page from %s; ignored", info.Name, c.name))
		}
		return
	}
	pg := &page{
		src: id, name: info.Name, stream: info.Stream, since: time.Now(), stop: make(chan struct{}),
		streams: map[string]string{}, mutedGrp: map[string]bool{}, mutedCl: map[string]int{},
	}
	p.cur = pg
	p.mu.Unlock()

	p.apply(pg)
	p.Src.Overlay(id, audio.PageChime())
	p.emit(id, "ops", fmt.Sprintf("PAGE from %s on every speaker", info.Name))
	go p.keep(pg)
}

// Release ends the page from id once its last audio has played.
func (p *Pager) Release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.cur
	if c == nil || c.src != id || c.ending {
		return
	}
	tail := p.SpeakerBuffer() + tailMargin
	if info, ok := p.Src.PageInfo(id); ok {
		tail += time.Duration(info.BufferMs) * time.Millisecond
	}
	c.ending = true
	c.endTimer = time.AfterFunc(tail, func() { p.finish(c, "") })
}

// End stops the current page now (console).
func (p *Pager) End() {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if c != nil {
		p.finish(c, "ended from the console")
	}
}

// SourceGone is called when a source disconnects: its page ends now.
func (p *Pager) SourceGone(id string) {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if c != nil && c.src == id {
		p.finish(c, "the paging mic disconnected")
	}
}

func (p *Pager) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil {
		return State{}
	}
	return State{Active: true, Source: p.cur.src, Name: p.cur.name, Since: p.cur.since, Ending: p.cur.ending}
}

// apply puts every group on the paging stream, unmuted, remembering how it
// was. Safe to repeat: only the first sighting of a group is remembered.
func (p *Pager) apply(pg *page) {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.done {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	st, err := p.SC.GetStatus(ctx)
	if err != nil {
		p.applyFailed(pg, err)
		return
	}
	for _, g := range st.Server.Groups {
		if g.StreamID != pg.stream {
			if _, seen := pg.streams[g.ID]; !seen {
				pg.streams[g.ID] = g.StreamID
			}
			if err := p.SC.SetGroupStream(ctx, g.ID, pg.stream); err != nil {
				p.applyFailed(pg, err)
			}
		}
		if g.Muted {
			pg.mutedGrp[g.ID] = true
			p.SC.SetGroupMute(ctx, g.ID, false)
		}
		for _, c := range g.Clients {
			if c.Config.Volume.Muted {
				pg.mutedCl[c.ID] = c.Config.Volume.Percent
				p.SC.SetVolume(ctx, c.ID, c.Config.Volume.Percent, false)
			}
		}
	}
}

func (p *Pager) applyFailed(pg *page, err error) {
	pg.applyErrs++
	if pg.applyErrs == 1 {
		p.emit(pg.src, "bad", "page could not reach every speaker: "+err.Error())
	}
}

func (p *Pager) keep(pg *page) {
	t := time.NewTicker(p.KeepEvery)
	defer t.Stop()
	for {
		select {
		case <-pg.stop:
			return
		case <-t.C:
		}
		if time.Since(pg.since) > p.MaxPage {
			p.finish(pg, fmt.Sprintf("talk held over %s; is the switch stuck?", p.MaxPage))
			return
		}
		p.apply(pg)
	}
}

// finish restores what the page changed. why is empty for a normal release.
func (p *Pager) finish(pg *page, why string) {
	p.mu.Lock()
	if p.cur != pg {
		p.mu.Unlock()
		return
	}
	p.cur = nil
	if pg.endTimer != nil {
		pg.endTimer.Stop()
	}
	p.mu.Unlock()
	close(pg.stop)

	pg.mu.Lock()
	pg.done = true
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	for g, s := range pg.streams {
		if err := p.SC.SetGroupStream(ctx, g, s); err != nil {
			log.Printf("paging: restore group %s to %s: %v", g, s, err)
		}
	}
	for g := range pg.mutedGrp {
		p.SC.SetGroupMute(ctx, g, true)
	}
	for c, vol := range pg.mutedCl {
		p.SC.SetVolume(ctx, c, vol, true)
	}
	cancel()
	pg.mu.Unlock()

	text := fmt.Sprintf("page from %s ended after %s", pg.name, time.Since(pg.since).Round(time.Second))
	level := "ops"
	if why != "" {
		text += ": " + why
		level = "warn"
	}
	p.emit(pg.src, level, text)
}
