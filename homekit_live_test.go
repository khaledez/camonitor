package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These tests drive a real hap.Server over a real TCP port. They exist
// because the unit tests above mock the store, and mocking the store hid a
// genuine defect: hap.Server closes the http.Server it was built with and
// never rebuilds it, so bouncing the accessory has to construct a fresh
// server. Nothing short of binding a socket catches that.

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitForPort polls until the address accepts a connection, or fails.
func waitForPort(t *testing.T, port int, want bool, within time.Duration) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
		}
		if (err == nil) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if want {
		t.Fatalf("%s never started accepting within %v", addr, within)
	}
	t.Fatalf("%s still accepting after %v", addr, within)
}

func newLiveManager(t *testing.T, port int) *HomeKitManager {
	t.Helper()
	cfg := HomeKitConfig{Pin: "031-45-154", Port: port, Store: t.TempDir()}
	// nil bell/snapshot: bridge only, so these tests bind exactly one port.
	m, err := NewHomeKitManager(cfg, testStreams, &fakeDoors{}, &fakeGree{name: "AC", status: off}, nil, nil)
	if err != nil {
		t.Fatalf("NewHomeKitManager: %v", err)
	}

	// Confine Bonjour to loopback. These accessories carry the same names
	// as a real deployment, and a test run must not put a second
	// "camonitor" bridge on the developer's network for the Home app to
	// find. (It did, before this line existed.)
	for _, s := range m.servers {
		s.srv.Ifaces = []string{loopbackInterface(t)}
	}
	return m
}

func loopbackInterface(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list interfaces: %v", err)
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback != 0 {
			return i.Name
		}
	}
	t.Fatal("no loopback interface")
	return ""
}

func TestBridgeServesAndStopsWithContext(t *testing.T) {
	port := freePort(t)
	m := newLiveManager(t, port)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	waitForPort(t, port, true, 10*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	waitForPort(t, port, false, 10*time.Second)
}

// A HomeKit port already held by something else must not take the service
// down with it — doors, cameras and the web UI matter more than HomeKit.
func TestPortConflictDoesNotKillTheService(t *testing.T) {
	port := freePort(t)
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer blocker.Close()

	m := newLiveManager(t, port)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	// It should keep retrying rather than exiting or panicking.
	select {
	case <-done:
		t.Fatal("Run exited on a port conflict; it should retry")
	case <-time.After(2 * time.Second):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// The setup code is a permanent credential for the door locks, so it must
// not be served once the bridge is paired.
func TestSetupCodeIsWithheldOncePaired(t *testing.T) {
	m := newLiveManager(t, freePort(t))
	s := m.servers[0]

	var st HomeKitStatus
	decodeStatus(t, m, &st)
	if st.Pin == "" {
		t.Error("pin withheld while unpaired; the user cannot pair")
	}
	if code := recordQR(t, m).Code; code != 200 {
		t.Errorf("qr.png = %d while unpaired, want 200", code)
	}

	// Simulate a paired controller by writing what hap writes.
	if err := s.store.Set("controller.pairing", []byte("{}")); err != nil {
		t.Fatalf("seed pairing: %v", err)
	}

	decodeStatus(t, m, &st)
	if !st.Paired {
		t.Fatal("status still reports unpaired after a pairing was stored")
	}
	if st.Pin != "" {
		t.Errorf("pin %q served after pairing; it grants door-lock control", st.Pin)
	}
	if code := recordQR(t, m).Code; code != 404 {
		t.Errorf("qr.png = %d after pairing, want 404", code)
	}
}

func decodeStatus(t *testing.T, m *HomeKitManager, out *HomeKitStatus) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.HandleStatus(rec, httptest.NewRequest(http.MethodGet, "/homekit/status", nil))
	*out = HomeKitStatus{}
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("decode status: %v", err)
	}
}

func recordQR(t *testing.T, m *HomeKitManager) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	m.HandleQR(rec, httptest.NewRequest(http.MethodGet, "/homekit/qr.png", nil))
	return rec
}
