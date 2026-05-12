// Dahua "event-attach" subscriber. The VTO firmware streams device events
// (button press, motion, door-open, etc.) over a long-lived HTTP response
// at /cgi-bin/eventManager.cgi?action=attach. The body is a
// multipart/x-mixed-replace stream where each part is a single event:
//
//	--myboundary
//	Content-Type: text/plain
//	Content-Length: <N>
//
//	Code=<event-code>;action=<Start|Stop|Pulse>;index=<n>;data={ json }
//
// Heartbeats look the same with body "Heartbeat".
//
// The bell-press path we observed on the user's VTO emits TWO events:
//
//   - Code=Invite;action=Pulse   (the moment the call is initiated)
//   - Code=CallNoAnswered;action=Start   (sustained while ringing)
//
// We treat either as a doorbell event and hand it to BellBus.Fire, which
// has its own per-stream debounce so the duplicate doesn't matter.
//
// This whole flow is OUTBOUND HTTP only — no NAT/hostNetwork gymnastics
// like the earlier SIP path needed, and the existing Dahua admin
// credentials (`user`/`pass` in StreamConfig) authenticate the request
// via the same digest helpers as door open / snapshot.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	eventAttachReconnectDelay = 5 * time.Second
	// keep heartbeat low so an upstream proxy / dead TCP doesn't keep us
	// connected to a silent peer for minutes.
	eventAttachHeartbeat = 10
)

// EventAttachClient subscribes to /cgi-bin/eventManager.cgi on every
// configured stream. One goroutine per VTO; each fires bell events into
// the shared BellBus when relevant codes arrive.
type EventAttachClient struct {
	streams []StreamConfig
	bell    *BellBus
	client  *http.Client
}

func NewEventAttachClient(streams []StreamConfig, bell *BellBus) *EventAttachClient {
	return &EventAttachClient{
		streams: streams,
		bell:    bell,
		// Timeout: 0 — we expect the response body to stream indefinitely.
		// Individual reads are bounded by the server's heartbeat interval.
		client: &http.Client{Timeout: 0},
	}
}

// Run starts one subscriber goroutine per stream and blocks until ctx is
// cancelled. Each subscriber reconnects with a fixed delay on disconnect.
func (e *EventAttachClient) Run(ctx context.Context) {
	if len(e.streams) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, s := range e.streams {
		wg.Add(1)
		go func(s StreamConfig) {
			defer wg.Done()
			e.subscribeLoop(ctx, s)
		}(s)
	}
	wg.Wait()
}

func (e *EventAttachClient) subscribeLoop(ctx context.Context, s StreamConfig) {
	for {
		err := e.attachOnce(ctx, s)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			log.Printf("event-attach [%s]: disconnected: %v (retry in %s)",
				s.ID, err, eventAttachReconnectDelay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(eventAttachReconnectDelay):
		}
	}
}

// attachOnce performs the digest handshake and consumes the multipart
// stream until the connection drops or ctx is cancelled.
func (e *EventAttachClient) attachOnce(ctx context.Context, s StreamConfig) error {
	target := fmt.Sprintf(
		"http://%s/cgi-bin/eventManager.cgi?action=attach&codes=%%5BAll%%5D&heartbeat=%d",
		s.Host, eventAttachHeartbeat,
	)

	// First request — unauthenticated, used to fetch the digest challenge.
	resp, err := e.do(ctx, target, "")
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		// Some firmware skips auth; consume the stream directly.
		return e.consume(ctx, resp, s)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		drainAndClose(resp.Body)
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	chal, err := parseDigestChallenge(resp.Header.Get("WWW-Authenticate"))
	drainAndClose(resp.Body)
	if err != nil {
		return fmt.Errorf("parse challenge: %w", err)
	}

	parsed, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	auth := buildDigestResponse(http.MethodGet, parsed.RequestURI(), s.User, s.Pass, chal)

	authed, err := e.do(ctx, target, auth)
	if err != nil {
		return err
	}
	if authed.StatusCode != http.StatusOK {
		drainAndClose(authed.Body)
		return fmt.Errorf("auth status %d", authed.StatusCode)
	}
	return e.consume(ctx, authed, s)
}

func (e *EventAttachClient) do(ctx context.Context, target, authHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return e.client.Do(req)
}

// consume reads the multipart stream until error / disconnect. The Dahua
// firmware uses a fixed boundary string "myboundary"; each part has a
// Content-Length header so we read exactly that many body bytes.
func (e *EventAttachClient) consume(ctx context.Context, resp *http.Response, s StreamConfig) error {
	defer resp.Body.Close()

	log.Printf("event-attach [%s]: connected", s.ID)
	r := bufio.NewReader(resp.Body)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := readNextPart(r)
		if err != nil {
			return err
		}
		if isHeartbeat(body) {
			continue
		}
		e.handleEvent(s, body)
	}
}

// readNextPart advances to the next "--myboundary" marker, then reads the
// part's Content-Length-delimited body. We deliberately ignore other
// headers; the body itself carries the event code/action/data we need.
func readNextPart(r *bufio.Reader) ([]byte, error) {
	// Scan forward to a "--myboundary" line. We tolerate stray blank
	// lines and CR/LF noise between parts.
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.TrimRight(line, "\r\n") == "--myboundary" {
			break
		}
	}

	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil {
				contentLength = n
			}
		}
	}
	if contentLength < 0 {
		return nil, errors.New("event part missing Content-Length")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func isHeartbeat(body []byte) bool {
	// Dahua's heartbeat body is the literal text "Heartbeat".
	return strings.TrimSpace(string(body)) == "Heartbeat"
}

// handleEvent decides whether the part represents a doorbell ring and
// fires a bell event if so. The event body's leading "Code=<value>;..."
// is enough — we don't need to parse the trailing JSON data block.
func (e *EventAttachClient) handleEvent(s StreamConfig, body []byte) {
	text := strings.TrimSpace(string(body))
	code := eventCode(text)
	switch code {
	case "Invite", "CallNoAnswered":
		// One bell press emits both events back-to-back; BellBus's
		// debounce coalesces them into a single notification.
		log.Printf("event-attach [%s]: bell event Code=%s", s.ID, code)
		e.bell.Fire(context.Background(), s.ID)
	default:
		// Uncomment when debugging which codes a particular VTO emits.
		// log.Printf("event-attach [%s]: ignored Code=%s", s.ID, code)
	}
}

func eventCode(body string) string {
	// Body format: "Code=<value>;action=...;..."
	const prefix = "Code="
	if !strings.HasPrefix(body, prefix) {
		return ""
	}
	rest := body[len(prefix):]
	if i := strings.IndexAny(rest, ";\r\n"); i >= 0 {
		return rest[:i]
	}
	return rest
}
