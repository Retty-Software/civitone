package player

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/audio"
)

const chunkFrames = audio.SampleRate / 50 // 20 ms

// Player is an audio.Capture fed by a playlist folder or a radio URL.
type Player struct {
	chunks chan []int16
	cancel context.CancelFunc

	mu      sync.Mutex
	now     string // what's playing, for the page
	problem string
}

func (p *Player) Chunks() <-chan []int16 { return p.chunks }
func (p *Player) Err() error             { return nil }
func (p *Player) Close()                 { p.cancel() }

// NowPlaying says what's playing, or what's wrong.
func (p *Player) NowPlaying() (now, problem string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now, p.problem
}

func (p *Player) set(now, problem string) {
	p.mu.Lock()
	p.now, p.problem = now, problem
	p.mu.Unlock()
}

func newPlayer() (*Player, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return &Player{chunks: make(chan []int16, 8), cancel: cancel}, ctx
}

// pacer hands out 48 kHz stereo in 20 ms chunks on wall-clock time.
type pacer struct {
	p       *Player
	start   time.Time
	sent    int64
	pending []int16
}

// write queues samples and blocks until they are due.
func (pc *pacer) write(ctx context.Context, s []int16) error {
	pc.pending = append(pc.pending, s...)
	for len(pc.pending) >= chunkFrames*2 {
		if pc.start.IsZero() {
			pc.start = time.Now()
		}
		due := pc.start.Add(time.Duration(pc.sent) * time.Second / audio.SampleRate)
		if d := time.Until(due); d > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		} else if d < -time.Second {
			pc.start, pc.sent = time.Now(), 0 // fell far behind (a slow stream): re-anchor
		}
		out := append([]int16(nil), pc.pending[:chunkFrames*2]...)
		pc.pending = pc.pending[chunkFrames*2:]
		select {
		case pc.p.chunks <- out:
		default: // consumer stalled; drop rather than build latency
		}
		pc.sent += chunkFrames
	}
	return nil
}

// silence keeps time while there is nothing to play.
func (pc *pacer) silence(ctx context.Context, d time.Duration) error {
	return pc.write(ctx, make([]int16, int(d.Seconds()*audio.SampleRate)*2))
}

// play decodes d to the end through the resampler and pacer.
func (pc *pacer) play(ctx context.Context, d decoder) error {
	rs := newResampler(d.Rate(), d.Channels())
	buf := make([]int16, 4096*d.Channels())
	for ctx.Err() == nil {
		n, err := d.Read(buf)
		if n > 0 {
			if werr := pc.write(ctx, rs.process(buf[:n])); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Playlist plays every playable file under dir, shuffled, forever. It
// rescans the folder each time through, so added songs join the next round.
func Playlist(dir string) *Player {
	p, ctx := newPlayer()
	go func() {
		defer close(p.chunks)
		pc := &pacer{p: p}
		for ctx.Err() == nil {
			files, err := scan(dir)
			if err != nil || len(files) == 0 {
				msg := fmt.Sprintf("no music files in %s (MP3, FLAC, Ogg, WAV)", dir)
				if err != nil {
					msg = err.Error()
				}
				p.set("", msg)
				if pc.silence(ctx, 5*time.Second) != nil {
					return
				}
				continue
			}
			rand.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
			for _, f := range files {
				if ctx.Err() != nil {
					return
				}
				d, err := openFile(f)
				if err != nil {
					p.set("", "skipped "+err.Error())
					continue
				}
				p.set(strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)), "")
				if err := pc.play(ctx, d); err != nil && ctx.Err() == nil {
					p.set("", fmt.Sprintf("%s: %v", filepath.Base(f), err))
				}
				d.Close()
			}
		}
	}()
	return p
}

func scan(dir string) ([]string, error) {
	if dir == "" {
		return nil, errors.New("pick a music folder")
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("can't open folder %s", dir)
	}
	var out []string
	filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && playable(path) {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, nil
}

// Radio plays an MP3 internet stream (Icecast, Shoutcast), reconnecting when
// it drops. AAC streams are not supported yet.
func Radio(url string) *Player {
	p, ctx := newPlayer()
	go func() {
		defer close(p.chunks)
		pc := &pacer{p: p}
		for ctx.Err() == nil {
			err := radioOnce(ctx, p, pc, url)
			if ctx.Err() != nil {
				return
			}
			p.set("", err.Error()+"; retrying")
			if pc.silence(ctx, 3*time.Second) != nil {
				return
			}
		}
	}()
	return p
}

func radioOnce(ctx context.Context, p *Player, pc *pacer, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "CivitoneSource/1")
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		return fmt.Errorf("can't reach the stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream answered %s", resp.Status)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	ext := ".mp3"
	switch {
	case strings.Contains(ct, "ogg"):
		ext = ".ogg"
	case strings.Contains(ct, "aac"), strings.Contains(ct, "mp4"):
		return errors.New("AAC streams aren't supported yet; use the station's MP3 stream")
	case strings.Contains(ct, "mpegurl"), strings.Contains(ct, "x-scpls"):
		return errors.New("that's a playlist file; use the stream URL inside it")
	}
	d, err := newDecoder(ext, bufio.NewReaderSize(resp.Body, 64<<10), nil)
	if err != nil {
		return fmt.Errorf("can't decode the stream: %v", err)
	}
	name := resp.Header.Get("icy-name")
	if name == "" {
		name = url
	}
	p.set(name, "")
	if err := pc.play(ctx, d); err != nil {
		return err
	}
	return errors.New("the stream ended")
}
