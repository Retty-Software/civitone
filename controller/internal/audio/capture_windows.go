//go:build windows

package audio

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// wasapi captures one endpoint in shared mode: a capture device (mic, line
// in, VB-CABLE output), or a playback device in loopback ("whatever this
// computer plays"). Windows converts to 48 kHz / 16-bit / stereo when it can
// (AUTOCONVERTPCM); otherwise we take the mix format and convert here. The
// rest of the pipeline only ever sees 48k/16/2.
type wasapi struct {
	chunks chan []int16
	stop   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

func (w *wasapi) Chunks() <-chan []int16 { return w.chunks }

func (w *wasapi) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *wasapi) Close() { w.once.Do(func() { close(w.stop) }) }

func openCapture(device string, loopback bool) (capturer, error) {
	w := &wasapi{chunks: make(chan []int16, 64), stop: make(chan struct{})}
	ready := make(chan error, 1)
	go w.loop(device, loopback, ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return w, nil
}

// withCOM runs fn on a locked OS thread with COM initialised.
func withCOM(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		// S_FALSE (already initialised on this thread) is fine.
		if oe, ok := err.(*ole.OleError); !ok || oe.Code() != 1 {
			return fmt.Errorf("CoInitializeEx: %w", err)
		}
	}
	defer ole.CoUninitialize()
	return fn()
}

// findDevice returns the active endpoint of the given flow whose friendly
// name contains want (case-insensitive), or the list of names when nothing
// matches. An empty want on a render flow means the default output device.
func findDevice(flow uint32, want string) (*wca.IMMDevice, []string, error) {
	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		return nil, nil, fmt.Errorf("device enumerator: %w", err)
	}
	defer mmde.Release()
	var dc *wca.IMMDeviceCollection
	if err := mmde.EnumAudioEndpoints(flow, wca.DEVICE_STATE_ACTIVE, &dc); err != nil {
		return nil, nil, fmt.Errorf("enumerate devices: %w", err)
	}
	defer dc.Release()
	var n uint32
	if err := dc.GetCount(&n); err != nil {
		return nil, nil, err
	}
	var names []string
	var match *wca.IMMDevice
	for i := uint32(0); i < n; i++ {
		var mmd *wca.IMMDevice
		if err := dc.Item(i, &mmd); err != nil {
			continue
		}
		name := friendlyName(mmd)
		names = append(names, name)
		if match == nil && want != "" && strings.Contains(strings.ToLower(name), strings.ToLower(want)) {
			match = mmd
			continue
		}
		mmd.Release()
	}
	if match == nil && want == "" && flow == wca.ERender {
		var def *wca.IMMDevice
		if err := mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &def); err == nil {
			match = def
		}
	}
	return match, names, nil
}

func friendlyName(mmd *wca.IMMDevice) string {
	var ps *wca.IPropertyStore
	if err := mmd.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return "?"
	}
	defer ps.Release()
	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return "?"
	}
	return pv.String()
}

func listDevices(loopback bool) ([]string, error) {
	flow := uint32(wca.ECapture)
	if loopback {
		flow = wca.ERender
	}
	var names []string
	err := withCOM(func() error {
		mmd, all, err := findDevice(flow, "\x00none")
		if mmd != nil {
			mmd.Release()
		}
		names = all
		return err
	})
	return names, err
}

func (w *wasapi) fail(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

// format describes what GetBuffer hands us when Windows didn't convert.
type format struct {
	channels int
	float    bool // 32-bit float, else 16-bit int
	rate     int
}

func (w *wasapi) loop(device string, loopback bool, ready chan<- error) {
	defer close(w.chunks)
	started := false
	what := "capture"
	if loopback {
		what = "playback (loopback)"
	}
	err := withCOM(func() error {
		flow := uint32(wca.ECapture)
		if loopback {
			flow = wca.ERender
		}
		mmd, names, err := findDevice(flow, device)
		if err != nil {
			return err
		}
		if mmd == nil {
			return fmt.Errorf("no active %s device matching %q (have: %s)", what, device, strings.Join(names, ", "))
		}
		defer mmd.Release()

		var ac *wca.IAudioClient
		if err := mmd.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
			return fmt.Errorf("activate: %w", err)
		}
		defer ac.Release()

		const bufferHNS = 200 * 10000 // 200 ms in 100 ns units
		base := uint32(0)
		if loopback {
			base = wca.AUDCLNT_STREAMFLAGS_LOOPBACK
		}
		wfx := &wca.WAVEFORMATEX{
			WFormatTag:      wca.WAVE_FORMAT_PCM,
			NChannels:       Channels,
			NSamplesPerSec:  SampleRate,
			WBitsPerSample:  16,
			NBlockAlign:     Channels * 2,
			NAvgBytesPerSec: SampleRate * Channels * 2,
		}
		var native *format // nil: Windows converts to 48k/16/2
		flags := base | wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY
		if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, bufferHNS, 0, wfx, nil); err != nil {
			// Some drivers refuse conversion in loopback: use the mix format.
			var mix *wca.WAVEFORMATEX
			if merr := ac.GetMixFormat(&mix); merr != nil {
				return fmt.Errorf("initialize 48k/16/2: %w", err)
			}
			native = &format{channels: int(mix.NChannels), float: mix.WBitsPerSample == 32, rate: int(mix.NSamplesPerSec)}
			if native.rate != SampleRate || (mix.WBitsPerSample != 32 && mix.WBitsPerSample != 16) || native.channels < 1 {
				return fmt.Errorf("device runs at %d Hz / %d-bit; set it to 48000 Hz in Sound settings", mix.NSamplesPerSec, mix.WBitsPerSample)
			}
			if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, base, bufferHNS, 0, mix, nil); err != nil {
				return fmt.Errorf("initialize %d Hz mix format: %w", native.rate, err)
			}
		}
		var acc *wca.IAudioCaptureClient
		if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
			return fmt.Errorf("capture client: %w", err)
		}
		defer acc.Release()
		if err := ac.Start(); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		defer ac.Stop()
		started = true
		ready <- nil

		// Loopback delivers nothing while nothing plays. Keep the stream
		// running in real time by filling those gaps with silence.
		start := time.Now()
		var delivered int64 // frames handed on
		send := func(chunk []int16) {
			delivered += int64(len(chunk) / Channels)
			select {
			case w.chunks <- chunk:
			default:
				// Consumer stalled; drop rather than build latency.
			}
		}

		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-w.stop:
				return nil
			case <-tick.C:
			}
			got := false
			for {
				var next uint32
				if err := acc.GetNextPacketSize(&next); err != nil {
					return fmt.Errorf("device lost: %w", err)
				}
				if next == 0 {
					break
				}
				var data *byte
				var frames, bflags uint32
				var devPos, qpcPos uint64
				if err := acc.GetBuffer(&data, &frames, &bflags, &devPos, &qpcPos); err != nil {
					return fmt.Errorf("device lost: %w", err)
				}
				chunk := make([]int16, int(frames)*Channels)
				if bflags&wca.AUDCLNT_BUFFERFLAGS_SILENT == 0 && frames > 0 {
					convert(chunk, data, int(frames), native)
				}
				acc.ReleaseBuffer(frames)
				send(chunk)
				got = true
			}
			if loopback && !got {
				due := int64(time.Since(start).Seconds()*SampleRate) - delivered
				if due > SampleRate/25 { // more than 40 ms behind: silence
					send(make([]int16, int(due)*Channels))
				}
			}
		}
	})
	if !started {
		if err == nil {
			err = fmt.Errorf("%s %q did not start", what, device)
		}
		ready <- err
		return
	}
	if err != nil {
		w.fail(err)
	}
}

// convert copies frames from a WASAPI buffer into 48k/16/2. f nil means the
// buffer is already 16-bit stereo.
func convert(dst []int16, data *byte, frames int, f *format) {
	if f == nil {
		copy(dst, unsafe.Slice((*int16)(unsafe.Pointer(data)), frames*Channels))
		return
	}
	n := frames * f.channels
	if f.float {
		src := unsafe.Slice((*float32)(unsafe.Pointer(data)), n)
		for i := 0; i < frames; i++ {
			l := src[i*f.channels]
			r := l
			if f.channels > 1 {
				r = src[i*f.channels+1]
			}
			dst[2*i], dst[2*i+1] = f32to16(l), f32to16(r)
		}
		return
	}
	src := unsafe.Slice((*int16)(unsafe.Pointer(data)), n)
	for i := 0; i < frames; i++ {
		dst[2*i] = src[i*f.channels]
		dst[2*i+1] = dst[2*i]
		if f.channels > 1 {
			dst[2*i+1] = src[i*f.channels+1]
		}
	}
}

func f32to16(v float32) int16 {
	x := math.Round(float64(v) * 32767)
	if x > 32767 {
		return 32767
	}
	if x < -32768 {
		return -32768
	}
	return int16(x)
}
