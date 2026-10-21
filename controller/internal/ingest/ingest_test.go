package ingest

import (
	"context"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSnap is a snapserver TCP source that counts bytes and remembers the
// loudest sample it saw.
type fakeSnap struct {
	ln    net.Listener
	bytes atomic.Int64
	peak  atomic.Int32
}

func newFakeSnap(t *testing.T) *fakeSnap {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSnap{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					for i := 0; i+1 < n; i += 2 {
						v := int32(int16(binary.LittleEndian.Uint16(buf[i:])))
						if v < 0 {
							v = -v
						}
						if v > f.peak.Load() {
							f.peak.Store(v)
						}
					}
					f.bytes.Add(int64(n))
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return f
}

func setup(t *testing.T) (*Ingest, *fakeSnap) {
	snap := newFakeSnap(t)
	in := New("src-1", snap.ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go in.Run(ctx)
	return in, snap
}

// tone is 20 ms of mono s16le at the given amplitude.
func tone(amp int16) []byte {
	b := make([]byte, 0, 960*2)
	for i := 0; i < 960; i++ {
		v := amp
		if i%2 == 1 {
			v = -amp
		}
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	return b
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestIdleWritesNothing(t *testing.T) {
	in, snap := setup(t)
	in.Push(tone(1000)) // not active: dropped
	time.Sleep(200 * time.Millisecond)
	if n := snap.bytes.Load(); n != 0 {
		t.Fatalf("wrote %d bytes with no source", n)
	}
}

func TestAudioReachesSnapserverAtRealTime(t *testing.T) {
	in, snap := setup(t)
	in.SetActive(true)
	time.Sleep(100 * time.Millisecond) // pacer running: its start trim is past
	for i := 0; i < 60; i++ {          // 1.2 s in one burst, as Wi-Fi might deliver
		in.Push(tone(12000))
	}
	waitFor(t, "tone at snapserver", func() bool { return snap.peak.Load() == 12000 })

	// The pacer must not dump the burst: ~300 ms of stereo is 57.6 kB.
	time.Sleep(200 * time.Millisecond) // past the initial lead
	start := snap.bytes.Load()
	time.Sleep(300 * time.Millisecond)
	if got := snap.bytes.Load() - start; got < 40_000 || got > 75_000 {
		t.Fatalf("wrote %d bytes in 300 ms, want ~57600", got)
	}
	if st := in.Stats(); !st.Active || st.BufferMs <= 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestStarvedSourceCountsUnderrun(t *testing.T) {
	in, _ := setup(t)
	in.SetActive(true)
	for i := 0; i < 30; i++ { // 600 ms: primes, then runs dry
		in.Push(tone(1000))
	}
	waitFor(t, "underrun", func() bool { return in.Stats().Underruns > 0 })
}

func TestInactiveStopsWriting(t *testing.T) {
	in, snap := setup(t)
	in.SetActive(true)
	for i := 0; i < 30; i++ {
		in.Push(tone(1000))
	}
	waitFor(t, "audio", func() bool { return snap.bytes.Load() > 0 })
	in.SetActive(false)
	time.Sleep(100 * time.Millisecond)
	n := snap.bytes.Load()
	time.Sleep(200 * time.Millisecond)
	if snap.bytes.Load() != n {
		t.Fatal("still writing after the source left")
	}
}

func TestLongBufferIsCutBack(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 6 s")
	}
	in, _ := setup(t)
	in.SetActive(true)
	time.Sleep(300 * time.Millisecond) // pacer running, so the start trim is past
	// 1.5 s at once, then real time: the queue sits ~1 s, above stuckOver.
	for i := 0; i < 75; i++ {
		in.Push(tone(1000))
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				in.Push(tone(1000))
			}
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if st := in.Stats(); st.Overruns > 0 && st.BufferMs < 700 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("buffer never cut back: %+v", in.Stats())
}

func TestAudioQueuedBeforeSnapserverIsTrimmed(t *testing.T) {
	// Snapserver's port isn't open yet: Run keeps retrying while audio queues.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	in := New("src-1", addr)
	in.SetActive(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go in.Run(ctx)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				in.Push(tone(1000))
			}
		}
	}()
	time.Sleep(1200 * time.Millisecond) // ~1.2 s queued, Run has failed once

	// The port opens; the first pacing tick trims to prefill.
	snap := &fakeSnap{}
	l2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skip("port reused:", err)
	}
	snap.ln = l2
	defer l2.Close()
	go func() {
		c, err := l2.Accept()
		if err == nil {
			buf := make([]byte, 4096)
			for {
				if _, err := c.Read(buf); err != nil {
					return
				}
			}
		}
	}()
	waitFor(t, "trimmed buffer", func() bool { b := in.Stats().BufferMs; return b > 0 && b <= 560 })
}

func TestDefaultLimitsMatchTheOriginalTuning(t *testing.T) {
	l := limitsFor(DefaultBufferMs)
	if l.prefill != 24000 || l.high != 38400 || l.low != 12000 || l.stuck != 43200 || l.maxQueue != 96000 || l.lead != 100*time.Millisecond {
		t.Fatalf("%+v", l)
	}
	if l := limitsFor(5); l.targetMs != MinBufferMs {
		t.Fatalf("not clamped: %+v", l)
	}
}

func TestSmallerBufferSettles(t *testing.T) {
	in, _ := setup(t)
	in.SetBuffer(150)
	in.SetActive(true)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				in.Push(tone(1000))
			}
		}
	}()
	time.Sleep(1500 * time.Millisecond)
	st := in.Stats()
	if st.TargetMs != 150 || st.BufferMs < 80 || st.BufferMs > 220 {
		t.Fatalf("stats %+v, want ~150 ms queued", st)
	}
}

func TestOverlayReplacesSourceAudio(t *testing.T) {
	in := New("src-test", "")
	in.SetActive(true)
	pcm := make([]byte, 0, 2*30000)
	for range 30000 {
		pcm = binary.LittleEndian.AppendUint16(pcm, 100)
	}
	in.Push(pcm)
	chime := make([]int16, 1000)
	for i := range chime {
		chime[i] = 7
	}
	in.Overlay(chime)
	first, second := in.take(960, 1), in.take(960, 2)
	for i, v := range first {
		if v != 7 {
			t.Fatalf("first[%d] = %d, want the overlay", i, v)
		}
	}
	if second[39] != 7 || second[40] != 100 {
		t.Fatalf("second block %d,%d: want overlay through sample 39, source after", second[39], second[40])
	}
	// The source's audio drained underneath: two blocks gone, not zero.
	if got, want := in.Stats().BufferMs, (30000-2*960)*1000/48000; got != want {
		t.Fatalf("buffer %d ms, want %d", got, want)
	}
}
