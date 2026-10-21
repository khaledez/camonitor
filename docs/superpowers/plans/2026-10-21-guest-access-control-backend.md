# Guest Access Control (Backend) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an owner-managed set of guest phones open the gate and the out-door through a public, token-gated Tailscale Funnel surface, with per-device enrollment and instant revocation — no cameras yet.

**Architecture:** A new persistent guest store (hashed tokens, one-time enrollment codes) on the state PVC; a Funnel-only guest HTTP surface with three routes (enroll, gate/open, outdoor/open) guarded by a token middleware; a tailnet-only admin surface (create/list/revoke) that renders enrollment payloads; and a `main.go` change that splits the single shared mux into three listener-scoped surfaces.

**Tech Stack:** Go 1.27, standard library only (`net/http`, `crypto/rand`, `crypto/sha256`, `encoding/json`), `tailscale.com/tsnet` (already a dependency). No new modules.

**Spec:** `docs/superpowers/specs/2026-10-21-guest-mobile-app-design.md`

## Global Constraints

- Pure Go, no cgo: the image is `FROM scratch`, `CGO_ENABLED=0`, `readOnlyRootFilesystem: true`. No new dependency that breaks a static build.
- New persistent state lives on the `monitor-state` PVC at `/var/lib/camonitor` (default guest store path `/var/lib/camonitor/guests.json`).
- Device tokens are stored **hashed (SHA-256), never in plaintext**. The plaintext token is returned to the device exactly once, at enrollment.
- Enrollment codes are **one-time and short-lived** (TTL 10 minutes); consumed on first successful enroll.
- Listener binding: admin + enrollment-mint routes on the **tsnet listener only**; guest routes (`/guest/...`) on the **Funnel listener only**; both are **never** registered on the plain `:8080` mux. `:8080` and tsnet keep the existing routes unchanged.
- Guest actions in this plan are exactly two: **gate** → `CentsysController.Open(ctx)`, **out-door** → `doorOpener.Open(ctx, "vto2")`. No other action exists on the guest surface.

## Review Focus

- **Revoked token still works:** a token whose guest is revoked must be rejected on the very next call (pinned in Task 1 `Authenticate` test and Task 2 middleware test).
- **Enrollment-code replay:** a code already used to enroll one device must fail for a second device (Task 1 `Enroll` one-time test).
- **Expired enrollment code:** a code older than its TTL must fail even if never used (Task 1 expiry test).
- **Missing/garbage token reaching an action:** `/guest/gate/open` with no or unknown bearer token must 401 and must not call the opener (Task 2 middleware test).
- **Sensitive route on the wrong listener:** admin/guest handlers must not be reachable on the `:8080` mux (Task 4 mux-wiring test).

---

### Task 1: Guest store

**Files:**
- Create: `gueststore.go`
- Test: `gueststore_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Guest struct { ID, Label string; Created, LastSeen time.Time; Enrolled, Revoked bool }` (JSON-tagged; the token hash and enrollment-code hash are unexported persisted fields, never in this exported view).
  - `func OpenGuestStore(path string) (*GuestStore, error)` — loads or creates the JSON file.
  - `func (s *GuestStore) Create(label string) (g Guest, enrollCode string, err error)` — new record, returns the one-time plaintext code.
  - `func (s *GuestStore) List() []Guest`
  - `func (s *GuestStore) Revoke(id string) error` — `ErrGuestNotFound` if absent.
  - `func (s *GuestStore) Enroll(code string) (token string, g Guest, err error)` — `ErrEnrollInvalid` / `ErrEnrollExpired`.
  - `func (s *GuestStore) Authenticate(token string) (Guest, bool)` — false for unknown/revoked; updates `LastSeen` on success.
  - Exported: `ErrGuestNotFound, ErrEnrollInvalid, ErrEnrollExpired`.
  - Injectable clock `now func() time.Time` (unexported field, defaults `time.Now`) so TTL is testable without sleeping.

- [ ] **Step 1: Write the failing tests** in `gueststore_test.go`:
  - `TestEnrollHappyPath`: `Create` → `Enroll(code)` returns a non-empty token; `Authenticate(token)` returns the guest with `ok==true` and `Enrolled==true`.
  - `TestEnrollIsOneTime`: after a successful `Enroll(code)`, a second `Enroll(code)` returns `ErrEnrollInvalid`.
  - `TestEnrollExpired`: with the store clock advanced past TTL (10m), `Enroll(code)` returns `ErrEnrollExpired`.
  - `TestAuthenticateRejectsRevoked`: enroll, then `Revoke(id)`; `Authenticate(token)` returns `ok==false`.
  - `TestAuthenticateRejectsUnknown`: `Authenticate("garbage")` returns `ok==false`.
  - `TestPersistenceReload`: `Create`+`Enroll`, open a second `GuestStore` on the same path, `Authenticate(token)` still succeeds; stored file contains no plaintext token (assert the token string is absent from the raw file bytes).

- [ ] **Step 2: Run the tests to verify they fail**
  Run: `go test . -run 'TestEnroll|TestAuthenticate|TestPersistence' -v`
  Expected: FAIL (undefined `OpenGuestStore`).

- [ ] **Step 3: Implement `gueststore.go`.**
  Mutex-guarded in-memory map persisted as JSON via atomic write (temp file + `os.Rename`), `0o600`. Token = 32 bytes from `crypto/rand`, base64url; persist only `sha256` hex. Enrollment code = 10 chars base32 (crock-free, uppercase), persist its sha256 + expiry; clear on consume. `Authenticate` compares the SHA-256 of the presented token against stored hashes. Reject revoked in both `Enroll`-issued-token lookups and `Authenticate`.

- [ ] **Step 4: Run the tests to verify they pass**
  Run: `go test . -run 'TestEnroll|TestAuthenticate|TestPersistence' -v`
  Expected: PASS.

- [ ] **Step 5: Commit**
  ```bash
  git add gueststore.go gueststore_test.go
  git commit -m "feat(guest): persistent guest store with hashed tokens and one-time enrollment"
  ```

---

### Task 2: Guest API surface (Funnel)

**Files:**
- Create: `guestapi.go`
- Test: `guestapi_test.go`

**Interfaces:**
- Consumes: `*GuestStore` (Task 1).
- Produces:
  - `type guestActions interface { OpenGate(ctx context.Context) error; OpenOutdoor(ctx context.Context) error }`
  - `func NewGuestAPI(store *GuestStore, actions guestActions) *GuestAPI`
  - `func (a *GuestAPI) Routes() http.Handler` — a `*http.ServeMux` with `POST /guest/enroll`, `POST /guest/gate/open`, `POST /guest/outdoor/open`.
  - Enroll request/response JSON: `{"code":"..."}` → `{"token":"..."}`. Actions take `Authorization: Bearer <token>` and return `204` on success, `502` if the opener errors, `401` on bad token.

- [ ] **Step 1: Write the failing tests** in `guestapi_test.go` using `httptest` and a `fakeActions` recording which opener was called:
  - `TestEnrollReturnsToken`: seed a guest+code in a temp store; `POST /guest/enroll` with the code → 200 and a non-empty `token`.
  - `TestActionRequiresToken`: `POST /guest/gate/open` with no `Authorization` → 401, and `fakeActions.OpenGate` was **not** called.
  - `TestActionRejectsRevokedToken`: enroll, revoke, then call with that token → 401, opener not called.
  - `TestGateActionCallsOpener`: enroll, call `/guest/gate/open` with the token → 204 and `fakeActions.OpenGate` called exactly once.
  - `TestOutdoorActionCallsOpener`: same for `/guest/outdoor/open` → `OpenOutdoor`.
  - `TestOpenerErrorIs502`: `fakeActions.OpenGate` returns an error → 502.

- [ ] **Step 2: Run to verify fail**
  Run: `go test . -run 'TestEnroll|TestAction|TestGateAction|TestOutdoor|TestOpenerError' -v`
  Expected: FAIL (undefined `NewGuestAPI`).

- [ ] **Step 3: Implement `guestapi.go`.**
  A `requireToken(http.HandlerFunc) http.HandlerFunc` middleware extracts the bearer, calls `store.Authenticate`, 401s on failure, else runs the action with a short `context.WithTimeout`. Enroll handler maps `ErrEnrollInvalid/Expired` → 400.

- [ ] **Step 4: Run to verify pass**
  Run: `go test . -run 'TestEnroll|TestAction|TestGateAction|TestOutdoor|TestOpenerError' -v`
  Expected: PASS.

- [ ] **Step 5: Commit**
  ```bash
  git add guestapi.go guestapi_test.go
  git commit -m "feat(guest): token-gated guest API for enroll + gate/outdoor open"
  ```

---

### Task 3: Admin surface (tailnet)

**Files:**
- Create: `guestadmin.go`
- Test: `guestadmin_test.go`

**Interfaces:**
- Consumes: `*GuestStore` (Task 1).
- Produces:
  - `func NewGuestAdmin(store *GuestStore, funnelBase string) *GuestAdmin` — `funnelBase` is the public guest URL (e.g. `https://monitor.<tailnet>.ts.net`).
  - `func (a *GuestAdmin) Routes() http.Handler` — `POST /admin/guests`, `GET /admin/guests`, `DELETE /admin/guests/{id}` (Go 1.22+ path wildcard).
  - `POST` body `{"label":"Housekeeper"}` → `{"id","label","enrollCode","enrollURL"}` where `enrollURL = funnelBase + "/guest/enroll"` and the app renders the QR from `{enrollURL, enrollCode}`.

- [ ] **Step 1: Write the failing tests** in `guestadmin_test.go` (httptest + temp store):
  - `TestCreateReturnsEnrollment`: `POST /admin/guests {label}` → 200 with non-empty `id`, `enrollCode`, and `enrollURL` containing the funnel base.
  - `TestListReturnsGuests`: after two creates, `GET /admin/guests` → 200 with a 2-element array; the enroll code / token are **not** present in the list payload.
  - `TestDeleteRevokes`: create, `DELETE /admin/guests/{id}` → 204; the store reports that guest `Revoked`.

- [ ] **Step 2: Run to verify fail**
  Run: `go test . -run 'TestCreateReturns|TestListReturns|TestDeleteRevokes' -v`
  Expected: FAIL (undefined `NewGuestAdmin`).

- [ ] **Step 3: Implement `guestadmin.go`.** Thin JSON handlers over the store; `DELETE` maps `ErrGuestNotFound` → 404.

- [ ] **Step 4: Run to verify pass**
  Run: `go test . -run 'TestCreateReturns|TestListReturns|TestDeleteRevokes' -v`
  Expected: PASS.

- [ ] **Step 5: Commit**
  ```bash
  git add guestadmin.go guestadmin_test.go
  git commit -m "feat(guest): tailnet admin API to create, list and revoke guests"
  ```

---

### Task 4: Wire listeners, muxes, Funnel, and the actions adapter

**Files:**
- Modify: `main.go` (the mux/listener block, ~426–522, and config load)
- Create: `guestwiring.go` (a testable mux-builder helper, kept out of `main` so it can be unit-tested)
- Test: `guestwiring_test.go`
- Modify: config struct (`main.go`) — add `Guest *GuestConfig` with `{ StorePath string }` (default `/var/lib/camonitor/guests.json`).

**Interfaces:**
- Consumes: `*GuestAPI` (Task 2), `*GuestAdmin` (Task 3).
- Produces:
  - `func buildSurfaces(base http.Handler, admin http.Handler) (lan http.Handler, ts http.Handler)` — `lan` is `base` unchanged; `ts` serves `base` plus `admin` under `/admin/`. (Guest handler is served directly on the Funnel listener, not through this helper.)
  - A `guestActions` adapter in `main.go` binding gate→`centsysCtrl.Open`, outdoor→`doors.Open(ctx,"vto2")`; a nil Centsys controller makes `OpenGate` return an error (surfaced as 502).

- [ ] **Step 1: Write the failing test** in `guestwiring_test.go`:
  - `TestAdminNotOnLanSurface`: build `base` with one marker route and `admin` with `/admin/guests`; `buildSurfaces` → request `/admin/guests` on `lan` returns 404, on `ts` returns non-404; the marker route works on both.

- [ ] **Step 2: Run to verify fail**
  Run: `go test . -run TestAdminNotOnLanSurface -v`
  Expected: FAIL (undefined `buildSurfaces`).

- [ ] **Step 3: Implement `buildSurfaces` in `guestwiring.go`**, then wire `main.go`:
  - Build the existing routes into `base` once.
  - `lan, ts := buildSurfaces(base, guestAdmin.Routes())`; serve `lan` on `:8080` (`plainSrv`), `ts` on the tsnet listener (`tsSrv`).
  - Bring up a **Funnel listener** on the tsnet node and serve `guestAPI.Routes()` on it. *Builder's call:* exact `tsnet` Funnel API (e.g. `ts.ListenFunnel("tcp", ":443")`); gate it behind the `-tailscale` flag being set, and log a clear error if Funnel isn't permitted on the tailnet (see spec Prerequisites).
  - Construct `GuestStore` from `cfg.Guest` (skip the whole guest/admin/funnel wiring when `cfg.Guest` is nil) and the `guestActions` adapter.

- [ ] **Step 4: Run tests + build**
  Run: `go test . -run TestAdminNotOnLanSurface -v && go build ./...`
  Expected: PASS and a clean build.

- [ ] **Step 5: Commit**
  ```bash
  git add guestwiring.go guestwiring_test.go main.go
  git commit -m "feat(guest): split listeners — guest API on Funnel, admin on tailnet only"
  ```

---

## Notes for the executor

- **Not in this plan (later plans):** camera streaming from the NVR / LL-HLS (Plan 2), and the iOS and Android apps (Plans 3–4). The guest surface here is doors only.
- **Manual end-to-end check** after Task 4 (not a unit test, since it needs the live tailnet + Funnel): deploy, confirm `POST /admin/guests` over the tailnet returns an enroll payload, enroll a token against the Funnel URL, and confirm `/guest/gate/open` opens the gate while `/guest/gate/open` with a revoked token 401s. This is the real acceptance gate and belongs in the deploy step, not the Go tests.
