// Bell event coordinator. A single BellBus instance is shared by sip.go
// (which Fires on incoming SIP INVITE), the SSE endpoint that pushes events
// to the browser, the /snapshot endpoint that serves JPEGs, and the /rings
// endpoint that exposes the in-memory ring history.
//
// On each Fire we:
//
//  1. Apply a per-stream debounce so a visitor mashing the call button
//     doesn't generate ten WhatsApp messages.
//  2. Fetch a fresh snapshot from the camera (HTTP digest, see snapshot.go),
//     append the event (with JPEG) to the bounded history ring buffer, and
//     update the "latest per stream" pointer used by the live overlay.
//  3. Hand the JPEG to the optional WhatsApp sender so it lands on the
//     configured recipients' phones.
//  4. Broadcast a small JSON event to every SSE subscriber so any open
//     browser tile can flash and pull the snapshot from /snapshot.
//
// Steps 2-4 run on a background goroutine: Fire returns immediately so the
// SIP transaction can reply 486 Busy Here without waiting on HTTP/WhatsApp.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	bellDebounce       = 15 * time.Second
	snapshotTimeout    = 5 * time.Second
	whatsappTimeout    = 30 * time.Second
	sseKeepalivePeriod = 25 * time.Second
	// historyCap caps the number of past rings we keep in memory. Each
	// entry holds its own JPEG (~100-200 KB typical), so 50 events fits
	// in <10 MB — fine for a homelab service, bounded so a runaway VTO
	// can't grow our heap unbounded.
	historyCap = 50
)

// BellEvent is what we push to SSE subscribers and surface via /rings.
// Snapshot bytes are NOT included in the JSON to keep payloads small; the
// browser pulls JPEGs separately via /snapshot?event=<id>.
type BellEvent struct {
	EventID  string    `json:"eventId"`
	StreamID string    `json:"id"`
	Name     string    `json:"name"`
	Time     time.Time `json:"time"`
	// HasSnapshot is true when a JPEG was successfully captured for this
	// ring. The browser uses it to decide whether to render an <img> in
	// the history list.
	HasSnapshot bool `json:"hasSnapshot"`
}

// snapshotFetcher fetches a JPEG from a camera. Returned by snapshot.go.
type snapshotFetcher func(ctx context.Context, s StreamConfig) ([]byte, error)

// whatsappSender sends a single image+caption to the configured recipients.
// nil is allowed and means "no WhatsApp configured".
type whatsappSender func(ctx context.Context, jpeg []byte, caption string) error

type BellBus struct {
	streams   map[string]StreamConfig
	fetchSnap snapshotFetcher
	sendWA    whatsappSender // nil if WhatsApp is not configured
	// tz is the IANA location used when rendering wall-clock times for
	// outbound notifications (e.g. the WhatsApp caption). Defaults to
	// time.UTC when the config omits the timezone field.
	tz *time.Location
	// store persists ring history so a pod restart / deploy doesn't lose
	// recent events + snapshots. nil means in-memory only.
	store *BellStore

	mu        sync.Mutex
	lastFire  map[string]time.Time
	history   []historyEntry // ring buffer, append-only with head trimmed
	latest    map[string]string // streamID → most recent eventID for that stream
	subs      map[chan BellEvent]struct{}

	// nextSeq monotonic counter used to build event IDs. Atomic so we can
	// read it without locking when generating an ID before taking the mu.
	nextSeq atomic.Uint64
}

type historyEntry struct {
	event BellEvent
	jpeg  []byte // nil if snapshot failed
}

func NewBellBus(streams []StreamConfig, fetch snapshotFetcher, send whatsappSender, tz *time.Location, store *BellStore) *BellBus {
	if tz == nil {
		tz = time.UTC
	}
	byID := make(map[string]StreamConfig, len(streams))
	for _, s := range streams {
		byID[s.ID] = s
	}
	b := &BellBus{
		streams:   byID,
		fetchSnap: fetch,
		sendWA:    send,
		tz:        tz,
		store:     store,
		lastFire:  make(map[string]time.Time),
		history:   make([]historyEntry, 0, historyCap),
		latest:    make(map[string]string),
		subs:      make(map[chan BellEvent]struct{}),
	}
	// Replay the persisted history into the in-memory ring buffer so the
	// /rings endpoint and the side-panel "history" list survive deploys.
	if entries, err := store.LoadRecent(historyCap); err != nil {
		log.Printf("bell: load history failed: %v (continuing with empty history)", err)
	} else {
		for _, e := range entries {
			b.history = append(b.history, historyEntry{event: e.Event, jpeg: e.JPEG})
			b.latest[e.Event.StreamID] = e.Event.EventID
		}
		if len(entries) > 0 {
			log.Printf("bell: replayed %d history entries from store", len(entries))
		}
	}
	return b
}

// Fire is called by the SIP handler when an INVITE arrives. It is
// non-blocking — the snapshot fetch and notification fan-out happen on a
// goroutine so the SIP transaction can be answered immediately.
func (b *BellBus) Fire(ctx context.Context, streamID string) {
	s, ok := b.streams[streamID]
	if !ok {
		log.Printf("bell: fire for unknown stream %q", streamID)
		return
	}

	now := time.Now()
	b.mu.Lock()
	last := b.lastFire[streamID]
	suppressed := now.Sub(last) < bellDebounce
	if !suppressed {
		b.lastFire[streamID] = now
	}
	b.mu.Unlock()

	if suppressed {
		log.Printf("bell [%s]: suppressed (within %s of previous ring)", streamID, bellDebounce)
		return
	}

	log.Printf("bell [%s]: ring", streamID)
	// Detach from the SIP transaction's context so a quick CANCEL/BYE
	// doesn't kill our snapshot fetch + WhatsApp send midway.
	go b.handle(s, now)
}

func (b *BellBus) handle(s StreamConfig, t time.Time) {
	displayName := s.Name
	if displayName == "" {
		displayName = s.ID
	}

	snapCtx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	jpeg, err := b.fetchSnap(snapCtx, s)
	cancel()
	if err != nil {
		log.Printf("bell [%s]: snapshot failed: %v", s.ID, err)
	} else {
		log.Printf("bell [%s]: snapshot %d bytes", s.ID, len(jpeg))
	}

	ev := BellEvent{
		EventID:     b.makeEventID(t),
		StreamID:    s.ID,
		Name:        displayName,
		Time:        t,
		HasSnapshot: len(jpeg) > 0,
	}
	b.appendHistory(historyEntry{event: ev, jpeg: jpeg})
	if err := b.store.Save(ev, jpeg); err != nil {
		log.Printf("bell [%s]: persist failed: %v", s.ID, err)
	} else if err := b.store.Prune(historyCap); err != nil {
		log.Printf("bell [%s]: prune failed: %v", s.ID, err)
	}
	b.broadcast(ev)

	if b.sendWA != nil && len(jpeg) > 0 {
		caption := fmt.Sprintf("🔔 %s — %s", displayName, t.In(b.tz).Format("15:04:05"))
		waCtx, cancel := context.WithTimeout(context.Background(), whatsappTimeout)
		defer cancel()
		if err := b.sendWA(waCtx, jpeg, caption); err != nil {
			log.Printf("bell [%s]: whatsapp send failed: %v", s.ID, err)
		}
	}
}

// makeEventID builds a sortable, URL-safe event ID from the timestamp and
// a monotonic sequence. Format: "<unixNano>-<seq>". Sequence keeps IDs
// unique even when two rings hit the same nanosecond (improbable on a
// real clock, but cheap insurance).
func (b *BellBus) makeEventID(t time.Time) string {
	return fmt.Sprintf("%d-%d", t.UnixNano(), b.nextSeq.Add(1))
}

func (b *BellBus) appendHistory(h historyEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.history) >= historyCap {
		// Trim from the front. We grow `history` to historyCap and then
		// slide so memory stays bounded; the slice header still points at
		// the original backing array (Go won't free it), but the backing
		// array is reused — total footprint is one historyCap-sized array.
		copy(b.history, b.history[1:])
		b.history = b.history[:historyCap-1]
	}
	b.history = append(b.history, h)
	b.latest[h.event.StreamID] = h.event.EventID
}

// LatestSnapshot returns the most recent JPEG cached for a stream. Used by
// the live ring-overlay (which only cares about the freshest frame).
func (b *BellBus) LatestSnapshot(streamID string) ([]byte, time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.latest[streamID]
	if !ok {
		return nil, time.Time{}, false
	}
	for i := len(b.history) - 1; i >= 0; i-- {
		if b.history[i].event.EventID == id {
			h := b.history[i]
			return h.jpeg, h.event.Time, len(h.jpeg) > 0
		}
	}
	return nil, time.Time{}, false
}

// SnapshotByEvent returns the JPEG for a specific historical event.
func (b *BellBus) SnapshotByEvent(eventID string) ([]byte, time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.history) - 1; i >= 0; i-- {
		if b.history[i].event.EventID == eventID {
			h := b.history[i]
			return h.jpeg, h.event.Time, len(h.jpeg) > 0
		}
	}
	return nil, time.Time{}, false
}

// History returns the in-memory ring history, newest first. The returned
// slice is a fresh copy so callers can iterate without holding b.mu.
func (b *BellBus) History() []BellEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BellEvent, len(b.history))
	for i, h := range b.history {
		out[len(b.history)-1-i] = h.event
	}
	return out
}

// Subscribe returns a buffered channel of events plus a cancel function the
// caller MUST invoke when done. We buffer modestly (8 events) so a slow
// reader doesn't block the broadcaster — overflow drops the oldest event
// for that subscriber.
func (b *BellBus) Subscribe() (<-chan BellEvent, func()) {
	ch := make(chan BellEvent, 8)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	unsub := func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, unsub
}

func (b *BellBus) broadcast(ev BellEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Slow subscriber: drop the oldest queued event and retry once.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

// HandleEvents serves the SSE stream at GET /events.
func (b *BellBus) HandleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, unsub := b.Subscribe()
	defer unsub()

	keepalive := time.NewTicker(sseKeepalivePeriod)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			// SSE comment line — keeps proxies from closing an idle
			// connection. Comments start with ':' and have no event type.
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			payload, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "event: ring\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// HandleSnapshot serves GET /snapshot?id=<streamID> (latest per stream) or
// GET /snapshot?event=<eventID> (specific historical event). The `t` query
// param is only used by the browser as a cache-buster; ignored server-side.
func (b *BellBus) HandleSnapshot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var jpeg []byte
	var at time.Time
	var ok bool
	if eventID := q.Get("event"); eventID != "" {
		jpeg, at, ok = b.SnapshotByEvent(eventID)
	} else if id := q.Get("id"); id != "" {
		jpeg, at, ok = b.LatestSnapshot(id)
	} else {
		http.Error(w, "missing id or event", http.StatusBadRequest)
		return
	}
	if !ok || len(jpeg) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Last-Modified", at.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jpeg)))
	_, _ = w.Write(jpeg)
}

// HandleHistory serves GET /rings — a JSON list of the in-memory ring
// history, newest first. Used by the web UI's "history" panel.
func (b *BellBus) HandleHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(b.History())
}
