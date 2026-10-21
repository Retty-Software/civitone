package audio

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"time"
)

// Chime is the identify sound: a rising C-major arpeggio of bell-like notes,
// played twice. Interleaved stereo int16 at 48 kHz, both channels the same.
func Chime() []int16 {
	notes := []float64{523.25, 659.25, 783.99, 1046.5}
	const (
		noteSec = 0.22
		tailSec = 0.9 // the last note rings out
		gapSec  = 0.35
		peak    = 0.5 // about -6 dBFS
	)
	var mono []float64
	for rep := 0; rep < 2; rep++ {
		for i, f := range notes {
			dur := noteSec
			if i == len(notes)-1 {
				dur = tailSec
			}
			n := int(dur * SampleRate)
			for k := 0; k < n; k++ {
				t := float64(k) / SampleRate
				env := math.Min(t/0.005, 1) * math.Exp(-t*5)
				mono = append(mono, env*(0.8*math.Sin(2*math.Pi*f*t)+0.2*math.Sin(4*math.Pi*f*t)))
			}
		}
		mono = append(mono, make([]float64, int(gapSec*SampleRate))...)
	}
	out := make([]int16, 2*len(mono))
	for i, v := range mono {
		s := int16(v * peak * 32767)
		out[2*i], out[2*i+1] = s, s
	}
	return out
}

// ChimeDuration is how long Chime plays.
func ChimeDuration() time.Duration {
	return time.Duration(len(Chime())/2) * time.Second / SampleRate
}

// PlayTo writes interleaved stereo PCM to a snapserver TCP source in real
// time (20 ms chunks), then closes the connection so the stream goes idle.
func PlayTo(ctx context.Context, addr string, pcm []int16) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	const chunk = SampleRate / 50 * Channels // 20 ms
	buf := make([]byte, 0, chunk*2)
	start := time.Now()
	for off := 0; off < len(pcm); off += chunk {
		end := min(off+chunk, len(pcm))
		buf = buf[:0]
		for _, s := range pcm[off:end] {
			buf = binary.LittleEndian.AppendUint16(buf, uint16(s))
		}
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write(buf); err != nil {
			return err
		}
		// Pace to real time so snapserver timestamps it as it plays.
		due := start.Add(time.Duration(end/Channels) * time.Second / SampleRate)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(due)):
		}
	}
	return nil
}

// PageChime is the alert before a page: 300 ms of silence (speakers are
// still switching to the paging stream), then a falling two-tone, the
// classic "ding-dong". Mono int16 at 48 kHz.
func PageChime() []int16 {
	const (
		leadSec = 0.3
		noteSec = 0.6
		peak    = 0.5 // about -6 dBFS
	)
	out := make([]int16, int(leadSec*SampleRate), int((leadSec+2*noteSec)*SampleRate))
	for _, f := range []float64{783.99, 622.25} { // G5, E-flat 5
		for k := 0; k < int(noteSec*SampleRate); k++ {
			t := float64(k) / SampleRate
			env := math.Min(t/0.005, 1) * math.Exp(-t*3.5)
			v := env * (0.8*math.Sin(2*math.Pi*f*t) + 0.2*math.Sin(4*math.Pi*f*t))
			out = append(out, int16(v*peak*32767))
		}
	}
	return out
}
