# Bench checklist: prove the board (step 1, no Civitone code)

## Findings, October 6, 2026 (the first bench board)

- Board is **Amped-ESP32-Plus Rev B (02/26)**: ESP32-WROVER-IE, PCM5122 at
  I2C 0x4D (SDA 21, SCL 27), amp is **TPA3110D2** (needs >= 8 V; silent on
  USB power alone). Shipped with blank flash (backup in `firmware/backups/`).
- CH340 USB serial: use 115200 baud for read/write. 460800+ corrupts reads.
  Opening the serial port resets the board.
- USB and 24 V together are safe (Rev B schematic: VBUS feeds +5V and VDD
  through 1N5819WS diodes, D6). But **plug USB in first, then 24 V**: if
  24 V is already on, the CH340 never enumerates on the Mac.
- On USB alone VDD is ~4.7 V, below the TPA3110's 8 V minimum: line-out
  works, speaker is silent.
- Stock firmware 2026-07-13 works on this board. `firmware/diag/` is a
  standalone I2C/I2S test that proved the DAC independently.
- **ffmpeg's `sine` source is -18 dBFS by default.** Any `volume=` filter is
  on top of that. Use `sine=...:amplitude=...` or no filter.
- **Stock volume is linear in dB over 103 dB**: 40% = -62 dB, 80% = -21 dB.
  Below ~60% is effectively silent. Civitone firmware should map 0-100 to
  roughly -60..0 dB.
- Stock firmware finds the server by mDNS only by default. On a separate
  IoT VLAN it never finds it; set it manually (works across VLANs):
  `curl -X POST "http://NODE/post?param=snapserver_use_mdns&value=0"`, then
  `snapserver_host`, `snapserver_port`, then `POST /restart`.
- WiFi via Improv over USB: `firmware/tools/improv_provision.py PORT SSID PASS`.
- `RESYNCING HARD` in the node log: the "latency" figure is just the
  server/client clock offset, not an error. Resyncs with 0 queued chunks are
  normal end-of-stream. The rest (see below) came from the server.

### Sync and pops, resolved (October 6, 2026)

- **Pops:** stock firmware mutes by toggling GPIO13 = TPA3110 SD#, which pops
  every time. Fixed in our firmware (patch 0001): amp on once at boot, mute in
  the DAC register only. Result: no pops except one at power-on.
- **~50 ms blips:** every one lined up with `onResync (<stream>): NN ms` in
  the **snapserver** log. The server fell behind its own clock and shifted
  timestamps; the node followed correctly. Happened with ffmpeg over TCP and
  with snapserver reading a file itself, at normal and top macOS priority
  (`taskpolicy -t 0 -l 0`): ~1 resync per 30-60 s on this Intel MacBook.
  Not an I2S underrun (patch 0004 counts them: zero during playback).
- **Conclusion:** node + firmware are clean. Server timing must be proven on
  the real controller (Windows festival laptop, or a Pi). The Go controller
  should surface `onResync` lines in the Activity log, and its RadioDJ feeder
  must deliver PCM at a steady pace with a small jitter buffer.
- **Fix, layer 1:** snapserver resyncs when its read timer wakes later than
  one chunk, so `chunk_ms` is its stall tolerance. `chunk_ms = 50` (was 20)
  cut audible resyncs from ~5 to 1 per 2.5 min on the bench Mac; mid-tone
  blips gone. The one left was a 76 ms stall right after a stream start.
  Now the default in `controller/snapserver.conf`. Still to do: controller
  feeder keeps a ~100 ms backlog (layer 2); node soft-corrects small server
  shifts instead of mute + resync (layer 3, firmware patch).
- Test tooling: `snapserver.bench.conf` with a `file://` source plays a known
  tone file on the server's own clock. snapserver loops the file with a 100 ms
  idle gap, which the node hears as a blip (artifact, ignore).

Goal: hear synced audio on an Amped-ESP32-Plus from a Snapcast server on the
Mac, then find the lowest buffer it holds. That number becomes the Parade
profile.

## Hardware

- [ ] External WiFi antenna attached before power (the bench build taught us this)
- [ ] 24 V supply to the barrel/terminal input; check polarity
- [ ] Two 8 ohm speakers (HP5S-8) on L and R, or one speaker on L for now
- [ ] USB to the Mac for flashing and serial

## Flash stock firmware

1. Open the esparagus web installer: https://sonocotta.github.io/esparagus-snapclient/
   (Chrome or Edge; Web Serial). Pick Amped-ESP32, classic ESP32 variant.
2. After flash, set WiFi via Improv in the same page.
3. Find the node's IP from the serial log or the router.

## Snapcast server on the Mac

```sh
brew install snapcast
snapserver -c controller/snapserver.conf
```

`controller/snapserver.conf` defines a TCP source on 4953 and a named pipe.
Feed audio:

```sh
ffmpeg -re -i ~/Music/test.flac -f s16le -ar 48000 -ac 2 tcp://127.0.0.1:4953
```

Open http://localhost:1780 (Snapweb) to see the client appear.

- [ ] Node appears in Snapweb within 30 s of boot
- [ ] Audio plays, correct pitch, both channels
- [ ] Volume slider in Snapweb changes level smoothly (this is the DAC volume;
      note how far up it has to go before the amp pot matters)

## Measure the buffer floor

In `snapserver.conf`, `buffer` is in ms. Start at 1000, then halve.

| buffer ms | 10 min, zero dropouts? | notes (RSSI, distance, obstacles) |
|-----------|------------------------|-----------------------------------|
| 1000      |                        |                                   |
| 500       |                        |                                   |
| 300       |                        |                                   |
| 200       |                        |                                   |
| 150       |                        |                                   |
| 120       |                        |                                   |

Snapweb shows client latency; the serial log shows underruns.

- [ ] Lowest buffer with zero dropouts over 10 min: ______ ms
- [ ] Same test with the node across the room / outside / through a wall

Also try `chunk_ms = 10` (default 20). Smaller chunks allow a smaller buffer at
the cost of more packets.

## Speakers A and B (fw 0.3.0)

One speaker on each output, music playing. In the console: Nodes → the node →
Speakers.

- [ ] Chirp A: the chime comes only from the left-output speaker. Chirp B: only the right
- [ ] Mute B: B goes silent, A keeps playing at the same level. Unmute
- [ ] Trim B to −6 dB: B clearly quieter, A unchanged. Move zone volume: both follow, B stays 6 dB under
- [ ] Trim B to −24 dB: still faintly audible (not muted)
- [ ] Reboot the node: the trim and mute come back as set
- [ ] Mute all, then unmute: both speakers come back with their trims

Logs without USB: `python3 firmware/tools/udplog.py <node ip>` from the same LAN.

## Setup page only on the setup network (fw 0.4.1)

- [ ] From the LAN, `http://<node ip>/` and `http://<node ip>/setup` don't connect (no web server)
- [ ] Forget the node in the console: its `civitone-xxxx` network appears; join it with a phone and the setup page opens by itself
- [ ] While the setup network is up, from the LAN `http://<node ip>/setup` answers 403
- [ ] Finish setup with a new adopt code: the node is adopted again and the setup network closes about a minute later

## Second node

- [ ] Flash the second board, same steps
- [ ] Both in one Snapcast group, stand between them: no echo or flange
- [ ] Repeat the buffer floor with two nodes; the worse one wins

## Record

Write the numbers into `docs/system-design.md` under Latency. Also record:
amp pot position that gives a comfortable max at DAC volume 100, and the die
temperature after an hour in a closed box if you have one.
