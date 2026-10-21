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
	admin := http.NewServeMux()
	admin.Handle("/admin/guests", ok)

	lan, ts := buildSurfaces(base, admin)

	status := func(h http.Handler, path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if got := status(lan, "/admin/guests"); got != http.StatusNotFound {
		t.Errorf("lan /admin/guests = %d, want 404", got)
	}
	if got := status(ts, "/admin/guests"); got == http.StatusNotFound {
		t.Errorf("ts /admin/guests = 404, want routed")
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
