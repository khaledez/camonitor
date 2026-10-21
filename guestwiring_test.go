package main

import (
	"net/http"
	"net/http/httptest"
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
