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
