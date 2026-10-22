package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// GuestAdmin is the owner-only guest management API; serve it on the tailnet, never the public funnel.
type GuestAdmin struct {
	store      *GuestStore
	funnelBase string
	audit      *AuditStore
}

func NewGuestAdmin(store *GuestStore, funnelBase string, audit *AuditStore) *GuestAdmin {
	return &GuestAdmin{store: store, funnelBase: funnelBase, audit: audit}
}

func (a *GuestAdmin) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/guests", a.create)
	mux.HandleFunc("GET /admin/guests", a.list)
	mux.HandleFunc("DELETE /admin/guests/{id}", a.revoke)
	mux.HandleFunc("GET /admin/audit", a.listAudit)
	return mux
}

func (a *GuestAdmin) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit")) // bad/missing -> 0 -> store default
	events, err := a.audit.List(AuditFilter{GuestID: q.Get("guest"), Action: q.Get("action"), Limit: limit})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, events)
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
	enrollURL := a.funnelBase + "/guest/enroll"
	// Invite format is the contract the mobile app parses.
	qr, err := qrDataURI(inviteURL(enrollURL, enrollCode))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{
		"id":         guest.ID,
		"label":      guest.Label,
		"enrollCode": enrollCode,
		"enrollURL":  enrollURL,
		"qr":         qr,
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

func qrDataURI(content string) (string, error) {
	png, err := qrcode.Encode(content, qrcode.Medium, 256)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

func inviteURL(enrollURL, code string) string {
	return enrollURL + "?code=" + url.QueryEscape(code)
}
