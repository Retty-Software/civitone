# Node setup and adoption

How a speaker node gets onto Wi-Fi and into a Civitone server. Works the same
for a self-hosted controller and the hosted service; nothing depends on a
factory label.

## For the person setting up a pole

1. In the console: **Setup → New adopt code**. Codes look like
   `7QK-M2X`, last an hour, and work for as many nodes as you set up in that
   hour. Tick **Approve automatically** to skip step 5 (handy for a batch
   install; leave it off otherwise).
2. Power the node. With nothing to connect to, it opens an open Wi-Fi network
   named `civitone-xxxx` (the last four hex digits of its MAC).
3. Join it with a phone. The setup page opens by itself; if not, browse to
   `http://192.168.4.1/setup`.
4. Pick the Wi-Fi network and type its password, check the server address
   (pre-filled with the hosted server in official builds), enter the adopt
   code, **Connect**. The page shows each step: joining Wi-Fi, reaching the
   server, code accepted.
5. In the console the node appears under **Waiting for approval**. **Identify**
   plays a chime on it so you know which pole it is. Name it, **Approve**.
6. The setup network closes 15 s after the node is on Wi-Fi and accepted.

## When the setup network comes back

- No saved Wi-Fi network.
- Neither a device token nor an adopt code (never set up, **Forget** in the
  console, or the server rejected it).
- No Wi-Fi connection for 1 minute (`CONFIG_CIVITONE_SETUP_AFTER_S`). The node
  keeps retrying its networks in the background, slowly, so the setup network
  stays usable.

## Moving a node

Unplug it, take it to the new spot, power it. If none of its saved networks is
there, the setup network appears after a minute. On an adopted node the setup
page only asks for Wi-Fi and keeps the adoption ("Adopted on …"); tick **Move
to another server, or re-add it** to enter a server and a new adopt code
instead. A known network with an empty password keeps the saved password.

## Forget and Reset (console → Node detail)

| | token on server | node's Wi-Fi | node's server/snapserver address | after |
|---|---|---|---|---|
| **Forget** | revoked | kept | cleared | setup network, waiting for a new adopt code |
| **Reset** | revoked | erased | cleared | setup network, blank |

Forget works on an offline node too: its token is dead, so the next time it
connects it gets 4001 and goes back to setup. Reset needs the node online.
Both remove the node from snapserver's client list.

## Server address

| typed | control channel |
|-------|-----------------|
| `civitone.example.com` | `wss://civitone.example.com/node` (TLS, public CA bundle) |
| `192.168.1.10:8080`, `pi.local:8080` | `ws://…/node` (LAN, no TLS) |
| `ws://…`, `wss://…`, `http://…`, `https://…` | as given, path `/node` |

The snapserver address is never typed: approval sends it (`--snap-host`, or by
default the host the node dialled, port `--snap-port` 1704).

## Wi-Fi networks

Up to three, newest first, in NVS (`civitone/net0_ssid` ... `net2_pass`).
After four failed attempts on one the node tries the next. Nodes provisioned
before setup mode keep their existing network: it is imported as the first.

## Security notes

- The setup network is open and only exists while the node needs setup.
- The node runs no web server otherwise (fw 0.4.1+). The setup page's server
  starts and stops with the setup network and answers only requests that
  arrive on it, so nobody on the node's normal Wi-Fi can change its Wi-Fi,
  server or adopt code. snapclient's own web UI is off.
- Firmware comes only from the controller, with the node's device token, and
  is checked against the SHA-256 the controller sends (fw 0.4.0+; there is no
  OTA port).
- Adopt codes are short-lived and rate-limited: 10 wrong codes from one
  address per 10 minutes, then that address is refused for 10 minutes.
- Approval is on by default, so a leaked code still needs an operator.
- Device tokens are 32 random bytes; the server keeps only their SHA-256.
- Close codes: **4003** adopt code wrong/expired or node rejected (node clears
  the code, setup page says why); **4001** token unknown (node clears the token,
  back to setup).
