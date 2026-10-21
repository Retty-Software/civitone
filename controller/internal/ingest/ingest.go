// Package ingest turns a source's audio into the steady stream snapserver
// expects. Sources (source boards, Civitone Source on a computer) push mono
// s16le PCM at 48 kHz over the control channel; Wi-Fi and the internet
// deliver it in bursts, so a jitter buffer re-times it into real time on the
// source's snapserver TCP port.
package ingest

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/audio"
)

const (
	rate       = 48000
	tick       = 20 * time.Millisecond
	driftEvery = 10 // ticks between drift corrections (~100 ppm)
	stuckFor   = 5 * time.Second
	eventGap   = 30 * time.Second

	// DefaultBufferMs suits a source on the far side of the internet
	// (laptop to cloud): 200 ms underran a few times a minute on Oct 12,
	// 2026. Operators tune it per source in the console.
	DefaultBufferMs = 500
	MinBufferMs     = 60
	MaxBufferMs     = 2000
)

// limits are the buffer thresholds for one target, in mono samples.
type limits struct {
	targetMs int
	prefill  int           // queued before playback starts; trims cut back to this
	high     int           // above: drop a sample now and then (source clock fast)
	low      int           // below: repeat a sample now and then (source clock slow)
	stuck    int           // above for stuckFor (a burst at connect): cut back to prefill
	maxQueue int           // above: the source is far ahead, cut back at once
	lead     time.Duration // written ahead of real time into snapserver's socket
}

func limitsFor(ms int) limits {
	ms = max(MinBufferMs, min(MaxBufferMs, ms))
	p := ms * rate / 1000
	return limits{
		targetMs: ms,
		prefill:  p,
		high:     p + max(p*3/5, rate*60/1000),
		low:      p / 2,
		stuck:    p + max(p*3/5, rate*60/1000) + rate/10,
		maxQueue: max(2*rate, 3*p),
		// Exactly on time, any stall here (GC, a busy shared CPU) made
		// snapserver resync the stream: an audible gap (Oct 13, 2026).
		lead: time.Duration(max(20, min(100, ms/2))) * time.Millisecond,
	}
}

// Stats is what the console shows for one source's buffer.
type Stats struct {
	Active    bool   `json:"active"`
	TargetMs  int    `json:"target_ms"` // the setting
	BufferMs  int    `json:"buffer_ms"` // what's queued now
	Underruns uint64 `json:"underruns"`
	Overruns  uint64 `json:"overruns"`
}

// Ingest owns the jitter buffer and the snapserver connection for one source.
type Ingest struct {
	ID       string        // source device id, for events
	SnapAddr string        // snapserver TCP source
	Shaper   *audio.Shaper // optional
	Meter    *audio.Meter  // optional
	OnEvent  func(deviceID, level, text string)

	mu      sync.Mutex
	queue   []int16 // mono samples waiting to play
	overlay []int16 // plays instead of the queue until used up (page chime)
	active  bool    // the source is connected
	primed  bool
	stats   Stats
	lastEvt map[string]time.Time
	overAt  time.Time // when the queue first went above lim.stuck
	lim     limits
}

func New(id, snapAddr string) *Ingest {
	return &Ingest{ID: id, SnapAddr: snapAddr, lastEvt: map[string]time.Time{}, lim: limitsFor(DefaultBufferMs)}
}

// SetBuffer changes the buffer target while running. Going down drops the
// excess at once (a short skip); going up refills (a short gap).
func (in *Ingest) SetBuffer(ms int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.lim = limitsFor(ms)
	if len(in.queue) > in.lim.prefill {
		in.queue = append(in.queue[:0], in.queue[len(in.queue)-in.lim.prefill:]...)
	} else {
		in.primed = false
	}
}

// emitLimited drops repeats of the same kind of event within eventGap.
func (in *Ingest) emitLimited(kind, level, text string) {
	in.mu.Lock()
	if t, ok := in.lastEvt[kind]; ok && time.Since(t) < eventGap {
		in.mu.Unlock()
		return
	}
	in.lastEvt[kind] = time.Now()
	in.mu.Unlock()
	log.Printf("source %s: %s", in.ID, text)
	if in.OnEvent != nil {
		in.OnEvent(in.ID, level, text)
	}
}

// SetActive marks the source connected or gone. Either way the buffer
// starts over: a reconnect refills it before playing.
func (in *Ingest) SetActive(on bool) {
	in.mu.Lock()
	in.active = on
	in.queue = in.queue[:0]
	in.primed = false
	in.mu.Unlock()
}

// Push queues mono s16le PCM.
func (in *Ingest) Push(pcm []byte) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active {
		return
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		in.queue = append(in.queue, int16(binary.LittleEndian.Uint16(pcm[i:])))
	}
	if len(in.queue) > in.lim.maxQueue {
		in.queue = append(in.queue[:0], in.queue[len(in.queue)-in.lim.prefill:]...)
		in.stats.Overruns++
		go in.emitLimited("overrun", "warn", fmt.Sprintf("source is running ahead; buffer cut back to %d ms", in.lim.targetMs))
	}
}

// Overlay plays mono pcm in place of the source's audio, starting now. The
// source's audio keeps flowing underneath and is discarded meanwhile, so the
// buffer doesn't grow.
func (in *Ingest) Overlay(pcm []int16) {
	in.mu.Lock()
	in.overlay = append([]int16(nil), pcm...)
	in.mu.Unlock()
}

// take returns n mono samples for playback, or nil while the source is not
// connected. ticks drives the occasional drift correction.
func (in *Ingest) take(n int, ticks int) []int16 {
	out := in.takeQueue(n, ticks)
	in.mu.Lock()
	if out != nil && len(in.overlay) > 0 {
		k := copy(out, in.overlay)
		in.overlay = in.overlay[k:]
	}
	in.mu.Unlock()
	return out
}

func (in *Ingest) takeQueue(n int, ticks int) []int16 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active {
		return nil
	}
	out := make([]int16, n)
	if !in.primed {
		if len(in.queue) < in.lim.prefill {
			return out // silence while the buffer fills
		}
		in.primed = true
	}
	if len(in.queue) > in.lim.stuck {
		if in.overAt.IsZero() {
			in.overAt = time.Now()
		} else if time.Since(in.overAt) > stuckFor {
			in.queue = append(in.queue[:0], in.queue[len(in.queue)-in.lim.prefill:]...)
			in.overAt = time.Time{}
			in.stats.Overruns++
			go in.emitLimited("overrun", "warn", fmt.Sprintf("buffer was running long; cut back to %d ms", in.lim.targetMs))
		}
	} else {
		in.overAt = time.Time{}
	}
	want := n
	if ticks%driftEvery == 0 {
		switch {
		case len(in.queue) > in.lim.high:
			want = n + 1 // source clock is fast: consume one extra
		case len(in.queue) < in.lim.low:
			want = n - 1 // source clock is slow: stretch by one
		}
	}
	if len(in.queue) < want {
		copy(out, in.queue)
		in.queue = in.queue[:0]
		in.primed = false
		in.stats.Underruns++
		go in.emitLimited("underrun", "warn", "source audio arrived late; filled with silence")
		return out
	}
	switch want {
	case n + 1:
		copy(out, in.queue[:n/2])
		copy(out[n/2:], in.queue[n/2+1:want])
	case n - 1:
		copy(out, in.queue[:want])
		out[n-1] = out[n-2]
	default:
		copy(out, in.queue[:n])
	}
	in.queue = append(in.queue[:0], in.queue[want:]...)
	return out
}

// trim drops all but the newest prefill of queued audio.
func (in *Ingest) trim() {
	in.mu.Lock()
	if len(in.queue) > in.lim.prefill {
		in.queue = append(in.queue[:0], in.queue[len(in.queue)-in.lim.prefill:]...)
	}
	in.mu.Unlock()
}

// Stats reads the buffer counters.
func (in *Ingest) Stats() Stats {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := in.stats
	s.Active = in.active
	s.TargetMs = in.lim.targetMs
	if in.active {
		s.BufferMs = len(in.queue) * 1000 / rate
	}
	return s
}

// Run paces the buffer into snapserver until ctx is done. It writes only
// while the source is connected, so snapserver reports the stream idle
// otherwise.
func (in *Ingest) Run(ctx context.Context) {
	var last string
	for ctx.Err() == nil {
		if err := in.runOnce(ctx); err != nil && ctx.Err() == nil && err.Error() != last {
			last = err.Error()
			log.Printf("source %s: snapserver %s: %v (retrying every second)", in.ID, in.SnapAddr, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (in *Ingest) runOnce(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", in.SnapAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	t := time.NewTicker(tick)
	defer t.Stop()
	var (
		start   time.Time
		written int64
		ticks   int
		buf     []byte
	)
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			if start.IsZero() {
				in.mu.Lock()
				lead := in.lim.lead
				in.mu.Unlock()
				start, written = now.Add(-lead), 0
				// Audio that queued while snapserver was unreachable (a new
				// stream's port not open yet, a restart) would otherwise
				// play late forever: keep only the newest prefill.
				in.trim()
			}
			// Write what real time says is due, so ticker jitter never drifts.
			due := int(now.Sub(start).Seconds()*rate) - int(written)
			if due <= 0 {
				continue
			}
			ticks++
			mono := in.take(due, ticks)
			if mono == nil {
				start = time.Time{} // idle: restart the clock when the source returns
				continue
			}
			written += int64(len(mono))
			stereo := make([]int16, 2*len(mono))
			for i, v := range mono {
				stereo[2*i], stereo[2*i+1] = v, v
			}
			if in.Shaper != nil {
				in.Shaper.Process(stereo)
			}
			if in.Meter != nil {
				in.Meter.Observe(stereo)
			}
			buf = buf[:0]
			for _, v := range stereo {
				buf = binary.LittleEndian.AppendUint16(buf, uint16(v))
			}
			conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Write(buf); err != nil {
				return err
			}
		}
	}
}
