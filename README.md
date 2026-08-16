# camonitor

Bridge multiple Dahua VTO (or other RTSP) cameras into a single WebRTC web
page, with a per-stream HD/SD toggle, a one-click door-unlock button, and
an optional Gree Wi-Fi air-conditioner monitoring/control panel.

It also bridges the doors and the air conditioner into Apple HomeKit, so
they can be controlled from the iPhone Home app and Siri.

The whole server is one static Go binary. WebRTC comes from
[pion](https://github.com/pion/webrtc) and HomeKit from
[brutella/hap](https://github.com/brutella/hap); the RTSP client is hand-
rolled in `rtsp.go` (RTSP-over-TCP, HTTP-Digest, single H.264 or H.265
media) and the Gree client is hand-rolled in `gree.go` (AES-128-ECB
JSON-over-UDP on port 7000).

## Run with Docker (recommended)

```sh
# 1. Create a config file (see Configuration below).
cp config.example.json /etc/camonitor/config.json
$EDITOR /etc/camonitor/config.json

# 2. Pull and run. Pin to a specific version (e.g. :0.1.0) for production
#    or use :latest while you're trying it out. Note: image tags drop the
#    leading "v" — git tag v1.2.3 → image tag 1.2.3.
docker run --rm \
  --name camonitor \
  --network host \
  -v /etc/camonitor/config.json:/etc/camonitor/config.json:ro \
  -v /var/lib/camonitor:/var/lib/camonitor \
  ghcr.io/khaledez/camonitor:latest
```

Open <http://localhost:8080> (or `http://<host-ip>:8080` from another
device on the LAN). Each configured camera gets a tile in the grid with
an `HD/SD` toggle (top-left) and an `open door` button (top-right).

The bind mount on `/var/lib/camonitor` only matters if you enable the
WhatsApp bell-notification feature — it persists the linked-device session
so you only scan the QR code once. Skip it if you're not using WhatsApp.

The published image is multi-arch (`linux/amd64`, `linux/arm64`) and is
built `FROM scratch` — only the static binary is in it. With WhatsApp /
SIP support bundled in it now sits at ~65&nbsp;MB (modernc.org/sqlite is
the bulk of the growth).

SIP runs on UDP/5060 by default. `--network host` is the simplest way to
expose that port; under bridge networking add `-p 5060:5060/udp` and set
`sip.contact_host` to the host's LAN IP so the VTO can route INVITEs back.

### Networking — reaching cameras on your LAN

camonitor makes outbound RTSP and HTTP connections to each configured
camera. Those cameras almost always live on the host's local network
(e.g. `192.168.x.x`), and the container needs a path to them.

`--network host` is the simplest and most reliable way to make that
happen. The container shares the host's network namespace, so it sees
the LAN exactly as the host does — no NAT, no port juggling, and the
camera sees the host's real IP (which matters if you've configured an
allowlist on the device). With `--network host` you don't need
`-p 8080:8080`; the service binds straight to port 8080 on the host.

Alternatives if host networking isn't an option:

- **Default bridge with port publish** — replace `--network host` with
  `-p 8080:8080`. On Linux this almost always works because Docker NATs
  the container's outbound traffic onto the LAN. Cameras will see the
  host's IP for inbound requests.

- **Docker Desktop on macOS/Windows** — host networking is supported
  natively as of Docker Desktop 4.34. On older versions, fall back to
  `-p 8080:8080`; outbound bridge traffic to LAN devices still works,
  it just routes through Docker Desktop's VM.

- **macvlan** — only worth it if you need the container to have its own
  IP on the LAN (e.g. to satisfy a strict camera-side allowlist that
  rejects the host's IP). Significantly more setup; refer to the
  [Docker macvlan docs](https://docs.docker.com/network/drivers/macvlan/).

## Configuration

`config.json` lives wherever you point `-config` (default `./config.json`).
The schema is intentionally thin — pass per-camera connection details and
let camonitor build the Dahua-specific URLs.

```json
{
  "listen": ":8080",
  "streams": [
    {
      "id":   "vto1",
      "name": "Front Door",
      "host": "192.168.88.200",
      "user": "admin",
      "pass": "your-password"
    },
    {
      "id":   "vto2",
      "name": "Gate",
      "host": "192.168.88.202",
      "user": "admin",
      "pass": "your-password"
    }
  ]
}
```

| field      | required | meaning |
| ---------- | -------- | ------- |
| `listen`   |          | HTTP listen address. Defaults to `:8080`. |
| `id`       | yes      | Stable stream identifier — used in URLs and as a DOM key. |
| `name`     |          | Display label shown in the grid. Defaults to `id`. |
| `host`     | yes¹     | Camera IP/hostname. Default RTSP port (554) and HTTP port (80) are assumed. |
| `user`     |          | Account username. Used for both RTSP and the door-open HTTP endpoint. |
| `pass`     |          | Account password. |
| `door`     | no       | Set `true` for door stations (VTO). Shows the "open door" button and subscribes to the Dahua bell/event stream. Leave unset for plain cameras. |
| `codec`    | no       | Video codec delivered over RTSP: `h264` (default) or `h265`. Use `h265` for HEVC cameras (e.g. Tiandy 4K). |
| `rtsp_path_template` | no | printf-style RTSP path with one `%d` placeholder filled with subtype+1 (1 = main/HD, 2 = sub/SD). Lets non-Dahua cameras (e.g. Tiandy `/stream1`, `/stream2`) reuse the HD/SD toggle. |
| `rtsp_url` | no       | Override for the full RTSP URL. Use this for non-Dahua cameras or non-default channels/subtypes. |
| `door_url` | no       | Override for the door-open HTTP endpoint. Use this if your VTO firmware exposes a different path. |
| `sip_ext`  | no       | SIP extension number to register on the VTO's built-in SIP server (e.g. `9901`). Set this to receive bell-press notifications; leave empty to disable SIP for this camera. |
| `sip_pass` | no       | Password for `sip_ext`. Configured on the VTO under *Talk → Management*. |

¹ `host` is required unless both `rtsp_url` and `door_url` (if you want
door support) are explicitly set.

The browser never sees `host` / `user` / `pass` — only `id` and `name` are
sent to the page, so passwords don't cross the wire to the viewer.

## HD vs SD

Each tile boots in HD (Dahua main stream, `subtype=0`) and can be flipped
to SD (sub stream, `subtype=1`) via the per-tile button. Switching cancels
the active RTSP connection and starts a fresh one at the new resolution
behind the same WebRTC track — no SDP renegotiation. The browser typically
freezes for a fraction of a second until the next keyframe arrives.

If your camera's main stream is H.265 (some newer Dahua firmware, and most
Tiandy 4K units), set `"codec": "h265"` on that stream so the WebRTC track
negotiates HEVC. Note that HEVC WebRTC playback requires a browser with
H.265 decode support (Safari, or Chrome/Edge with hardware decode);
Firefox does not support H.265 in WebRTC.

## Door open

The "open door" button posts to `/door/open?id=<streamID>`, which in turn
sends an HTTP-Digest-authenticated GET to the camera's
`/cgi-bin/accessControl.cgi?action=openDoor` endpoint. Both `qop=auth`
and the legacy no-`qop` digest flavours are supported; cameras that
return `401 Invalid Authority!` are usually answering a wrong digest
response, not a permissions failure.

If your VTO firmware uses a different path, set `door_url` per stream.

## Bell notifications (SIP + WhatsApp)

When the visitor presses the call button, the VTO sends a SIP `INVITE` to
every extension registered on its built-in SIP server. camonitor can act
as one of those extensions: on each ring it grabs a snapshot from the
camera, flashes the corresponding tile in any open browser, and ships
the image to a list of WhatsApp recipients.

Per-camera setup on the VTO (one-time, via its web UI):

1. *Network → SIP Server* — enable the built-in SIP server.
2. *Talk → Management* (sometimes *VTH Management*) — add an extension
   for camonitor. Note the extension number and password.
3. Add the new extension to the call/ring group so pressing the call
   button rings it.

Then in `config.json`:

```json
{
  "sip": { "bind": ":5060", "contact_host": "" },
  "whatsapp": {
    "session": "/var/lib/camonitor/wa.db",
    "recipients": ["+15551234567", "+15557654321"]
  },
  "streams": [
    { "id": "vto1", "host": "192.168.88.200", "user": "admin",
      "pass": "...", "sip_ext": "9901", "sip_pass": "..." }
  ]
}
```

`sip.bind` is the local UDP address camonitor listens on (default
`:5060`). `sip.contact_host` is what we advertise to the VTO; leave it
empty to auto-detect the host's primary LAN IPv4 — that works under
`--network host`. Set it explicitly if you're multi-homed or running
under bridge networking.

`whatsapp.session` is a SQLite file that holds the linked-device session.
Mount it on a persistent volume so first-time QR pairing only happens
once. `whatsapp.recipients` are E.164 phone numbers (the leading `+` is
optional; everything except digits is stripped).

### First-time WhatsApp pairing

On first run camonitor surfaces the pairing QR two ways — pick whichever
is easier:

- **Web UI** (preferred). Open the page in a browser; the side panel
  auto-opens with the QR and pairing instructions. The panel polls and
  refreshes the QR as whatsmeow rotates it (every ~20s).
- **Terminal** as a fallback. The same QR is printed to stdout as a
  half-block ASCII rendering, so `docker logs -f camonitor` works for
  headless setups.

In WhatsApp on your phone, go to *Settings → Linked Devices → Link a
Device* and scan. The session persists to `whatsapp.session`;
subsequent restarts skip the QR step.

The same side panel also shows the list of configured recipients and the
last 50 ring events with thumbnails. Click any thumbnail for a full-size
view of the snapshot the camera captured at the instant the bell rang.

Notes:

- camonitor replies `486 Busy Here` to every `INVITE` — the visitor's
  side at the VTO hears a beep, but no two-way audio is established
  (phase 2 work). The snapshot + WhatsApp + browser notification still
  fire on every press.
- Bell events are debounced per stream (~15s) so a kid mashing the
  button doesn't spam your phone.
- If `whatsapp` is omitted from config, the SSE/browser path still
  works — only WhatsApp delivery is skipped. Likewise, streams without
  `sip_ext` don't register; their RTSP/door functionality is unchanged.
- WhatsApp delivery here uses the unofficial whatsmeow library (same
  protocol as WhatsApp Web). It works with personal numbers and needs
  no Meta Business account, but is technically against WhatsApp ToS.
  For low-volume household use the practical risk is small; if you
  need an ToS-clean path, swap in the WhatsApp Cloud API.

## Gree air conditioner

Add an optional `gree` block to config to enable monitoring & control of a
Gree Wi-Fi unit (the JSON-over-UDP "Gree Smart" protocol on port 7000):

```json
{
  "gree": {
    "host": "192.168.88.44",
    "port": 7000,
    "name": "Living Room AC"
  }
}
```

| field  | required | meaning |
| ------ | -------- | ------- |
| `host` | yes      | AC IP/hostname. |
| `port` | no       | UDP port. Defaults to `7000`. |
| `name` | no       | Display label in the UI. Defaults to `host`. |

A ❄ button appears in the header when a unit is configured. The panel shows
current state (power, mode, set temp, fan speed, swing, room temp) and lets
you toggle power, change mode/temp/fan/swing. Status is polled every ~10s so
changes made from the physical remote show up too.

Backend endpoints: `GET /gree/status` returns the current state;
`POST /gree/set` applies one or more params (e.g. `{"power":1,"mode":1,
"temp":24,"fan":3}`). The server scans, binds, and caches the unit's
per-device AES key automatically.

## HomeKit

Add an optional `homekit` block to expose the door stations and the air
conditioner to the Apple Home app:

```json
{
  "homekit": {
    "pin": "031-45-154",
    "port": 51826,
    "store": "/var/lib/camonitor/homekit",
    "relock_after": "5s"
  }
}
```

| field | required | meaning |
| ----- | -------- | ------- |
| `pin` | yes | Setup code, with or without dashes. Fixed in config so wiping the pairing store doesn't change the code you wrote down. Apple rejects trivial codes (`111-11-111`, `123-45-678`, …) and so does camonitor, at startup. |
| `port` | no | Bridge TCP port. Defaults to 51826. |
| `store` | no | Directory for pairing state. Defaults to `/var/lib/camonitor/homekit`. Put it on the same persistent volume as `wa.db` so pairing survives restarts. |
| `relock_after` | no | How long a lock reports Unsecured after a successful open. Defaults to `5s`. |

A 🏠 button appears in the header. Open it, then in the Home app tap
**+ → Add Accessory** and scan the QR. The same code is printed to stdout
as an ASCII QR while unpaired, so `docker logs -f camonitor` works for a
headless setup.

What you get:

- **One lock per door station** (every stream with `"door": true`). The
  Dahua relay is a momentary pulse and reports no state, so the lock shows
  Unsecured for `relock_after` and then returns to Secured. A failed open
  shows as Jammed rather than silently succeeding.
- **The air conditioner** as a thermostat tile: power, room temperature,
  set point, heat/cool/auto, fan speed and swing. Gree's dry and fan-only
  modes have no HomeKit equivalent, so they get their own **AC Dry** and
  **AC Fan Only** switches on the same accessory. Fan speed maps
  20/40/60/80/100% to Gree's speeds 1–5; **0% is Gree's fan-auto**, since
  HomeKit has no separate auto setting for a thermostat's fan.

Notes:

- Pairing is LAN-only; Bonjour does not cross the tailnet. Away-from-home
  control needs an Apple Home hub (Apple TV or HomePod) on the same
  network. The tailnet web UI remains the fallback.
- Discovery uses mDNS on UDP/5353. Under `--network host` (or
  `hostNetwork: true`) that is the host's port — if something else on the
  host already binds it, the Home app will not find the bridge.
- Cameras are **not** here yet. HomeKit refuses to bridge cameras and
  requires H.264 over SRTP, so they need their own pairable accessories;
  that is phase 2. The Tiandy units stream H.265 and will stay web-UI-only
  unless they can be reconfigured to emit H.264.
- HomeKit Secure Video is out of scope. Recording stays in the web UI and
  WhatsApp paths.

The design is written up in
[`docs/superpowers/specs/2026-08-16-homekit-design.md`](docs/superpowers/specs/2026-08-16-homekit-design.md).

## Run from source

Requires Go 1.26 or newer.

```sh
go build -o camonitor .
./camonitor -config config.json
```

## License

MIT — see [LICENSE](LICENSE).
