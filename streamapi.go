package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The app must still treat a 403 on the stream as expiry and re-POST /session to renew.
const streamSessionTTL = 15 * time.Minute

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

	// The app re-requests sessions often (every tile open, every player retry);
	// logging each would bury real gate and door events in the audit view.
	now          func() time.Time // injectable so tests can skip the throttle wait
	mu           sync.Mutex
	lastLoggedAt map[string]time.Time // guest ID + camera -> last logged session
}

const minStreamSecretLen = 32

func NewStreamAPI(store *GuestStore, hub streamHandlers, secret []byte, audit *AuditStore) (*StreamAPI, error) {
	if len(secret) < minStreamSecretLen {
		return nil, errors.New("stream secret must be at least 32 bytes")
	}
	return &StreamAPI{store: store, hub: hub, secret: secret, audit: audit,
		now: time.Now, lastLoggedAt: map[string]time.Time{}}, nil
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

// shouldLogSession is true once per guest and camera per session TTL: "who
// watched, and when" survives, the repeats do not.
func (a *StreamAPI) shouldLogSession(guestID, camera string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := guestID + "/" + camera
	now := a.now()
	if last, seen := a.lastLoggedAt[key]; seen && now.Sub(last) < streamSessionTTL {
		return false
	}
	a.lastLoggedAt[key] = now
	return true
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
	if a.shouldLogSession(guest.ID, camera) {
		a.record(r, AuditEvent{Result: "success", GuestID: guest.ID, GuestLabel: guest.Label})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"url":     "/guest/cam/" + camera + "/hls/index.m3u8?s=" + url.QueryEscape(signStream(a.secret, camera, exp)),
		"expires": exp.Unix(),
	})
}

func (a *StreamAPI) hls(w http.ResponseWriter, r *http.Request) {
	camera := r.PathValue("camera")
	sig := r.URL.Query().Get("s")
	if !verifyStream(a.secret, camera, sig, time.Now()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// gohlslib copies the request query onto every URI it writes, and AVPlayer
	// rebuilds that query on each reload; it fails playback (-12312) if a URI
	// changes between reloads. Hide our token so signPlaylistURIs is the only source.
	query := r.URL.Query()
	query.Del("s")
	r.URL.RawQuery = query.Encode()
	hub, known := a.hub.Handler(camera)
	if !known {
		http.NotFound(w, r)
		return
	}
	served := http.StripPrefix("/guest/cam/"+camera+"/hls", hub)
	if !strings.HasSuffix(r.URL.Path, ".m3u8") {
		served.ServeHTTP(w, r)
		return
	}
	// Players fetch child URIs verbatim and won't add our query, so stamp the
	// same ?s= on each one; they then share this request's expiry window.
	rec := httptest.NewRecorder()
	served.ServeHTTP(rec, r)
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(rec.Code)
	if rec.Code != http.StatusOK {
		w.Write(rec.Body.Bytes())
		return
	}
	w.Write([]byte(signPlaylistURIs(rec.Body.String(), sig)))
}

var tagURIAttr = regexp.MustCompile(`URI="([^"]*)"`)

func signPlaylistURIs(playlist, sig string) string {
	lines := strings.Split(playlist, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "#"):
			lines[i] = tagURIAttr.ReplaceAllStringFunc(line, func(m string) string {
				return `URI="` + addQuery(m[len(`URI="`):len(m)-1], sig) + `"`
			})
		case strings.TrimSpace(line) != "":
			lines[i] = addQuery(strings.TrimRight(line, "\r"), sig) + line[len(strings.TrimRight(line, "\r")):]
		}
	}
	return strings.Join(lines, "\n")
}

func addQuery(uri, sig string) string {
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	return uri + sep + "s=" + url.QueryEscape(sig)
}
