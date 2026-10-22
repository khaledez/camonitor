# Guest Camera Streaming (backend) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an enrolled guest watch the two cameras (`.10`, `.15`) live in-app over low-latency HEVC HLS, sourced from the NVR, token-gated via short-lived signed URLs — plus a guest self-revoke endpoint.

**Architecture:** A `StreamHub` keeps one upstream RTSP pull per camera from the NVR (`gortsplib`, H265) and muxes it to Low-Latency HLS (`gohlslib`, HEVC tagged `hvc1`), fanned out to all current viewers (one pull regardless of viewer count; lazy-start on first viewer, stop after idle). A guest exchanges its device token once at `POST /guest/cam/{10|15}/session` for an **HMAC-signed, expiring playlist URL**; the playlist and segment GETs validate that signature (native players don't attach auth headers to child requests). These routes live on the guest **Funnel** surface. A guest can also call `POST /guest/forget` to revoke its own device server-side.

**Tech Stack:** Go 1.27; `github.com/bluenviron/gortsplib/v4` + `github.com/bluenviron/gohlslib/v2` (+ `mediacommon`) — all pure Go / CGO-free. stdlib `crypto/hmac`, `net/http`.

**Spec:** docs/superpowers/specs/2026-10-21-guest-mobile-app-design.md (Streaming section). Verified by the 2026-10-22 spike: HEVC-in-HLS tagged `hvc1` plays on the owner's iPhone (Safari + Brave) — no transcode needed.

## Global Constraints

- **Pure Go only** (gortsplib/gohlslib/mediacommon are CGO-free); keep `CGO_ENABLED=0` / FROM-scratch. Confirm with `CGO_ENABLED=0 go build ./...`.
- **HEVC passthrough, no transcode** (the image forbids a software encoder). HLS segments are fMP4 with the HEVC codec **tagged `hvc1`** (spike finding — `hev1` won't play on Apple). gohlslib handles this.
- **Re-time from RTP, not camera clocks** — the Tiandy RTSP packets have unset/non-monotonic timestamps; rely on gohlslib's RTP-derived timing, never the upstream PTS.
- **One upstream pull per camera**, shared across all viewers; lazy-start on first viewer and stop after an idle period — never a pull per guest, never a permanent pull with no viewers.
- **Guest stream access is signed, not header-authed:** the device token is exchanged ONCE (Authorization: Bearer) at the session endpoint for a short-lived HMAC-signed URL; the playlist and every segment GET carry a signed, expiring query token that the server validates — the device token never rides the many segment requests.
- **Secrets stay in config/secret:** NVR RTSP URLs + creds and the HMAC `stream_secret` live in the `monitor-config` secret, never in a response or a log. The stream routes are on the Funnel surface; nothing new on the tailnet admin surface or `:8080`.
- Log each stream session grant to the existing `AuditStore` (action `cam`, result `success`/`denied`), so viewing shows up in the audit log.

## Review Focus

- **No token → no stream:** `POST /guest/cam/*/session` without a valid, non-revoked bearer token returns 401; a revoked guest cannot start a session. (Task 3/4)
- **Unguessable + expiring:** a playlist/segment URL without a valid HMAC signature is 403; an expired signature is 403. A guest cannot forge or share a long-lived URL. (Task 3)
- **One pull, many viewers:** N concurrent viewers of one camera cause exactly ONE upstream RTSP pull; the hub fans out. (Task 2)
- **Upstream drop → reconnect, not crash:** if the NVR RTSP connection drops, the hub reconnects and viewers resume; a dead upstream never panics the server or wedges a goroutine. (Task 2)
- **Idle → stop:** when the last viewer leaves, the upstream pull stops after the idle timeout (no permanent NVR load). (Task 2)

## Prerequisite (owner, external)

The **NVR (`192.168.88.210`) sub-channel RTSP URLs + credentials** for the cameras mapped to `.10` and `.15` are needed (the cameras themselves expose only a 4640×1728 main stream — the low-res guest feed must come from the NVR sub-channel). Owner provides NVR creds; Task 1 confirms the exact per-camera sub-channel URL, codec, and resolution. Until then the hub can be pointed at any RTSP URL (including a camera) for development.

---

### Task 1: Stream-source config + NVR confirmation

**Files:**
- Modify: `main.go` (config), `config.example.json`, `README.md`

**Interfaces:**
- Produces: `GuestConfig` gains `Cameras map[string]string json:"cameras,omitempty"` (camera id → upstream RTSP URL, e.g. `{"10":"rtsp://user:pass@192.168.88.210:554/<sub>","15":"…"}`) and `StreamSecret string json:"stream_secret,omitempty"`. `loadConfig` validates: when `Cameras` is non-empty, `StreamSecret` must be set (else error, like the `public_base` guard). Document the NVR sub-channel URL format in the README.

- [ ] **Step 1: Write the failing config test** — `TestGuestCamerasRequireStreamSecret` (cameras set + empty secret → `loadConfig` error; both set → ok; neither → ok).
- [ ] **Step 2: Run it, verify it fails.**
- [ ] **Step 3: Add the fields + the guard.** (Separately, confirm the NVR sub-channel URLs by probing with the owner's creds — a manual/ops step, recorded in the README; not code.)
- [ ] **Step 4: Run it, verify it passes; `CGO_ENABLED=0 go build ./...` clean.**
- [ ] **Step 5: Commit** (`feat(guest): camera stream source + secret config`).

---

### Task 2: StreamHub — RTSP → LL-HLS, fan-out, lazy lifecycle

**Files:**
- Create: `streamhub.go`, `streamhub_test.go`

**Interfaces:**
- Produces:
  - `type StreamHub struct { … }`; `func NewStreamHub(sources map[string]string, idle time.Duration) *StreamHub` (nil-safe; empty sources → camera lookups 404).
  - `func (h *StreamHub) Handler(camera string) (http.Handler, bool)` — returns the gohlslib muxer's HTTP handler for that camera (serving `index.m3u8` + segments), starting the upstream pull lazily on first use; `false` for an unknown camera.
  - Internally per camera: a `gortsplib` client DESCRIBE→SETUP→PLAY on the configured URL, an `rtph265` depacketizer feeding `gohlslib.Muxer{Variant: LowLatency, Tracks: [H265]}` via `WriteH265`; a viewer refcount; reconnect-with-backoff on upstream error; stop the pull `idle` after the refcount hits zero.
  - `func (h *StreamHub) Close()`.
- Consumed by: Task 3.

- [ ] **Step 1: Write failing lifecycle tests** against an **injectable source** (abstract the upstream as an interface so tests use a fake that emits synthetic H265 access units — the real `gortsplib` dial is integration, not unit):
  - `TestOnePullManyViewers` — 3 concurrent `Handler`/viewer acquisitions of one camera → the fake source was opened exactly once.
  - `TestIdleStopsPull` — after the last viewer releases, within `idle` the fake source is closed.
  - `TestReconnectOnUpstreamError` — the fake source erroring once → the hub reopens it (backoff), no panic, no goroutine leak (use `goleak` or a wait+count).
  - `TestUnknownCamera` — `Handler("99")` → `false`.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement `StreamHub`** with the gortsplib→gohlslib pipeline behind the source interface; HEVC tagged `hvc1` via gohlslib's H265 track; RTP-derived timing.
- [ ] **Step 4: Run unit tests + `CGO_ENABLED=0 go build ./...`.** Record in the report that a real-NVR integration check (pull → playlist plays) must be run manually against the live NVR (the spike proved the codec path; this verifies the wiring) — name the command, don't block the task on hardware.
- [ ] **Step 5: Commit** (`feat(guest): RTSP→LL-HLS stream hub with fan-out`).

---

### Task 3: Signed stream URLs + session/playlist/segment handlers

**Files:**
- Create: `streamsign.go`, `streamapi.go`, `streamsign_test.go`, `streamapi_test.go`

**Interfaces:**
- Produces:
  - `func signStream(secret []byte, camera string, exp time.Time) string` and `func verifyStream(secret []byte, camera, token string, now time.Time) bool` — HMAC-SHA256 over `camera|exp`, base64url `exp.token`; constant-time compare; false when expired or tampered.
  - A `StreamAPI` that mounts on the guest mux: `POST /guest/cam/{camera}/session` (token-gated via the existing `requireToken`-style check) → `200 {"url":"/guest/cam/{camera}/hls/index.m3u8?s=<sig>","expires":<unix>}` (sig valid ~60s to fetch the playlist; the playlist/segment URLs it serves carry their own longer-lived-but-bounded sig, e.g. 5 min, rotated per session); `GET /guest/cam/{camera}/hls/*` → validate `?s=` sig+expiry, then delegate to `StreamHub.Handler(camera)`; 401 (no token at session), 403 (bad/expired sig), 404 (unknown camera).
  - Logs the session grant/denial to `AuditStore`.
- Consumed by: Task 4.

- [ ] **Step 1: Write failing sign tests** — round-trip valid; tampered camera/sig → false; expired → false; constant-time (no early-return on length). 
- [ ] **Step 2: Write failing handler tests** (httptest + a fake hub + a real signer): session without token → 401; with token → 200 + a URL whose sig `verifyStream` accepts; GET hls with a good sig → 200 (delegates to fake hub); with a bad sig → 403; expired sig → 403; unknown camera → 404.
- [ ] **Step 3: Run, verify fail.**
- [ ] **Step 4: Implement** `streamsign.go` + `streamapi.go`.
- [ ] **Step 5: Run, verify pass.**
- [ ] **Step 6: Commit** (`feat(guest): signed stream sessions + hls handlers`).

---

### Task 4: Funnel wiring, self-revoke, config glue

**Files:**
- Modify: `guestapi.go` (or `guestwiring.go`/`main.go`), `guestapi_test.go`, `main.go`

**Interfaces:**
- Consumes: `StreamHub` (T2), `StreamAPI` (T3), `GuestStore.Revoke`/`Authenticate`, `AuditStore`.
- Produces:
  - The guest Funnel mux gains the three cam routes (from `StreamAPI`) alongside the existing enroll/gate/outdoor.
  - `POST /guest/forget` — token-gated; revokes the authenticated device server-side (`store.Revoke` by the guest resolved from the token), logs an audit `forget`/`success`, returns 204. (Self-revoke — the app's "remove access from this phone"; the local token wipe happens in the app, this makes it stick server-side.)
  - Wiring: build the `StreamHub` from `cfg.Guest.Cameras` + idle timeout; pass the `StreamSecret`; mount everything on the Funnel surface only. Degrade (nil hub, cam routes 404) when no cameras configured — never crash.

- [ ] **Step 1: Write failing tests** — `/guest/cam/10/session` requires a token (401 without); `POST /guest/forget` with a valid token revokes that device (subsequent `Authenticate` of its token → false) and returns 204, without a token → 401; cam routes are on the Funnel mux, not the tailnet/admin mux; no-cameras config → cam session 404, guest open still works.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** the wiring + self-revoke.
- [ ] **Step 4: Full suite + `go vet` + `CGO_ENABLED=0 go build ./...` green.**
- [ ] **Step 5: Commit** (`feat(guest): mount stream routes on funnel + self-revoke`).

---

## Out of scope (separate follow-ups)

- **Owner web UI re-point** (`.10`/`.15` WebRTC sourced from the NVR instead of the cameras directly, keeping the owner's HD/SD + WebRTC): a separate, delicate change to the existing stream path — plan it after this lands.
- **The apps' camera views** (Arabic/RTL iOS + Android: add the two camera tiles → full player via `AVPlayer`/`ExoPlayer` LL-HLS, plus the self-revoke UI): a separate plan per platform, consuming these endpoints.
