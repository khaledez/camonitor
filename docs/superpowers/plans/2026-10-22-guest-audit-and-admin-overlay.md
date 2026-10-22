# Guest Audit Log + Admin Overlay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record every guest action and denial in an on-disk audit log, and replace the cramped guests drawer in the web UI with a full-width "Manage & Audit" overlay (guest list + activity log + QR onboarding), keeping the camera grid as the home screen.

**Architecture:** A new `AuditStore` (SQLite via the pure-Go `modernc.org/sqlite` driver, mirroring `bellstore.go`) persists events on the `monitor-state` PVC with a 60-day nightly prune. The guest API logs enroll/open/denial events best-effort (logging never fails a guest action). A tailnet-only `GET /admin/audit` serves the log. The web UI turns the 👥 button into a full-width overlay over the camera grid, themed to the existing dark app, with Manage and Activity sections and the add-guest→QR flow.

**Tech Stack:** Go 1.27, `modernc.org/sqlite` (already a dependency), stdlib `net/http`. Vanilla JS in `web/index.html`.

**Spec:** docs/superpowers/specs/2026-10-21-guest-mobile-app-design.md (this extends it with an audit trail + a reworked owner admin UI; design mock approved 2026-10-22, camera-home + full-width overlay).

## Global Constraints

- Pure Go only — use the existing `modernc.org/sqlite` driver (`sql.Open("sqlite", …)`); NO cgo, keep the `FROM scratch` / `CGO_ENABLED=0` build working. Mirror `bellstore.go`'s patterns (DSN, WAL/pragmas, prune).
- **Audit logging is best-effort:** a logging failure MUST NOT fail or delay a guest's gate/door/enroll action — log the error and continue. A nil `*AuditStore` is valid and no-ops (like `BellStore`).
- **Never log secrets:** no tokens, no enrollment codes. Log guest id/label, action, result, and source.
- **Retention: 60 days.** Prune on open and on a daily ticker: delete rows older than 60 days.
- **Surface rules (security-critical, unchanged model):** the audit API and all admin routes stay on the **tailnet listener only** — never on the Funnel listener and never on the LAN `:8080` mux. The guest Funnel API keeps only its four routes. The camera grid and all existing `:8080` routes stay exactly as they are.
- Web overlay matches the existing dark theme tokens in `web/index.html` (`#0e0e10` ground, `#161618`/`#1c1c20` surfaces, `#2a2a2e`/`#3a3a3f` borders, `#d6d6d6` text, `#888` muted, status green `#5ec97c` / red `#d97070`, one accent consistent with the app). Use the approved mock's LAYOUT, not its light palette.

## Review Focus

- **Logging failure during an action:** if `AuditStore.Log` errors (disk full, locked), the gate still opens and the handler still returns 204. (Task 3)
- **Denied/unknown token:** an open attempt with a missing/unknown/revoked bearer token is logged as a denial (guest "unknown", result "denied") with its source, and still returns 401. (Task 3)
- **Audit never on a public surface:** `GET /admin/audit` is absent from the Funnel listener and the `:8080` mux; present only on the tailnet listener. (Task 4)
- **Source behind Funnel:** the logged source IP reflects the real client, not the Funnel ingress, when a header carries it; falls back safely to `RemoteAddr`. (Task 2)
- **Empty log / first run:** a fresh DB (no rows) returns `[]` (not null) from the API and the web Activity view renders an empty state, no error. (Task 1 + Task 5)

---

### Task 1: AuditStore (SQLite)

**Files:**
- Create: `auditstore.go`, `auditstore_test.go`

**Interfaces:**
- Produces:
  - `type AuditEvent struct { Time time.Time; GuestID string; GuestLabel string; Action string; Result string; SourceIP string; SourceDevice string }` (JSON tags: `time,guest_id,guest_label,action,result,source_ip,source_device`).
  - `type AuditStore struct { … }` (nil is valid — methods no-op / return empty, like `BellStore`).
  - `func OpenAuditStore(path string) (*AuditStore, error)` — opens/creates the SQLite DB (WAL, same DSN/pragmas as `bellstore.go`), creates the `audit` table (autoincrement id, ts INTEGER unix-seconds, guest_id, guest_label, action, result, source_ip, source_device), prunes >60d on open.
  - `func (s *AuditStore) Log(e AuditEvent) error` — inserts one row; `nil` receiver returns nil.
  - `func (s *AuditStore) List(f AuditFilter) ([]AuditEvent, error)` where `type AuditFilter struct { GuestID string; Action string; Limit int }` — most-recent-first, optional equality filters, default/max limit (e.g. 200); `nil` receiver returns `[]AuditEvent{}`.
  - `func (s *AuditStore) Prune(before time.Time) (int64, error)` — deletes rows older than `before`.
- Consumed by: Tasks 3, 4.

- [ ] **Step 1: Write failing tests** (`t.TempDir()` for the DB path):

```go
func TestAuditLogAndListRoundTrip(t *testing.T) // log 3 events → List returns them most-recent-first with fields intact
func TestAuditFilterByGuestAndAction(t *testing.T) // filter GuestID / Action narrows results
func TestAuditPruneDropsOld(t *testing.T) // a row with ts 61d ago is pruned, a 59d one kept
func TestAuditNilStoreIsNoop(t *testing.T) // var s *AuditStore; s.Log(...) == nil; s.List(...) == empty
func TestAuditListEmptyReturnsEmptySlice(t *testing.T) // fresh DB → len 0, not nil
```

- [ ] **Step 2: Run tests, verify they fail.**
- [ ] **Step 3: Implement `auditstore.go`** mirroring `bellstore.go` (open, schema, nil-safety, prune).
- [ ] **Step 4: Run tests, verify pass; `go build ./...` clean (FROM-scratch driver).**
- [ ] **Step 5: Commit** (`feat(guest): sqlite audit store with 60-day prune`).

---

### Task 2: Request source extraction

**Files:**
- Create: `guestsource.go`, `guestsource_test.go`

**Interfaces:**
- Produces: `func clientSource(r *http.Request) (ip string, device string)` — `ip` is the real client when a forwarding header carries it, else the host part of `r.RemoteAddr`; `device` is a trimmed User-Agent (cap length, e.g. 120 chars; empty → "").
- Consumed by: Task 3.

- [ ] **Step 1: Write failing tests**

```go
func TestClientSourceUsesForwardedHeader(t *testing.T) // X-Forwarded-For "1.2.3.4, 10.0.0.1" → "1.2.3.4"
func TestClientSourceFallsBackToRemoteAddr(t *testing.T) // no header, RemoteAddr "9.9.9.9:5555" → "9.9.9.9"
func TestClientSourceDeviceFromUserAgent(t *testing.T) // UA string → trimmed/capped device
func TestClientSourceEmptyWhenNoUA(t *testing.T)
```

- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement `clientSource`.** Builder's note: behind Tailscale Funnel the real client IP may arrive via a forwarding header rather than `RemoteAddr` (the Funnel ingress). Prefer `X-Forwarded-For` (first hop) if present; otherwise `RemoteAddr`'s host. Keep this the single place that decides, so it's trivial to adjust once verified against a live Funnel request. Parse `RemoteAddr` with `net.SplitHostPort` (tolerate a missing port).
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Commit** (`feat(guest): client source (ip/device) extraction`).

---

### Task 3: Log guest actions + denials

**Files:**
- Modify: `guestapi.go`, `guestapi_test.go`

**Interfaces:**
- Consumes: `AuditStore`/`AuditEvent` (T1), `clientSource` (T2), `GuestStore.Enroll` (returns `(token, Guest, error)`), `GuestStore.Authenticate` (returns `(Guest, bool)`).
- Produces: `NewGuestAPI` gains an `audit *AuditStore` parameter (store it; nil tolerated). Internally:
  - `enroll`: on success log `{Action:"enroll", Result:"success", Guest from Enroll, source}`; on `ErrEnrollInvalid`/`ErrEnrollExpired` log `{Action:"enroll", Result:"denied", GuestLabel:"", source}` (no code logged). Still return the same status codes.
  - `requireToken` becomes `requireToken(action string, open func(context.Context) error)` and is registered as `requireToken("gate", …)` / `requireToken("outdoor", …)`. On bad/missing token → log `{Action:action, Result:"denied", GuestID:"", GuestLabel:"unknown", source}`, return 401. On success → run opener; log `{Action:action, Result:"success"|"failed", guest, source}` (failed on opener error, still 502). Capture the `Guest` from `Authenticate`.
  - All logging via a small helper; errors from `Log` are ignored (best-effort) — never change the HTTP outcome.

- [ ] **Step 1: Write failing tests** (a fake/in-memory audit sink implementing a minimal interface, or a real `OpenAuditStore(t.TempDir())` then `List`):

```go
func TestEnrollSuccessLogged()        // 200 → one enroll/success event with the guest
func TestEnrollBadCodeLoggedDenied()  // 400 → enroll/denied event, no token/code in it
func TestOpenGateSuccessLogged()      // 204 → gate/success event with guest id+label + source ip
func TestOpenUnauthorizedLoggedDenied()// 401 → gate/denied event, guest "unknown"
func TestOpenDeviceUnavailableLoggedFailed() // 502 → gate/failed event
func TestLogErrorDoesNotBreakAction() // audit Log returning error still yields 204
```

- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** the audit wiring in `guestapi.go` (thread `action` through `requireToken`, capture the guest, call `clientSource`, log best-effort).
- [ ] **Step 4: Run all guestapi tests, verify pass.**
- [ ] **Step 5: Commit** (`feat(guest): audit enroll, open, and denial events`).

---

### Task 4: Admin audit endpoint + wiring + prune

**Files:**
- Modify: `guestadmin.go`, `guestadmin_test.go`, `guestwiring.go`, `main.go`

**Interfaces:**
- Consumes: `AuditStore.List` (T1), `GuestConfig`.
- Produces:
  - `GuestConfig` gains `AuditPath string json:"audit_path,omitempty"` (default `/var/lib/camonitor/guest-audit.db` set in `loadConfig`).
  - `NewGuestAdmin` gains an `audit *AuditStore`; adds `GET /admin/audit` → JSON `[]AuditEvent` via `List`, honoring `?guest=<id>&action=<name>&limit=<n>` query params; reuses `writeJSON`. Returns `[]` (not null) when empty.
  - Wiring (`guestwiring.go`/`main.go`): open the audit store when guest access is configured (degrade to nil + log on open failure, like the bell store — never crash), pass it to both `NewGuestAPI` and `NewGuestAdmin`, and start a daily prune goroutine (`Prune(now-60d)`). The admin routes (incl. `/admin/audit`) remain on the tailnet surface only.

- [ ] **Step 1: Write failing tests**

```go
func TestAdminAuditReturnsEvents()     // seed store, GET /admin/audit → JSON list newest-first
func TestAdminAuditFiltersByGuest()    // ?guest=g1 narrows
func TestAdminAuditEmptyIsArray()      // fresh → "[]" not "null"
// extend the existing TestAdminNotOnLanSurface to assert /admin/audit is NOT on the LAN/Funnel surface
func TestGuestConfigDefaultsAuditPath()// loadConfig sets the default when guest configured and audit_path empty
```

- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** the endpoint, config default, wiring, and the prune ticker.
- [ ] **Step 4: Run full suite + `go vet`; verify pass, FROM-scratch build clean.**
- [ ] **Step 5: Commit** (`feat(guest): tailnet-only /admin/audit, config, daily prune`).

---

### Task 5: Web admin overlay

**Files:**
- Modify: `web/index.html`

**Interfaces:**
- Consumes: `GET /admin/guests`, `DELETE /admin/guests/{id}`, `POST /admin/guests`, `GET /admin/audit` (all tailnet, same origin as the page).

**Deliverable:** the 👥 Guests button opens a **full-width overlay** over the camera grid (not the old narrow `#guests` aside), themed to the existing dark app, following the approved mock's layout:
- A top bar with **← Cameras** (closes the overlay back to the grid) and two tabs: **Manage** and **Activity**.
- **Manage:** the guest list (label, status pill Active/Pending/Revoked, last activity, device, Revoke/Cancel) + an **Add guest** action that reveals the name field → on create shows the QR (existing `qr` data-URI from `POST /admin/guests`) + copy-able enroll link + expiry/single-use note. (Reuse the existing create/revoke JS; restyle into the overlay.)
- **Activity:** a table from `GET /admin/audit` — time, guest, action (with an icon), result pill (Success / Denied / Failed), source — with simple guest/action filter selects that re-query. Empty state when no rows.
- The camera grid, header, and every other panel (bell, climate, HomeKit, Centsys) stay exactly as they are; only the guests panel is replaced by the overlay and the new Activity view is added.

- [ ] **Step 1: Replace the `#guests` aside with the full-width overlay markup + the two tabs + the Activity table container.**
- [ ] **Step 2: Wire the JS:** open/close the overlay from 👥 / ← Cameras; tab switching; `refreshGuests()` (existing) for Manage; a new `refreshAudit()` fetching `/admin/audit` (+ filters) and rendering rows; keep create/revoke working.
- [ ] **Step 3: Verify** the page still loads and the camera grid + other panels are untouched: `go build ./...` (the page is embedded/served as-is — confirm it's still valid and the existing handlers serve it). Load-check the markup is well-formed. (No unit test for the page; the Go API is covered by Tasks 1–4.)
- [ ] **Step 4: Commit** (`feat(web): full-width guests & activity admin overlay`).
