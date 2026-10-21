package main

import (
	"context"
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

// GuestAPI is the public (Funnel-facing) surface: exactly three routes.
type GuestAPI struct {
	store   *GuestStore
	actions guestActions
}

func NewGuestAPI(store *GuestStore, actions guestActions) *GuestAPI {
	return &GuestAPI{store: store, actions: actions}
}

func (a *GuestAPI) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /guest/enroll", a.enroll)
	mux.HandleFunc("POST /guest/gate/open", a.requireToken(a.actions.OpenGate))
	mux.HandleFunc("POST /guest/outdoor/open", a.requireToken(a.actions.OpenOutdoor))
	return mux
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
	token, _, err := a.store.Enroll(req.Code)
	if errors.Is(err, ErrEnrollInvalid) || errors.Is(err, ErrEnrollExpired) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}

// requireToken authenticates the bearer token, then runs open with a short timeout.
func (a *GuestAPI) requireToken(open func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, ok := a.store.Authenticate(token); !hasBearer || !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), guestActionTimeout)
		defer cancel()
		if err := open(ctx); err != nil {
			http.Error(w, "action failed", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
