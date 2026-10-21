package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// GuestAdmin is the owner-only guest management API; serve it on the tailnet, never the public funnel.
type GuestAdmin struct {
	store      *GuestStore
	funnelBase string
}

func NewGuestAdmin(store *GuestStore, funnelBase string) *GuestAdmin {
	return &GuestAdmin{store: store, funnelBase: funnelBase}
}

func (a *GuestAdmin) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/guests", a.create)
	mux.HandleFunc("GET /admin/guests", a.list)
	mux.HandleFunc("DELETE /admin/guests/{id}", a.revoke)
	return mux
}

func (a *GuestAdmin) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	guest, enrollCode, err := a.store.Create(body.Label)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{
		"id":         guest.ID,
		"label":      guest.Label,
		"enrollCode": enrollCode,
		"enrollURL":  a.funnelBase + "/guest/enroll",
	})
}

func (a *GuestAdmin) list(w http.ResponseWriter, r *http.Request) {
	type guestView struct {
		ID       string    `json:"id"`
		Label    string    `json:"label"`
		Created  time.Time `json:"created"`
		LastSeen time.Time `json:"lastSeen"`
		Enrolled bool      `json:"enrolled"`
		Revoked  bool      `json:"revoked"`
	}
	views := []guestView{}
	for _, g := range a.store.List() {
		views = append(views, guestView{g.ID, g.Label, g.Created, g.LastSeen, g.Enrolled, g.Revoked})
	}
	writeJSON(w, views)
}

func (a *GuestAdmin) revoke(w http.ResponseWriter, r *http.Request) {
	err := a.store.Revoke(r.PathValue("id"))
	if errors.Is(err, ErrGuestNotFound) {
		http.Error(w, "guest not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
