# Civitone node firmware

Target: Sonocotta Amped-ESP32-Plus, classic ESP32 (WROVER-N8R8), external
antenna. Framework: ESP-IDF.

## Build and flash

```sh
git submodule update --init --recursive   # upstream snapclient, pinned
./build.sh                                # Docker: espressif/idf:v5.5.1
PATH=<venv with esptool>:$PATH build/amped-esp32-plus/flash.sh /dev/cu.usbserial-XXXX
```

`snapclient/` is upstream CarlosDerSeher/snapclient at 5cda3a7 (the commit
Sonocotta builds from), never edited in place. `build.sh` copies it to
`build/src`, applies `patches/*.patch` in order, and builds with
`configs/sdkconfig.amped-esp32-plus` (from esparagus-snapclient).
Flashing keeps WiFi and server settings (NVS at 0x9000).

| Patch | What |
|---|---|
| 0001 | Amp enable on once at boot, mute in DAC only (no TPA3110 pops). Volume 1-100% maps to -60..0 dB. |
| 0002 | Log jumps >2 ms in the server clock offset estimate; log medians on hard resync. (diagnostic) |
| 0004 | I2S DMA 4 x 1023 frames (~85 ms headroom, was ~43 ms); count and log DMA underruns. |
| 0005 | WiFi power save off (IDF default MIN_MODEM gave 0.2-1.3 s ping spikes and choppy audio) and HT20 instead of HT40. Oct 12: loss 6% -> 1.8%, worst ping 1.3 s -> 125 ms at -59 dBm. |
| optional/0003 | Poll PCM5122 status registers 4x/s and log changes. (diagnostic, not applied) |

Upstream bug found: Sonocotta's installer manifest writes `ota_data_initial`
at 0x11000, inside NVS; the otadata partition is at 0x1d000. That is why the
stock image logs `ota data partition invalid` on every boot. `flash.sh` uses
0x1d000.

## Remaining plan

## Plan

1. Fork https://github.com/sonocotta/esparagus-snapclient into this
   directory (git subtree or submodule, decide then). Confirm it builds and
   behaves identically to the web-installer image.
2. Add a `civitone_ctrl` component:
   - persistent identity in NVS: device_id, device_token, controller_url,
     saved WiFi list, zone, last volume, last profile
   - outbound WebSocket to the controller, reconnect with backoff
   - heartbeat every 5 s (fields in docs/system-design.md)
   - commands: set_volume, mute, set_stream, set_profile, identify, reboot,
     update_firmware, forget
   - adoption state machine: FACTORY -> PROVISIONED -> PENDING -> ADOPTED
3. Replace the stock provisioning with the Civitone setup flow
   (docs/node-setup.md): setup AP `civitone-XXXX`, captive page, several
   WiFi networks, controller URL (mDNS pre-fill), claim code screen.
4. Local status page at the node's IP, read only plus identify, re-run setup,
   hold-to-factory-reset.
5. RGB LED states per the legend in the node UI mockup.
6. OTA from the controller with rollback if the control channel does not
   come up within 2 minutes.

## Hardware notes carried over from the hand-built node

- No onboard antenna on WROOM-32U; the Amped board has a u.FL connector, use it.
- Volume is DAC digital volume; the amp trim pot is set once.
- Die temperature is the proxy for enclosure heat; warn above 70 C.
