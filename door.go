// Door-control endpoint. The Dahua VTO accepts an HTTP GET to
//
//	/cgi-bin/accessControl.cgi?action=openDoor&channel=1&UserID=101&Type=Remote
//
// authenticated with the same credentials as RTSP. The URL is documented in
// Dahua's HTTP API guides for VTO products and triggers the relay that
// drives the magnetic lock.
//
// This file owns the full client→camera flow: a browser hits
// POST /door/open?id=<streamID>, we look up the stream, do one challenge-
// response Digest exchange, and reply with 204 No Content on success.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const doorOpenTimeout = 5 * time.Second

type DoorClient struct {
	streams map[string]StreamConfig
	http    *http.Client
}

func NewDoorClient(streams []StreamConfig) *DoorClient {
	m := make(map[string]StreamConfig, len(streams))
	for _, s := range streams {
		m[s.ID] = s
	}
	return &DoorClient{
		streams: m,
		http:    &http.Client{Timeout: doorOpenTimeout},
	}
}

// HandleOpen serves POST /door/open?id=<streamID>.
func (d *DoorClient) HandleOpen(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), doorOpenTimeout)
	defer cancel()

	if err := d.Open(ctx, id); err != nil {
		log.Printf("door open [%s]: %v", id, err)
		switch {
		case errors.Is(err, errUnknownStream):
			http.Error(w, "unknown stream", http.StatusNotFound)
		case errors.Is(err, context.DeadlineExceeded):
			http.Error(w, "camera did not respond in time", http.StatusGatewayTimeout)
		default:
			http.Error(w, "door open failed", http.StatusBadGateway)
		}
		return
	}
	log.Printf("door open [%s]: ok", id)
	w.WriteHeader(http.StatusNoContent)
}

var errUnknownStream = errors.New("unknown stream")

// Open performs the digest-authenticated GET that triggers the door relay.
// The first request is sent unauthenticated to obtain a 401 challenge with
// the realm/nonce; the second request carries the computed Authorization
// header.
//
// Camera response bodies are logged on the server and never propagated up
// — they can contain HTML/error markup that we don't want flowing back to
// the browser via http.Error.
func (d *DoorClient) Open(ctx context.Context, streamID string) error {
	s, ok := d.streams[streamID]
	if !ok {
		return fmt.Errorf("%w: %q", errUnknownStream, streamID)
	}

	target := s.DoorOpenURL()

	resp, err := d.send(ctx, target, "")
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		drainAndClose(resp.Body)
		return nil
	}
	if resp.StatusCode != http.StatusUnauthorized {
		logBody(streamID, resp)
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	chal, err := parseDigestChallenge(resp.Header.Get("WWW-Authenticate"))
	drainAndClose(resp.Body)
	if err != nil {
		return fmt.Errorf("parse challenge: %w", err)
	}

	parsed, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("parse door url: %w", err)
	}
	auth := buildDigestResponse(http.MethodGet, parsed.RequestURI(), s.User, s.Pass, chal)

	authed, err := d.send(ctx, target, auth)
	if err != nil {
		return err
	}
	defer drainAndClose(authed.Body)
	if authed.StatusCode != http.StatusOK {
		logBody(streamID, authed)
		return fmt.Errorf("auth failed: status %d", authed.StatusCode)
	}
	return nil
}

func (d *DoorClient) send(ctx context.Context, target, authHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return d.http.Do(req)
}

func drainAndClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4096))
	_ = rc.Close()
}

// logBody records the camera's response body to server logs for debugging
// failed door-open attempts. It MUST NOT return the body to the caller —
// devices have been observed returning HTML, which would end up rendered
// inside http.Error if propagated.
func logBody(streamID string, resp *http.Response) {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	body := strings.TrimSpace(string(b))
	if body == "" {
		return
	}
	log.Printf("door [%s] http %d body: %s", streamID, resp.StatusCode, body)
}
