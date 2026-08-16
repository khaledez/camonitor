# HomeKit support for camonitor

Status: approved 2026-08-16. Phase 1 (the bridge) shipped in v0.8.0 and
is verified on hardware. Phase 2 (camera and doorbell accessories) is
implemented; its streaming half is not yet hardware-verified.

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

Sharing the *code* does not mean sharing a *QR*. Each accessory has its
own setup id, and HomeKit matches a scanned payload by that id, so the
bridge's QR can only ever add the bridge. This was learned the hard way:
the first release served a single QR, and scanning it simply re-added the
bridge while the two doorbells stayed unpairable. The panel now shows one
QR per accessory still to be added.

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
| `homekit.go` | Config type, server lifecycle, pairing store, QR/status HTTP handlers |
| `hkbridge.go` | Phase 1 accessories: the two locks and the AC, bound to `DoorClient` and `GreeController` |
| `hklock.go` | Lock state machine (relock timer), clock-injectable |
| `hkclimate.go` | Gree ↔ HeaterCooler characteristic mapping, both directions |
| `hkcamera.go` | Doorbell + camera accessory, snapshot serving, stream negotiation |
| `hksrtp.go` | RTSP → SRTP forwarder |

`door.go`, `gree.go`, `bell.go`, and `rtsp.go` keep their current shape.
`GreeController` gains exported `Status()`, `Set(ctx, params)` and
`Name()` so HomeKit and the web UI share one path into the device rather
than HomeKit reaching through an HTTP handler.

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

### Removal, and why there is no unpair endpoint

Removing the accessory in the Home app is HomeKit's supported path, and
`hap` handles it correctly including re-advertising as pairable. camonitor
therefore exposes no unpair of its own. An earlier draft did, and review
found it caused three separate defects at once:

- A `hap.Server` cannot be restarted — it closes the `http.Server` it was
  built with and never rebuilds it — so bouncing the accessory left it
  permanently unreachable until the process restarted.
- Constructing a second server over the same accessories instead
  double-registers `hap`'s per-characteristic notification callbacks, so
  every event reaches the controller twice, accumulating per bounce.
- The endpoint is unauthenticated like the rest of this mux, so anyone on
  the LAN could clear pairings and then pair their own device — turning
  transient LAN access into persistent, remote-capable control of the
  door locks.

Recovering a wedged pairing store is an operator task: delete the store
directory and restart.

For the same reason, the setup code and its QR are served **only while
unpaired**. The code is permanent — unlike the WhatsApp QR, which rotates
every ~20s and 404s after pairing — so continuing to serve it would hand
out a durable door-lock credential to anything that can reach port 8080.

### Pairing UX

A 🏠 button in the web UI header opens a panel mirroring the existing
WhatsApp one: the `X-HM://` QR code, the setup code as text, and the
number of paired controllers. The QR and code disappear once paired. The
same code is printed to stdout as a half-block ASCII QR at startup when
unpaired.
`rsc.io/qr` and `mdp/qrterminal/v3` are already dependencies.

New endpoints:

- `GET /homekit/status` → `{"configured":bool,"paired":bool,"accessories":[…]}`, plus `pin` while unpaired
- `GET /homekit/qr.png?accessory=<name>` → PNG of that accessory's `X-HM://` payload, 404 once that accessory is paired. Defaults to the bridge.

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
- On failure the write returns an error and the tile stays Secured.
  Reporting the failure matters: a tile that always claims success is
  worse than no tile.

  An earlier draft also set `LockCurrentState = Jammed` here. That was
  dropped after review: `hap` maps any handler error to
  `SERVICE_COMMUNICATION_FAILURE`, which the Home app renders as "No
  Response", so the Jammed value never reaches the user — it would have
  been decoration that also misdescribed the failure.
- A write of `LockTargetState = Secured` cancels any pending timer and
  sets Secured immediately. There is no lock command to send; the relay
  only opens.

Concurrent unlocks of the same door restart the timer rather than
stacking, and the timer handle is mutex-guarded: `hap` dispatches writes
on each connection's own goroutine, and a household normally has several
paired controllers, so two simultaneous unlocks are ordinary rather than
exotic. The state machine takes an injected clock so the timer is testable
without sleeping.

### Air conditioner

One `HeaterCooler` service, mapped against the existing `GreeStatus`:

| HomeKit characteristic | Gree field | Notes |
| ---------------------- | ---------- | ----- |
| `Active` | `Power` | |
| `CurrentTemperature` | `RoomTemp` | Falls back to `SetTemp` when Gree reports 0 (unit off or no sensor); HomeKit requires a plausible value. |
| — | `TempUnit` | HAP carries every temperature in Celsius and treats display units as a rendering hint only, so a unit configured in °F has its values converted, not relabelled. |
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

### Known limitation: Auto mode's temperature range

HomeKit renders `TargetHeaterCoolerState = Auto` as a dual-handle range
built from the two threshold characteristics. The Gree has one set point,
so both track it and the band collapses to a point; dragging it snaps
back. Heat and Cool each show a single target and behave normally.

Accepted rather than fixed. The alternatives — dropping Auto from the
supported states, or inventing a synthetic band around the set point —
both misrepresent a unit that genuinely has one target temperature.

### Freshness

`GreeController.refresh()` already polls every 10s so remote-driven
changes are visible. It gains a subscription hook that pushes updated
values into the HomeKit characteristics, so Home app tiles update via
HAP events rather than only on read.

An offline poll is **not** published. `greeClient.Status` returns a
zero-valued struct on any failure, so forwarding it would tell every
controller the AC had switched off at 0 °C — flapping the tile and firing
any automation keyed on it — every time one UDP exchange timed out. The
last known state stands until a successful poll replaces it.

HomeKit-initiated writes carry a 9s deadline. iOS abandons a
characteristic write at around 10s, and it holds a single HAP connection
per controller for the whole bridge, so an unbounded write to a flaky AC
would stall reads and writes for the door locks behind it.

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

**SetupEndpoints is a write-response characteristic**, and getting that
wrong is invisible until an iPhone is pointed at it. The controller learns
the accessory's address, port, SSRC and keys from the *reply to its own
write*, not from a later read. Two things follow, both of which the first
implementation got wrong:

- `hap` declares the characteristic `[pr, pw]` and its `Bytes` helper
  discards handler return values, so the permission and the
  `SetValueRequestFunc` are wired by hand. Answering by writing into the
  characteristic does not work: `hap` assigns the controller's value
  *after* the handler returns, overwriting the answer with the request.
- The accessory's RTP socket is bound during `SetupEndpoints`, not at
  stream-start, because the port is part of that answer — the controller
  addresses RTCP to it. Echoing the controller's own port back, or binding
  an ephemeral port later, advertises somewhere nothing is listening.

With either mistake iOS completes `SetupEndpoints`, finds no usable
endpoint, and abandons the stream without sending a start command. The
Home app shows "No Response" and the logs show a setup with no stream
following it — which is exactly how this was diagnosed.

**Streaming.** On `SetupEndpoints` iOS supplies its address and the SRTP
master key and salt; on `SelectedStreamConfiguration` it selects
resolution, framerate, bitrate, MTU, payload type, and SSRC. camonitor
then opens a dedicated RTSP session via `rtsp.go` — main stream for
requests at 1080p or above, sub stream below — rewrites SSRC, payload
type, and sequence numbers on each H.264 RTP packet, encrypts with
`pion/srtp/v3`, and sends UDP to the iOS endpoint. `pion/srtp/v3` is
already in the dependency graph via `pion/webrtc`.

Three known sharp edges:

- iOS rejects a camera accessory that advertises no audio codec. We
  advertise Opus in `SupportedAudioStreamConfiguration` and never send
  audio.
- The Dahua does not respond usefully to RTCP PLI, so first-frame
  latency is bounded by the camera's own IDR interval, and a stalled
  stream is recovered by tearing down and re-opening the RTSP session
  rather than by requesting a keyframe.
- The camera packetises to its own MTU, which may exceed the one iOS
  negotiates. Re-fragmenting H.264 FU-A units is the proper fix; for now
  an oversized packet is logged once per stream and passed through, since
  in practice camera packets sit under a normal 1500-byte path MTU. This
  is the most likely thing to need attention on real hardware.

The camera's SSRC, payload type and sequence numbering are all rewritten
onto the negotiated values. The first two because HomeKit rejects anything
it did not negotiate; the third because SRTP's replay window needs a
sequence space we control rather than one that jumps whenever the camera
reconnects. Timestamps pass through — RTSP H.264 and HomeKit both clock
video at 90 kHz.

## Failure behaviour

| Condition | Behaviour |
| --------- | --------- |
| `homekit` absent from config | Nothing starts. No behaviour change anywhere else. |
| Invalid `pin` | Fatal at startup with a message naming the constraint. |
| Pairing store cannot be opened | HomeKit is disabled with a log line; doors, web UI, WhatsApp, and Gree keep working. Pairing without persistence is worthless, but it is not worth killing the service over. |
| Port or mDNS conflict | Logged and retried every 5s. Under `hostNetwork` any other process on the node can take 51826, and crash-looping the pod would take doors and cameras down over a HomeKit-only problem. |
| Door open fails | The write fails, so the Home app reverts the toggle and reports the accessory as not responding. Logged. |
| Gree offline | Last-known values stand — an offline poll is dropped rather than published. Writes return an error so the Home app shows "No Response" rather than silently accepting. |
| RTSP session dies mid-stream | SRTP session torn down; iOS shows the stream as ended. |

## Testing

The repo currently has no tests. This work adds the first ones, confined
to pure logic with no network and no hardware:

- Gree ↔ HomeKit characteristic mapping in both directions, table-driven
  across every mode, fan speed, and the `RoomTemp == 0` fallback. This is
  where the real bugs are expected.
- Lock state machine: successful open, relock after the configured
  delay, a failed open leaving the tile Secured and scheduling nothing,
  explicit re-lock cancelling a pending timer, and concurrent opens
  restarting rather than stacking. Uses an injected clock; no sleeping.
  The concurrency case runs under `-race` and fails without the mutex.
- Setup-code validation, including Apple's rejected codes.
- Temperature round-trips through both Celsius and Fahrenheit units, and
  an offline poll leaving the last known values in place.
- `SetupEndpoints` end to end: that the write returns a decodable
  response rather than echoing the request, that the advertised port is
  the one actually bound, and that renegotiation closes the superseded
  socket. Both halves of the "No Response" defect are covered, and both
  tests were confirmed to fail against the code that shipped it.
- The SRTP forwarder end to end: packets shaped like the camera's go in,
  and a real UDP listener decrypts them with the key HomeKit would have
  supplied, asserting the SSRC, payload type, contiguous sequence and
  passed-through timestamp. This is as close to an iPhone as the suite
  gets, and it covers the part that cannot otherwise be checked without
  one.
- Camera accessory shape (category, doorbell primary, one stream
  management per simultaneous viewer, the muted microphone iOS requires),
  and that a ring fires only its own camera's doorbell.
- Manager topology: bridge plus one camera server per door station on
  ascending ports, each with a distinct setup payload, and the setup code
  surviving until every accessory is paired rather than only the bridge.

`DoorClient` and `GreeController` are consumed through narrow interfaces
at the HomeKit boundary so tests use fakes.

The live tier confines its Bonjour announcements to loopback. Its
accessories carry production names, and a test run must not put a second
"camonitor" bridge on the developer's network — which it did, once,
before that was fixed.

Mocking the store is not sufficient on its own. It hid a real defect —
that a `hap.Server` cannot be restarted — so a second, smaller tier of
tests binds a real port and drives a real `hap.Server`: the bridge serves
and stops with its context, a port conflict is retried rather than fatal,
and the setup code is withheld once paired.

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
