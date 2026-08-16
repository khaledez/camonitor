package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/rtp"
	"github.com/brutella/hap/service"
	"github.com/brutella/hap/tlv8"
)

func testBellBus(t *testing.T) (*BellBus, snapshotFetcher) {
	t.Helper()
	fetch := func(context.Context, StreamConfig) ([]byte, error) {
		return nil, errors.New("no camera in tests")
	}
	return NewBellBus(testStreams, fetch, nil, time.UTC, nil), fetch
}

func TestCameraAccessoryShape(t *testing.T) {
	bell, fetch := testBellBus(t)
	cam := newHKCamera(testStreams[0], bell, fetch)

	if cam.a.Type != accessory.TypeVideoDoorbell {
		t.Errorf("category = %d, want VideoDoorbell (%d)", cam.a.Type, accessory.TypeVideoDoorbell)
	}
	if cam.a.Name() != "FrontDoor" {
		t.Errorf("name = %q, want FrontDoor", cam.a.Name())
	}

	counts := map[string]int{}
	for _, s := range cam.a.Ss {
		counts[s.Type]++
	}
	if counts[service.TypeDoorbell] != 1 {
		t.Errorf("%d Doorbell services, want 1", counts[service.TypeDoorbell])
	}
	if counts[service.TypeCameraRTPStreamManagement] != concurrentStreams {
		t.Errorf("%d stream managements, want %d (one per simultaneous viewer)",
			counts[service.TypeCameraRTPStreamManagement], concurrentStreams)
	}
	// iOS rejects a camera accessory that advertises no audio at all, so
	// the muted microphone has to be present even though we send none.
	if counts[service.TypeMicrophone] != 1 {
		t.Errorf("%d Microphone services, want 1", counts[service.TypeMicrophone])
	}
	if !cam.doorbell.Primary {
		t.Error("Doorbell is not the primary service; the Home app keys its tile off this")
	}
}

func TestCameraStreamManagementAdvertisesSupportedConfigs(t *testing.T) {
	bell, fetch := testBellBus(t)
	cam := newHKCamera(testStreams[0], bell, fetch)

	for i, sess := range cam.sessions {
		if len(sess.svc.SupportedVideoStreamConfiguration.Bytes.Value()) == 0 {
			t.Errorf("session %d advertises no video configuration", i)
		}
		if len(sess.svc.SupportedAudioStreamConfiguration.Bytes.Value()) == 0 {
			t.Errorf("session %d advertises no audio configuration; iOS rejects that", i)
		}
		if len(sess.svc.SupportedRTPConfiguration.Bytes.Value()) == 0 {
			t.Errorf("session %d advertises no crypto suite", i)
		}
	}
}

func TestRingFiresOnlyForItsOwnStream(t *testing.T) {
	bell, fetch := testBellBus(t)
	cam := newHKCamera(testStreams[0], bell, fetch) // vto1

	before := cam.doorbell.ProgrammableSwitchEvent.Value()

	cam.ring(BellEvent{StreamID: "vto2"})
	if got := cam.doorbell.ProgrammableSwitchEvent.Value(); got != before {
		t.Error("doorbell fired for another camera's ring")
	}

	cam.ring(BellEvent{StreamID: "vto1"})
	if got := cam.doorbell.ProgrammableSwitchEvent.Value(); got != characteristic.ProgrammableSwitchEventSinglePress {
		t.Errorf("ProgrammableSwitchEvent = %d, want SinglePress", got)
	}
}

func TestStreamStartBeforeSetupIsRejected(t *testing.T) {
	sess := newHKStreamSession(testStreams[0], "vto1/0")
	// A start with no prior SetupEndpoints has nowhere to send and no key;
	// failing the write is the only honest answer.
	if err := sess.startStream(streamConfigFor(1280, 720)); err == nil {
		t.Error("startStream succeeded without endpoints being set up")
	}
}

func TestManagerPublishesOneCameraServerPerDoor(t *testing.T) {
	bell, fetch := testBellBus(t)
	cfg := HomeKitConfig{Pin: "031-45-154", Port: 51826, Store: t.TempDir()}

	m, err := NewHomeKitManager(cfg, testStreams, &fakeDoors{}, &fakeGree{name: "AC", status: off}, bell, fetch)
	if err != nil {
		t.Fatalf("NewHomeKitManager: %v", err)
	}

	// Bridge + one accessory per door station. The H.265 camera has no
	// door and must not appear.
	if len(m.servers) != 3 {
		t.Fatalf("%d servers, want bridge + 2 cameras", len(m.servers))
	}
	if len(m.cameras) != 2 {
		t.Fatalf("%d cameras, want 2", len(m.cameras))
	}

	wantAddrs := []string{":51826", ":51827", ":51828"}
	for i, want := range wantAddrs {
		if m.servers[i].addr != want {
			t.Errorf("server %d addr = %q, want %q", i, m.servers[i].addr, want)
		}
	}

	// Distinct setup payloads: HomeKit matches a scanned code against the
	// advertising accessory by setup id, so a shared one would make the
	// cameras indistinguishable.
	seen := map[string]bool{}
	for _, s := range m.servers {
		if seen[s.uri] {
			t.Errorf("duplicate setup payload %q", s.uri)
		}
		seen[s.uri] = true
	}
}

// The cameras pair separately and each needs the code at Add Accessory
// time, so the code must survive the bridge pairing and disappear only
// once every accessory is in.
func TestSetupCodeSurvivesUntilEveryAccessoryIsPaired(t *testing.T) {
	bell, fetch := testBellBus(t)
	cfg := HomeKitConfig{Pin: "031-45-154", Port: 51826, Store: t.TempDir()}

	m, err := NewHomeKitManager(cfg, testStreams, &fakeDoors{}, &fakeGree{name: "AC", status: off}, bell, fetch)
	if err != nil {
		t.Fatalf("NewHomeKitManager: %v", err)
	}

	for i, s := range m.servers {
		if m.fullyPaired() {
			t.Fatalf("reported fully paired with %d of %d accessories still unpaired",
				len(m.servers)-i, len(m.servers))
		}
		var st HomeKitStatus
		decodeStatus(t, m, &st)
		if st.Pin == "" {
			t.Fatalf("setup code withheld while %q is still unpaired", s.name)
		}

		if err := s.store.Set("controller.pairing", []byte("{}")); err != nil {
			t.Fatalf("seed pairing: %v", err)
		}
	}

	if !m.fullyPaired() {
		t.Fatal("not fully paired after every accessory was paired")
	}
	var st HomeKitStatus
	decodeStatus(t, m, &st)
	if st.Pin != "" {
		t.Errorf("setup code %q still served after setup completed", st.Pin)
	}
	if code := recordQR(t, m, "").Code; code != 404 {
		t.Errorf("qr.png = %d after setup completed, want 404", code)
	}
}

// streamConfigFor builds the SelectedRTPStreamConfiguration iOS would send
// for a given resolution.
func streamConfigFor(width, height uint16) (cfg rtp.StreamConfiguration) {
	cfg.Video.Attributes.Width = width
	cfg.Video.Attributes.Height = height
	cfg.Video.RTP.PayloadType = 99
	cfg.Video.RTP.MTU = 1378
	return cfg
}

// Each accessory needs its own QR: they share a setup code but not a setup
// id, and HomeKit matches a scanned payload by the latter. Serving one
// shared QR is what left the doorbells unpairable — scanning it just found
// the bridge again.
func TestEachAccessoryServesItsOwnQR(t *testing.T) {
	bell, fetch := testBellBus(t)
	cfg := HomeKitConfig{Pin: "031-45-154", Port: 51826, Store: t.TempDir()}

	m, err := NewHomeKitManager(cfg, testStreams, &fakeDoors{}, &fakeGree{name: "AC", status: off}, bell, fetch)
	if err != nil {
		t.Fatalf("NewHomeKitManager: %v", err)
	}

	seen := map[string]string{}
	for _, s := range m.servers {
		rec := recordQR(t, m, s.name)
		if rec.Code != 200 {
			t.Fatalf("qr for %q = %d, want 200", s.name, rec.Code)
		}
		body := rec.Body.String()
		if other, dup := seen[body]; dup {
			t.Errorf("%q and %q serve an identical QR; one of them cannot be added", s.name, other)
		}
		seen[body] = s.name
	}

	if got := recordQR(t, m, "NoSuchAccessory").Code; got != 404 {
		t.Errorf("qr for an unknown accessory = %d, want 404", got)
	}
}

// Pairing one accessory must not take the others' codes away — that is the
// whole point of the per-accessory split.
func TestPairingOneAccessoryLeavesTheOthersPairable(t *testing.T) {
	bell, fetch := testBellBus(t)
	cfg := HomeKitConfig{Pin: "031-45-154", Port: 51826, Store: t.TempDir()}

	m, err := NewHomeKitManager(cfg, testStreams, &fakeDoors{}, &fakeGree{name: "AC", status: off}, bell, fetch)
	if err != nil {
		t.Fatalf("NewHomeKitManager: %v", err)
	}

	bridge := m.servers[0]
	if err := bridge.store.Set("controller.pairing", []byte("{}")); err != nil {
		t.Fatalf("seed pairing: %v", err)
	}

	if got := recordQR(t, m, bridge.name).Code; got != 404 {
		t.Errorf("qr for the paired bridge = %d, want 404", got)
	}
	for _, s := range m.servers[1:] {
		if got := recordQR(t, m, s.name).Code; got != 200 {
			t.Errorf("qr for still-unpaired %q = %d, want 200", s.name, got)
		}
	}

	var st HomeKitStatus
	decodeStatus(t, m, &st)
	if st.Accessories[0].Paired != true {
		t.Error("bridge not reported as paired")
	}
	for _, a := range st.Accessories[1:] {
		if a.Paired {
			t.Errorf("%q reported paired when it is not", a.Name)
		}
	}
}

// setupEndpointsRequest builds the TLV iOS writes to begin a stream.
func setupEndpointsRequest(t *testing.T, controllerPort uint16) string {
	t.Helper()
	key, salt := randomSRTPKeySalt()
	b, err := tlv8.Marshal(rtp.SetupEndpoints{
		SessionId: []byte("0123456789abcdef"),
		ControllerAddr: rtp.Addr{
			IPVersion:    rtp.IPAddrVersionv4,
			IPAddr:       "127.0.0.1",
			VideoRtpPort: controllerPort,
			AudioRtpPort: controllerPort + 1,
		},
		Video: rtp.CryptoSuite{Type: rtp.CryptoSuite_AES_CM_128_HMAC_SHA1_80, MasterKey: key, MasterSalt: salt},
		Audio: rtp.CryptoSuite{Type: rtp.CryptoSuite_AES_CM_128_HMAC_SHA1_80, MasterKey: key, MasterSalt: salt},
	})
	if err != nil {
		t.Fatalf("marshal SetupEndpoints: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// TestSetupEndpointsAnswersTheController is the regression test for the
// defect that shipped in v0.9.0: the answer was written into the same
// characteristic the controller had just written, and hap overwrites it
// with the request immediately afterwards. iOS was left with no endpoint
// and abandoned the stream before ever sending a start command — the Home
// app showed "No Response".
func TestSetupEndpointsAnswersTheController(t *testing.T) {
	sess := newHKStreamSession(testStreams[0], "vto1/0")

	if !sess.svc.SetupEndpoints.IsWriteResponse() {
		t.Fatal("SetupEndpoints is not marked write-response; the controller cannot receive the answer")
	}

	const controllerPort = 50000
	request := setupEndpointsRequest(t, controllerPort)

	value, code := sess.svc.SetupEndpoints.C.SetValueRequest(
		request, httptest.NewRequest(http.MethodPut, "/characteristics", nil))
	if code != 0 {
		t.Fatalf("SetupEndpoints write rejected with status %d", code)
	}

	encoded, ok := value.(string)
	if !ok || encoded == "" {
		t.Fatalf("write returned %#v, want a base64 TLV answer", value)
	}
	if encoded == request {
		t.Fatal("write echoed the controller's own request back; that is the v0.9.0 bug")
	}

	resp := decodeSetupResponse(t, encoded)
	if resp.Status != rtp.SessionStatusSuccess {
		t.Errorf("status = %d, want success", resp.Status)
	}

	// The advertised port must be one we actually bound — the controller
	// addresses RTCP to it. v0.9.0 echoed the controller's own port.
	bound := uint16(sess.currentSetup().conn.LocalAddr().(*net.UDPAddr).Port)
	if resp.AccessoryAddr.VideoRtpPort != bound {
		t.Errorf("advertised video port %d, but bound %d",
			resp.AccessoryAddr.VideoRtpPort, bound)
	}
	if resp.AccessoryAddr.VideoRtpPort == controllerPort {
		t.Error("advertised the controller's own port as the accessory's")
	}
	if resp.SsrcVideo < 0 {
		t.Errorf("SsrcVideo = %d, want non-negative", resp.SsrcVideo)
	}

	// A plain read must return the answer too, not the request hap left in
	// the stored value.
	readBack, code := sess.svc.SetupEndpoints.C.ValueRequest(
		httptest.NewRequest(http.MethodGet, "/characteristics", nil))
	if code != 0 {
		t.Fatalf("read rejected with status %d", code)
	}
	if readBack != encoded {
		t.Error("reading SetupEndpoints did not return the answer")
	}
}

// Renegotiation is routine on iOS; each attempt must not leak its socket.
func TestRepeatedSetupClosesThePreviousSocket(t *testing.T) {
	sess := newHKStreamSession(testStreams[0], "vto1/0")
	put := httptest.NewRequest(http.MethodPut, "/characteristics", nil)

	if _, code := sess.svc.SetupEndpoints.C.SetValueRequest(setupEndpointsRequest(t, 50000), put); code != 0 {
		t.Fatalf("first setup rejected: %d", code)
	}
	first := sess.currentSetup().conn

	if _, code := sess.svc.SetupEndpoints.C.SetValueRequest(setupEndpointsRequest(t, 50002), put); code != 0 {
		t.Fatalf("second setup rejected: %d", code)
	}
	second := sess.currentSetup().conn

	if first == second {
		t.Fatal("second setup reused the first socket")
	}
	// Writing to a closed socket errors; that is how we know it was closed.
	if _, err := first.WriteToUDP([]byte("x"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}); err == nil {
		t.Error("the superseded socket is still open")
	}
}

func decodeSetupResponse(t *testing.T, encoded string) rtp.SetupEndpointsResponse {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	var resp rtp.SetupEndpointsResponse
	if err := tlv8.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp
}
