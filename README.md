# camonitor

Bridge multiple Dahua VTO (or other RTSP) cameras into a single WebRTC web
page, with a per-stream HD/SD toggle and a one-click door-unlock button.

The whole server is one static Go binary. The only third-party Go dependency
is [pion/webrtc](https://github.com/pion/webrtc); the RTSP client is hand-
rolled in `rtsp.go` (RTSP-over-TCP, HTTP-Digest, single H.264 media).

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

If your camera's main stream is H.265 (some newer Dahua firmware), the
RTSP client will fail to find an H.264 media and the tile will show no
video; flip back to SD. (H.265 support is a future extension.)

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

## Run from source

Requires Go 1.26 or newer.

```sh
go build -o camonitor .
./camonitor -config config.json
```

## License

MIT — see [LICENSE](LICENSE).
