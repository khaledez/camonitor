package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const streamSessionTTL = 5 * time.Minute

type streamHandlers interface {
	Handler(camera string) (http.Handler, bool)
}

// StreamAPI is the public guest camera surface. The device token is only
// accepted at /session; playlist and segment requests carry the signed,
// expiring ?s= token it hands out, so the device token never leaves the app.
type StreamAPI struct {
	store  *GuestStore
	hub    streamHandlers
	secret []byte
	audit  *AuditStore // nil disables auditing
}

func NewStreamAPI(store *GuestStore, hub streamHandlers, secret []byte, audit *AuditStore) *StreamAPI {
	return &StreamAPI{store: store, hub: hub, secret: secret, audit: audit}
}

func (a *StreamAPI) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /guest/cam/{camera}/session", a.session)
	mux.HandleFunc("GET /guest/cam/{camera}/hls/{file...}", a.hls)
	return mux
}

// record is best-effort: an audit failure must never change the HTTP outcome.
func (a *StreamAPI) record(r *http.Request, e AuditEvent) {
	e.Action = "cam"
	e.Time = time.Now()
	e.SourceIP, e.SourceDevice = clientSource(r)
	_ = a.audit.Log(e)
}

func (a *StreamAPI) session(w http.ResponseWriter, r *http.Request) {
	camera := r.PathValue("camera")
	token, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	guest, ok := a.store.Authenticate(token)
	if !hasBearer || !ok {
		a.record(r, AuditEvent{Result: "denied", GuestLabel: "unknown"})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, known := a.hub.Handler(camera); !known {
		http.NotFound(w, r)
		return
	}
	exp := time.Now().Add(streamSessionTTL)
	a.record(r, AuditEvent{Result: "success", GuestID: guest.ID, GuestLabel: guest.Label})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"url":     "/guest/cam/" + camera + "/hls/index.m3u8?s=" + url.QueryEscape(signStream(a.secret, camera, exp)),
		"expires": exp.Unix(),
	})
}

func (a *StreamAPI) hls(w http.ResponseWriter, r *http.Request) {
	camera := r.PathValue("camera")
	if !verifyStream(a.secret, camera, r.URL.Query().Get("s"), time.Now()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	hub, known := a.hub.Handler(camera)
	if !known {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix("/guest/cam/"+camera+"/hls", hub).ServeHTTP(w, r)
}
