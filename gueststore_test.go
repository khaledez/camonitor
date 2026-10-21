package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestGuestStore(t *testing.T) (*GuestStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guests.json")
	s, err := OpenGuestStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s, path
}

func enrollNewGuest(t *testing.T, s *GuestStore) (Guest, string) {
	t.Helper()
	g, code, err := s.Create("alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	token, _, err := s.Enroll(code)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return g, token
}

func TestEnrollHappyPath(t *testing.T) {
	s, _ := newTestGuestStore(t)
	g, token := enrollNewGuest(t, s)
	if token == "" {
		t.Fatal("empty token")
	}
	got, ok := s.Authenticate(token)
	if !ok || got.ID != g.ID || !got.Enrolled {
		t.Fatalf("authenticate = %+v, %v", got, ok)
	}
}

func TestEnrollIsOneTime(t *testing.T) {
	s, _ := newTestGuestStore(t)
	_, code, _ := s.Create("alice")
	if _, _, err := s.Enroll(code); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Enroll(code); !errors.Is(err, ErrEnrollInvalid) {
		t.Fatalf("err = %v, want ErrEnrollInvalid", err)
	}
}

func TestEnrollExpired(t *testing.T) {
	s, _ := newTestGuestStore(t)
	_, code, _ := s.Create("alice")
	s.now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if _, _, err := s.Enroll(code); !errors.Is(err, ErrEnrollExpired) {
		t.Fatalf("err = %v, want ErrEnrollExpired", err)
	}
}

func TestAuthenticateRejectsRevoked(t *testing.T) {
	s, _ := newTestGuestStore(t)
	g, token := enrollNewGuest(t, s)
	if err := s.Revoke(g.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Authenticate(token); ok {
		t.Fatal("revoked guest authenticated")
	}
	if err := s.Revoke("nope"); !errors.Is(err, ErrGuestNotFound) {
		t.Fatalf("err = %v, want ErrGuestNotFound", err)
	}
}

func TestAuthenticateRejectsUnknown(t *testing.T) {
	s, _ := newTestGuestStore(t)
	if _, ok := s.Authenticate("garbage"); ok {
		t.Fatal("garbage authenticated")
	}
}

func TestPersistenceReload(t *testing.T) {
	s, path := newTestGuestStore(t)
	_, token := enrollNewGuest(t, s)

	s2, err := OpenGuestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Authenticate(token); !ok {
		t.Fatal("token not valid after reload")
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(token)) {
		t.Fatal("plaintext token found in stored file")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
