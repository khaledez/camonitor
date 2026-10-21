package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type fakeActions struct {
	gateCalls, outdoorCalls int
	err                     error
}

func (f *fakeActions) OpenGate(ctx context.Context) error    { f.gateCalls++; return f.err }
func (f *fakeActions) OpenOutdoor(ctx context.Context) error { f.outdoorCalls++; return f.err }

func newTestAPI(t *testing.T) (*GuestStore, *fakeActions, http.Handler) {
	t.Helper()
	store, err := OpenGuestStore(filepath.Join(t.TempDir(), "guests.json"))
	if err != nil {
		t.Fatal(err)
	}
	actions := &fakeActions{}
	return store, actions, NewGuestAPI(store, actions).Routes()
}

func do(h http.Handler, path, body, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// enrolledToken creates a guest and enrolls it directly through the store.
func enrolledToken(t *testing.T, store *GuestStore) (Guest, string) {
	t.Helper()
	g, code, err := store.Create("alice")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Enroll(code)
	if err != nil {
		t.Fatal(err)
	}
	return g, token
}

func TestEnrollReturnsToken(t *testing.T) {
	store, _, h := newTestAPI(t)
	_, code, _ := store.Create("alice")
	rec := do(h, "/guest/enroll", `{"code":"`+code+`"}`, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"token":"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestEnrollBadCodeIs400(t *testing.T) {
	_, _, h := newTestAPI(t)
	if rec := do(h, "/guest/enroll", `{"code":"nope"}`, ""); rec.Code != 400 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestActionRequiresToken(t *testing.T) {
	_, actions, h := newTestAPI(t)
	if rec := do(h, "/guest/gate/open", "", ""); rec.Code != 401 {
		t.Fatalf("got %d", rec.Code)
	}
	if actions.gateCalls != 0 {
		t.Fatal("opener called without token")
	}
}

func TestActionRejectsRevokedToken(t *testing.T) {
	store, actions, h := newTestAPI(t)
	g, token := enrolledToken(t, store)
	if err := store.Revoke(g.ID); err != nil {
		t.Fatal(err)
	}
	if rec := do(h, "/guest/gate/open", "", token); rec.Code != 401 {
		t.Fatalf("got %d", rec.Code)
	}
	if actions.gateCalls != 0 {
		t.Fatal("opener called for revoked token")
	}
}

func TestGateActionCallsOpener(t *testing.T) {
	store, actions, h := newTestAPI(t)
	_, token := enrolledToken(t, store)
	if rec := do(h, "/guest/gate/open", "", token); rec.Code != 204 {
		t.Fatalf("got %d", rec.Code)
	}
	if actions.gateCalls != 1 || actions.outdoorCalls != 0 {
		t.Fatalf("gate=%d outdoor=%d", actions.gateCalls, actions.outdoorCalls)
	}
}

func TestOutdoorActionCallsOpener(t *testing.T) {
	store, actions, h := newTestAPI(t)
	_, token := enrolledToken(t, store)
	if rec := do(h, "/guest/outdoor/open", "", token); rec.Code != 204 {
		t.Fatalf("got %d", rec.Code)
	}
	if actions.outdoorCalls != 1 || actions.gateCalls != 0 {
		t.Fatalf("gate=%d outdoor=%d", actions.gateCalls, actions.outdoorCalls)
	}
}

func TestOpenerErrorIs502(t *testing.T) {
	store, actions, h := newTestAPI(t)
	_, token := enrolledToken(t, store)
	actions.err = errors.New("boom")
	if rec := do(h, "/guest/gate/open", "", token); rec.Code != 502 {
		t.Fatalf("got %d", rec.Code)
	}
}
