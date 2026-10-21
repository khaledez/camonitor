# Guest mobile app for gate, door, and camera access — design

Status: draft for approval · Date: 2026-10-21

## Intent and why

The owner wants to give a controlled set of people — family, close friends,
and staff such as housekeeping — the ability to open the property's **gate**
and **out-door**, and to watch **two cameras** (the Tiandy units at
`192.168.88.10` and `192.168.88.15`), from their phones.

These people are deliberately *not* added to the owner's Apple Home, and the
owner does not want them joining the Tailscale tailnet or installing the
Tailscale app. They should install **one app, the owner's**, and nothing
else. The owner alone decides who is in and who is out, and must be able to
cut someone off instantly and completely (e.g. when a housekeeper stops
working for them).

Today `camonitor` (the existing Go service, deployed on Talos/k8s on the
`zima` node) has **no per-request authentication**: every endpoint is open to
anything that can reach it, and access is controlled entirely by tailnet
membership. That model cannot safely be extended to guests, so this project
adds a real, scoped, per-device authorization layer and a narrow public
surface for guests, without changing how the owner uses the system today.

## Goals

- Native **iOS (Swift/SwiftUI)** and **Android (Kotlin/Jetpack Compose)**
  apps — two native codebases, no cross-platform framework.
- A guest can: open the gate, open the out-door, view camera .10, view
  camera .15. Nothing else.
- The owner adds a guest by showing a **QR code on an admin screen**; the
  guest scans it once in the app and is enrolled.
- Each enrolled phone holds its **own** credential; the owner can **revoke
  any single device instantly**, with no shared secret that survives.
- Guests reach the system **without the Tailscale app** and without tailnet
  membership.
- Camera viewing is sourced from the **Tiandy NVR (`192.168.88.210`)**, not
  the cameras directly — for both guests *and* the existing owner view.
- Keep the owner's current experience (tailnet web UI, WebRTC, HomeKit,
  doors, admin) working and unchanged in how it's reached.

## Non-goals

- No per-user permission tiers. Everyone on the list gets the identical four
  actions. (Explicitly confirmed by the owner.)
- No guest access to anything else: not the front-door station (`vto1`), not
  any other camera, not the owner web UI, not the HomeKit/WhatsApp/Centsys
  admin screens, not door history.
- Guests are never put on the tailnet (Option A, "embed Tailscale in the
  app", was considered and rejected — see Alternatives).
- Not a multi-tenant / multi-property product. One owner, one site.
- No account system, email/SMS, or passwords for guests — enrollment is QR
  only, identity is the per-device token.

## Anti-goals (would make this a failure even if it "works")

- A removed guest retaining any way in (a cached token still accepted, a
  shared password, a screenshotted QR that re-enrolls).
- Guests being able to reach the front door, other cameras, or any admin
  action by calling an endpoint directly.
- Putting the owner's full UI or admin surface on the public internet.
- Cameras being hit directly by guest traffic (connection exhaustion,
  quality drops on the recorder).

## Constraints

- camonitor is a single static Go binary (`FROM scratch` image), deployed via
  `deploy/k8s/deployment.yaml`, config delivered through the `monitor-config`
  secret, persistent state on the `monitor-state` PVC (`/var/lib/camonitor`).
  New persistent state (guest tokens) lives on that PVC.
- It already runs an embedded Tailscale node (`tsnet`, hostname `monitor`)
  serving HTTPS on the tailnet only. No public exposure today.
- Tailscale **Funnel** carries only HTTPS (TCP) on ports 443 / 8443 / 10000.
  It cannot carry WebRTC's UDP media.
- Cameras are Tiandy, H.265. The NVR at `.210` speaks RTSP on 554
  (confirmed: `OPTIONS` returns `DESCRIBE/PLAY/SETUP`), with HTTP on 80/443
  and Tiandy's private port 3001.
- The owner's own devices are already on the tailnet and run Tailscale.

## The design

### Two surfaces, two trust levels

camonitor exposes **two distinct HTTP surfaces**:

1. **Owner/admin surface — tailnet only, unchanged.** Everything that exists
   today (web UI, WebRTC, doors, HomeKit, Centsys, WhatsApp) plus the **new
   admin/enrollment endpoints** stays on the private tailnet listener. It is
   never funnelled. Trust model stays "on the tailnet = trusted", consistent
   with today. The owner's admin app reaches these over the tailnet (the
   owner runs Tailscale; guests do not).

2. **Guest surface — public via Funnel, token-gated, four actions only.** A
   separate listener, exposed with Tailscale Funnel, serving only:
   - `POST /guest/enroll` — exchange a one-time enrollment code for a device
     token.
   - `POST /guest/gate/open` — trigger the Centsys gate.
   - `POST /guest/outdoor/open` — trigger the out-door (`vto2`).
   - `GET  /guest/cam/{10|15}/...` — the low-latency camera stream (see
     Streaming).
   Every call except enroll requires a valid, non-revoked device token.
   There is no route here to the front door, other cameras, or any admin
   action — the handlers simply don't exist on this surface.

   *Builder's call:* whether the guest surface is a second port (8443) or a
   path prefix on a Funnel-enabled 443, and the exact `tsnet` Funnel API used
   to bring it up.

**Listener binding is security-critical.** Today camonitor serves one shared
mux on *both* the tsnet HTTPS listener *and* the plain `:8080` listener — and
because the pod runs `hostNetwork: true`, **`:8080` is reachable by anything
on the LAN, unauthenticated.** That LAN exposure is **intentional** (it lets
the Apple TV / HomeKit hub reach camonitor on the LAN) and stays as-is — the
point here is *not* to lock down `:8080`, but to make sure the new sensitive
endpoints are never served on it. They must be bound deliberately, not added
to the shared mux:
- **Admin + enrollment** (mint code, create/list/revoke guests) → the
  **tsnet listener only**. Never on `:8080`, or the whole LAN could mint
  enrollment codes and revoke guests.
- **Guest API** (enroll, open, stream) → the **Funnel listener only**.
- **`:8080` keeps its current routes unchanged** — the existing UI, doors,
  cameras and health probe stay exactly as they are today (that LAN access
  is relied on). The only rule is that the *new* admin and guest routes are
  never registered on it.

So the listeners get distinct muxes: the existing routes are served on both
`:8080` and tsnet (as now); admin routes are added to the tsnet mux only; the
guest routes are their own mux on the Funnel listener. A shared base
registration avoids duplicating the existing routes.

### Identity, enrollment, revocation

- A **guest record** in new camonitor state (on the `monitor-state` PVC):
  `{ id, label, created, last_seen, revoked }`. Tokens are stored **hashed**,
  never in plaintext.
- **Enrollment (QR):** on the admin screen the owner creates a record and
  camonitor mints a **one-time, short-TTL enrollment code**. The QR encodes
  the guest surface's public URL + that code. The guest app scans it and
  `POST /guest/enroll`s the code; camonitor verifies it's unused and unexpired,
  issues a **long-lived opaque device token** (≥256-bit random), marks the
  code consumed, and returns the token. The app stores it in the **iOS
  Keychain / Android Keystore**. A screenshotted QR is useless after first
  use, and expires on its own if never used.
- **Every guest action** sends the device token (Authorization header);
  camonitor looks it up, rejects if missing/unknown/revoked, else performs the
  single scoped action and updates `last_seen`.
- **Revocation:** the owner deletes/marks-revoked the record on the admin
  screen; the token is refused on the very next call. No restart, nothing
  cached that outlives it. (Defence-in-depth option, builder's call: also
  support rotating a server-side secret to invalidate all tokens at once.)

### Admin screen

Part of the **owner's build of the same native app** (not a separate app):
when the app can reach camonitor's admin endpoints over the tailnet, it shows
admin — list guests, add (shows QR), revoke. These admin endpoints live on
the tailnet-only surface, so they inherit today's "on the tailnet = trusted"
model.

*My call:* admin lives in the owner app gated by tailnet reachability; an
optional admin PIN/secret can be layered on for defence-in-depth. The owner
installs Tailscale on their own admin phone (they are not a guest).

### Camera streaming

- **Single source: the NVR.** camonitor pulls .10 and .15 from the **NVR**
  over RTSP; the cameras themselves are never hit by this feature. This
  re-points the existing `tiandy1`/`tiandy2` stream configs at the NVR too,
  so .10/.15 only ever talk to the recorder.
- **Owner keeps HD/SD.** The current owner experience has a working HD/SD
  toggle (main vs sub stream), and the goal is to keep it. So the owner path
  maps that toggle onto the NVR's **main channel (HD)** and **sub channel
  (SD)** for each camera — it does *not* become substream-only. The owner
  keeps **WebRTC** (sub-second), now fed from the NVR instead of the camera.
  The NVR already records the main stream, so this adds only an RTSP pull it
  can serve.
- **Guests get the substream only**, over an **HTTP low-latency stream**
  (WebRTC can't traverse Funnel — it's UDP). Recommended: **LL-HLS**, which
  iOS `AVPlayer` and Android `ExoPlayer` play natively; ~1–3s latency, fine
  for "who's at the gate". This is new work (today camonitor only speaks
  WebRTC).
- **No transcoding; HEVC passthrough (decided).** The `FROM scratch`,
  static, read-only-rootfs image is a hard constraint, and a software H.265→
  H.264 encoder (ffmpeg/libx264) would break all three at once. So the guest
  path is **pure-Go remux only**: parse the NVR's H.265 and package it into
  fMP4/CMAF segments in Go, no re-encoding, no ffmpeg, image unchanged.
  Modern iOS and Android both play HEVC-in-HLS, so passthrough is expected to
  work. *Flagged risk:* if a target guest device turns out unable to play
  HEVC, that reopens the transcode question, which would force an image/
  deployment change — revisit with the owner rather than silently relaxing
  the constraint.
- **Stream auth via short-lived signed URLs (decided).** Native players
  (`AVPlayer` especially) don't reliably attach an `Authorization` header to
  child-playlist and segment GETs. So a guest exchanges its device token
  (header) at a `POST /guest/cam/{10|15}/session` for a short-lived
  **HMAC-signed playlist URL**; the playlist and its segments carry a signed,
  expiring query token that camonitor validates. This works with the stock
  native players on both platforms without header shims, and keeps the device
  token off the many segment requests.

### The apps

- **iOS:** Swift / SwiftUI. **Android:** Kotlin / Jetpack Compose.
- Guest flow: first run → scan QR → enrolled. Home screen: two big action
  buttons (Gate, Out-door) and two camera tiles (.10, .15) that open the
  live view. Token in secure storage; all calls to the Funnel guest URL.
- Owner build additionally surfaces the admin screen (over tailnet) described
  above.

## Alternatives considered

- **Option A — embed Tailscale in the guest app** (userspace `tsnet` /
  `libtailscale`, guests become tagged, revocable tailnet nodes). Research
  confirmed it's technically viable on both iOS and Android *without* a VPN
  entitlement. Rejected because: (a) a Tailscale ACL/grant still can't scope a
  device to specific actions, so the per-device token layer is needed anyway;
  (b) `libtailscale` is immature (no releases, build-from-source, bespoke
  native glue on two platforms); (c) the userspace tunnel pauses on app
  suspend, adding a reconnect delay at exactly the moment a guest wants to
  open the gate; (d) each guest consumes a device on the owner's tailnet.
  Option B gets the same security from the token layer with far less risk and
  a snappier UX, at the cost of a minimal token-gated public surface.
- **Own cloud reverse proxy + mTLS/token** instead of Funnel. Rejected:
  reintroduces cert management, a separate public host, and bespoke
  credential plumbing — rebuilding what Funnel gives for free — with no
  benefit here.

## Prerequisites (owner, external to the code)

- **Tailscale Funnel must be enabled for this node.** *(Owner confirmed OK,
  2026-10-21.)* Funnel requires the tailnet ACL to grant the `funnel` node
  attribute to `monitor` and HTTPS/MagicDNS to be on; otherwise the Funnel
  listener won't come up and Option B can't work. This is a one-time owner
  action in the Tailscale admin console, not something the code can do.
- **The guest URL is the node's public Funnel hostname** (`monitor.<tailnet>.
  ts.net`), baked into every enrollment QR. The owner accepts that hostname
  as the public guest entry point. (It changes only if the tailnet/node name
  changes, which would invalidate outstanding QRs — rare.)

## What's left to the builder

- Exact NVR RTSP URL format, channel numbers for .10/.15, substream path, and
  credentials (probe the NVR at build time).
- The `tsnet` Funnel API specifics and whether the guest surface is a port or
  a path prefix.
- LL-HLS vs an alternative low-latency HTTP transport, and remux-vs-transcode,
  decided against the NVR substream's real codec/resolution.
- All UI/UX detail and App Store / Play Store packaging.
- Whether to add the optional admin PIN and the global token-rotation lever.
```
