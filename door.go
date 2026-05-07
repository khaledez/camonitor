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
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, context.DeadlineExceeded):
			http.Error(w, "camera did not respond in time", http.StatusGatewayTimeout)
		default:
			http.Error(w, err.Error(), http.StatusBadGateway)
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
func (d *DoorClient) Open(ctx context.Context, streamID string) error {
	s, ok := d.streams[streamID]
	if !ok {
		return fmt.Errorf("%w: %q", errUnknownStream, streamID)
	}

	target := s.DoorOpenURL()
	parsed, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("parse door url: %w", err)
	}
	requestTarget := parsed.RequestURI() // path + query — what HTTP digest signs

	resp, err := d.send(ctx, target, "")
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		drainAndClose(resp.Body)
		return nil
	case http.StatusUnauthorized:
		// expected — fall through to the challenge-response branch
	default:
		body := readShortBody(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, body)
	}

	challengeHdr := resp.Header.Get("WWW-Authenticate")
	drainAndClose(resp.Body)

	chal, err := parseDigestChallenge(challengeHdr)
	if err != nil {
		return fmt.Errorf("parse challenge: %w", err)
	}
	auth := buildDigestResponse(http.MethodGet, requestTarget, s.User, s.Pass, chal)

	resp2, err := d.send(ctx, target, auth)
	if err != nil {
		return err
	}
	defer drainAndClose(resp2.Body)

	if resp2.StatusCode != http.StatusOK {
		body := readShortBody(resp2.Body)
		return fmt.Errorf("auth failed: status %d: %s", resp2.StatusCode, body)
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

func readShortBody(rc io.ReadCloser) string {
	b, _ := io.ReadAll(io.LimitReader(rc, 1024))
	return strings.TrimSpace(string(b))
}
