package audio

import (
	"math"
	"sync"
	"time"
)

// Snapserver marks a TCP source "playing" whenever bytes arrive, and capture
// sends digital silence when nothing is playing, so the console asks the
// meter instead: is there audio above the floor right now?
const (
	signalFloorDBFS = -60.0
	signalHold      = 3 * time.Second // bridges gaps between tracks and quiet passages
	captureStale    = 2 * time.Second
	peakWindow      = time.Second
)

// Level is a meter reading for the console.
type Level struct {
	Capturing bool    `json:"capturing"` // capture is delivering chunks
	Signal    bool    `json:"signal"`    // audio above the floor within signalHold
	PeakDBFS  float64 `json:"peak_dbfs"` // loudest sample in the last second; -120 for silence
}

// Meter tracks the level of one source after the shaper.
type Meter struct {
	mu         sync.Mutex
	now        func() time.Time
	lastChunk  time.Time
	lastSignal time.Time
	peak       int32 // in the current window
	prevPeak   int32 // in the previous window
	windowEnd  time.Time
	quick      int32 // since the last TakePeak
}

func NewMeter() *Meter { return &Meter{now: time.Now} }

// Observe records one chunk of interleaved int16 samples.
func (m *Meter) Observe(buf []int16) {
	var p int32
	for _, s := range buf {
		v := int32(s)
		if v < 0 {
			v = -v
		}
		if v > p {
			p = v
		}
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.After(m.windowEnd) {
		m.prevPeak, m.peak = m.peak, 0
		m.windowEnd = now.Add(peakWindow)
	}
	if p > m.peak {
		m.peak = p
	}
	if p > m.quick {
		m.quick = p
	}
	m.lastChunk = now
	if dbfs(p) > signalFloorDBFS {
		m.lastSignal = now
	}
}

// Level reads the meter.
func (m *Meter) Level() Level {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	l := Level{
		Capturing: !m.lastChunk.IsZero() && now.Sub(m.lastChunk) < captureStale,
		Signal:    !m.lastSignal.IsZero() && now.Sub(m.lastSignal) < signalHold,
		PeakDBFS:  -120,
	}
	if l.Capturing {
		l.PeakDBFS = math.Round(dbfs(max(m.peak, m.prevPeak))*10) / 10
	}
	return l
}

// TakePeak returns the loudest sample since the last call, in dBFS (-120 for
// silence or no audio), and starts over. The console's fast meter reads it
// about ten times a second; Level's one-second window is for everything else.
func (m *Meter) TakePeak() float64 {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.quick
	m.quick = 0
	if m.lastChunk.IsZero() || now.Sub(m.lastChunk) >= captureStale {
		return -120
	}
	return math.Round(dbfs(p)*10) / 10
}

func dbfs(peak int32) float64 {
	if peak <= 0 {
		return -120
	}
	return max(20*math.Log10(float64(peak)/32768), -120)
}
