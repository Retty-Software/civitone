package audio

import (
	"math"
	"sync/atomic"
)

// Shaper is the per-source DSP between capture and snapserver: optional mono
// downmix (pole nodes drive one speaker per channel, so both must carry the
// whole mix), makeup gain, and a peak limiter so the gain never clips.
//
// Input and output are interleaved 16-bit stereo at 48 kHz.
type Shaper struct {
	Mono    bool
	gain    atomic.Uint64 // linear makeup gain, as float64 bits; SetGain may run concurrently with Process
	ceiling float64       // linear output ceiling, full scale = 1
	release float64       // per-sample release coefficient
	env     float64       // current limiter gain, 1 = no reduction
}

// NewShaper builds a shaper. gainDB is makeup gain; ceilingDB is the limiter
// ceiling in dBFS (for example -1).
func NewShaper(mono bool, gainDB, ceilingDB float64) *Shaper {
	const releaseSec = 0.15
	s := &Shaper{
		Mono:    mono,
		ceiling: math.Pow(10, ceilingDB/20),
		release: math.Exp(-1 / (releaseSec * 48000)),
		env:     1,
	}
	s.SetGain(gainDB)
	return s
}

// SetGain changes the makeup gain, live.
func (s *Shaper) SetGain(gainDB float64) { s.gain.Store(math.Float64bits(math.Pow(10, gainDB/20))) }

// Process shapes one buffer of interleaved stereo samples in place.
//
// The limiter has instant attack and no lookahead: when a frame would exceed
// the ceiling the gain drops to exactly the ceiling on that frame, then
// recovers with a 150 ms release. That guarantees no overs at the cost of a
// little distortion on hard transients, which is the right trade for PA use.
func (s *Shaper) Process(buf []int16) {
	gain := math.Float64frombits(s.gain.Load())
	for i := 0; i+1 < len(buf); i += 2 {
		l := float64(buf[i]) / 32768
		r := float64(buf[i+1]) / 32768
		if s.Mono {
			m := (l + r) / 2
			l, r = m, m
		}
		l *= gain
		r *= gain

		peak := math.Max(math.Abs(l), math.Abs(r))
		target := 1.0
		if peak*s.env > s.ceiling {
			target = s.ceiling / peak
		}
		if target < s.env {
			s.env = target
		} else {
			s.env = target + (s.env-target)*s.release
			if peak*s.env > s.ceiling {
				s.env = s.ceiling / peak
			}
		}

		buf[i] = toInt16(l * s.env)
		buf[i+1] = toInt16(r * s.env)
	}
}

func toInt16(v float64) int16 {
	v = math.Round(v * 32768)
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}
