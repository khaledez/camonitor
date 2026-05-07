package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/pion/webrtc/v4"
)

// Hub owns one TrackLocalStaticRTP per configured stream and manages both:
//
//   - the RTSP reader feeding each track (one per stream, swappable between
//     "sd" and "hd" without renegotiating WebRTC), and
//   - the short-lived signaling state for browser sessions.
//
// One track per stream is shared across all viewers — pion fans the RTP out
// to every PeerConnection that has subscribed, so the upstream RTSP
// connection stays at 1× regardless of viewer count.
type Hub struct {
	parentCtx   context.Context
	streams     []StreamConfig
	streamsByID map[string]StreamConfig
	tracks      map[string]*webrtc.TrackLocalStaticRTP

	runners map[string]*streamRunner

	mu       sync.Mutex
	sessions map[string]*webrtc.PeerConnection
}

// streamRunner tracks the RTSP reader currently feeding a stream's track.
// Switching quality cancels the active reader, waits for it to exit so
// only one writer ever touches the track, then starts a new one.
type streamRunner struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	quality string // "sd" or "hd"
}

// Quality values exchanged with the browser. Mapped to Dahua RTSP subtype
// numbers (sub stream = 1, main stream = 0) when building the URL.
const (
	qualitySD = "sd"
	qualityHD = "hd"
)

func subtypeFor(quality string) int {
	if quality == qualityHD {
		return 0
	}
	return 1
}

// NewHub creates the hub and immediately starts an SD-quality RTSP reader
// for every configured stream. The readers stop when ctx is cancelled.
func NewHub(ctx context.Context, streams []StreamConfig) (*Hub, error) {
	h := &Hub{
		parentCtx:   ctx,
		streams:     streams,
		streamsByID: make(map[string]StreamConfig, len(streams)),
		tracks:      make(map[string]*webrtc.TrackLocalStaticRTP, len(streams)),
		runners:     make(map[string]*streamRunner, len(streams)),
		sessions:    make(map[string]*webrtc.PeerConnection),
	}
	for _, s := range streams {
		t, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
			"video-"+s.ID,
			"vto-"+s.ID,
		)
		if err != nil {
			return nil, err
		}
		h.tracks[s.ID] = t
		h.streamsByID[s.ID] = s
		h.runners[s.ID] = &streamRunner{}

		h.startReader(s.ID, qualityHD)
	}
	return h, nil
}

func (h *Hub) Track(streamID string) *webrtc.TrackLocalStaticRTP {
	return h.tracks[streamID]
}

// startReader cancels any active reader for streamID, waits for it to exit
// (so two readers don't briefly write to the same track), and launches a
// new one at the requested quality. Caller must NOT hold runner.mu.
func (h *Hub) startReader(streamID, quality string) {
	runner := h.runners[streamID]
	runner.mu.Lock()
	defer runner.mu.Unlock()

	if runner.cancel != nil {
		runner.cancel()
		<-runner.done
	}

	ctx, cancel := context.WithCancel(h.parentCtx)
	done := make(chan struct{})
	runner.cancel = cancel
	runner.done = done
	runner.quality = quality

	s := h.streamsByID[streamID]
	subtype := subtypeFor(quality)
	go func() {
		defer close(done)
		runRTSPReader(ctx, s, h, subtype)
	}()
}

// SetQuality switches a stream between "sd" and "hd". It is a no-op if the
// stream is already at the requested quality.
func (h *Hub) SetQuality(streamID, quality string) error {
	if quality != qualitySD && quality != qualityHD {
		return fmt.Errorf("invalid quality %q (use 'sd' or 'hd')", quality)
	}
	if _, ok := h.streamsByID[streamID]; !ok {
		return fmt.Errorf("%w: %q", errUnknownStream, streamID)
	}
	if h.Quality(streamID) == quality {
		return nil
	}
	h.startReader(streamID, quality)
	return nil
}

func (h *Hub) Quality(streamID string) string {
	runner := h.runners[streamID]
	if runner == nil {
		return ""
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.quality
}

// publicStream is the browser-facing view of a stream — id, name, and
// current quality. We deliberately omit host and credentials: the browser
// never needs them, and shipping them over the wire would expose camera
// passwords to anyone who can reach this server.
type publicStream struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Quality string `json:"quality"`
}

func (h *Hub) publicStreams() []publicStream {
	out := make([]publicStream, len(h.streams))
	for i, s := range h.streams {
		out[i] = publicStream{
			ID:      s.ID,
			Name:    s.Name,
			Quality: h.Quality(s.ID),
		}
	}
	return out
}

type offerResponse struct {
	SessionID string         `json:"sessionId"`
	SDP       string         `json:"sdp"`
	Type      string         `json:"type"`
	Streams   []publicStream `json:"streams"`
}

// HandleOffer creates a fresh PeerConnection populated with one sender per
// configured stream, generates an SDP offer with full ICE candidates, and
// returns it to the browser along with a session ID.
func (h *Hub) HandleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for _, s := range h.streams {
		track := h.tracks[s.ID]
		sender, err := pc.AddTrack(track)
		if err != nil {
			pc.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Drain RTCP from each sender so pion's congestion bookkeeping
		// keeps moving. We ignore the contents — there's no transcoding
		// to react to.
		go drainRTCP(sender)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-gatherComplete

	sid := newSessionID()
	h.mu.Lock()
	h.sessions[sid] = pc
	h.mu.Unlock()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("session %s state: %s", sid, state)
		isTerminal := state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected
		if !isTerminal {
			return
		}
		h.mu.Lock()
		if cur, ok := h.sessions[sid]; ok && cur == pc {
			delete(h.sessions, sid)
		}
		h.mu.Unlock()
		_ = pc.Close()
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(offerResponse{
		SessionID: sid,
		SDP:       pc.LocalDescription().SDP,
		Type:      "offer",
		Streams:   h.publicStreams(),
	})
}

type answerRequest struct {
	SessionID string `json:"sessionId"`
	SDP       string `json:"sdp"`
}

// HandleAnswer applies the browser's SDP answer to an open session.
func (h *Hub) HandleAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req answerRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	pc, ok := h.sessions[req.SessionID]
	h.mu.Unlock()
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  req.SDP,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleSetQuality serves POST /stream/quality?id=<id>&quality=hd|sd. The
// switch is asynchronous: this endpoint returns once the new RTSP reader
// has been launched. If the new source can't be reached the reader will
// keep retrying with backoff, and the browser cell will simply stop
// receiving frames until the user picks a different quality.
func (h *Hub) HandleSetQuality(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	quality := r.URL.Query().Get("quality")
	if id == "" || quality == "" {
		http.Error(w, "missing id or quality", http.StatusBadRequest)
		return
	}
	if err := h.SetQuality(id, quality); err != nil {
		log.Printf("set quality [%s=%s]: %v", id, quality, err)
		if errors.Is(err, errUnknownStream) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("set quality [%s]: %s", id, quality)
	w.WriteHeader(http.StatusNoContent)
}

func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

func newSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand fails only if the kernel RNG is unavailable; treat
		// as fatal because session collisions would be a real problem.
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
