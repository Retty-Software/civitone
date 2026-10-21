// Command fakesource stands in for a source (Civitone Source, an input
// board): it dials the controller's /source with an adopt code, waits to be
// approved, reconnects with its token and streams a tone or a chime melody in
// real time. For testing without hardware.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

func main() {
	url := flag.String("url", "ws://127.0.0.1:8080/source", "controller /source URL")
	id := flag.String("id", "src-fake", "device id")
	code := flag.String("code", "", "adopt code from the console")
	freq := flag.Float64("hz", 440, "tone frequency")
	dbfs := flag.Float64("dbfs", -20, "tone level")
	dur := flag.Duration("for", 10*time.Second, "how long to stream")
	melody := flag.Bool("melody", false, "play a repeating chime arpeggio instead of a steady tone")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	token := ""
	var c *websocket.Conn
	for c == nil {
		h := http.Header{}
		hello := map[string]any{"t": "hello", "device_id": *id, "role": "source", "kind": "test", "fw": "fake", "board": "fakesource", "rate": 48000, "channels": 1}
		if token != "" {
			h.Set("Authorization", "Bearer "+token)
		} else {
			hello["adopt_code"] = *code
		}
		conn, _, err := websocket.Dial(ctx, *url, &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			log.Fatal(err)
		}
		b, _ := json.Marshal(hello)
		conn.Write(ctx, websocket.MessageText, b)
		_, data, err := conn.Read(ctx) // welcome
		if err != nil {
			log.Fatalf("refused: %v", err)
		}
		var w map[string]any
		json.Unmarshal(data, &w)
		if w["state"] == "adopted" {
			c = conn
			break
		}
		log.Printf("waiting for approval in the console")
		for token == "" {
			_, data, err := conn.Read(ctx)
			if err != nil {
				log.Fatalf("closed: %v", err)
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			if m["cmd"] == "adopt" {
				token, _ = m["token"].(string)
				ack, _ := json.Marshal(map[string]any{"t": "ack", "id": m["id"], "ok": true})
				conn.Write(ctx, websocket.MessageText, ack)
				log.Printf("adopted as %v", m["name"])
			}
		}
		conn.Close(websocket.StatusNormalClosure, "reconnecting with token")
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	c.CloseRead(ctx)

	amp := 32767 * math.Pow(10, *dbfs/20)
	var n int
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			log.Printf("sent %d frames", i)
			return
		case <-t.C:
		}
		buf := make([]byte, 0, 1920)
		for k := 0; k < 960; k++ {
			v := int16(amp * math.Sin(2*math.Pi**freq*float64(n)/48000))
			if *melody {
				v = int16(amp * chime(n))
			}
			buf = binary.LittleEndian.AppendUint16(buf, uint16(v))
			n++
		}
		if err := c.Write(ctx, websocket.MessageBinary, buf); err != nil {
			log.Fatal(err)
		}
		if i%50 == 0 {
			hb, _ := json.Marshal(map[string]any{"t": "hb", "uptime": i / 50})
			c.Write(ctx, websocket.MessageText, hb)
		}
	}
}

// chime is a C-major arpeggio of bell-like notes, 400 ms each, with a soft
// attack and exponential decay, plus a quiet octave partial.
func chime(n int) float64 {
	notes := []float64{523.25, 659.25, 783.99, 1046.5, 783.99, 659.25}
	const noteLen = 48000 * 4 / 10
	f := notes[(n/noteLen)%len(notes)]
	t := float64(n%noteLen) / 48000
	env := math.Min(t/0.01, 1) * math.Exp(-t*6)
	return env * (0.8*math.Sin(2*math.Pi*f*t) + 0.2*math.Sin(4*math.Pi*f*t))
}
