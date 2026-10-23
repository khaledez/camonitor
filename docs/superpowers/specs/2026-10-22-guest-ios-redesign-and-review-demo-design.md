# Guest iOS redesign + App Review demo pass — design

Status: builder-checked, awaiting owner sign-off · Date: 2026-10-22

## Intent and why

External TestFlight review was refused (Guideline 2.1): Apple's reviewer
could not get past the QR scanner because there is no invite they can use.
Today's invites are one-time and expire after 10 minutes, so nothing
pasteable into App Store Connect works.

At the same time the guest app needs its redesign (canvas "Guest App":
Home, Camera, Access) so guests can watch the two cameras that the backend
already streams (camonitor 0.12.3).

Both ship together: the reviewer should see the **real** app with **real**
camera video, but must never be able to open the gate or the out-door.

## Goals

1. **Demo pass.** A long-lived, reusable invite QR the owner pastes into
   App Store Connect › TestFlight › Test Information › Beta App Review
   Information › Notes. Scanning it enrolls the phone as a *demo guest*:
   - live view of both cameras works, through the same streaming endpoints
     real guests use;
   - gate and out-door taps are refused by the **server** (never reach
     Centsys/VTO), and the app shows a clear message that opening is
     disabled for this demo pass (in the phone's language, see below);
   - every demo enroll / tap / camera view lands in the audit log as usual;
   - "Remove access from this phone" works and only affects that phone; the
     demo code keeps working for the next reviewer.
2. **iOS redesign** matching the canvas
   (https://claude.ai/artifact/M7NvBCnuCcu22jZU2kVKa9), accent `#2d7d8e`.
   - **Language follows the phone (owner's decision):** Arabic and English
     via a String Catalog; Arabic phones get the canvas's Arabic strings and
     RTL, everything else gets English LTR. Apple's reviewer can read every
     screen. Arabic strings are the canvas's; names: household
     "دار أبو خالد", camera `10` "الكاميرا الأمامية" (Front camera),
     camera `15` "الساحة" (Courtyard).
   - **Home:** two live camera tiles (front camera, courtyard) → Camera
     screen; big "Open gate" (filled) and "Open outer door" (outlined)
     buttons; footer "Access granted by Khaled · Manage" → Access.
   - **Camera:** dark full-screen live view with a "live" badge and clock;
     switch between the two cameras; gate / outer-door buttons along the
     bottom; back to Home.
   - **Access:** "This phone has access", the guest's name, "granted by
     Khaled", the enrol date; a "what you can do" list (gate, outer door,
     both cameras); the red "Remove access from this phone" button with its
     explainer line. For a demo pass, the gate and door rows show as
     unavailable.
   - Enrolment screen: QR scanning as today, restyled, **plus a "paste
     invite link" option (owner's decision)** that runs the pasted text
     through the same `parseInviteQR`, so a reviewer who can't point a
     camera at the QR can paste the link from the review notes. No
     demo-specific handling.

## Non-goals

- **Android.** Same design, follows in a separate plan once iOS is in review.
- **HD/SD toggle** on the Camera screen. The backend serves only the NVR
  sub-stream; there is no HD source. Drop the toggle (builder's call, noted
  here so the owner sees it).
- Any offline/mock mode inside the app. The demo is a real server-side pass.
- Owner-side UI for creating demo passes. One demo code, set in config.
- Push notifications, door history, two-way audio.

## Anti-goals

- A demo pass that can open anything, by any route (the server must refuse,
  not just the app hiding buttons).
- The demo code reaching the gate/door code paths at all.
- The existing anti-goal from the guest design still holds for *real*
  guests: their QR stays one-time. The demo QR is a deliberate, scoped
  exception: cameras only.

## Constraints

- Backend: Go, camonitor; guests in `guests.json` (`GuestStore`), one token
  per guest record; public surface on Funnel (`GuestAPI`, `StreamAPI`).
  Config via the `monitor-config` secret (`GuestConfig`, strict JSON —
  unknown fields fail startup).
- Streams: `POST /guest/cam/{10|15}/session` (bearer token) → `{url, expires}`
  with a relative, signed HLS URL valid 15 min; a 403 on the stream means
  expired → re-POST session. HEVC tagged `hvc1`, plays in `AVPlayer`.
- Enroll QR contract unchanged: `https://<public_base>/guest/enroll?code=…`.
  The app must not special-case the demo QR when parsing.
- iOS 17, SwiftUI, iPhone-only portrait, no third-party dependencies.
  Session in Keychain (`SessionStore`); adding fields must not break an
  already-enrolled phone's stored session.
- Privacy: anyone holding the demo QR can watch both cameras. The owner can
  turn the demo off by removing the config value (and revoking demo guests
  in the admin overlay) after review.

## Decided

- **Demo code lives in config**, a new optional `guest.demo_code` string in
  `GuestConfig`. Unset = no demo pass. Minimum length enforced at startup
  (it's a long-lived public secret; ≥ 16 chars).
- **Each demo scan creates a new guest record** labelled e.g.
  `App Review (demo)`, flagged `demo: true`, already enrolled, with its own
  token. Reason: the store holds one token per guest; this keeps that model
  and lets the owner see and revoke each reviewer device in the existing
  admin overlay. (shortcut: unbounded records per scan — fine for a handful
  of reviewers; prune if it ever grows.)
- **Door refusal is a 403** from `/guest/gate/open` and `/guest/outdoor/open`
  for demo guests, audited as `denied`, and checked *before* the opener is
  called. 401 keeps meaning "token dead → re-enrol"; 403 means "you're in,
  but not allowed to open".
- **Enroll response grows**: `{token, label, created, demo}` so the Access
  screen can show the name and date and the app knows it's a demo pass.
  `Session` gains these as optional fields, so old Keychain entries still
  decode (they show no name / date until re-enrolled).
- **Remove access** calls `POST /guest/forget`, then clears the Keychain
  regardless of the network result (the local wipe must always happen).
- **"Granted by Khaled"** is a fixed string in the app (one owner, one site).
- **The Access screen's "since" date is the guest's `Created`** (when the
  invite was minted; my call: at most 10 min off from the true enrolment
  time, and identical for demo guests). No new store field.
- **Home tiles play live, muted.** The NVR low-res sub-stream is live in
  production (owner confirmed), so two small players are affordable.

## Left to the builder

- Exact demo-refusal wording and where it shows (under the tapped button is
  fine).
- How stream-session renewal is triggered (player error → re-POST, or a
  timer before `expires`).
- Test split: Go handler tests for demo enroll + 403s; iOS unit tests for
  the 403 mapping, session decoding of old entries, and forget.

## Rollout

1. Backend: demo code, demo guests, 403s, richer enroll response — deploy.
2. iOS: redesign + cameras + Access/forget + demo message — TestFlight build.
3. Owner: put `guest.demo_code` in `monitor-config`, generate its QR
   (the existing admin QR rendering, or any QR tool on the enroll URL),
   paste the QR image **and the plain link** into Beta App Review
   Information › Notes with a one-line explanation (scan it, or paste the
   link on the setup screen), resubmit.
