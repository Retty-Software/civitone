# Civitone

Synchronized audio for outdoor speaker networks. Weatherproof speakers on
poles, each driven by an ESP32 board, play as one system: music to every zone
in sync, live announcements from a paging mic, and a web console to run it all.

- **Speaker nodes** join over Wi-Fi from any network and keep playing in sync,
  with trim and mute for each of their two speakers.
- **One controller** streams the audio (Snapcast), holds the node registry and
  serves the console. It runs on a small cloud server or a laptop.
- **Sources** are computers running Civitone Source (any sound card or app,
  such as RadioDJ) or ESP32 input boards with a push-to-talk paging mic.
- **Setup without tools**: a new node opens its own Wi-Fi with a setup page;
  pick a network, enter your server and an adopt code, approve it in the
  console.
- **Firmware updates from the console**, downloaded by each node with its own
  device token, checked and rolled back automatically if the new image can't
  reach the controller.

Run your own controller from this repository, or use the hosted service at
[civitone.com](https://civitone.com): nodes come set up for it out of the box.

## Repository

```
controller/   Go. The controller (cmd/civitone), Civitone Source (cmd/civitone-source),
              and the web console (web/static).
firmware/     ESP-IDF. Speaker node firmware: a patched snapclient
              (firmware/snapclient, upstream as a submodule) plus Civitone's
              control channel and setup page. Input board firmware in firmware/source.
installer/    Windows installer for Civitone Source.
deploy/       Run your own controller: Docker (deploy/docker) or a Debian server.
docs/         System design, control protocol, node setup, bench checklist.
```

## Hardware

- **Speaker node**: Sonocotta Amped-ESP32-Plus (ESP32, PCM5122 DAC, TPA3110
  amp), 24 V supply, two speakers. Classic ESP32 with an external antenna works
  best on poles.
- **Input board** (optional paging mic): ESP32 Audio Kit (ES8388) with a
  push-to-talk switch.

## Run your own controller

**With Docker** (any Linux server with a DNS name pointing at it): see
[deploy/docker](deploy/docker/README.md). One container runs the controller and
Snapcast; Compose adds Caddy for HTTPS. The console signs people in with
one-time links, and the [admin API](docs/admin-api.md) lets scripts set
access: owners, operators, and guests who get a few poles and sources only
during their event's hours.

**Without Docker**, on a Debian 12/13 server with a DNS name pointing at it:

```sh
deploy/deploy.sh root@<server> <hostname> <email-for-lets-encrypt>   # first time
deploy/deploy.sh root@<server>                                       # updates
```

That installs the controller and Snapcast, nginx with TLS, and a login for the
console. On each node's setup page, enter your hostname instead of the default.

## Develop

```sh
brew install go snapcast              # or your platform's packages
cd controller
go test ./...
go run ./cmd/civitone                 # starts snapserver, console on http://localhost:8080
```

`cmd/fakenode` and `cmd/fakesource` stand in for hardware. Firmware builds in
Espressif's ESP-IDF container:

```sh
git submodule update --init --recursive
firmware/build.sh amped-esp32-plus    # -> firmware/build/amped-esp32-plus/*.bin
```

Flash a new board once over USB (`flash.sh` in the build output); after that,
upload images in the console under Setup → Firmware. Logs over Wi-Fi:
`python3 firmware/tools/udplog.py <node ip>`.

## License

- `firmware/` is GPL-3.0-or-later (it builds on snapclient, which is GPL-3.0).
- Everything else is AGPL-3.0-or-later: if you run a modified controller as a
  service for others, you must offer them its source.

See `LICENSE`, `firmware/LICENSE`, and `TRADEMARKS.md`. "Civitone" and the
Civitone logo are trademarks of Retty Software; the licenses cover the code,
not the name.
