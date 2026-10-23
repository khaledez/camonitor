// Persistent guest records for gate / out-door access control.
//
// Guests are enrolled with a one-time code and then identified by a
// device token. Only SHA-256 hashes of codes and tokens are written to
// disk; the plaintext token is returned once, from Enroll.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const enrollCodeTTL = 10 * time.Minute

var (
	ErrGuestNotFound = errors.New("guest not found")
	ErrEnrollInvalid = errors.New("enrollment code invalid")
	ErrEnrollExpired = errors.New("enrollment code expired")
)

// Guest is the public view of a guest; it never carries hashes.
type Guest struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	Enrolled bool      `json:"enrolled"`
	Revoked  bool      `json:"revoked"`
	// Demo guests (App Store review) may watch cameras but never open doors.
	Demo bool `json:"demo,omitempty"`
}

// guestRecord is what lands on disk: the public view plus the hashes.
type guestRecord struct {
	Guest
	TokenHash  string    `json:"token_hash,omitempty"`
	CodeHash   string    `json:"code_hash,omitempty"`
	CodeExpiry time.Time `json:"code_expiry,omitempty"`
}

type GuestStore struct {
	mu     sync.Mutex
	path   string
	guests []*guestRecord
	now    func() time.Time // injectable so tests can skip the TTL wait
}

// OpenGuestStore loads the JSON file at path, or starts empty if it
// doesn't exist yet.
func OpenGuestStore(path string) (*GuestStore, error) {
	s := &GuestStore{path: path, now: time.Now}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &s.guests); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// Create adds an un-enrolled guest and returns its one-time enrollment code.
func (s *GuestStore) Create(label string) (Guest, string, error) {
	id, err := randomBytes(8)
	if err != nil {
		return Guest{}, "", err
	}
	codeBytes, err := randomBytes(7)
	if err != nil {
		return Guest{}, "", err
	}
	// 7 bytes -> 12 base32 chars; keep 10.
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(codeBytes)[:10]

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	rec := &guestRecord{
		Guest:      Guest{ID: hex.EncodeToString(id), Label: label, Created: now},
		CodeHash:   hashSecret(code),
		CodeExpiry: now.Add(enrollCodeTTL),
	}
	s.guests = append(s.guests, rec)
	if err := s.save(); err != nil {
		s.guests = s.guests[:len(s.guests)-1]
		return Guest{}, "", err
	}
	return rec.Guest, code, nil
}

func (s *GuestStore) List() []Guest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Guest, 0, len(s.guests))
	for _, r := range s.guests {
		out = append(out, r.Guest)
	}
	return out
}

func (s *GuestStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.guests {
		if r.ID == id {
			r.Revoked = true
			return s.save()
		}
	}
	return ErrGuestNotFound
}

// Enroll consumes a one-time code and returns the guest's device token.
func (s *GuestStore) Enroll(code string) (string, Guest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.find(func(r *guestRecord) bool { return r.CodeHash != "" && hashEqual(r.CodeHash, code) })
	if rec == nil || rec.Revoked {
		return "", Guest{}, ErrEnrollInvalid
	}
	if s.now().After(rec.CodeExpiry) {
		return "", Guest{}, ErrEnrollExpired
	}
	tokenBytes, err := randomBytes(32)
	if err != nil {
		return "", Guest{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)

	rec.TokenHash = hashSecret(token)
	rec.CodeHash = ""
	rec.CodeExpiry = time.Time{}
	rec.Enrolled = true
	rec.LastSeen = s.now()
	if err := s.save(); err != nil {
		return "", Guest{}, err
	}
	return token, rec.Guest, nil
}

// EnrollDemo creates an already-enrolled demo guest and returns its token.
// Each scan of the shared demo code gets its own record, so every reviewer
// device shows up in the admin list and can be revoked on its own.
// shortcut: one record per scan, never pruned; fine for a handful of reviewers.
func (s *GuestStore) EnrollDemo(label string) (string, Guest, error) {
	id, err := randomBytes(8)
	if err != nil {
		return "", Guest{}, err
	}
	tokenBytes, err := randomBytes(32)
	if err != nil {
		return "", Guest{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	rec := &guestRecord{
		Guest:     Guest{ID: hex.EncodeToString(id), Label: label, Created: now, LastSeen: now, Enrolled: true, Demo: true},
		TokenHash: hashSecret(token),
	}
	s.guests = append(s.guests, rec)
	if err := s.save(); err != nil {
		s.guests = s.guests[:len(s.guests)-1]
		return "", Guest{}, err
	}
	return token, rec.Guest, nil
}

// Authenticate returns the guest owning token; false if unknown or revoked.
func (s *GuestStore) Authenticate(token string) (Guest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.find(func(r *guestRecord) bool { return r.TokenHash != "" && hashEqual(r.TokenHash, token) })
	if rec == nil || rec.Revoked {
		return Guest{}, false
	}
	rec.LastSeen = s.now()
	// Best effort: a failed LastSeen write must not lock a guest out.
	_ = s.save()
	return rec.Guest, true
}

func (s *GuestStore) find(match func(*guestRecord) bool) *guestRecord {
	for _, r := range s.guests {
		if match(r) {
			return r
		}
	}
	return nil
}

// save writes atomically (temp file + rename). Caller holds s.mu.
func (s *GuestStore) save() error {
	data, err := json.MarshalIndent(s.guests, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// hashEqual compares a stored hash with the hash of a presented secret
// in constant time.
func hashEqual(storedHash, secret string) bool {
	return subtle.ConstantTimeCompare([]byte(storedHash), []byte(hashSecret(secret))) == 1
}
