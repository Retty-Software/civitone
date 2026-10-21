package audio

import (
	"testing"
	"time"
)

func TestMeterSilenceIsNotSignal(t *testing.T) {
	m := NewMeter()
	base := time.Now()
	m.now = func() time.Time { return base }
	m.Observe(make([]int16, 4800))
	l := m.Level()
	if !l.Capturing || l.Signal || l.PeakDBFS != -120 {
		t.Fatalf("silence: %+v", l)
	}
}

func TestMeterSignalHoldsThenDrops(t *testing.T) {
	m := NewMeter()
	base := time.Now()
	m.now = func() time.Time { return base }
	m.Observe([]int16{0, 16384, -8000, 0}) // -6 dBFS
	if l := m.Level(); !l.Signal || l.PeakDBFS != -6 {
		t.Fatalf("tone: %+v", l)
	}

	// Silence keeps arriving; signal holds for signalHold, then clears.
	m.now = func() time.Time { return base.Add(2 * time.Second) }
	m.Observe(make([]int16, 4800))
	if l := m.Level(); !l.Signal {
		t.Fatalf("within hold: %+v", l)
	}
	m.now = func() time.Time { return base.Add(4 * time.Second) }
	m.Observe(make([]int16, 4800))
	if l := m.Level(); l.Signal {
		t.Fatalf("after hold: %+v", l)
	}
}

func TestMeterNoiseBelowFloor(t *testing.T) {
	m := NewMeter()
	m.Observe([]int16{20, -25, 30}) // about -61 dBFS
	if l := m.Level(); l.Signal {
		t.Fatalf("dither counted as signal: %+v", l)
	}
}

func TestMeterStaleCapture(t *testing.T) {
	m := NewMeter()
	base := time.Now()
	m.now = func() time.Time { return base }
	m.Observe([]int16{16384})
	m.now = func() time.Time { return base.Add(5 * time.Second) }
	if l := m.Level(); l.Capturing || l.Signal {
		t.Fatalf("stale: %+v", l)
	}
}

func TestChimeIsAudibleAndUnclipped(t *testing.T) {
	c := Chime()
	if d := ChimeDuration(); d < 2*time.Second || d > 5*time.Second {
		t.Fatalf("duration %v", d)
	}
	m := NewMeter()
	m.Observe(c)
	if l := m.Level(); !l.Signal || l.PeakDBFS > -5 || l.PeakDBFS < -8 {
		t.Fatalf("level %+v", l)
	}
}

func TestTakePeakResetsAndLeavesLevelAlone(t *testing.T) {
	m := NewMeter()
	base := time.Now()
	m.now = func() time.Time { return base }
	m.Observe([]int16{0, 16384, 0}) // -6 dBFS
	if p := m.TakePeak(); p != -6 {
		t.Fatalf("first take = %v, want -6", p)
	}
	m.Observe([]int16{0, 1024, 0}) // -30 dBFS
	if p := m.TakePeak(); p != -30.1 {
		t.Fatalf("second take = %v, want -30.1 (only what came since)", p)
	}
	if p := m.TakePeak(); p != -120 {
		t.Fatalf("nothing new = %v, want -120", p)
	}
	if l := m.Level(); l.PeakDBFS != -6 {
		t.Fatalf("Level peak = %v, want the window's -6", l.PeakDBFS)
	}
	m.now = func() time.Time { return base.Add(3 * time.Second) }
	if p := m.TakePeak(); p != -120 {
		t.Fatalf("no audio = %v, want -120", p)
	}
}
