package audio

import (
	"math"
	"testing"
)

func sine(frames int, amp float64, left, right bool) []int16 {
	buf := make([]int16, frames*2)
	for i := 0; i < frames; i++ {
		v := int16(amp * 32767 * math.Sin(2*math.Pi*440*float64(i)/48000))
		if left {
			buf[2*i] = v
		}
		if right {
			buf[2*i+1] = v
		}
	}
	return buf
}

func peakDB(buf []int16) float64 {
	var p float64
	for _, v := range buf {
		p = math.Max(p, math.Abs(float64(v))/32768)
	}
	return 20 * math.Log10(p)
}

func TestMonoCarriesOneSidedContentToBothChannels(t *testing.T) {
	buf := sine(4800, 0.5, true, false) // left only
	NewShaper(true, 0, 0).Process(buf)
	for i := 0; i < len(buf); i += 2 {
		if buf[i] != buf[i+1] {
			t.Fatalf("frame %d: L=%d R=%d, want equal", i/2, buf[i], buf[i+1])
		}
	}
	// Left-only content at 0.5 lands at 0.25 in both channels (-12 dBFS).
	if got := peakDB(buf); math.Abs(got-(-12)) > 0.2 {
		t.Fatalf("peak %.2f dBFS, want about -12", got)
	}
}

func TestStereoPassesThroughUntouchedBelowCeiling(t *testing.T) {
	in := sine(4800, 0.25, true, false)
	buf := append([]int16(nil), in...)
	NewShaper(false, 0, -1).Process(buf)
	for i := range buf {
		if d := int(buf[i]) - int(in[i]); d > 1 || d < -1 {
			t.Fatalf("sample %d changed %d -> %d", i, in[i], buf[i])
		}
	}
}

func TestLimiterHoldsCeilingUnderHeavyGain(t *testing.T) {
	buf := sine(48000, 0.9, true, true)
	NewShaper(true, 12, -1).Process(buf)
	if got := peakDB(buf); got > -1+0.05 {
		t.Fatalf("peak %.3f dBFS exceeds the -1 dBFS ceiling", got)
	}
	// And it is actually loud, not limited into silence.
	if got := peakDB(buf[len(buf)/2:]); got < -1.5 {
		t.Fatalf("steady-state peak %.2f dBFS, want close to the ceiling", got)
	}
}

func TestLimiterRecoversAfterTransient(t *testing.T) {
	s := NewShaper(false, 0, -1)
	loud := sine(4800, 1.0, true, true)
	s.Process(loud)
	quiet := sine(48000, 0.1, true, true)
	s.Process(quiet)
	// After a second of quiet input the gain is back to unity: -20 dBFS in, -20 out.
	if got := peakDB(quiet[len(quiet)/2:]); math.Abs(got-(-20)) > 0.3 {
		t.Fatalf("after release peak %.2f dBFS, want about -20", got)
	}
}

func TestSetGainAppliesLive(t *testing.T) {
	s := NewShaper(false, 0, -1)
	s.SetGain(12)
	buf := sine(4800, 0.1, true, true)
	s.Process(buf)
	// -20 dBFS in, +12 dB: -8 out, well under the ceiling.
	if got := peakDB(buf); math.Abs(got-(-8)) > 0.3 {
		t.Fatalf("peak %.2f dBFS, want about -8", got)
	}
}
