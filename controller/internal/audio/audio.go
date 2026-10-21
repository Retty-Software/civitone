// Package audio is Civitone's sample plumbing: device capture (WASAPI on
// Windows; other platforms have no backend yet), the identify chime, a test
// generator, level metering and the PA shaper. Everything is 48 kHz /
// 16-bit / stereo interleaved.
package audio

import (
	"errors"
	"sync"
	"time"
)

const (
	SampleRate = 48000
	Channels   = 2
)

// Capture is a running capture or generator. Chunks delivers interleaved
// stereo int16 at 48 kHz; it is closed when capture stops, after which Err
// says why.
type Capture interface {
	Chunks() <-chan []int16
	Err() error
	Close()
}

type capturer = Capture

// ErrNoBackend is returned on platforms without a capture backend.
var ErrNoBackend = errors.New("audio capture is only implemented on Windows so far")

// Open starts capturing a device whose name contains device
// (case-insensitive). loopback captures what a playback device plays
// instead of a recording device; an empty name then means the default
// output.
func Open(device string, loopback bool) (Capture, error) { return openCapture(device, loopback) }

// Devices lists recording devices, or playback devices for loopback.
func Devices(loopback bool) ([]string, error) { return listDevices(loopback) }

// Generator plays a fixed buffer on a loop in real time, as a Capture. Used
// for the test-chime source and anywhere a fake input helps.
type generator struct {
	chunks chan []int16
	stop   chan struct{}
	once   sync.Once
}

// Loop returns a Capture that repeats pcm (interleaved stereo) forever, with
// gap of silence between repeats.
func Loop(pcm []int16, gap time.Duration) Capture {
	g := &generator{chunks: make(chan []int16, 16), stop: make(chan struct{})}
	full := append(append([]int16(nil), pcm...), make([]int16, int(gap.Seconds()*SampleRate)*Channels)...)
	go func() {
		defer close(g.chunks)
		const chunk = SampleRate / 50 * Channels // 20 ms
		start := time.Now()
		pos, sent := 0, 0
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-g.stop:
				return
			case <-t.C:
			}
			due := int(time.Since(start).Seconds()*SampleRate)*Channels - sent
			for due >= chunk {
				out := make([]int16, chunk)
				for i := range out {
					out[i] = full[pos]
					pos = (pos + 1) % len(full)
				}
				select {
				case g.chunks <- out:
				default:
				}
				sent += chunk
				due -= chunk
			}
		}
	}()
	return g
}

func (g *generator) Chunks() <-chan []int16 { return g.chunks }
func (g *generator) Err() error             { return nil }
func (g *generator) Close()                 { g.once.Do(func() { close(g.stop) }) }
