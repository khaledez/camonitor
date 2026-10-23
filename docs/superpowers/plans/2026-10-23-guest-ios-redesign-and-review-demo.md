# Guest iOS Redesign + Review Demo Pass — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A reusable cameras-only demo pass for Apple review, and the redesigned iOS app (Home / Camera / Access, Arabic + English, live cameras, paste-link enrolment, self-revoke).

**Architecture:** Backend: a config `guest.demo_code` that `GuestAPI.enroll` matches before the normal one-time codes; each match creates an enrolled `Demo` guest; door handlers return 403 for demo guests before calling the opener; enroll returns `{token,label,created,demo}`. iOS: `GuestClient` gains `cameraSession`/`forget` and a richer enroll result; `AppModel` owns session + actions; three SwiftUI screens in one `NavigationStack`; `AVPlayerLayer`-backed `LivePlayer` re-fetches the signed URL when the item fails (15-min expiry).

**Tech Stack:** Go 1.x stdlib (camonitor); Swift 5 / SwiftUI / AVFoundation, iOS 17, XcodeGen; `ar.lproj/Localizable.strings` (English keys are the source).

**Spec:** docs/superpowers/specs/2026-10-22-guest-ios-redesign-and-review-demo-design.md

## Global Constraints

- QR/link contract unchanged: `https://<host>/guest/enroll?code=<code>`; the app never special-cases the demo code.
- `guest.demo_code` optional; when set, length ≥ 16 or startup fails.
- Demo guest label: `App Review (demo)`.
- Door endpoints: demo guest → 403, audited `denied`, opener never called. 401 still means re-enrol.
- Enroll JSON: `{"token": string, "label": string, "created": <unix seconds int>, "demo": bool}`.
- Camera ids: `10` = Front camera / الكاميرا الأمامية, `15` = Courtyard / الساحة. Household: Abu Khaled's Home / دار أبو خالد. "Access granted by Khaled".
- Accent `#2d7d8e`; Camera screen dark (`#0d1316`); other screens `#f1f5f6`.
- No HD/SD toggle. No third-party dependencies. Old Keychain sessions (token + apiBase only) must still decode.
- Remove access: call `POST /guest/forget`, then wipe Keychain whatever the result.

## Review Focus

- Demo code typed with different case or whitespace → must not match (exact, constant-time compare). Test in Task 1.
- Revoked demo guest calling a door → 401, not 403. Test in Task 1.
- Phone enrolled before this update (session without label/created/demo) → app opens to Home, Access shows no name/date. Test in Task 2.
- Stream session 401 (guest revoked while watching) → app goes back to enrolment. Test in Task 2.
- Pasted text with surrounding whitespace/newline → still parses. Test in Task 2.

---

### Task 1: Backend demo pass

**Files:** Modify `main.go` (GuestConfig, loadConfig, wiring), `gueststore.go`, `guestapi.go`. Test: `guestapi_test.go`, `gueststore_test.go`, `main_test.go` (if config tests live there).

**Interfaces — Produces:**
- `GuestConfig.DemoCode string \`json:"demo_code,omitempty"\``
- `Guest.Demo bool \`json:"demo,omitempty"\``
- `func (s *GuestStore) EnrollDemo(label string) (string, Guest, error)` — new enrolled record, `Demo: true`, returns plaintext token.
- `GuestAPI.DemoCode string` (exported field, set in main after `NewGuestAPI`).

- [ ] Tests: `TestEnrollDemoCodeIsReusable` (two enrolls with demo code → two distinct tokens, both authenticate, `demo:true`, label `App Review (demo)`); `TestEnrollDemoCodeExactMatchOnly` (`" "+code`, lowercased → 400); `TestDemoGuestDoorForbidden` (gate & outdoor → 403, fake opener call count 0); `TestRevokedDemoGuestDoorUnauthorized` (→401); `TestEnrollResponseFields` (normal guest → label, created == Created.Unix(), demo false); loadConfig rejects `demo_code` shorter than 16.
- [ ] Run `go test ./...` → new tests FAIL.
- [ ] Implement. Demo match uses `subtle.ConstantTimeCompare`, checked only when `DemoCode != ""`. Door 403 check sits after authentication, before the timeout/opener.
- [ ] `go test ./...` → PASS. Commit `feat(guest): reusable cameras-only demo pass for app review`.

### Task 2: iOS client + model

**Files:** Modify `apps/ios/Sources/GuestClient.swift`, `SessionStore.swift`, `AppModel.swift`, `Enrollment.swift`. Tests: `apps/ios/Tests/GuestClientTests.swift`, `AppModelTests.swift`, `SessionStoreTests.swift`, `EnrollmentTests.swift`.

**Interfaces — Produces:**
- `struct Enrolled: Decodable { token: String; label: String?; created: Int?; demo: Bool? }`
- `GuestError` gains `.forbidden` (HTTP 403).
- `GuestClientProtocol`: `enroll(code:) -> Enrolled`, `openGate/openOutdoor(token:)`, `cameraSession(camera: String, token: String) -> URL` (absolute, resolved against apiBase), `forget(token:)`.
- `Session { apiBase, token, label: String?, created: Date?, demo: Bool? }` + `var isDemo: Bool { demo ?? false }`.
- `AppModel`: `session` readable (`private(set)`), `func streamURL(camera: String) async -> URL?` (401 → reset to enrolment), `func forget() async`, door 403 → `actionStatus` = demo message, `actionSucceeded = false`.
- `parseInviteQR` trims whitespace/newlines first.

- [ ] Tests: client maps 403→`.forbidden`; `cameraSession` POSTs `guest/cam/10/session` with bearer and returns `apiBase`-absolute URL including `?s=`; enroll decodes the four fields; old session JSON `{"apiBase":…,"token":…}` decodes; model: door `.forbidden` keeps enrolled + sets demo message; `forget()` clears Keychain and returns to enrolment even when the client throws; `streamURL` `.unauthorized` → enrolment; `parseInviteQR(" https://…?code=x\n")` succeeds.
- [ ] Run `xcodebuild test -scheme HomeAccess -destination 'platform=iOS Simulator,name=iPhone 18 Pro'` → FAIL.
- [ ] Implement.
- [ ] Tests PASS. Commit `feat(ios): enroll details, camera sessions, forget, demo refusal`.

### Task 3: iOS redesigned screens + localisation

**Files:** Create `apps/ios/Sources/LivePlayer.swift`, `CameraView.swift`, `AccessView.swift`, `Theme.swift`, `apps/ios/Resources/ar.lproj/Localizable.strings`. Modify `HomeView.swift`, `EnrollView.swift`, `HomeAccessApp.swift`, `project.yml` (`options.developmentLanguage: en`).

**Interfaces — Consumes:** Task 2's `AppModel`.

- [ ] `LivePlayer` (UIViewRepresentable over `AVPlayerLayer`, muted, `.resizeAspectFill` for tiles / `.resizeAspect` full view): on appear asks `model.streamURL(camera:)`, plays; on `AVPlayerItem` `.failed` or `failedToPlayToEndTime` fetches a fresh URL (back-off 2 s). Pauses on disappear.
- [ ] Home, Camera, Access per canvas; Home's settings button and footer → Access; tiles → Camera(camera id); Camera has the two-camera switch and both door buttons; Access shows name/date when known, "what you can do" with door rows marked unavailable for demo, remove button with confirmation dialog then `model.forget()`.
- [ ] Enrol: scanner + `PasteButton(payloadType: String.self)` → `model.handleScanned`.
- [ ] Every user-visible string English key; Arabic in `ar.lproj/Localizable.strings` (canvas wording).
- [ ] `xcodegen && xcodebuild build` + tests PASS; run in simulator in English and `-AppleLanguages (ar)` and screenshot Home. Commit `feat(ios): redesigned home, camera and access screens (ar/en)`.
