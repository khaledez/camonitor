# Dahua VTO / VTH protocol notes

Field notes from integrating camonitor with a real Dahua intercom system.
Written so the next developer reaching for **two-way audio (talkback)** —
or any other extension — doesn't have to repeat the discovery work.

## Devices in this deployment

| Role | IP | Model | Firmware |
|---|---|---|---|
| Main VTO (Front Door) | `192.168.88.200` | `DHI-VTO3211D-P1-S2` | `4.500.0000002.0.R` |
| Sub VTO (Gate) | `192.168.88.202` | `VTO3311Q-WP` | `4.510.0000002.1.R` |
| Indoor VTH | `192.168.88.201` | `VTH2421F_R` | `4.410.0.0` |

The main vs sub designation is a *Dahua-internal* concept: each VTO has a
`SIP.IsMainVTO` flag; one device in the cluster runs the SIP registrar
and holds the VTH list, the others register to it as sub-stations.

Numbering inside the cluster:

- VTOs are identified by a numeric `No.` (e.g. `8001` for `.200`,
  `8002` for `.202`). It shows up as the SIP `UserID` and as the
  `LocalNumber` in call logs.
- VTHs are addressed by `Room#Extension`, e.g. `9901#0`–`9901#9`. The
  `#` is a sub-extension within the same household — the physical VTH
  is usually `#0`; `#1`–`#9` are free for additional indoor displays /
  family devices. **camonitor currently registers as `9901#9`.**

## Authentication

Two credential surfaces:

- **HTTP CGI** (`/cgi-bin/*`): RFC 2617 HTTP Digest. Same admin
  credentials as the web UI. `digest.go` parses both the "qop=auth"
  form used here and the legacy no-qop form used by Dahua RTSP.
- **RTSP** (`rtsp://`): also HTTP Digest, embedded in the URL.
- **SIP** (`sip:user@host`): digest realm is `VDP`. The username string
  used in the digest response **must include the `#`** literally
  (`9901#9`, not `9901%239`) even though the VTO writes back `%23` on
  the wire. The Request-URI in REGISTER **must have no port**
  (`sip:192.168.88.200`, not `:5060`) or digest hashes won't match.
- **SDK / proprietary** (TCP 37777): a different protocol family — see
  *Talkback* below. Not currently used by camonitor.

VTOs apply mild anti-bruteforce throttling after several failed digest
attempts (silent UDP drops for ~30s after a 401 storm).

## Endpoints camonitor uses today

All HTTP, all on the VTO. Digest-authenticated with the admin
credentials in `config.json`.

| Purpose | Endpoint | Notes |
|---|---|---|
| Snapshot (JPEG) | `GET /cgi-bin/snapshot.cgi?channel=1` | `snapshot.go` |
| Door relay | `GET /cgi-bin/accessControl.cgi?action=openDoor&channel=1&UserID=101&Type=Remote` | `door.go` |
| Live event stream | `GET /cgi-bin/eventManager.cgi?action=attach&codes=[All]&heartbeat=10` | `eventattach.go` — see next section |
| RTSP main stream | `rtsp://<host>:554/cam/realmonitor?channel=1&subtype=0` | 1280×720, H.264 baseline, PCM L16/16000 audio |
| RTSP sub stream | `…&subtype=1` | 352×288 |
| Config read | `GET /cgi-bin/configManager.cgi?action=getConfig&name=<TABLE>` | Returns `table.<TABLE>.<field>=<value>` lines |
| Config write | `GET /cgi-bin/configManager.cgi?action=setConfig&<TABLE>.<field>=<value>` | Use `--data-urlencode` for the bracketed array indices |
| Reboot | `GET /cgi-bin/magicBox.cgi?action=reboot` | ~60–90s downtime |
| Call history | `GET /cgi-bin/recordFinder.cgi?action=find&name=VideoTalkLog&count=N` | shows `PeerNumber` actually dialed |

## The event stream is the real ring trigger

This was the key discovery of this integration. On every firmware we
tested, the door-button press does NOT generate a SIP INVITE through
the embedded SIP registrar, even when:

- `SIP.IsMainVTO=1` is set on the main VTO,
- the VTH is reachable and (allegedly) registered,
- camonitor has successfully registered as a `9901#X` sub-extension.

The button press DOES light up the VTH at `.201` — but via Dahua's
proprietary intercom protocol (TCP 37777, advertised in the event
payload as `TCPPort: 37777`), not SIP. The SIP registrar serves a
different purpose (presence, "where is room X reachable now") that
this firmware doesn't actually use for incoming calls. The
`VideoTalkLog` records the call but the SIP path stays silent.

What IS reliable across firmwares: `eventManager.cgi?action=attach`.
It's a long-lived HTTP response with `Content-Type:
multipart/x-mixed-replace; boundary=myboundary`. Each part is one event:

```
--myboundary
Content-Type: text/plain
Content-Length: 188

Code=CallNoAnswered;action=Start;index=1;data={
   "CallID" : "7",
   "IsEncryptedStream" : false,
   "LockNum" : 2,
   "SupportPaas" : false,
   "TCPPort" : 37777,
   "UserID" : "9901"
}
```

A single bell press emits two events back-to-back:

| Code | Action | Meaning |
|---|---|---|
| `Invite` | `Pulse` | the moment the INVITE-equivalent is fired |
| `CallNoAnswered` | `Start` | sustained while ringing |

Plus periodic `Heartbeat` parts (body literally `Heartbeat`) on the
interval set by the `heartbeat` query param.

`eventattach.go` consumes this stream and calls `BellBus.Fire(streamID)`
on either `Invite` or `CallNoAnswered`; `BellBus`'s per-stream debounce
collapses the duplicate.

**Implications for future work:**

- Any new "device did X" trigger we want to surface (door physically
  opened, motion, tamper, card swipe, etc.) almost certainly arrives
  via the same event stream — sniff once with `[All]` to see the code
  name, then narrow.
- Outbound HTTP from camonitor works through standard pod networking,
  so this path doesn't need `hostNetwork: true`.

## SIP: what works and what doesn't

The SIP plumbing in `sip.go` is fully functional — REGISTER + digest +
periodic refresh against the main VTO's embedded server — but on this
firmware it never receives an INVITE for the bell. It's left wired in
because (a) some other Dahua firmware revisions DO route through SIP
and (b) it's harmless when no INVITEs arrive.

Things you'll trip over if you revisit the SIP code:

- **No web UI for "be the SIP registrar."** The
  `Network → SIP Server` page only exposes external-server modes
  (DSS Express, VTO, ThirdParty). To turn this VTO into the registrar,
  set `SIP.IsMainVTO=1` via the API:
  `setConfig?SIP.IsMainVTO=1` (value must be `1`, not `true`; takes
  effect after reboot).
- **VTO accepts REGISTER but ignores OPTIONS.** SIP OPTIONS probes
  time out; only request-line-style probes (REGISTER, INVITE) get
  responses. So OPTIONS-based health checks are useless against Dahua.
- **`Expires` is clamped to 60s.** No matter what you ask for, the
  device responds with `Expires: 60`. `sip.go`'s `refreshInterval`
  honors this with a half-life refresh.
- **`#` in usernames is normalised to `%23` on the wire** but the
  digest username string must be the literal `#` form
  (`username="9901#9"`). Anything else returns `403 Forbidden`.
- **Request-URI must have no port.** Use `sip:host`, not `sip:host:port`,
  or digest hashes won't match. The 401 challenge advertises
  `uri="sip:<host>"` and the response has to be byte-identical.
- **Pod-to-LAN UDP doesn't work in this k8s cluster** (Flannel on
  Talos). TCP egress to the LAN works, public UDP DNS works, but UDP
  to a LAN IP silently drops. That's why the deployment runs with
  `hostNetwork: true` today — the event-attach path no longer needs
  it, but the SIP REGISTER does. If we ever drop `sip.go` entirely,
  the manifest can revert to standard pod networking.

## VTO ↔ VTH call flow (current understanding)

Drawing it out for clarity:

```
              [ button press on VTO ]
                       │
       ┌───────────────┼───────────────┐
       │               │               │
       ▼               ▼               ▼
  proprietary       eventMgr.cgi    SIP registrar
  TCP 37777         /attach stream  (UAS in this VTO)
  to VTH            (anyone listening)   │
       │               │               │
       ▼               ▼               ▼
   VTH rings,    camonitor's       …silent on this
   audio+video   eventattach.go    firmware; would
   bridged via   sees `Code=Invite`,fire INVITE on
   Dahua SDK     fires bell event  others
```

Two-way **audio** between the VTO speaker/mic and the VTH speaker/mic
goes over the same proprietary 37777 channel. The VTO is doing both
the call-setup signalling and the RTP-equivalent media transport on
that link.

## What we need to build for talkback (Phase 2)

The user goal is: from a browser tab, press a "talk" button and have
the visitor hear me through the VTO speaker (and ideally hear them
back through the VTO microphone). Two viable paths:

### Path A — implement enough of the Dahua SDK on TCP 37777

The native intercom protocol. Pros: this is what actually drives the
device; works regardless of firmware quirks. Cons: proprietary, no
official Go binding. Possible references:

- `python-dahua-vto` ([github.com/elad-bar/python-dahua-vto](https://github.com/elad-bar/python-dahua-vto))
  — reverse-engineered Python implementation of the binary protocol,
  including audio talk.
- `go2rtc` ([github.com/AlexxIT/go2rtc](https://github.com/AlexxIT/go2rtc))
  — has a Dahua intercom driver that does talkback. Worth reading
  for the wire format alone.

Minimum work: open a TCP connection to `<vto>:37777`, log in, send
the audio-talk request frames, stream G.711 PCM, handle the bidirectional
session.

### Path B — answer SIP and bridge audio properly

Today `sip.go` replies `486 Busy Here`. If a future firmware (or a
different model) routes the INVITE through SIP, we could `200 OK` it
instead, negotiate PCMA/8000 in the SDP, and bridge RTP both ways
into a WebRTC PeerConnection. This is the "phase 2" mentioned in the
existing comments.

Pros: clean, standards-based. Cons: only works on firmware that
actually delivers INVITEs, which the current main VTO does not.

### Path C — hack: HTTP audio playback

Some Dahua models expose `POST /cgi-bin/audio.cgi` to push audio
*into* the speaker (one-way only). Not verified on these models; if
it works, it's a quick way to do TTS announcements without solving
the talkback problem. Worth a 30-minute probe before committing to
Path A or B.

### Recommendation

Start with **Path A** scoped to TTS playback only: send a pre-recorded
WAV or TTS chunk to the VTO speaker on demand, no microphone return
yet. That validates the SDK plumbing with the smallest surface. Full
duplex (mic + speaker bridged into WebRTC) is a bigger project.

## Pitfalls / lessons learned

- **`setConfig` booleans use `1`/`0`, not `true`/`false`.** The latter
  return `Error` with no detail.
- **Bracketed array indices in `setConfig` URLs need encoding.**
  curl `--data-urlencode "Encode[0].MainFormat[0].Video.BitRate=2048"`
  works; raw `&Encode[0]...` does not — curl mangles the brackets.
- **VTO web SPA can wedge into a `dll.main.js` 404** on some firmware
  revisions; a reboot via `magicBox.cgi?action=reboot` reliably
  recovers it.
- **Pod-to-LAN UDP** is broken in our Flannel-on-Talos cluster (see
  SIP section). Anything UDP-to-LAN belongs in a `hostNetwork: true`
  pod or behind `hostPort`.
- **Different VTO models in the same cluster have different bitrate
  defaults.** Gate (`VTO3311Q-WP`) shipped at 1024 kbps for 720p main,
  Front Door (`VTO3211D-P1-S2`) at 2048 kbps. Match them with
  `setConfig?Encode[0].MainFormat[0].Video.BitRate=2048&…Quality=6`
  for consistent image quality.

## Reading list for the next pass

- Dahua HTTP API guide (PDF, leaked publicly) — comprehensive list
  of `configManager.cgi` table names.
- whatsmeow docs — relevant only if extending the WhatsApp side, but
  the README and `examples/` are short.
- go2rtc's `pkg/dahua/` directory — cleanest Go-side reference for
  the binary 37777 protocol.
