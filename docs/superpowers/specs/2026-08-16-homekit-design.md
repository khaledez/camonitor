# HomeKit support for camonitor

Status: approved 2026-08-16. Phase 1 implements the bridge; phase 2 adds
camera and doorbell accessories.

## Goal

Expose camonitor's devices to Apple Home so the user can, from an iPhone
or the Apple TV acting as a Home hub:

- unlock the two Dahua VTO door stations (`FrontDoor`, `Gate`),
- control the Gree air conditioner (`Living Room AC`),
- watch the two VTO cameras and receive doorbell notifications (phase 2).

Pairing happens by scanning a setup code, surfaced the same two ways the
WhatsApp pairing code already is: a panel in the web UI and an ASCII
rendering on stdout.

## Constraints that shaped the design

**Cameras cannot be bridged.** HomeKit requires camera accessories to be
standalone — each one pairs separately. Homebridge solves this with
"external accessories" that share the bridge's setup code and are added
individually in the Home app; camonitor does the same. One code, three
"Add Accessory" taps.

**HomeKit camera streams are H.264 over SRTP.** The two Tiandy cameras
(`Front`, `South`) deliver H.265 on both main and sub streams and are
therefore ineligible without transcoding. Bundling ffmpeg was rejected:
it would end the single-static-binary, `FROM scratch`, ~65 MB property
of the image and add real CPU load on the node. The two Dahua VTOs are
H.264 and are the ones that matter, since they are the ones with doors
and bells. If the Tiandy web UI can be reconfigured to emit H.264 on
`stream2`, they become eligible with a config change only.

**No HomeKit Secure Video.** HKSV needs iCloud+ and a protocol
implementation `brutella/hap` does not have. Recording stays in the
existing web UI and WhatsApp paths.

**Pairing is LAN-only.** Bonjour does not traverse the tailnet. The
user's Apple TV acts as a Home hub, so away-from-home control works
through Apple's relay; the tailnet web UI remains the fallback.

## Library choice

`github.com/brutella/hap`, the maintained Go HAP implementation. It
provides pair-setup and pair-verify (SRP-6a, Ed25519, HKDF,
ChaCha20-Poly1305), the encrypted session transport, the characteristic
model, a filesystem-backed pairing store, DNS-SD announcement via
`brutella/dnssd`, and the `service.CameraRTPStreamManagement` and `rtp`
TLV types needed for phase 2.

Hand-rolling HAP was rejected. The project's habit of hand-rolling
protocols (`rtsp.go`, `gree.go`, `digest.go`) is deliberate and good, but
HAP pairing is security-critical cryptography guarding a front-door lock;
a bug there has a different blast radius than a bug in an RTSP parser.

A sidecar Homebridge container was rejected: it splits configuration
across two places, adds a Node runtime, and would still need a custom
plugin for the doorbell path.

The README's "the only third-party Go dependency is pion/webrtc" claim is
already historical — `whatsmeow`, `tailscale.com`, `sipgo`, and
`modernc.org/sqlite` are all in `go.mod`. The README is updated to match
reality as part of this work.

## Architecture

### Files

| File | Contents |
| ---- | -------- |
| `homekit.go` | Config type, server lifecycle, pairing store, QR/status/unpair HTTP handlers |
| `hkbridge.go` | Phase 1 accessories: the two locks and the AC, bound to `DoorClient` and `GreeController` |
| `hklock.go` | Lock state machine (relock timer, jam reporting), clock-injectable |
| `hkclimate.go` | Gree ↔ HeaterCooler characteristic mapping, both directions |
| `hkcamera.go` | Phase 2: doorbell + camera accessory, snapshot serving, stream negotiation |
| `hksrtp.go` | Phase 2: RTSP → SRTP forwarder |

`door.go`, `gree.go`, `bell.go`, and `rtsp.go` keep their current shape.
`GreeController` gains exported `Status()` and `Set(ctx, params)` so
HomeKit and the web UI share one path into the device rather than
HomeKit reaching through an HTTP handler.

### Accessory topology

| HAP server | TCP port | Accessories |
| ---------- | -------- | ----------- |
| Bridge `camonitor` | `port` (51826) | `FrontDoor` lock, `Gate` lock, `Living Room AC` |
| `FrontDoor` (phase 2) | `port+1` | Doorbell + camera |
| `Gate` (phase 2) | `port+2` | Doorbell + camera |

All servers share one setup code, so phase 2 is purely additive: no
re-pairing of the bridge, no lost automations.

The locks live on the bridge, not on the camera accessories. The Home
app's camera live view surfaces accessories assigned to the **same room**
as the camera, so putting each doorbell and its lock in one room gives
the unlock-from-live-view experience without a duplicate lock tile.

### Configuration

A new optional top-level block. Its absence disables HomeKit entirely,
matching how `gree` and `whatsapp` already behave.

```json
"homekit": {
  "pin": "031-45-154",
  "port": 51826,
  "store": "/var/lib/camonitor/homekit",
  "relock_after": "5s"
}
```

| field | required | meaning |
| ----- | -------- | ------- |
| `pin` | yes | Setup code, `XXX-XX-XXX`. Fixed in config rather than generated so wiping the store and re-pairing does not change the code. |
| `port` | no | Base TCP port for the bridge. Defaults to 51826. Camera accessories use `port+1`, `port+2`, … in stream order. |
| `store` | no | Directory for HAP pairing state. Defaults to `/var/lib/camonitor/homekit`. |
| `relock_after` | no | How long the lock reports Unsecured after a successful open. Go duration string, defaults to `5s`. |

`store` lands on the existing `monitor-state` PVC alongside `wa.db` and
the bell history, so no new volume is needed and
`readOnlyRootFilesystem: true` stays.

Apple rejects a handful of setup codes as too weak (`000-00-000`,
`111-11-111`, `123-45-678`, and similar). Config validation rejects those
at startup with a clear message rather than letting pairing fail
mysteriously later.

### Pairing UX

A 🏠 button in the web UI header opens a panel mirroring the existing
WhatsApp one: the `X-HM://` QR code, the setup code as text, the number
of paired controllers, and an "unpair all" button. The same code is
printed to stdout as a half-block ASCII QR at startup when unpaired.
`rsc.io/qr` and `mdp/qrterminal/v3` are already dependencies.

New endpoints:

- `GET /homekit/status` → `{"configured":bool,"paired":bool,"controllers":int,"pin":string}`
- `GET /homekit/qr.png` → PNG of the `X-HM://` setup payload
- `POST /homekit/unpair` → removes all pairings, re-enters pairable state

When `homekit` is absent from config, `/homekit/status` still answers
`{"configured":false}` so the web UI can render without a conditional
fetch — the same pattern `/gree/status` and `/wa/status` use.

## Phase 1 accessories

### Locks

One `LockMechanism` per stream with `door: true`.

The Dahua relay is a momentary pulse and the VTO reports no lock state,
so `LockCurrentState` is inferred rather than sensed:

- A write of `LockTargetState = Unsecured` calls `DoorClient.Open()`.
  On success `LockCurrentState` becomes Unsecured and a `relock_after`
  timer flips both characteristics back to Secured.
- On failure `LockCurrentState` becomes Jammed (2), then Secured after a
  short beat. Reporting the failure matters: a tile that always claims
  success is worse than no tile.
- A write of `LockTargetState = Secured` cancels any pending timer and
  sets Secured immediately. There is no lock command to send; the relay
  only opens.

Concurrent unlocks of the same door restart the timer rather than
stacking. The state machine takes an injected clock so the timer is
testable without sleeping.

### Air conditioner

One `HeaterCooler` service, mapped against the existing `GreeStatus`:

| HomeKit characteristic | Gree field | Notes |
| ---------------------- | ---------- | ----- |
| `Active` | `Power` | |
| `CurrentTemperature` | `RoomTemp` | Falls back to `SetTemp` when Gree reports 0 (unit off or no sensor); HomeKit requires a plausible value. |
| `TargetHeaterCoolerState` | `Mode` | auto ← 0, heat ← 4, cool ← 1 |
| `CurrentHeaterCoolerState` | `Power` + `Mode` | inactive / idle / heating / cooling |
| `CoolingThresholdTemperature` | `SetTemp` | a write to either threshold sets `temp` |
| `HeatingThresholdTemperature` | `SetTemp` | |
| `RotationSpeed` | `FanSpeed` | 0–100 in steps of 20 ↔ 0–5, so 0% is Gree's fan-auto |
| `SwingMode` | `SwingV` | disabled ↔ 0, enabled ↔ `SwUpDn` 1 (full swing); the fixed louvre positions 2–6 have no binary equivalent and read as disabled |
| `TemperatureDisplayUnits` | `TempUnit` | |

Fan-auto keeps Gree's own encoding rather than being dropped: HomeKit has
no separate auto setting for a thermostat's fan, and inventing a
percentage for it would round-trip into an explicit speed the user never
chose. It costs one line in the README.

Deliberately omitted: `Turbo`, `Quiet`, `Health`, `Sleep`, `Air`,
`EnergySave`, and `Light`.

Two `Switch` services, `AC Dry` and `AC Fan Only`, cover the modes
HeaterCooler cannot express. They are mutually exclusive: turning one on
sets `Mode` to 2 or 3 and turns the other off; writing
`TargetHeaterCoolerState` turns both off. While the unit is in dry or
fan-only, `TargetHeaterCoolerState` reports its last heat/cool/auto
value — lossy, but `Active` still reports power truthfully, which is
what the tile is judged on.

### Freshness

`GreeController.refresh()` already polls every 10s so remote-driven
changes are visible. It gains a subscription hook that pushes updated
values into the HomeKit characteristics, so Home app tiles update via
HAP events rather than only on read.

## Phase 2 accessories

One standalone `hap.Server` per door stream, each with its own
subdirectory under `store`.

Services: `Doorbell` (primary), `CameraRTPStreamManagement` ×2 so an
iPhone and the Apple TV can watch simultaneously, and stub `Microphone`
and `Speaker` services, muted. No two-way audio in this phase.

**Doorbell.** `BellBus.Subscribe()` already delivers debounced ring
events and `BellBus.LatestSnapshot()` already caches the JPEG captured at
the instant of the press. A ring fires `ProgrammableSwitchEvent =
SinglePress`; HomeKit's snapshot request serves the cached JPEG, falling
back to a live `FetchSnapshot` when it is stale or missing. This is the
rich notification, built almost entirely from existing machinery.

**Streaming.** On `SetupEndpoints` iOS supplies its address and the SRTP
master key and salt; on `SelectedStreamConfiguration` it selects
resolution, framerate, bitrate, MTU, payload type, and SSRC. camonitor
then opens a dedicated RTSP session via `rtsp.go` — main stream for
requests at 1080p or above, sub stream below — rewrites SSRC, payload
type, and sequence numbers on each H.264 RTP packet, encrypts with
`pion/srtp/v3`, and sends UDP to the iOS endpoint. `pion/srtp/v3` is
already in the dependency graph via `pion/webrtc`.

Two known sharp edges:

- iOS rejects a camera accessory that advertises no audio codec. We
  advertise Opus in `SupportedAudioStreamConfiguration` and never send
  audio.
- The Dahua does not respond usefully to RTCP PLI, so first-frame
  latency is bounded by the camera's own IDR interval, and a stalled
  stream is recovered by tearing down and re-opening the RTSP session
  rather than by requesting a keyframe.

## Failure behaviour

| Condition | Behaviour |
| --------- | --------- |
| `homekit` absent from config | Nothing starts. No behaviour change anywhere else. |
| Invalid `pin` | Fatal at startup with a message naming the constraint. |
| Pairing store cannot be opened | HomeKit is disabled with a log line; doors, web UI, WhatsApp, and Gree keep working. Pairing without persistence is worthless, but it is not worth killing the service over. |
| Port or mDNS conflict | Fatal at startup. Half-advertising is worse than not starting. |
| Door open fails | `LockCurrentState` → Jammed, then Secured. Logged. |
| Gree offline | Reads serve last-known values; writes return an error so the Home app shows "No Response" rather than silently accepting. |
| RTSP session dies mid-stream | SRTP session torn down; iOS shows the stream as ended. |

## Testing

The repo currently has no tests. This work adds the first ones, confined
to pure logic with no network and no hardware:

- Gree ↔ HomeKit characteristic mapping in both directions, table-driven
  across every mode, fan speed, and the `RoomTemp == 0` fallback. This is
  where the real bugs are expected.
- Lock state machine: successful open, relock after the configured
  delay, failure → Jammed → Secured, explicit re-lock cancelling a
  pending timer, and concurrent opens restarting rather than stacking.
  Uses an injected clock; no sleeping.
- Setup-code validation, including Apple's rejected codes.
- Phase 2: TLV round-trips for `SetupEndpoints` and
  `SelectedStreamConfiguration`, and the RTP header rewrite against a
  golden packet.

`DoorClient` and `GreeController` are consumed through narrow interfaces
at the HomeKit boundary so tests use fakes.

Hardware verification on zima closes each phase: pair from the iPhone,
confirm all tiles appear, unlock both doors, exercise every AC mode and
fan speed, and — in phase 2 — press each bell and open the live view
from the notification.

## Deployment

`deploy/k8s/deployment.yaml` gains the three TCP ports (51826–51828) for
documentation value; `hostNetwork: true` already makes them reachable and
already puts Bonjour on the node's UDP/5353. One thing to verify on zima:
nothing else may bind 5353, or `dnssd` will collide. Talos does not run
avahi, so this is expected to be clear — but it is the first thing to
check if the Home app cannot discover the bridge.

The README gains a HomeKit section covering config, pairing, what each
accessory exposes, and the H.265 limitation, and its dependency claim is
corrected.
