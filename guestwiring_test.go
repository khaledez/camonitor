package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAdminNotOnLanSurface(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	base := http.NewServeMux()
	base.Handle("/marker", ok)
	guests, err := OpenGuestStore(filepath.Join(t.TempDir(), "guests.json"))
	if err != nil {
		t.Fatal(err)
	}
	admin := NewGuestAdmin(guests, "https://m.ts.net", nil).Routes()

	lan, ts := buildSurfaces(base, admin)

	status := func(h http.Handler, path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	for _, path := range []string{"/admin/guests", "/admin/audit"} {
		if got := status(lan, path); got != http.StatusNotFound {
			t.Errorf("lan %s = %d, want 404", path, got)
		}
		if got := status(ts, path); got == http.StatusNotFound {
			t.Errorf("ts %s = 404, want routed", path)
		}
	}
	for name, h := range map[string]http.Handler{"lan": lan, "ts": ts} {
		if got := status(h, "/marker"); got != http.StatusOK {
			t.Errorf("%s /marker = %d, want 200", name, got)
		}
	}
}

func TestGuestConfigRequiresPublicBase(t *testing.T) {
	load := func(guest string) error {
		path := filepath.Join(t.TempDir(), "c.json")
		body := `{"guest": ` + guest + `, "streams": [{"id": "vto1", "host": "192.168.88.200"}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfig(path)
		return err
	}
	if load(`{}`) == nil {
		t.Error("accepted guest block without public_base")
	}
	if err := load(`{"public_base": "https://m.ts.net:8443"}`); err != nil {
		t.Errorf("rejected valid guest block: %v", err)
	}
}

func TestGuestConfigDefaultsAuditPath(t *testing.T) {
	load := func(guest string) string {
		path := filepath.Join(t.TempDir(), "c.json")
		body := `{"guest": ` + guest + `, "streams": [{"id": "vto1", "host": "192.168.88.200"}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Guest.AuditPath
	}
	if got := load(`{"public_base": "https://m.ts.net:8443"}`); got != "/var/lib/camonitor/guest-audit.db" {
		t.Errorf("default = %q", got)
	}
	if got := load(`{"public_base": "https://m.ts.net:8443", "audit_path": "/x/a.db"}`); got != "/x/a.db" {
		t.Errorf("explicit = %q", got)
	}
}

func TestGuestCamerasRequireStreamSecret(t *testing.T) {
	load := func(guest string) error {
		path := filepath.Join(t.TempDir(), "c.json")
		body := `{"guest": ` + guest + `, "streams": [{"id": "vto1", "host": "192.168.88.200"}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfig(path)
		return err
	}
	const base = `"public_base": "https://m.ts.net:8443"`
	const cameras = `"cameras": {"10": "rtsp://u:p@192.168.88.210:554/x"}`
	if load(`{`+base+`, `+cameras+`}`) == nil {
		t.Error("accepted guest.cameras without stream_secret")
	}
	if err := load(`{` + base + `, ` + cameras + `, "stream_secret": "s"}`); err != nil {
		t.Errorf("rejected cameras with secret: %v", err)
	}
	if err := load(`{` + base + `}`); err != nil {
		t.Errorf("rejected guest block without cameras: %v", err)
	}
}

func newFunnelTest(t *testing.T, withCameras bool) (*GuestStore, *AuditStore, http.Handler) {
	t.Helper()
	store, err := OpenGuestStore(filepath.Join(t.TempDir(), "guests.json"))
	if err != nil {
		t.Fatal(err)
	}
	audit, err := OpenAuditStore(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	guestAPI := NewGuestAPI(store, &fakeActions{}, audit)
	var streamAPI *StreamAPI
	if withCameras {
		streamAPI, err = NewStreamAPI(store, fakeHub{}, testStreamSecret, audit)
		if err != nil {
			t.Fatal(err)
		}
	}
	return store, audit, buildGuestFunnel(guestAPI, streamAPI)
}

func TestFunnelServesCamRoutesBehindToken(t *testing.T) {
	_, _, funnel := newFunnelTest(t, true)
	if got := do(funnel, "/guest/cam/10/session", "", "").Code; got != http.StatusUnauthorized {
		t.Errorf("cam session without token = %d, want 401", got)
	}
}

func TestFunnelWithoutCamerasHas404CamAndWorkingGate(t *testing.T) {
	store, _, funnel := newFunnelTest(t, false)
	_, token := enrolledToken(t, store)
	if got := do(funnel, "/guest/cam/10/session", "", token).Code; got != http.StatusNotFound {
		t.Errorf("cam session = %d, want 404", got)
	}
	if got := do(funnel, "/guest/gate/open", "", token).Code; got != http.StatusNoContent {
		t.Errorf("gate = %d, want 204", got)
	}
}

func TestCamRoutesNotOnAdminSurfaces(t *testing.T) {
	base := http.NewServeMux()
	lan, ts := buildSurfaces(base, http.NotFoundHandler())
	for name, h := range map[string]http.Handler{"lan": lan, "ts": ts} {
		if got := do(h, "/guest/cam/10/session", "", "").Code; got != http.StatusNotFound {
			t.Errorf("%s cam session = %d, want 404", name, got)
		}
	}
}

func TestForgetRevokesCallerAndAudits(t *testing.T) {
	store, audit, funnel := newFunnelTest(t, false)
	guest, token := enrolledToken(t, store)

	if got := do(funnel, "/guest/forget", "", "").Code; got != http.StatusUnauthorized {
		t.Errorf("forget without token = %d, want 401", got)
	}
	if got := do(funnel, "/guest/forget", "", token).Code; got != http.StatusNoContent {
		t.Fatalf("forget = %d, want 204", got)
	}
	if _, ok := store.Authenticate(token); ok {
		t.Error("token still authenticates after forget")
	}
	events, err := audit.List(AuditFilter{Action: "forget", GuestID: guest.ID})
	if err != nil || len(events) != 1 || events[0].Result != "success" {
		t.Errorf("forget audit = %+v, err %v", events, err)
	}
}
