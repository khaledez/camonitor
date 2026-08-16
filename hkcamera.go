// HomeKit video-doorbell accessories — phase 2.
//
// HomeKit refuses to bridge cameras, so each door station is its own
// pairable accessory on its own port, sharing the bridge's setup code.
// The Home app finds them as nearby accessories once the bridge is paired.
//
// The doorbell half is almost free: BellBus already debounces ring events
// and caches the JPEG the camera grabbed at the instant of the press, so a
// ring is one characteristic event and a snapshot request is a map lookup.
// The streaming half is hksrtp.go.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/rtp"
	"github.com/brutella/hap/service"
	"github.com/brutella/hap/tlv8"
)

const (
	// concurrentStreams is how many controllers may watch one camera at
	// once. Two covers the common case of an iPhone and the Apple TV.
	// Each costs its own RTSP session to the camera.
	concurrentStreams = 2

	// mainStreamMinWidth is the requested width at or above which we pull
	// the camera's main stream instead of its sub stream.
	mainStreamMinWidth = 1280
)

// hkCamera is one standalone HomeKit video-doorbell accessory.
type hkCamera struct {
	stream   StreamConfig
	bell     *BellBus
	snapshot snapshotFetcher

	a        *accessory.A
	doorbell *service.Doorbell
	sessions []*hkStreamSession
}

func newHKCamera(s StreamConfig, bell *BellBus, snapshot snapshotFetcher) *hkCamera {
	name := s.Name
	if name == "" {
		name = s.ID
	}

	c := &hkCamera{
		stream:   s,
		bell:     bell,
		snapshot: snapshot,
		doorbell: service.NewDoorbell(),
	}

	c.a = accessory.New(accessory.Info{
		Name:         name,
		Manufacturer: bridgeManufacturer,
		Model:        "Dahua VTO",
		SerialNumber: s.ID,
		Firmware:     accessoryFirmware,
	}, accessory.TypeVideoDoorbell)
	// Standalone accessory: it is the only thing on its server, so it
	// takes the bridge's own id rather than a slot in the bridge's range.
	c.a.Id = bridgeAID

	c.doorbell.Primary = true
	nameCharacteristic(c.doorbell.S, name)
	c.a.AddS(c.doorbell.S)

	for i := range concurrentStreams {
		sess := newHKStreamSession(s, fmt.Sprintf("%s/%d", s.ID, i))
		c.sessions = append(c.sessions, sess)
		c.a.AddS(sess.svc.S)
	}

	// HomeKit rejects a camera that advertises no audio codec, so we
	// declare Opus and carry a muted Microphone. No audio is actually
	// sent — two-way audio is a later phase.
	mic := service.NewMicrophone()
	mic.Mute.SetValue(true)
	c.a.AddS(mic.S)

	return c
}

func nameCharacteristic(s *service.S, name string) {
	n := characteristic.NewName()
	n.SetValue(name)
	s.AddC(n.C)
}

// ring fires the doorbell characteristic if the event belongs to this
// camera's stream. BellBus already debounces, so no rate limiting here.
func (c *hkCamera) ring(ev BellEvent) {
	if ev.StreamID != c.stream.ID {
		return
	}
	log.Printf("homekit camera [%s]: doorbell press", c.stream.ID)
	c.doorbell.ProgrammableSwitchEvent.SetValue(characteristic.ProgrammableSwitchEventSinglePress)
}

// handleSnapshot serves HAP's POST /resource, which is how the Home app
// fetches the still it shows on the tile and in a ring notification.
//
// The cached frame is preferred: BellBus captured it at the instant the
// bell rang, which is the moment the notification is about, and it costs
// nothing. A live fetch is the fallback for an idle tile.
func (c *hkCamera) handleSnapshot(srv *hap.Server) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if !srv.IsAuthorized(r) {
			_ = hap.JsonError(w, hap.JsonStatusInsufficientPrivileges)
			return
		}

		var req struct {
			ResourceType string `json:"resource-type"`
			Width        int    `json:"image-width"`
			Height       int    `json:"image-height"`
		}
		// A malformed body is not worth failing over; HomeKit only ever
		// asks for an image here.
		_ = json.NewDecoder(r.Body).Decode(&req)

		jpeg, ok := c.snapshotJPEG(r.Context())
		if !ok {
			http.Error(w, "no snapshot available", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jpeg)))
		_, _ = w.Write(jpeg)
	}
}

func (c *hkCamera) snapshotJPEG(ctx context.Context) ([]byte, bool) {
	if jpeg, _, ok := c.bell.LatestSnapshot(c.stream.ID); ok && len(jpeg) > 0 {
		return jpeg, true
	}

	// snapshotTimeout is bell.go's: same job, same camera, same reason to
	// not block on an unresponsive device.
	fetchCtx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	jpeg, err := c.snapshot(fetchCtx, c.stream)
	if err != nil {
		log.Printf("homekit camera [%s]: snapshot: %v", c.stream.ID, err)
		return nil, false
	}
	return jpeg, true
}

// stop tears down any streams still running for this camera.
func (c *hkCamera) stop() {
	for _, s := range c.sessions {
		s.stopStream()
	}
}

// hkStreamSession owns one CameraRTPStreamManagement service and whatever
// stream is currently running through it.
type hkStreamSession struct {
	svc    *service.CameraRTPStreamManagement
	stream StreamConfig
	label  string

	mu      sync.Mutex
	pending *pendingSetup
	cancel  context.CancelFunc
	fwd     *srtpForwarder
}

// pendingSetup is what SetupEndpoints established, held until
// SelectedRTPStreamConfiguration says to start.
type pendingSetup struct {
	// conn is the accessory's RTP socket, bound here rather than at
	// stream-start because its port is part of the answer the controller
	// is given — it addresses RTCP to it.
	conn      *net.UDPConn
	target    *net.UDPAddr
	videoKey  []byte
	videoSalt []byte
	videoSSRC uint32
	response  []byte // the TLV answered with, replayed on read
}

func (p *pendingSetup) close() {
	if p != nil && p.conn != nil {
		p.conn.Close()
	}
}

func newHKStreamSession(s StreamConfig, label string) *hkStreamSession {
	sess := &hkStreamSession{
		svc:    service.NewCameraRTPStreamManagement(),
		stream: s,
		label:  label,
	}

	setTLV(sess.svc.SupportedVideoStreamConfiguration.Bytes, rtp.DefaultVideoStreamConfiguration())
	setTLV(sess.svc.SupportedAudioStreamConfiguration.Bytes, rtp.DefaultAudioStreamConfiguration())
	setTLV(sess.svc.SupportedRTPConfiguration.Bytes, rtp.NewConfiguration(rtp.CryptoSuite_AES_CM_128_HMAC_SHA1_80))
	sess.setStreamingStatus(rtp.StreamingStatusAvailable)

	// SetupEndpoints is a write-response characteristic: the controller
	// learns our address, SSRC and keys from the reply to its own write.
	// hap does not mark it as one and its Bytes helper discards handler
	// return values, so both are wired by hand.
	//
	// This matters more than it looks. hap assigns the controller's value
	// to the characteristic *after* the handler returns, so answering by
	// writing into the characteristic — the obvious approach, and the one
	// that shipped in v0.9.0 — has the answer immediately overwritten by
	// the request. iOS then has no endpoint to stream to and gives up
	// without ever sending a start command.
	sess.svc.SetupEndpoints.Permissions = []string{
		characteristic.PermissionRead,
		characteristic.PermissionWrite,
		characteristic.PermissionWriteResponse,
	}
	sess.svc.SetupEndpoints.SetValueRequestFunc = func(v any, _ *http.Request) (any, int) {
		encoded, ok := v.(string)
		if !ok {
			return nil, hapStatusInvalidValue
		}
		b, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, hapStatusInvalidValue
		}
		resp, err := sess.onSetupEndpoints(b)
		if err != nil {
			log.Printf("homekit camera [%s]: setup endpoints: %v", sess.label, err)
			return nil, hapStatusInvalidValue
		}
		return base64.StdEncoding.EncodeToString(resp), 0
	}
	// And a plain read must return the answer too, not the request hap
	// left in the stored value.
	sess.svc.SetupEndpoints.ValueRequestFunc = func(*http.Request) (any, int) {
		s := sess.currentSetup()
		if s == nil {
			return "", 0
		}
		return base64.StdEncoding.EncodeToString(s.response), 0
	}

	sess.svc.SelectedRTPStreamConfiguration.OnSetRemoteValue(sess.onSelectedConfiguration)
	return sess
}

// hapStatusInvalidValue is HAP-R2's "invalid value in write" status.
const hapStatusInvalidValue = -70410

func (s *hkStreamSession) currentSetup() *pendingSetup {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

func setTLV(c *characteristic.Bytes, v any) {
	b, err := tlv8.Marshal(v)
	if err != nil {
		log.Printf("homekit camera: marshal tlv8: %v", err)
		return
	}
	c.SetValue(b)
}

func (s *hkStreamSession) setStreamingStatus(status byte) {
	setTLV(s.svc.StreamingStatus.Bytes, rtp.StreamingStatus{Status: status})
}

// onSetupEndpoints answers the controller's "here is where to send, and
// the key to encrypt with" with our own address, port, SSRC and key. It
// returns the TLV to reply with.
func (s *hkStreamSession) onSetupEndpoints(b []byte) ([]byte, error) {
	var req rtp.SetupEndpoints
	if err := tlv8.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("parse SetupEndpoints: %w", err)
	}

	target, err := net.ResolveUDPAddr("udp",
		net.JoinHostPort(req.ControllerAddr.IPAddr, fmt.Sprintf("%d", req.ControllerAddr.VideoRtpPort)))
	if err != nil {
		return nil, fmt.Errorf("controller address: %w", err)
	}

	// The accessory's own address has to be one the controller can reach.
	// Under hostNetwork that is simply the LAN address facing it.
	localIP, err := localAddrFacing(target)
	if err != nil {
		return nil, err
	}

	// Bind now, so the port we advertise is the port we will actually
	// send from and receive RTCP on.
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind rtp socket: %w", err)
	}
	localPort := uint16(conn.LocalAddr().(*net.UDPAddr).Port)

	setup := &pendingSetup{
		conn:      conn,
		target:    target,
		videoKey:  req.Video.MasterKey,
		videoSalt: req.Video.MasterSalt,
		// Masked to stay inside int32: the TLV field is signed, and a
		// negative SSRC is not something a controller expects.
		videoSSRC: randomUint32() & 0x7FFFFFFF,
	}

	// Our half of the key exchange. We send no audio, but the response
	// must still carry a well-formed audio suite.
	accKey, accSalt := randomSRTPKeySalt()
	resp, err := tlv8.Marshal(rtp.SetupEndpointsResponse{
		SessionId: req.SessionId,
		Status:    rtp.SessionStatusSuccess,
		AccessoryAddr: rtp.Addr{
			IPVersion:    req.ControllerAddr.IPVersion,
			IPAddr:       localIP,
			VideoRtpPort: localPort,
			AudioRtpPort: localPort,
		},
		Video: rtp.CryptoSuite{
			Type:       rtp.CryptoSuite_AES_CM_128_HMAC_SHA1_80,
			MasterKey:  accKey,
			MasterSalt: accSalt,
		},
		Audio: rtp.CryptoSuite{
			Type:       rtp.CryptoSuite_AES_CM_128_HMAC_SHA1_80,
			MasterKey:  accKey,
			MasterSalt: accSalt,
		},
		SsrcVideo: int32(setup.videoSSRC),
		SsrcAudio: int32(randomUint32() & 0x7FFFFFFF),
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("marshal SetupEndpoints response: %w", err)
	}
	setup.response = resp

	s.mu.Lock()
	previous := s.pending
	s.pending = setup
	s.mu.Unlock()
	// iOS renegotiates freely; without this each attempt leaks a socket.
	previous.close()

	log.Printf("homekit camera [%s]: endpoints set up — controller %s, accessory %s:%d",
		s.label, target, localIP, localPort)
	return resp, nil
}

// onSelectedConfiguration starts, stops or reconfigures the stream.
func (s *hkStreamSession) onSelectedConfiguration(b []byte) error {
	var cfg rtp.StreamConfiguration
	if err := tlv8.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("parse SelectedRTPStreamConfiguration: %w", err)
	}

	switch cfg.Command.Type {
	case rtp.SessionControlCommandTypeStart, rtp.SessionControlCommandTypeResume:
		return s.startStream(cfg)
	case rtp.SessionControlCommandTypeEnd, rtp.SessionControlCommandTypeSuspend:
		s.stopStream()
		return nil
	case rtp.SessionControlCommandTypeReconfigure:
		// Simplest correct handling: tear down and rebuild on the new
		// parameters rather than mutating a live forwarder.
		s.stopStream()
		return s.startStream(cfg)
	}
	return nil
}

func (s *hkStreamSession) startStream(cfg rtp.StreamConfiguration) error {
	s.mu.Lock()
	setup := s.pending
	s.mu.Unlock()

	if setup == nil {
		return fmt.Errorf("stream start before endpoints were set up")
	}

	fwd, err := newSRTPForwarder(
		s.stream.ID,
		setup.conn,
		setup.target,
		setup.videoKey,
		setup.videoSalt,
		setup.videoSSRC,
		cfg.Video.RTP.PayloadType,
		int(cfg.Video.RTP.MTU),
	)
	if err != nil {
		return err
	}

	subtype := subtypeForWidth(int(cfg.Video.Attributes.Width))
	ctx, cancel := context.WithCancel(context.Background())

	s.mu.Lock()
	// Defensive: a start over a live session would otherwise orphan it.
	s.stopLocked()
	s.fwd = fwd
	s.cancel = cancel
	s.mu.Unlock()

	s.setStreamingStatus(rtp.StreamingStatusBusy)
	log.Printf("homekit camera [%s]: streaming %dx%d to %s (subtype %d)",
		s.label, cfg.Video.Attributes.Width, cfg.Video.Attributes.Height, setup.target, subtype)

	go func() {
		// Video only: the audio track is deliberately unset, so
		// runRTSPReader drops the camera's audio rather than forwarding
		// it to a stream HomeKit was never told to expect.
		runRTSPReader(ctx, s.stream, streamTargets{videoTrack: fwd}, subtype)
		fwd.Close()
	}()
	return nil
}

func (s *hkStreamSession) stopStream() {
	s.mu.Lock()
	stopped := s.stopLocked()
	s.mu.Unlock()

	if stopped {
		s.setStreamingStatus(rtp.StreamingStatusAvailable)
		log.Printf("homekit camera [%s]: stream ended", s.label)
	}
}

// stopLocked cancels the reader and closes the socket. Caller holds mu.
func (s *hkStreamSession) stopLocked() bool {
	if s.cancel == nil {
		return false
	}
	s.cancel()
	s.cancel = nil
	if s.fwd != nil {
		s.fwd.Close()
		s.fwd = nil
	}
	return true
}

// subtypeForWidth picks the Dahua stream variant. HomeKit typically asks
// for 1280x720 or larger on an iPhone and smaller on a watch.
func subtypeForWidth(width int) int {
	if width >= mainStreamMinWidth {
		return 0 // main / HD
	}
	return 1 // sub / SD
}

// localAddrFacing returns the local IP the kernel would use to reach the
// controller. A UDP dial does not send anything, it just resolves the
// route — which is exactly the address HomeKit needs us to advertise.
func localAddrFacing(target *net.UDPAddr) (string, error) {
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		return "", fmt.Errorf("resolve local address for %s: %w", target, err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}
