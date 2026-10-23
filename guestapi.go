package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const guestActionTimeout = 10 * time.Second

type guestActions interface {
	OpenGate(ctx context.Context) error
	OpenOutdoor(ctx context.Context) error
}

// GuestAPI is the public (Funnel-facing) surface: enroll, door buttons and self-revoke.
type GuestAPI struct {
	store   *GuestStore
	actions guestActions
	audit   *AuditStore // nil disables auditing
	// DemoCode, when set, is a reusable enroll code for App Store review:
	// it enrolls a cameras-only demo guest. Empty disables it.
	DemoCode string
}

const demoGuestLabel = "App Review (demo)"

func NewGuestAPI(store *GuestStore, actions guestActions, audit *AuditStore) *GuestAPI {
	return &GuestAPI{store: store, actions: actions, audit: audit}
}

// record is best-effort: an audit failure must never change the HTTP outcome.
func (a *GuestAPI) record(r *http.Request, e AuditEvent) {
	e.Time = time.Now()
	e.SourceIP, e.SourceDevice = clientSource(r)
	_ = a.audit.Log(e)
}

func (a *GuestAPI) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /guest/enroll", a.enroll)
	mux.HandleFunc("POST /guest/gate/open", a.requireToken("gate", a.actions.OpenGate))
	mux.HandleFunc("POST /guest/outdoor/open", a.requireToken("outdoor", a.actions.OpenOutdoor))
	mux.HandleFunc("POST /guest/forget", a.forget)
	return mux
}

// forget is the guest's own "remove access from this phone": revoke the caller's device.
func (a *GuestAPI) forget(w http.ResponseWriter, r *http.Request) {
	token, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	guest, ok := a.store.Authenticate(token)
	if !hasBearer || !ok {
		a.record(r, AuditEvent{Action: "forget", Result: "denied", GuestLabel: "unknown"})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := a.store.Revoke(guest.ID); err != nil {
		a.record(r, AuditEvent{Action: "forget", Result: "failed", GuestID: guest.ID, GuestLabel: guest.Label})
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.record(r, AuditEvent{Action: "forget", Result: "success", GuestID: guest.ID, GuestLabel: guest.Label})
	w.WriteHeader(http.StatusNoContent)
}

func (a *GuestAPI) enroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	isDemoCode := a.DemoCode != "" && subtle.ConstantTimeCompare([]byte(req.Code), []byte(a.DemoCode)) == 1
	var token string
	var guest Guest
	var err error
	if isDemoCode {
		token, guest, err = a.store.EnrollDemo(demoGuestLabel)
	} else {
		token, guest, err = a.store.Enroll(req.Code)
	}
	if errors.Is(err, ErrEnrollInvalid) || errors.Is(err, ErrEnrollExpired) {
		a.record(r, AuditEvent{Action: "enroll", Result: "denied"})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.record(r, AuditEvent{Action: "enroll", Result: "success", GuestID: guest.ID, GuestLabel: guest.Label})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token":   token,
		"label":   guest.Label,
		"created": guest.Created.Unix(),
		"demo":    guest.Demo,
	})
}

// requireToken authenticates the bearer token, then runs open with a short timeout.
func (a *GuestAPI) requireToken(action string, open func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		guest, ok := a.store.Authenticate(token)
		if !hasBearer || !ok {
			a.record(r, AuditEvent{Action: action, Result: "denied", GuestLabel: "unknown"})
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if guest.Demo {
			a.record(r, AuditEvent{Action: action, Result: "denied", GuestID: guest.ID, GuestLabel: guest.Label})
			http.Error(w, "demo pass cannot open doors", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), guestActionTimeout)
		defer cancel()
		if err := open(ctx); err != nil {
			a.record(r, AuditEvent{Action: action, Result: "failed", GuestID: guest.ID, GuestLabel: guest.Label})
			http.Error(w, "action failed", http.StatusBadGateway)
			return
		}
		a.record(r, AuditEvent{Action: action, Result: "success", GuestID: guest.ID, GuestLabel: guest.Label})
		w.WriteHeader(http.StatusNoContent)
	}
}
