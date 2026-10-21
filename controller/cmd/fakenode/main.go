// Command fakenode stands in for a speaker node's control channel: hello with
// an adopt code, heartbeats, acks, and it stores the token from "adopt" and
// reconnects with it. For testing adoption without hardware.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

func main() {
	url := flag.String("url", "ws://127.0.0.1:8080/node", "controller /node URL")
	id := flag.String("id", "civ-fake", "device id")
	code := flag.String("code", "", "adopt code from the console")
	mac := flag.String("mac", "02:00:00:00:00:01", "MAC, matched against snapclient's to join audio and control")
	rssi := flag.Int("rssi", -55, "typical Wi-Fi signal, jittered a little each heartbeat")
	flag.Parse()
	start := time.Now()
	speakers := map[string]any{"a": map[string]any{"trim_db": 0, "muted": false}, "b": map[string]any{"trim_db": 0, "muted": false}}

	token := ""
	for {
		h := http.Header{}
		if token != "" {
			h.Set("Authorization", "Bearer "+token)
		}
		ctx := context.Background()
		c, _, err := websocket.Dial(ctx, *url, &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			log.Printf("dial: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		hello := map[string]any{"t": "hello", "device_id": *id, "mac": *mac, "fw": "fake", "board": "fakenode"}
		if token == "" {
			hello["adopt_code"] = *code
			log.Printf("unadopted, using code %s", *code)
		}
		b, _ := json.Marshal(hello)
		c.Write(ctx, websocket.MessageText, b)
		go func() {
			for {
				hb, _ := json.Marshal(map[string]any{"t": "hb", "state": "playing", "rssi": *rssi + rand.Intn(5) - 2, "ip": "10.0.0.9", "ssid": "IoT",
					"uptime": int(time.Since(start).Seconds()), "buffer_ms": 2000, "sync_err_us": rand.Intn(4000) - 2000, "dropouts_1m": 0, "underruns": 0,
					"heap": 4168495, "speakers": speakers})
				if c.Write(ctx, websocket.MessageText, hb) != nil {
					return
				}
				time.Sleep(5 * time.Second)
			}
		}()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				log.Printf("closed: %v", err)
				if websocket.CloseStatus(err) == 4001 {
					token = ""
				}
				break
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			log.Printf("got %v", m["cmd"])
			ack, _ := json.Marshal(map[string]any{"t": "ack", "id": m["id"], "ok": true})
			c.Write(ctx, websocket.MessageText, ack)
			switch m["cmd"] {
			case "set_speakers":
				for _, k := range []string{"a", "b"} {
					if v, ok := m[k]; ok {
						speakers[k] = v
					}
				}
			case "adopt":
				token, _ = m["token"].(string)
				c.Close(websocket.StatusNormalClosure, "restarting")
			case "forget", "reset":
				token = ""
				c.Close(websocket.StatusNormalClosure, "restarting")
			}
		}
		time.Sleep(time.Second)
	}
}
