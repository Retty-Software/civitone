package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestEventsNewestFirstWithLimit(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	for _, txt := range []string{"one", "two", "three"} {
		if err := r.Log(ctx, "civ-1a2b", "info", txt); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := r.Events(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 2 || ev[0].Text != "three" || ev[1].Text != "two" {
		t.Fatalf("got %+v", ev)
	}
}

func TestAdoptRenameForget(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if _, ok, err := r.Node(ctx, "civ-1a2b"); ok || err != nil {
		t.Fatalf("before adopt: ok=%v err=%v", ok, err)
	}
	if err := r.Adopt(ctx, "civ-1a2b", "node", "Main", "hash1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Adopt(ctx, "civ-1a2b", "node", "Main", "hash2"); err != nil { // re-adopt replaces
		t.Fatal(err)
	}
	if err := r.Rename(ctx, "civ-1a2b", "Main & 2nd"); err != nil {
		t.Fatal(err)
	}
	n, ok, err := r.Node(ctx, "civ-1a2b")
	if !ok || err != nil || n.Token != "hash2" || n.Name != "Main & 2nd" || n.AdoptedAt.IsZero() {
		t.Fatalf("node = %+v ok=%v err=%v", n, ok, err)
	}
	if err := r.Forget(ctx, "civ-1a2b"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.Node(ctx, "civ-1a2b"); ok {
		t.Fatal("still there after forget")
	}
}

func TestAdoptCodes(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	now := time.Now()
	r.CreateCode(ctx, AdoptCode{Code: "ABC234", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Auto: true})
	r.CreateCode(ctx, AdoptCode{Code: "OLD999", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)})

	if ok, auto, err := r.UseCode(ctx, "ABC234"); !ok || !auto || err != nil {
		t.Fatalf("valid code: ok=%v auto=%v err=%v", ok, auto, err)
	}
	r.UseCode(ctx, "ABC234") // reusable within the hour
	if ok, _, _ := r.UseCode(ctx, "OLD999"); ok {
		t.Fatal("expired code accepted")
	}
	if ok, _, _ := r.UseCode(ctx, "NOPE22"); ok {
		t.Fatal("unknown code accepted")
	}
	codes, err := r.ActiveCodes(ctx)
	if err != nil || len(codes) != 1 || codes[0].Uses != 2 {
		t.Fatalf("active = %+v err=%v", codes, err)
	}
	r.RevokeCode(ctx, "ABC234")
	if ok, _, _ := r.UseCode(ctx, "ABC234"); ok {
		t.Fatal("revoked code accepted")
	}
}

func TestSourcesGetStreamsAndPorts(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	r.Adopt(ctx, "civ-1a2b", "node", "Main", "h")
	r.Adopt(ctx, "src-a", "source", "RadioDJ", "h")
	r.Adopt(ctx, "src-b", "source", "TV Studio", "h")

	p, _ := r.FreePort(ctx, 4960)
	r.SetStream(ctx, "src-a", "src-a", p)
	p2, _ := r.FreePort(ctx, 4960)
	r.SetStream(ctx, "src-b", "src-b", p2)
	if p != 4960 || p2 != 4961 {
		t.Fatalf("ports %d %d", p, p2)
	}
	r.Forget(ctx, "src-a")
	if p, _ := r.FreePort(ctx, 4960); p != 4960 {
		t.Fatalf("freed port not reused: %d", p)
	}
	src, err := r.ByRole(ctx, "source")
	if err != nil || len(src) != 1 || src[0].Name != "TV Studio" || src[0].Port != 4961 || src[0].Role != "source" {
		t.Fatalf("sources = %+v err=%v", src, err)
	}
}

func TestMigrationIsRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	for i := 0; i < 2; i++ {
		r, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		r.Close()
	}
}

func TestSettingsAndSourceBuffer(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if v := r.Setting(ctx, "speaker_buffer_ms", "2000"); v != "2000" {
		t.Fatalf("default = %q", v)
	}
	r.SetSetting(ctx, "speaker_buffer_ms", "900")
	r.SetSetting(ctx, "speaker_buffer_ms", "800")
	if v := r.Setting(ctx, "speaker_buffer_ms", "2000"); v != "800" {
		t.Fatalf("got %q", v)
	}
	r.Adopt(ctx, "src-a", "source", "RadioDJ", "h")
	r.SetBuffer(ctx, "src-a", 250)
	if n, _, _ := r.Node(ctx, "src-a"); n.BufferMs != 250 {
		t.Fatalf("buffer = %d", n.BufferMs)
	}
}

func TestSpeakersDefaultAndSaved(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	r.Adopt(ctx, "civ-1a2b", "node", "Main", "h")
	if n, _, _ := r.Node(ctx, "civ-1a2b"); n.Speakers != (Speakers{}) {
		t.Fatalf("new node speakers = %+v, want both at 0 dB", n.Speakers)
	}
	want := Speakers{A: Speaker{TrimDB: -6}, B: Speaker{Muted: true}}
	if err := r.SetSpeakers(ctx, "civ-1a2b", want); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := r.Node(ctx, "civ-1a2b"); n.Speakers != want {
		t.Fatalf("speakers = %+v, want %+v", n.Speakers, want)
	}
}
