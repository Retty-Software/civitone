// Package player turns files and internet radio into a live audio.Capture:
// decode (MP3, FLAC, Ogg Vorbis, WAV; pure Go), resample to 48 kHz stereo,
// and hand out 20 ms chunks in real time.
package player

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hajimehoshi/go-mp3"
	"github.com/jfreymuth/oggvorbis"
	"github.com/mewkiz/flac"
)

// decoder yields interleaved int16 frames at its own rate and channel count.
type decoder interface {
	Rate() int
	Channels() int
	// Read fills buf with interleaved samples; io.EOF at the end.
	Read(buf []int16) (int, error)
	Close() error
}

// Formats lists the file extensions a playlist plays.
var Formats = []string{".mp3", ".flac", ".ogg", ".oga", ".wav"}

func playable(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, f := range Formats {
		if ext == f {
			return true
		}
	}
	return false
}

func openFile(path string) (decoder, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	d, err := newDecoder(strings.ToLower(filepath.Ext(path)), bufio.NewReaderSize(f, 64<<10), f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return d, nil
}

func newDecoder(ext string, r io.Reader, c io.Closer) (decoder, error) {
	switch ext {
	case ".mp3":
		d, err := mp3.NewDecoder(r)
		if err != nil {
			return nil, err
		}
		return &mp3Dec{d: d, c: c}, nil
	case ".flac":
		s, err := flac.New(r)
		if err != nil {
			return nil, err
		}
		return &flacDec{s: s, c: c}, nil
	case ".ogg", ".oga":
		d, err := oggvorbis.NewReader(r)
		if err != nil {
			return nil, err
		}
		return &vorbisDec{d: d, c: c}, nil
	case ".wav":
		return newWAV(r, c)
	}
	return nil, fmt.Errorf("unsupported format %q", ext)
}

// --- MP3: 16-bit little-endian stereo bytes --------------------------------

type mp3Dec struct {
	d   *mp3.Decoder
	c   io.Closer
	raw []byte
}

func (m *mp3Dec) Rate() int     { return m.d.SampleRate() }
func (m *mp3Dec) Channels() int { return 2 }
func (m *mp3Dec) Close() error  { return closeIf(m.c) }
func (m *mp3Dec) Read(buf []int16) (int, error) {
	if cap(m.raw) < len(buf)*2 {
		m.raw = make([]byte, len(buf)*2)
	}
	raw := m.raw[:len(buf)*2]
	n, err := io.ReadFull(m.d, raw)
	n &^= 1
	for i := 0; i < n/2; i++ {
		buf[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n / 2, err
}

// --- FLAC: per-channel int32 at the stream's bit depth -----------------------

type flacDec struct {
	s       *flac.Stream
	c       io.Closer
	pending []int16
}

func (f *flacDec) Rate() int     { return int(f.s.Info.SampleRate) }
func (f *flacDec) Channels() int { return int(f.s.Info.NChannels) }
func (f *flacDec) Close() error  { return closeIf(f.c) }
func (f *flacDec) Read(buf []int16) (int, error) {
	for len(f.pending) < len(buf) {
		fr, err := f.s.ParseNext()
		if err != nil {
			if err == io.EOF && len(f.pending) > 0 {
				break
			}
			return 0, err
		}
		shift := int(f.s.Info.BitsPerSample) - 16
		n := len(fr.Subframes[0].Samples)
		for i := 0; i < n; i++ {
			for _, sf := range fr.Subframes {
				v := sf.Samples[i]
				if shift > 0 {
					v >>= shift
				} else if shift < 0 {
					v <<= -shift
				}
				f.pending = append(f.pending, int16(v))
			}
		}
	}
	n := copy(buf, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

// --- Ogg Vorbis: interleaved float32 -----------------------------------------

type vorbisDec struct {
	d *oggvorbis.Reader
	c io.Closer
	f []float32
}

func (v *vorbisDec) Rate() int     { return v.d.SampleRate() }
func (v *vorbisDec) Channels() int { return v.d.Channels() }
func (v *vorbisDec) Close() error  { return closeIf(v.c) }
func (v *vorbisDec) Read(buf []int16) (int, error) {
	if cap(v.f) < len(buf) {
		v.f = make([]float32, len(buf))
	}
	n, err := v.d.Read(v.f[:len(buf)])
	for i := 0; i < n; i++ {
		x := v.f[i] * 32767
		if x > 32767 {
			x = 32767
		} else if x < -32768 {
			x = -32768
		}
		buf[i] = int16(x)
	}
	return n, err
}

// --- WAV: 16-bit PCM only -----------------------------------------------------

type wavDec struct {
	r        io.Reader
	c        io.Closer
	rate     int
	channels int
	left     int64 // bytes of sample data left
	raw      []byte
}

func newWAV(r io.Reader, c io.Closer) (*wavDec, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil || string(hdr[:4]) != "RIFF" || string(hdr[8:]) != "WAVE" {
		return nil, errors.New("not a WAV file")
	}
	w := &wavDec{r: r, c: c}
	for {
		var ch [8]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			return nil, errors.New("WAV has no data chunk")
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:]))
		switch string(ch[:4]) {
		case "fmt ":
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil || len(b) < 16 {
				return nil, errors.New("bad WAV format chunk")
			}
			if tag := binary.LittleEndian.Uint16(b); tag != 1 && tag != 0xFFFE {
				return nil, errors.New("only PCM WAV files are supported")
			}
			w.channels = int(binary.LittleEndian.Uint16(b[2:]))
			w.rate = int(binary.LittleEndian.Uint32(b[4:]))
			if bits := binary.LittleEndian.Uint16(b[14:]); bits != 16 {
				return nil, fmt.Errorf("only 16-bit WAV files are supported (this one is %d-bit)", bits)
			}
		case "data":
			if w.channels == 0 {
				return nil, errors.New("WAV data before format")
			}
			w.left = size
			return w, nil
		default:
			if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
				return nil, err
			}
		}
	}
}

func (w *wavDec) Rate() int     { return w.rate }
func (w *wavDec) Channels() int { return w.channels }
func (w *wavDec) Close() error  { return closeIf(w.c) }
func (w *wavDec) Read(buf []int16) (int, error) {
	if w.left <= 0 {
		return 0, io.EOF
	}
	want := int64(len(buf) * 2)
	if want > w.left {
		want = w.left
	}
	if cap(w.raw) < int(want) {
		w.raw = make([]byte, want)
	}
	n, err := io.ReadFull(w.r, w.raw[:want])
	n &^= 1
	w.left -= int64(n)
	for i := 0; i < n/2; i++ {
		buf[i] = int16(binary.LittleEndian.Uint16(w.raw[2*i:]))
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n / 2, err
}

func closeIf(c io.Closer) error {
	if c != nil {
		return c.Close()
	}
	return nil
}
