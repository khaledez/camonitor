package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/rtp"
	"github.com/brutella/hap/service"
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
	if code := recordQR(t, m).Code; code != 404 {
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
