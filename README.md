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
  -p 8080:8080 \
  -v /etc/camonitor/config.json:/etc/camonitor/config.json:ro \
  ghcr.io/khaledez/camonitor:latest
```

Open <http://localhost:8080>. Each configured camera gets a tile in the
grid with an `HD/SD` toggle (top-left) and an `open door` button
(top-right).

The published image is multi-arch (`linux/amd64`, `linux/arm64`) and is
built `FROM scratch` — only the static binary is in it (~14&nbsp;MB).

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

## Run from source

Requires Go 1.26 or newer.

```sh
go build -o camonitor .
./camonitor -config config.json
```

## License

MIT — see [LICENSE](LICENSE).
