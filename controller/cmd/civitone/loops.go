package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/access"
	"github.com/Retty-Software/civitone/controller/internal/snap"
	"github.com/Retty-Software/civitone/controller/internal/sources"
)

// countUsage adds up playing time per day, in the site's time zone: how
// long anything played, and pole-seconds (each online pole while its zone
// played), which is what bandwidth follows.
func (s *site) countUsage(ctx context.Context, sc *snap.Link, names namer) {
	const every = 10 * time.Second
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := sc.GetStatus(cctx)
		cancel()
		if err != nil {
			continue
		}
		n := playingPoles(st, names)
		if n == 0 {
			continue
		}
		day := s.now().In(s.Loc()).Format("2006-01-02")
		secs := int64(every / time.Second)
		if err := s.reg.AddUsage(ctx, day, secs, int64(n)*secs); err != nil {
			log.Printf("usage: %v", err)
		}
	}
}

// streamOf maps a source device id to the snapserver stream it feeds.
func streamOf(src *sources.Manager) func(string) string {
	return func(id string) string {
		for _, s := range src.Infos() {
			if s.DeviceID == id {
				return s.Stream
			}
		}
		return ""
	}
}

// closeEvents watches events open and close. When one closes, every zone
// still playing one of its sources goes back to the default source, unless
// another open event uses that source too. Nothing goes silent.
func (s *site) closeEvents(ctx context.Context, sc *snap.Link, src *sources.Manager, names namer, changed func()) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	prev := s.Doc().OpenEvents(s.now(), s.Loc())
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		doc := s.Doc()
		cur := doc.OpenEvents(s.now(), s.Loc())
		var closed []access.Event
		for id := range prev {
			if !cur[id] {
				if e, ok := doc.Event(id); ok {
					closed = append(closed, e)
				}
			}
		}
		opened := false
		for id := range cur {
			if !prev[id] {
				opened = true
				if e, ok := doc.Event(id); ok {
					_ = s.reg.Log(ctx, "console", "ops", fmt.Sprintf("%s is open", e.Name))
				}
			}
		}
		prev = cur
		if opened || len(closed) > 0 {
			changed()
		}
		if len(closed) == 0 {
			continue
		}
		s.revert(ctx, doc, cur, closed, sc, streamOf(src), names)
	}
}

func (s *site) revert(ctx context.Context, doc access.Doc, open map[string]bool, closed []access.Event, sc *snap.Link, streamOf func(string) string, names namer) {
	keep := map[string]bool{}
	for id := range open {
		if e, ok := doc.Event(id); ok {
			for _, src := range e.Sources {
				keep[streamOf(src)], keep[src] = true, true
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := sc.GetStatus(cctx)
	if err != nil {
		log.Printf("events: can't revert zones: %v", err)
		return
	}
	for _, e := range closed {
		streams := map[string]bool{}
		for _, src := range e.Sources {
			if id := streamOf(src); id != "" && !keep[id] {
				streams[id] = true
			}
			if !keep[src] {
				streams[src] = true
			}
		}
		for _, g := range st.Server.Groups {
			if !streams[g.StreamID] {
				continue
			}
			_, zone := names.group(g)
			if doc.DefaultSource == "" || doc.DefaultSource == g.StreamID {
				_ = s.reg.Log(ctx, "console", "warn", fmt.Sprintf("%s closed: %s still plays %s (no default source set)", e.Name, zone, names.stream(g.StreamID)))
				continue
			}
			if err := sc.SetGroupStream(cctx, g.ID, doc.DefaultSource); err != nil {
				_ = s.reg.Log(ctx, "console", "warn", fmt.Sprintf("%s closed: couldn't move %s back to %s: %v", e.Name, zone, names.stream(doc.DefaultSource), err))
				continue
			}
			_ = s.reg.Log(ctx, "console", "ops", fmt.Sprintf("%s closed: %s back to %s", e.Name, zone, names.stream(doc.DefaultSource)))
		}
		_ = s.reg.Log(ctx, "console", "ops", e.Name+" closed")
	}
}
