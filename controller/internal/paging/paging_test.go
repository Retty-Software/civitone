package paging

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/snap"
	"github.com/Retty-Software/civitone/controller/internal/sources"
)

type fakeSnap struct {
	mu     sync.Mutex
	groups []snap.Group
}

func (f *fakeSnap) GetStatus(ctx context.Context) (*snap.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := &snap.Status{}
	st.Server.Groups = append([]snap.Group(nil), f.groups...)
	for i := range st.Server.Groups {
		st.Server.Groups[i].Clients = append([]snap.Client(nil), f.groups[i].Clients...)
	}
	return st, nil
}

func (f *fakeSnap) group(id string) *snap.Group {
	for i := range f.groups {
		if f.groups[i].ID == id {
			return &f.groups[i]
		}
	}
	return nil
}

func (f *fakeSnap) SetGroupStream(ctx context.Context, id, stream string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.group(id).StreamID = stream
	return nil
}

func (f *fakeSnap) SetGroupMute(ctx context.Context, id string, muted bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.group(id).Muted = muted
	return nil
}

func (f *fakeSnap) SetVolume(ctx context.Context, id string, pct int, muted bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for gi := range f.groups {
		for ci := range f.groups[gi].Clients {
			if c := &f.groups[gi].Clients[ci]; c.ID == id {
				c.Config.Volume.Percent, c.Config.Volume.Muted = pct, muted
			}
		}
	}
	return nil
}

func (f *fakeSnap) streams() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]string{}
	for _, g := range f.groups {
		m[g.ID] = g.StreamID
	}
	return m
}

type fakeSources struct {
	mu       sync.Mutex
	pages    map[string]sources.Page
	overlays int
}

func (f *fakeSources) PageInfo(id string) (sources.Page, bool) {
	p, ok := f.pages[id]
	return p, ok
}

func (f *fakeSources) Overlay(id string, pcm []int16) {
	f.mu.Lock()
	f.overlays++
	f.mu.Unlock()
}

func setup() (*Pager, *fakeSnap, *fakeSources) {
	sc := &fakeSnap{groups: []snap.Group{
		{ID: "g1", StreamID: "parade"},
		{ID: "g2", StreamID: "None", Muted: true},
		{ID: "g3", StreamID: "music", Clients: []snap.Client{{ID: "c3"}}},
	}}
	sc.groups[2].Clients[0].Config.Volume.Percent, sc.groups[2].Clients[0].Config.Volume.Muted = 40, true
	src := &fakeSources{pages: map[string]sources.Page{
		"src-page":   {Name: "Ops mic", Stream: "src-page", Paging: true, BufferMs: 60},
		"src-parade": {Name: "Parade", Stream: "parade", Paging: false},
	}}
	p := New(sc, src)
	p.SpeakerBuffer = func() time.Duration { return 50 * time.Millisecond }
	p.KeepEvery = 20 * time.Millisecond
	return p, sc, src
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPageTakesEverySpeakerAndRestores(t *testing.T) {
	p, sc, src := setup()
	p.PTT("src-page", true)
	for g, s := range sc.streams() {
		if s != "src-page" {
			t.Fatalf("group %s on %q during the page", g, s)
		}
	}
	if sc.group("g2").Muted || sc.groups[2].Clients[0].Config.Volume.Muted {
		t.Fatal("muted group or speaker left muted during the page")
	}
	if src.overlays != 1 || !p.State().Active {
		t.Fatalf("overlays %d, state %+v: want the chime and an active page", src.overlays, p.State())
	}

	p.PTT("src-page", false)
	if !p.State().Ending {
		t.Fatal("release should play out the buffers before restoring")
	}
	eventually(t, "restore", func() bool { return !p.State().Active })
	want := map[string]string{"g1": "parade", "g2": "None", "g3": "music"}
	for g, s := range sc.streams() {
		if s != want[g] {
			t.Fatalf("group %s restored to %q, want %q", g, s, want[g])
		}
	}
	if !sc.group("g2").Muted || !sc.groups[2].Clients[0].Config.Volume.Muted || sc.groups[2].Clients[0].Config.Volume.Percent != 40 {
		t.Fatal("mutes not restored")
	}
}

func TestPressDuringTailKeepsThePage(t *testing.T) {
	p, sc, _ := setup()
	p.PTT("src-page", true)
	p.PTT("src-page", false)
	p.PTT("src-page", true)
	time.Sleep(800 * time.Millisecond) // well past the tail
	if st := p.State(); !st.Active || st.Ending {
		t.Fatalf("state %+v: a press during the tail should keep paging", st)
	}
	if sc.streams()["g1"] != "src-page" {
		t.Fatal("speakers left the page")
	}
}

func TestOnlyThePagingMicPages(t *testing.T) {
	p, sc, _ := setup()
	p.PTT("src-parade", true)
	if p.State().Active || sc.streams()["g3"] != "music" {
		t.Fatal("a source not marked as the paging mic started a page")
	}
}

func TestDisconnectEndsThePage(t *testing.T) {
	p, sc, _ := setup()
	p.PTT("src-page", true)
	p.SourceGone("src-page")
	if p.State().Active || sc.streams()["g1"] != "parade" {
		t.Fatal("page outlived its mic")
	}
}

func TestStuckSwitchTimesOut(t *testing.T) {
	p, sc, _ := setup()
	p.MaxPage = 100 * time.Millisecond
	p.PTT("src-page", true)
	eventually(t, "the stuck page to end", func() bool { return !p.State().Active })
	if sc.streams()["g3"] != "music" {
		t.Fatal("not restored after the timeout")
	}
}

func TestSpeakerJoiningMidPageIsPaged(t *testing.T) {
	p, sc, _ := setup()
	p.PTT("src-page", true)
	sc.mu.Lock()
	sc.groups = append(sc.groups, snap.Group{ID: "g4", StreamID: "music"})
	sc.mu.Unlock()
	eventually(t, "the new group to be paged", func() bool { return sc.streams()["g4"] == "src-page" })
	p.End()
	if sc.streams()["g4"] != "music" {
		t.Fatal("late group not restored")
	}
}
