# Guest Android App (v1, door-only) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A native Kotlin/Jetpack-Compose Android app a guest installs once, scans an invite QR to enroll, and then taps to open the Gate or the Out-door.

**Architecture:** Single-activity Compose app. On launch a ViewModel reads a saved session (API base + device token) from Keystore-backed encrypted storage; with none it shows a QR scanner. Scanning an invite QR yields the Funnel base URL and a one-time code; the app enrolls (`POST /guest/enroll`), stores the returned token encrypted, and shows the home screen with two action buttons. Each button `POST`s to the guest API with a `Bearer` token. A 401 (revoked) wipes the session and returns to the scanner. No camera/video in v1. This is the Kotlin twin of the shipped iOS app and consumes the **same frozen guest API**.

**Tech Stack:** Kotlin, Jetpack Compose, coroutines. AndroidX only (Compose, CameraX, `androidx.security:security-crypto`) plus ML Kit **on-device bundled** barcode scanning for the QR. HTTP via the JDK `HttpURLConnection` on `Dispatchers.IO` (no OkHttp/Retrofit). Gradle (Kotlin DSL) with the wrapper.

**Spec:** docs/superpowers/specs/2026-10-21-guest-mobile-app-design.md
**Sibling (shipped):** docs/superpowers/plans/2026-10-21-guest-ios-app.md — same behavior; match it.

## Global Constraints

- Native Kotlin/Compose. **No cross-platform framework.** **No OkHttp/Retrofit/Ktor** — use `HttpURLConnection` wrapped in `suspend` functions on `Dispatchers.IO`. AndroidX (Compose, CameraX, security-crypto) and ML Kit `barcode-scanning` (bundled, on-device, no network) are the only third-party libs allowed.
- `applicationId` **net.khaledez.homeaccess**, app label **"Home Access"**. **minSdk 26**, target the current stable SDK.
- App lives in the camonitor repo at **`apps/android/`**.
- The device **token is stored only in Keystore-backed `EncryptedSharedPreferences`** — never plain `SharedPreferences`, a file, or any log line (`Log.*`).
- The **API base URL is never hardcoded** — derived at enrollment from the scanned QR and persisted with the token.
- **QR payload contract (shared, identical to iOS):** the QR encodes `https://<host>/guest/enroll?code=<enrollCode>`. `apiBase` = that URL stripped to scheme+host+port (drop userinfo, path, query); accept ONLY `https`, a path ending `/guest/enroll`, and a non-empty `code`. Actions are `apiBase + "/guest/gate/open"` and `apiBase + "/guest/outdoor/open"`.
- Guest API semantics (frozen): `POST /guest/enroll` body `{"code":"…"}` → `200 {"token":"…"}` | `400` | `500`. `POST /guest/{gate,outdoor}/open` with `Authorization: Bearer <token>` → `204` | `401` | `502`. Network/timeout → a network error.
- **Dedupe rejected invites ONLY on `badCode`** (a `400`), NOT on network/other errors — so a transient network blip never blocks re-scanning the same single-use invite. (Learned from the iOS build.)
- v1 scope is **Gate + Out-door only.** No camera tiles, no streaming, no owner/admin screen (admin QR is minted in the tailnet web UI, already shipped).
- Build & test: `cd apps/android && ./gradlew testDebugUnitTest` (JVM unit tests) and `./gradlew assembleDebug`. Instrumented/emulator tests are out of scope for CI here.

## Review Focus

- **Revoked mid-use:** an action returns `401` → ViewModel clears the stored session and returns to the scanner; does not retry or stay on home. (Task 5)
- **Bad/used/expired code:** enroll `400` → clear "invite expired or already used" message, scanner keeps running, and the SAME code is not auto-resubmitted (dedupe). (Task 5)
- **Foreign/garbage QR:** a decoded string that isn't our `…/guest/enroll?code=…` is rejected by the parser (returns null), no enroll attempt, scanner keeps running. (Task 3 + 5)
- **Action failure vs success:** `502` and network/timeout never render as "opened"; the button re-enables with a distinct message. (Task 3 mapping + Task 5 UI)
- **Offline at launch:** a saved session loads and home renders with no network call; failures only occur on a button tap. (Task 2 + Task 5)

---

### Task 1: Gradle project scaffold

**Files:**
- Create: `apps/android/settings.gradle.kts`, `apps/android/build.gradle.kts`, `apps/android/gradle.properties`, `apps/android/app/build.gradle.kts`, `apps/android/app/src/main/AndroidManifest.xml`, `apps/android/app/src/main/java/net/khaledez/homeaccess/MainActivity.kt`, `apps/android/app/src/test/java/net/khaledez/homeaccess/SmokeTest.kt`, `apps/android/.gitignore` (ignore `.gradle/`, `build/`, `local.properties`), the Gradle wrapper.

**Interfaces:**
- Produces: a buildable `:app` module (Compose enabled, `applicationId net.khaledez.homeaccess`, minSdk 26) + a JVM `test` source set. Later tasks add Kotlin files under `app/src/main/java/net/khaledez/homeaccess/` and tests under `app/src/test/java/...`.

- [ ] **Step 1: Generate the Gradle project + wrapper**

Create the Kotlin-DSL Gradle files and `gradle wrapper` (or vendor the wrapper). Configure the Android SDK location: write `local.properties` with `sdk.dir=$HOME/Library/Android/sdk` (gitignored). Enable Compose (`buildFeatures { compose = true }`) with the matching compiler extension, and Kotlin JVM target 17.

- [ ] **Step 2: Minimal activity + smoke test**

`MainActivity`: a `ComponentActivity` with `setContent { Text("Home Access") }`. `SmokeTest.kt`: a JUnit test `@Test fun truth() = assertTrue(true)`.

- [ ] **Step 3: Build + test**

Run: `cd apps/android && ./gradlew testDebugUnitTest assembleDebug`
Expected: BUILD SUCCESSFUL; the smoke test passes.

- [ ] **Step 4: Commit** (`feat(android): scaffold Home Access guest app`).

---

### Task 2: Encrypted session store

**Files:**
- Create: `app/src/main/java/net/khaledez/homeaccess/SessionStore.kt`, `app/src/test/java/net/khaledez/homeaccess/FakeSessionStore.kt`

**Interfaces:**
- Produces:
  - `data class Session(val apiBase: String, val token: String)` (apiBase as a validated URL string).
  - `interface SessionStore { fun save(s: Session); fun load(): Session?; fun clear() }`.
  - `class KeystoreSessionStore(context: Context) : SessionStore` — backed by `EncryptedSharedPreferences` (MasterKey AES256-GCM), storing the session as two keyed strings (or one JSON string). Build-verified only (needs a device/emulator), not unit-tested in CI.
  - `class FakeSessionStore : SessionStore` (in the test source set) — an in-memory map, for ViewModel unit tests.
- Consumed by: Task 5 (ViewModel) and Task 6 (wiring, which injects `KeystoreSessionStore`).

- [ ] **Step 1: Define `Session` + `SessionStore` + `KeystoreSessionStore`**

Implement the encrypted-prefs store. No logging of the token.

- [ ] **Step 2: Implement `FakeSessionStore` in the test source set and a JVM test of it**

A JUnit test of the fake's save/load/clear/overwrite round-trip (proves the interface contract the ViewModel relies on). The real Keystore impl is covered by the build only — note this in the report.

- [ ] **Step 3: Build + test**

Run: `cd apps/android && ./gradlew testDebugUnitTest assembleDebug` — green.

- [ ] **Step 4: Commit** (`feat(android): encrypted session store + fake`).

---

### Task 3: Invite parser + guest API client

**Files:**
- Create: `app/src/main/java/net/khaledez/homeaccess/Enrollment.kt`, `app/src/main/java/net/khaledez/homeaccess/GuestClient.kt`, `app/src/test/java/net/khaledez/homeaccess/EnrollmentTest.kt`, `app/src/test/java/net/khaledez/homeaccess/GuestClientTest.kt`

**Interfaces:**
- Produces:
  - `data class Invite(val apiBase: String, val code: String)`
  - `fun parseInviteQR(raw: String): Invite?` — accept only `https`, host non-empty, path ending `/guest/enroll`, non-empty `code` query param; `apiBase` = scheme://host[:port] (userinfo, path, query, fragment stripped). Use `java.net.URI`.
  - `sealed interface GuestError` (or `enum`): `BadCode, Unauthorized, DeviceUnavailable, Server, Network`, each thrown as an exception type or returned via a Result — pick one and be consistent.
  - `class GuestClient(val apiBase: String)` with `suspend fun enroll(code: String): String`, `suspend fun openGate(token: String)`, `suspend fun openOutdoor(token: String)` — `HttpURLConnection` on `Dispatchers.IO`; map 200→token, 400→BadCode, 401→Unauthorized, 502→DeviceUnavailable, other non-2xx→Server, `IOException`→Network. Set `Authorization: Bearer <token>` on the open calls, `Content-Type: application/json` + `{"code":…}` body on enroll.
- Consumed by: Task 5.

- [ ] **Step 1: Write failing parser tests** (valid invite; reject wrong path, missing code, non-URL, `http`; strip userinfo `https://user:pass@h:8443/guest/enroll?code=X` → apiBase `https://h:8443`).

- [ ] **Step 2: Implement `parseInviteQR`** with `java.net.URI`.

- [ ] **Step 3: Write failing client tests** — stand up a localhost server with the JDK built-in `com.sun.net.httpserver.HttpServer` (no third-party MockWebServer). Assert: enroll 200→token; 400→BadCode; gate 204 success asserting method `POST`, path `/guest/gate/open`, header `Authorization: Bearer T`; 401→Unauthorized; 502→DeviceUnavailable; outdoor path `/guest/outdoor/open`; an IOException path → Network.

- [ ] **Step 4: Implement `GuestClient`.**

- [ ] **Step 5: Build + test** green.

- [ ] **Step 6: Commit** (`feat(android): invite parser and guest API client`).

---

### Task 4: QR scanner Composable

**Files:**
- Create: `app/src/main/java/net/khaledez/homeaccess/QrScanner.kt`

**Interfaces:**
- Produces: `@Composable fun QrScanner(onCode: (String) -> Unit)` — a CameraX `PreviewView` with an `ImageAnalysis` using ML Kit `BarcodeScanning` (bundled, QR format); on the FIRST decoded QR string it invokes `onCode` exactly once (guard a `reported` flag) on the main thread and stops analyzing. Consumed by Task 5.

- [ ] **Step 1: Implement `QrScanner`** — CameraX bind (`ProcessCameraProvider`), `BarcodeScannerOptions` limited to `FORMAT_QR_CODE`, one-shot guard. Declare the `CAMERA` permission + `<uses-feature camera>` in the manifest; the enroll screen requests the runtime permission (Task 5). Handle no-camera/permission-not-granted without crashing (render nothing / a placeholder; the parent shows the fallback).

- [ ] **Step 2: Build only**

Run: `cd apps/android && ./gradlew assembleDebug` — BUILD SUCCESSFUL. (Camera analysis is not unit-tested — the decode-handling path is covered through Task 3's parser; this is the one deliberately unit-untested surface, same as iOS.)

- [ ] **Step 3: Commit** (`feat(android): QR scanner composable`).

---

### Task 5: ViewModel + screens

**Files:**
- Create: `app/src/main/java/net/khaledez/homeaccess/GuestViewModel.kt`, `app/src/main/java/net/khaledez/homeaccess/EnrollScreen.kt`, `app/src/main/java/net/khaledez/homeaccess/HomeScreen.kt`, `app/src/test/java/net/khaledez/homeaccess/GuestViewModelTest.kt`

**Interfaces:**
- Consumes: `Session`/`SessionStore`/`FakeSessionStore` (T2), `parseInviteQR`/`Invite`/`GuestClient`/`GuestError` (T3), `QrScanner` (T4).
- Produces:
  - `class GuestViewModel(private val store: SessionStore, private val makeClient: (String) -> GuestApi = { GuestClient(it) })` where `interface GuestApi { suspend fun enroll(code:String):String; suspend fun openGate(token:String); suspend fun openOutdoor(token:String) }` and `GuestClient : GuestApi`. Holds observable state: `phase` (`NeedsEnrollment | Enrolled`), an enroll-error message, an action-status message. `init` loads the store to set the initial phase.
  - `suspend fun handleScanned(raw: String)`: `parseInviteQR` → if null set "not a valid invite" and stay; if `invite.code == lastRejectedCode` return without re-enrolling; else `enroll` → success: save session + `Enrolled` + clear `lastRejectedCode`; `BadCode`: set message + `lastRejectedCode = invite.code`, stay; other/Network: set a try-again message, do NOT set `lastRejectedCode`, stay.
  - `fun openGate()` / `fun openOutdoor()`: call the client with the stored token; `Unauthorized` → `store.clear()` + `NeedsEnrollment`; other errors → distinct status (never success-on-failure); success → a success status that auto-clears after ~2s (failure messages persist).

- [ ] **Step 1: Write failing `GuestViewModel` tests** (with `FakeSessionStore` + a fake `GuestApi`): starts NeedsEnrollment without a session; valid invite → Enrolled + session saved; bad code → message + stays + same code not resubmitted (1 enroll call); same code after a NETWORK failure IS resubmitted (2 calls); foreign QR → no enroll call; openGate Unauthorized → NeedsEnrollment + store cleared; openGate DeviceUnavailable → status set, stays Enrolled; openGate/openOutdoor pass the stored token. Use `kotlinx-coroutines-test` `runTest`.

- [ ] **Step 2: Implement `GuestViewModel`.**

- [ ] **Step 3: Implement `EnrollScreen` and `HomeScreen`**

`EnrollScreen`: requests the CAMERA runtime permission; when granted shows `QrScanner { raw -> vm.handleScanned(raw) }` and re-arms it after any scan that leaves phase NeedsEnrollment (recompose the scanner with a changing `key(...)`); when denied/permanently-denied shows "Camera access needed — enable it in Settings"; shows the enroll-error message. `HomeScreen`: two large buttons "Open Gate" / "Open Out-door" (disabled + spinner while in-flight), success/failure status.

- [ ] **Step 4: Build + test** green.

- [ ] **Step 5: Commit** (`feat(android): guest view model and screens`).

---

### Task 6: App wiring

**Files:**
- Modify: `app/src/main/java/net/khaledez/homeaccess/MainActivity.kt`

**Interfaces:**
- Consumes: `GuestViewModel` (T5), `KeystoreSessionStore` (T2).

- [ ] **Step 1: Wire `MainActivity`** to build a `GuestViewModel(KeystoreSessionStore(applicationContext))` and render `EnrollScreen`/`HomeScreen` based on `phase`.

- [ ] **Step 2: Final build + test**

Run: `cd apps/android && ./gradlew testDebugUnitTest assembleDebug` — all green.

- [ ] **Step 3: Commit** (`feat(android): wire main activity`).
