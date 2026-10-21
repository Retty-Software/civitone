package player

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// wav builds a 16-bit PCM WAV of a sine.
func wav(rate, channels int, secs, hz float64) []byte {
	n := int(secs * float64(rate))
	var data bytes.Buffer
	for i := 0; i < n; i++ {
		v := int16(10000 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		for c := 0; c < channels; c++ {
			binary.Write(&data, binary.LittleEndian, v)
		}
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+data.Len()))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(channels), uint32(rate),
		uint32(rate * channels * 2), uint16(channels * 2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(data.Len()))
	b.Write(data.Bytes())
	return b.Bytes()
}

func TestResample44100To48000(t *testing.T) {
	rs := newResampler(44100, 1)
	in := make([]int16, 44100)
	for i := range in {
		in[i] = int16(10000 * math.Sin(2*math.Pi*1000*float64(i)/44100))
	}
	var out []int16
	for off := 0; off < len(in); off += 1000 { // in blocks, as decoders deliver
		out = append(out, rs.process(in[off:min(off+1000, len(in))])...)
	}
	frames := len(out) / 2
	if frames < 47990 || frames > 48010 {
		t.Fatalf("1 s at 44.1k gave %d frames", frames)
	}
	// 1 kHz: ~2000 zero crossings per second, either channel.
	cross := 0
	for i := 1; i < frames; i++ {
		if (out[2*i-2] < 0) != (out[2*i] < 0) {
			cross++
		}
	}
	if cross < 1990 || cross > 2010 {
		t.Fatalf("%d zero crossings, want ~2000", cross)
	}
}

func TestWAVDecode(t *testing.T) {
	d, err := newWAV(bytes.NewReader(wav(22050, 2, 0.5, 440)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Rate() != 22050 || d.Channels() != 2 {
		t.Fatalf("rate %d ch %d", d.Rate(), d.Channels())
	}
	buf := make([]int16, 4096)
	total := 0
	for {
		n, err := d.Read(buf)
		total += n
		if err != nil {
			break
		}
	}
	if total != 22050 { // 0.5 s * 22050 * 2 channels
		t.Fatalf("read %d samples", total)
	}
}

func TestPlaylistPlaysFolder(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Opening Song.wav"), wav(44100, 2, 1, 440), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not audio"), 0o644)
	p := Playlist(dir)
	defer p.Close()

	var peak int16
	start := time.Now()
	got := 0
	for time.Since(start) < 600*time.Millisecond {
		c := <-p.Chunks()
		got += len(c)
		for _, s := range c {
			if s > peak {
				peak = s
			}
		}
	}
	if peak < 9000 {
		t.Fatalf("peak %d, want the sine", peak)
	}
	// Real time: ~600 ms is ~57600 samples; generous bounds for CI.
	if got < 40000 || got > 80000 {
		t.Fatalf("%d samples in 600 ms", got)
	}
	if now, problem := p.NowPlaying(); now != "Opening Song" || problem != "" {
		t.Fatalf("now playing %q, problem %q", now, problem)
	}
}

func TestPlaylistEmptyFolderSaysSo(t *testing.T) {
	p := Playlist(t.TempDir())
	defer p.Close()
	time.Sleep(100 * time.Millisecond)
	<-p.Chunks() // silence keeps flowing
	if _, problem := p.NowPlaying(); problem == "" {
		t.Fatal("no problem reported for an empty folder")
	}
}
