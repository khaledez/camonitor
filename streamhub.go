package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/pion/rtp"
)

const (
	h265ClockRate    = 90000
	muxerWaitTimeout = 10 * time.Second // how long a request waits for the first keyframe
	reconnectBackoff = 2 * time.Second
)

// accessUnitFunc receives one H265 access unit. pts is in 90kHz units,
// derived from RTP timestamps (the cameras' own clocks are unreliable).
type accessUnitFunc func(codec *codecs.H265, pts int64, au [][]byte) error

// streamSource is one upstream connection attempt. Run blocks until ctx is
// cancelled (returns nil) or the upstream fails (returns the error).
type streamSource interface {
	Run(ctx context.Context, onAU accessUnitFunc) error
}

// StreamHub pulls each camera's RTSP H265 stream once and serves it to any
// number of viewers as LL-HLS. The pull runs only while someone is watching.
type StreamHub struct {
	urls      map[string]string
	idle      time.Duration
	backoff   time.Duration
	newSource func(url string) streamSource

	mu     sync.Mutex
	cams   map[string]*streamCam
	closed bool
	wg     sync.WaitGroup
}

type streamCam struct {
	url       string
	viewers   int
	idleTimer *time.Timer
	run       *streamRun // nil while no pull is active
}

// streamRun is one start-to-stop lifetime of a camera's pull.
type streamRun struct {
	cancel context.CancelFunc

	mu    sync.Mutex
	muxer *gohlslib.Muxer
	track *gohlslib.Track
	ready chan struct{} // closed once the muxer exists; replaced on teardown
	// first frame anchors pts and wall-clock time
	startPTS  int64
	startWall time.Time
}

func NewStreamHub(sources map[string]string, idle time.Duration) *StreamHub {
	h := &StreamHub{
		urls:      sources,
		idle:      idle,
		backoff:   reconnectBackoff,
		newSource: func(url string) streamSource { return &rtspH265Source{url: url} },
		cams:      map[string]*streamCam{},
	}
	for name, url := range sources {
		h.cams[name] = &streamCam{url: url}
	}
	return h
}

// Handler returns the HLS handler for a camera and makes sure the pull is
// running. Every request served through it counts as a viewer, so the pull
// stops `idle` after the last request.
func (h *StreamHub) Handler(camera string) (http.Handler, bool) {
	if h == nil {
		return nil, false
	}
	release, ok := h.acquire(camera)
	if !ok {
		return nil, false
	}
	release() // arms the idle timer; real viewers are the requests below
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := h.acquire(camera)
		if !ok {
			http.Error(w, "stream unavailable", http.StatusServiceUnavailable)
			return
		}
		defer release()
		h.serve(camera, w, r)
	}), true
}

// acquire registers a viewer, starting the pull if needed.
func (h *StreamHub) acquire(camera string) (release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cam := h.cams[camera]
	if cam == nil || h.closed {
		return nil, false
	}
	cam.viewers++
	if cam.idleTimer != nil {
		cam.idleTimer.Stop()
		cam.idleTimer = nil
	}
	if cam.run == nil {
		h.startLocked(cam)
	}
	var once sync.Once
	return func() { once.Do(func() { h.release(cam) }) }, true
}

func (h *StreamHub) release(cam *streamCam) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cam.viewers--
	if cam.viewers > 0 || h.closed {
		return
	}
	cam.idleTimer = time.AfterFunc(h.idle, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cam.viewers == 0 && cam.run != nil {
			cam.run.cancel()
			cam.run = nil
		}
	})
}

func (h *StreamHub) startLocked(cam *streamCam) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &streamRun{cancel: cancel, ready: make(chan struct{})}
	cam.run = r
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.pull(ctx, r, cam.url)
	}()
}

// redactURL strips the credentialed URL (and bare password) from msg so NVR
// creds never reach the log.
func redactURL(msg, rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.User == nil {
		return msg
	}
	msg = strings.ReplaceAll(msg, rawURL, u.Host)
	if pw, ok := u.User.Password(); ok && pw != "" {
		msg = strings.ReplaceAll(msg, pw, "***")
	}
	return msg
}

// pull keeps one source connected until ctx is cancelled.
func (h *StreamHub) pull(ctx context.Context, r *streamRun, url string) {
	defer r.closeMuxer()
	for ctx.Err() == nil {
		err := h.newSource(url).Run(ctx, r.write)
		r.closeMuxer()
		if err != nil && ctx.Err() == nil {
			log.Printf("streamhub: upstream error, retrying: %s", redactURL(err.Error(), url))
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.backoff):
		}
	}
}

func (h *StreamHub) serve(camera string, w http.ResponseWriter, req *http.Request) {
	h.mu.Lock()
	r := h.cams[camera].run
	h.mu.Unlock()
	if r == nil {
		http.Error(w, "stream unavailable", http.StatusServiceUnavailable)
		return
	}
	// the muxer only exists once the first keyframe arrived
	timeout := time.NewTimer(muxerWaitTimeout)
	defer timeout.Stop()
	for {
		r.mu.Lock()
		m, ready := r.muxer, r.ready
		r.mu.Unlock()
		if m != nil {
			m.Handle(w, req)
			return
		}
		select {
		case <-ready:
		case <-timeout.C:
			http.Error(w, "stream not ready", http.StatusServiceUnavailable)
			return
		case <-req.Context().Done():
			return
		}
	}
}

// Close stops every pull and waits for the goroutines to exit.
func (h *StreamHub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.closed = true
	for _, cam := range h.cams {
		if cam.idleTimer != nil {
			cam.idleTimer.Stop()
		}
		if cam.run != nil {
			cam.run.cancel()
			cam.run = nil
		}
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// write feeds one access unit to the muxer, creating it on the first one.
func (r *streamRun) write(codec *codecs.H265, pts int64, au [][]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.muxer == nil {
		track := &gohlslib.Track{Codec: codec, ClockRate: h265ClockRate}
		m := &gohlslib.Muxer{Variant: gohlslib.MuxerVariantLowLatency, Tracks: []*gohlslib.Track{track}}
		if err := m.Start(); err != nil {
			return fmt.Errorf("start hls muxer: %w", err)
		}
		r.muxer, r.track = m, track
		r.startPTS, r.startWall = pts, time.Now()
		close(r.ready)
	}
	rel := pts - r.startPTS
	// Split seconds from the fraction: rel*time.Second overflows int64 after ~28h.
	secs, frac := rel/h265ClockRate, rel%h265ClockRate
	ntp := r.startWall.Add(time.Duration(secs)*time.Second + time.Duration(frac)*time.Second/h265ClockRate)
	return r.muxer.WriteH265(r.track, ntp, rel, au)
}

// closeMuxer drops the muxer so the next connection starts a fresh playlist.
func (r *streamRun) closeMuxer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.muxer == nil {
		return
	}
	r.muxer.Close()
	r.muxer, r.track = nil, nil
	r.ready = make(chan struct{})
}

// rtspH265Source is the real upstream: RTSP DESCRIBE/SETUP/PLAY, RTP H265
// depacketizing. PTS comes from the RTP timestamps via gortsplib.
type rtspH265Source struct{ url string }

func (s *rtspH265Source) Run(ctx context.Context, onAU accessUnitFunc) error {
	u, err := base.ParseURL(s.url)
	if err != nil {
		return err
	}
	c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		return err
	}
	defer c.Close()

	desc, _, err := c.Describe(u)
	if err != nil {
		return err
	}
	var forma *format.H265
	medi := desc.FindFormat(&forma)
	if medi == nil {
		return errors.New("no H265 track in RTSP stream")
	}
	dec, err := forma.CreateDecoder()
	if err != nil {
		return err
	}
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return err
	}

	// Parameter sets come from the SDP, or in-band if the SDP lacks them.
	codec := &codecs.H265{VPS: forma.VPS, SPS: forma.SPS, PPS: forma.PPS}
	gotKeyframe := false
	cbErr := make(chan error, 1)

	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		pts, ok := c.PacketPTS(medi, pkt)
		if !ok {
			return
		}
		au, err := dec.Decode(pkt)
		if err != nil {
			return // incomplete frame or mid-frame join; wait for the next
		}
		// gohlslib reads the codec from its own goroutines, so once the first
		// keyframe has gone out the shared pointer must never be written again.
		for _, nalu := range au {
			if gotKeyframe || len(nalu) < 2 {
				continue
			}
			switch h265.NALUType(nalu[0] >> 1 & 0x3f) {
			case h265.NALUType_VPS_NUT:
				codec.VPS = nalu
			case h265.NALUType_SPS_NUT:
				codec.SPS = nalu
			case h265.NALUType_PPS_NUT:
				codec.PPS = nalu
			}
		}
		haveParams := codec.VPS != nil && codec.SPS != nil && codec.PPS != nil
		if !gotKeyframe && !(haveParams && h265.IsRandomAccess(au)) {
			return
		}
		gotKeyframe = true
		if err := onAU(codec, pts, au); err != nil {
			select {
			case cbErr <- err:
			default:
			}
		}
	})

	if _, err := c.Play(nil); err != nil {
		return err
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- c.Wait() }()
	select {
	case <-ctx.Done():
		c.Close()
		<-waitErr
		return nil
	case err := <-cbErr:
		return err
	case err := <-waitErr:
		return err
	}
}
