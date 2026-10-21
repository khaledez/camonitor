package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newTestAdmin(t *testing.T) (http.Handler, *GuestStore) {
	t.Helper()
	store, err := OpenGuestStore(filepath.Join(t.TempDir(), "guests.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewGuestAdmin(store, "https://monitor.example.ts.net").Routes(), store
}

func createGuest(t *testing.T, h http.Handler, label string) map[string]string {
	t.Helper()
	req := httptest.NewRequest("POST", "/admin/guests", strings.NewReader(`{"label":"`+label+`"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create status = %d", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCreateReturnsEnrollment(t *testing.T) {
	h, _ := newTestAdmin(t)
	out := createGuest(t, h, "Housekeeper")
	if out["id"] == "" || out["enrollCode"] == "" {
		t.Fatalf("missing id/enrollCode: %v", out)
	}
	if out["enrollURL"] != "https://monitor.example.ts.net/guest/enroll" {
		t.Fatalf("enrollURL = %q", out["enrollURL"])
	}
}

func TestListReturnsGuests(t *testing.T) {
	h, _ := newTestAdmin(t)
	a := createGuest(t, h, "A")
	createGuest(t, h, "B")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/guests", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d", len(list))
	}
	if strings.Contains(rec.Body.String(), a["enrollCode"]) || strings.Contains(strings.ToLower(rec.Body.String()), "token") {
		t.Fatalf("list leaks secrets: %s", rec.Body.String())
	}
}

func TestDeleteRevokes(t *testing.T) {
	h, store := newTestAdmin(t)
	g := createGuest(t, h, "A")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/admin/guests/"+g["id"], nil))
	if rec.Code != 204 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !store.List()[0].Revoked {
		t.Fatal("guest not revoked")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/admin/guests/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown id status = %d", rec.Code)
	}
}
