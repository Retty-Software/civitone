# Civitone system design (summary)

Full version with diagrams: `system-design.html`. Node setup:
`node-setup.md`. Control protocol: `control-protocol.md`.

## Parts

- **node**: Amped-ESP32-Plus (classic ESP32, external antenna) in an IP65 box,
  24 V, two HP5S-8 speakers. Firmware forked from esparagus-snapclient.
- **controller**: one Go binary. Starts snapserver, registry (SQLite),
  control WebSocket for nodes, console for operators. Runs on the festival
  laptop (Windows, RadioDJ) this year; a Pi 4 later.
- **console**: web app served by the controller.

## Rules that make the requirements hold

1. A node only dials out: control WebSocket (TLS, device token) and the
   Snapcast TCP stream. The controller never reaches a node; nodes never see
   each other. This is why locked-down WiFi works without WireGuard.
2. Adoption issues a device token the node keeps forever. Claim code is on
   the label. Forget revokes it.
3. The controller is on site with the sources. Latency equals the Snapcast
   buffer. Parade profile 200 ms (stretch 150), Festival 800 ms.
4. Volume lives in the DAC (PCM5122 digital volume). Amp pot is a set-once
   ceiling at install.
5. Heartbeat every 5 s: state, rssi, buffer_ms, sync_err_us, dropouts_1m,
   volume, temp_c, vin, stream, fw, uptime, ip, ssid. Degraded after 2 missed,
   offline after 6.

## Latency (fill in from the bench)

| measurement | value |
|-------------|-------|
| lowest buffer, one node, 10 min, zero dropouts | ___ ms |
| lowest buffer, two nodes | ___ ms |
| chunk_ms used | ___ |
| chosen Parade profile | ___ ms |

## Ports

| what | port | direction |
|------|------|-----------|
| console + control WebSocket | 8080 (dev), 443 (deployed) | node/operator -> controller |
| snapserver stream | 1704 | node -> controller |
| snapserver JSON-RPC | 1705 | controller (local) |
| snapserver TCP source | 4953 | controller (local) |
