package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/nodes"
	"github.com/Retty-Software/civitone/controller/internal/registry"
)

// A speaker node drives two speakers, A on the left output and B on the
// right, each with a trim below the zone volume and a mute. The registry is
// the record; the node keeps a copy in NVS and applies it in its DAC. It's
// sent on every connect and on every change.
const minTrimDB = -24

var errNoSpeakerFW = errors.New("this node's firmware has no speaker controls yet (needs 0.3.0)")

type speakers struct {
	reg   *registry.Registry
	nm    *nodes.Manager
	chime *chimer

	mu    sync.Mutex
	cache map[string]registry.Speakers // device id -> saved settings, for the console
	chirp sync.Mutex                   // one chirp at a time: each restores what the last changed
}

func (s *speakers) load(ctx context.Context) error {
	ns, err := s.reg.ByRole(ctx, "node")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = map[string]registry.Speakers{}
	for _, n := range ns {
		s.cache[n.DeviceID] = n.Speakers
	}
	return nil
}

// all is a copy for the console push.
func (s *speakers) all() map[string]registry.Speakers {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]registry.Speakers, len(s.cache))
	for k, v := range s.cache {
		out[k] = v
	}
	return out
}

func (s *speakers) get(id string) registry.Speakers {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[id]
}

// speakerPatch is a change to either speaker, either field.
type speakerPatch struct {
	A *speakerFields `json:"a"`
	B *speakerFields `json:"b"`
}

type speakerFields struct {
	TrimDB *float64 `json:"trim_db"`
	Muted  *bool    `json:"muted"`
}

func (f *speakerFields) apply(sp *registry.Speaker) error {
	if f == nil {
		return nil
	}
	if f.TrimDB != nil {
		t := math.Round(*f.TrimDB*2) / 2 // the DAC steps in 0.5 dB
		if t > 0 || t < minTrimDB {
			return fmt.Errorf("trim_db must be %d..0", minTrimDB)
		}
		sp.TrimDB = t
	}
	if f.Muted != nil {
		sp.Muted = *f.Muted
	}
	return nil
}

// set saves a change and sends it to the node. err means nothing was saved
// (bad input, unknown node). notApplied means it's saved but the node didn't
// take it: offline (it gets it on reconnect) or older firmware.
func (s *speakers) set(ctx context.Context, id string, p speakerPatch) (sp registry.Speakers, err, notApplied error) {
	n, ok, err := s.reg.Node(ctx, id)
	if err != nil {
		return sp, err, nil
	}
	if !ok || n.Role == nodes.RoleSource {
		return sp, fmt.Errorf("%s is not an adopted speaker node", id), nil
	}
	sp = n.Speakers
	if err := p.A.apply(&sp.A); err != nil {
		return sp, err, nil
	}
	if err := p.B.apply(&sp.B); err != nil {
		return sp, err, nil
	}
	if err := s.reg.SetSpeakers(ctx, id, sp); err != nil {
		return sp, err, nil
	}
	s.mu.Lock()
	s.cache[id] = sp
	s.mu.Unlock()
	return sp, nil, s.send(ctx, id, sp, true)
}

func (s *speakers) send(ctx context.Context, id string, sp registry.Speakers, save bool) error {
	args := map[string]any{"a": sp.A, "b": sp.B}
	if !save {
		args["save"] = false
	}
	err := s.nm.Send(ctx, id, "set_speakers", args)
	if err != nil && strings.Contains(err.Error(), "unknown cmd") {
		return errNoSpeakerFW
	}
	return err
}

// connected brings a node that just connected in line with the registry.
// OnConnect runs before the node is welcomed, so this waits a moment.
func (s *speakers) connected(st nodes.State) {
	if !st.Adopted || st.Role == nodes.RoleSource {
		return
	}
	go func() {
		time.Sleep(time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// From the registry, not the cache: a node forgotten and adopted
		// again starts over at 0 dB.
		n, ok, err := s.reg.Node(ctx, st.DeviceID)
		if err != nil || !ok {
			return
		}
		s.mu.Lock()
		s.cache[st.DeviceID] = n.Speakers
		s.mu.Unlock()
		if err := s.send(ctx, st.DeviceID, n.Speakers, true); err != nil && err != errNoSpeakerFW {
			log.Printf("%s: speakers on connect: %v", st.DeviceID, err)
		}
	}()
}

// chirpOne plays the identify chime on one speaker: the other is muted for
// the chime (not saved on the node), then both go back to their settings.
func (s *speakers) chirpOne(ctx context.Context, id, which, mac string) error {
	s.chirp.Lock()
	defer s.chirp.Unlock()
	saved := s.get(id)
	solo := saved
	switch which {
	case "a":
		solo.A.Muted, solo.B.Muted = false, true
	case "b":
		solo.A.Muted, solo.B.Muted = true, false
	default:
		return fmt.Errorf(`speaker must be "a" or "b"`)
	}
	if err := s.send(ctx, id, solo, false); err != nil {
		return err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.send(rctx, id, saved, false); err != nil {
			log.Printf("%s: chirp: restoring speakers: %v", id, err)
		}
	}()
	return s.chime.chime(ctx, mac)
}

func speakerText(sp registry.Speaker) string {
	if sp.Muted {
		return "muted"
	}
	return fmt.Sprintf("%g dB", sp.TrimDB)
}
