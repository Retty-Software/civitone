# Node control protocol

One WebSocket per node, always dialled by the node (system design rule 1).
Audio stays on the Snapcast stream (port 1704); this channel carries identity,
health and commands. Volume and stream routing stay in snapserver, which
already owns them; the controller changes them over JSON-RPC.

## Connection

- URL: `ws://<controller>:8080/node` in development, `wss://<controller>/node`
  deployed. The node stores `controller_url` in NVS. Until adoption sets it,
  the node derives it from the snapserver host: `ws://<snapserver_host>:8080/node`.
- TLS: `wss://` is verified against the ESP-IDF public CA bundle, so the
  hosted server and self-hosters with a real certificate (Let's Encrypt)
  work with no per-site setup. `ws://` is for LANs.
- Auth: an adopted node sends `Authorization: Bearer <device_token>` on the
  upgrade request. A node without a token says hello with the adopt code
  from its setup page and is held as *pending* until an operator approves it.
  See Adoption below.
- Reconnect with backoff 1 s, 2 s, 4 s ... capped at 30 s, plus up to 20 %
  jitter.
- One JSON object per text frame. Unknown fields are ignored on both sides,
  unknown `t` values are logged and ignored.

## Node -> controller

`hello`, first frame after every connect:

```json
{"t":"hello","device_id":"civ-1a2b","mac":"24:0A:C4:00:1A:2B","fw":"0.1.0+5cda3a7",
 "board":"amped-esp32-plus","adopt_code":"7QK-M2X"}
```

`adopt_code` is sent only while unadopted. `device_id` is `civ-` plus the last four
hex digits of the MAC and never changes.

`hb`, every 5 s:

```json
{"t":"hb","state":"playing","rssi":-59,"buffer_ms":1000,"sync_err_us":-2383,
 "dropouts_1m":0,"underruns":4,"volume":50,"muted":false,
 "stream":"Music","uptime":8123,"ip":"192.168.1.40","ssid":"PoleNet","heap":4168495,
 "speakers":{"a":{"trim_db":0,"muted":false},"b":{"trim_db":-6,"muted":false}}}
```

`speakers` (speaker nodes, fw 0.3.0+) is what the DAC is applying now; the
console compares it with what the controller saved.

Fields the board cannot measure are omitted, never zero-filled. The
Amped-ESP32-Plus has no supply-voltage or temperature sensor the firmware can
read, so there is no `vin` or `temp_c`. `state`: `connecting` | `idle` | `playing` | `error`.

`ack`, once per command:

```json
{"t":"ack","id":17,"ok":true}
{"t":"ack","id":18,"ok":false,"err":"unknown cmd"}
```

## Controller -> node

`cmd`, each with a controller-chosen `id`:

| cmd | fields | effect |
|-----|--------|--------|
| `identify` | `seconds` | node: log (LED later). Controller also plays a chime on it through snapserver's hidden `Chime` stream |
| `reboot` | | restart after the ack is sent |
| `set_server` | `host`, `port` | snapserver address, saved in NVS, reconnects audio |
| `set_controller` | `url` | control URL, saved in NVS, reconnects this channel |
| `set_speakers` | `a`, `b`: each `{"trim_db": -24..0, "muted": bool}`, either may be left out; `save` (default true) | speaker nodes: trim (0.5 dB steps, below the zone volume) and mute for speaker A (left output) and B (right), applied in the DAC and kept in NVS. `"save": false` applies without saving (the controller's chirp). The controller resends the saved settings on every connect |
| `update_firmware` | `path` (or `url`), `sha256`, `size` | speaker nodes, fw 0.4.0+: download the image from the controller (`path` is relative to the control URL, `/firmware/<sha256>.bin`, sent with `Authorization: Bearer <token>` and `X-Civitone-Device`), check the SHA-256, write the spare OTA slot, reboot. The heartbeat carries `ota` (`downloading` with `pct`, `rebooting`, `failed` with `err`) and `fw_probation` until the new image is welcomed as adopted, which marks it good; a reboot before that rolls back. The console only sends images uploaded to `/api/firmware`. There is no other way in: the upstream TCP 8032 OTA server is removed (patch 0011) |
| `adopt` | `token`, `name`, `snap_host`, `snap_port` | store token and snapserver address, restart, reconnect with the token |
| `forget` | | wipe the token, restart, come back pending |

A pending node accepts only `identify` and `adopt` from the controller.

## Adoption

See [node-setup.md](node-setup.md) for the whole flow. On the wire:

- Unadopted, `hello` carries `adopt_code` (made in the console). Wrong or
  expired code: close **4003**. Accepted: the controller answers
  `{"t":"welcome","state":"pending"}` and holds the node until an operator
  approves (or at once if the code auto-approves).
- Approval sends `cmd` `adopt` with `token`, `name`, `snap_host`,
  `snap_port`. The node stores them, restarts, and reconnects with
  `Authorization: Bearer <token>`; `welcome` then says `"adopted"`.
- Unknown token: close **4001**; the node clears it and goes back to setup.
- `POST /api/nodes/{id}/approve {"name"}`, `/reject`, `/forget`, `/rename`;
  `POST /api/adopt-codes {"auto_approve","minutes"}`, `GET`, `DELETE
  /api/adopt-codes/{code}`.

## Health on the controller

Missing heartbeats: healthy -> degraded after 2 (10 s) -> offline after 6
(30 s), per system design rule 5. Socket close marks offline immediately.
