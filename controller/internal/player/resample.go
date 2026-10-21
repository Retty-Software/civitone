package player

import "github.com/Retty-Software/civitone/controller/internal/audio"

// resampler converts any rate and channel count to 48 kHz stereo by linear
// interpolation. Fine for PA horns; not for mastering.
type resampler struct {
	in, ch int
	step   float64 // input frames per output frame
	pos    float64 // position in the input, relative to prev
	prev   [2]float64
	primed bool
}

func newResampler(rate, channels int) *resampler {
	return &resampler{in: rate, ch: channels, step: float64(rate) / audio.SampleRate}
}

// frame returns one input frame as stereo.
func (r *resampler) frame(s []int16, i int) [2]float64 {
	l := float64(s[i*r.ch])
	rr := l
	if r.ch > 1 {
		rr = float64(s[i*r.ch+1])
	}
	return [2]float64{l, rr}
}

// process takes interleaved input and returns interleaved 48 kHz stereo.
func (r *resampler) process(s []int16) []int16 {
	frames := len(s) / r.ch
	if frames == 0 {
		return nil
	}
	if r.in == audio.SampleRate {
		out := make([]int16, frames*2)
		for i := 0; i < frames; i++ {
			f := r.frame(s, i)
			out[2*i], out[2*i+1] = int16(f[0]), int16(f[1])
		}
		return out
	}
	if !r.primed {
		r.prev, r.primed = r.frame(s, 0), true
	}
	out := make([]int16, 0, int(float64(frames)/r.step+2)*2)
	// Input frame -1 is prev; frames 0..n-1 are s.
	for {
		i := int(r.pos) // interpolate between input i-1 and i
		if i >= frames {
			break
		}
		a := r.prev
		if i > 0 {
			a = r.frame(s, i-1)
		}
		b := r.frame(s, i)
		t := r.pos - float64(i)
		out = append(out, int16(a[0]+(b[0]-a[0])*t), int16(a[1]+(b[1]-a[1])*t))
		r.pos += r.step
	}
	r.pos -= float64(frames)
	r.prev = r.frame(s, frames-1)
	return out
}
