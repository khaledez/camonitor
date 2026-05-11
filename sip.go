// SIP UA. Each configured stream that has sip_ext set registers as an
// extension on the VTO's built-in SIP server. When a visitor presses the
// call button the VTO sends an INVITE to every registered extension; we
// fire a bell event (which fetches a snapshot and notifies WhatsApp /
// SSE subscribers) and reply 486 Busy Here so the VTO stops ringing us.
//
// All streams share one UDP socket — sipgo's Server is the listener and
// Client sends outbound REGISTERs through the same UA so responses come
// back on the same port the VTO is talking to. Incoming INVITEs are
// matched to a stream by the source address of the UDP packet (not by
// From-header user), because Dahua firmware emits inconsistent From URIs.
//
// Audio is intentionally NOT negotiated here. Phase 2 will auto-answer
// and bridge RTP ↔ WebRTC; for now we just want to know the bell rang.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const (
	sipRegisterExpiry  = 3600 * time.Second
	sipRegisterRefresh = 1800 * time.Second // refresh well before expiry
	sipRequestTimeout  = 10 * time.Second
)

type SIPConfig struct {
	// Bind is the local UDP address SIP listens on. Defaults to ":5060".
	Bind string `json:"bind"`
	// ContactHost is what we advertise in the Contact header so the VTO
	// knows where to send INVITEs. Auto-detected if empty (first non-
	// loopback IPv4). Override if auto-detection picks the wrong NIC
	// (multi-homed host, Docker bridge networking, etc.).
	ContactHost string `json:"contact_host"`
}

type SIPServer struct {
	cfg       SIPConfig
	streams   []StreamConfig
	streamsByHost map[string]string // VTO IP → stream ID
	bell      *BellBus

	contactHost string
	contactPort int

	ua     *sipgo.UserAgent
	server *sipgo.Server
	client *sipgo.Client
}

// NewSIPServer builds (but does not start) a SIP UA for every stream that
// has sip_ext set. Returns (nil, nil) if no stream is SIP-configured —
// that's the "SIP disabled" path and the caller should just skip Run().
func NewSIPServer(cfg SIPConfig, streams []StreamConfig, bell *BellBus) (*SIPServer, error) {
	enabled := make([]StreamConfig, 0, len(streams))
	for _, s := range streams {
		if s.SIPExt != "" {
			enabled = append(enabled, s)
		}
	}
	if len(enabled) == 0 {
		return nil, nil
	}
	if cfg.Bind == "" {
		cfg.Bind = ":5060"
	}

	contactHost := cfg.ContactHost
	if contactHost == "" {
		ip, err := detectLocalIPv4()
		if err != nil {
			return nil, fmt.Errorf("sip: detect contact host: %w", err)
		}
		contactHost = ip
	}
	contactPort, err := portFromBind(cfg.Bind)
	if err != nil {
		return nil, fmt.Errorf("sip: parse bind: %w", err)
	}

	byHost := make(map[string]string, len(enabled))
	for _, s := range enabled {
		byHost[s.Host] = s.ID
	}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("camonitor"))
	if err != nil {
		return nil, fmt.Errorf("sip: new ua: %w", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		ua.Close()
		return nil, fmt.Errorf("sip: new server: %w", err)
	}
	cli, err := sipgo.NewClient(ua, sipgo.WithClientHostname(contactHost))
	if err != nil {
		ua.Close()
		return nil, fmt.Errorf("sip: new client: %w", err)
	}

	s := &SIPServer{
		cfg:           cfg,
		streams:       enabled,
		streamsByHost: byHost,
		bell:          bell,
		contactHost:   contactHost,
		contactPort:   contactPort,
		ua:            ua,
		server:        srv,
		client:        cli,
	}
	srv.OnInvite(s.handleInvite)
	// Ack/Cancel/Bye for the "session" we never actually accept — answering
	// with 486 means the call goes ACK-486 and stops. But Dahua sometimes
	// sends a CANCEL if the visitor hangs up first; respond 200 OK to keep
	// the dialog state machine clean.
	srv.OnCancel(s.handleCancel)
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) { /* nothing to do */ })

	return s, nil
}

// Run starts the SIP listener and one registration goroutine per stream.
// Blocks until ctx is cancelled.
func (s *SIPServer) Run(ctx context.Context) error {
	log.Printf("sip: listen %s (contact %s:%d), %d stream(s) registering",
		s.cfg.Bind, s.contactHost, s.contactPort, len(s.streams))

	listenErr := make(chan error, 1)
	go func() {
		listenErr <- s.server.ListenAndServe(ctx, "udp", s.cfg.Bind)
	}()

	// Brief pause so the listener is up before any REGISTER goes out and
	// the response can come back on the same socket.
	time.Sleep(100 * time.Millisecond)

	var wg sync.WaitGroup
	for _, st := range s.streams {
		wg.Add(1)
		go func(st StreamConfig) {
			defer wg.Done()
			s.registerLoop(ctx, st)
		}(st)
	}

	select {
	case <-ctx.Done():
	case err := <-listenErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("sip: listener exited: %v", err)
		}
	}
	wg.Wait()
	s.ua.Close()
	return nil
}

func (s *SIPServer) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	srcHost := sourceHost(req)
	streamID, ok := s.streamsByHost[srcHost]
	if !ok {
		log.Printf("sip: INVITE from unknown source %s — rejecting", srcHost)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 404, "Not Found", nil))
		return
	}

	s.bell.Fire(context.Background(), streamID)

	// 486 Busy Here tells the VTO this extension can't take the call right
	// now, which is true (no audio path yet). The VTO stops ringing us and
	// the call setup state machine completes cleanly via ACK.
	if err := tx.Respond(sip.NewResponseFromRequest(req, 486, "Busy Here", nil)); err != nil {
		log.Printf("sip [%s]: respond 486 failed: %v", streamID, err)
	}
}

func (s *SIPServer) handleCancel(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
}

// registerLoop sends an initial REGISTER and refreshes it before each
// expiry. It exits when ctx is cancelled. Failures are logged and retried
// with a fixed backoff — the VTO is on the same LAN so flakiness is rare,
// and exponential backoff just delays recovery when the camera reboots.
func (s *SIPServer) registerLoop(ctx context.Context, st StreamConfig) {
	const retryWait = 30 * time.Second
	for {
		if err := s.registerOnce(ctx, st); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("sip [%s]: register failed: %v (retrying in %s)", st.ID, err, retryWait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryWait):
			}
			continue
		}
		log.Printf("sip [%s]: registered as ext %s on %s", st.ID, st.SIPExt, st.Host)
		select {
		case <-ctx.Done():
			return
		case <-time.After(sipRegisterRefresh):
		}
	}
}

// registerOnce performs a single REGISTER request, handling the digest
// challenge with the existing helpers from digest.go.
func (s *SIPServer) registerOnce(ctx context.Context, st StreamConfig) error {
	target := sip.Uri{Scheme: "sip", Host: st.Host, Port: 5060}
	from := sip.Uri{Scheme: "sip", User: st.SIPExt, Host: st.Host}
	contact := sip.Uri{Scheme: "sip", User: st.SIPExt, Host: s.contactHost, Port: s.contactPort}

	build := func(authHeader string) *sip.Request {
		req := sip.NewRequest(sip.REGISTER, target)
		// NewRequest auto-populates From/To from the request URI without a
		// tag and with the wrong user. We replace them explicitly so the
		// VTO accepts the registration. The From-tag is mandatory per
		// RFC 3261 §8.1.1.3.
		fromParams := sip.NewParams()
		fromParams.Add("tag", randomTag())
		fromHeader := &sip.FromHeader{Address: from, Params: fromParams}
		toHeader := &sip.ToHeader{Address: from, Params: sip.NewParams()}
		req.ReplaceHeader(fromHeader)
		req.ReplaceHeader(toHeader)
		req.AppendHeader(&sip.ContactHeader{Address: contact, Params: sip.NewParams()})
		req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(int(sipRegisterExpiry.Seconds()))))
		if authHeader != "" {
			req.AppendHeader(sip.NewHeader("Authorization", authHeader))
		}
		return req
	}

	reqCtx, cancel := context.WithTimeout(ctx, sipRequestTimeout)
	defer cancel()

	resp, err := s.do(reqCtx, build(""))
	if err != nil {
		return err
	}
	if resp.StatusCode == 200 {
		return nil
	}
	if resp.StatusCode != 401 && resp.StatusCode != 407 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	chalHeader := resp.GetHeader("WWW-Authenticate")
	if chalHeader == nil {
		chalHeader = resp.GetHeader("Proxy-Authenticate")
	}
	if chalHeader == nil {
		return fmt.Errorf("status %d without auth challenge", resp.StatusCode)
	}
	chal, err := parseDigestChallenge(chalHeader.Value())
	if err != nil {
		return fmt.Errorf("parse digest challenge: %w", err)
	}

	uri := fmt.Sprintf("sip:%s:%d", st.Host, 5060)
	auth := buildDigestResponse("REGISTER", uri, st.SIPExt, st.SIPPass, chal)

	resp2, err := s.do(reqCtx, build(auth))
	if err != nil {
		return err
	}
	if resp2.StatusCode != 200 {
		return fmt.Errorf("auth failed: status %d", resp2.StatusCode)
	}
	return nil
}

// do sends a request and returns the final response, terminating the
// client transaction afterwards. Any provisional 1xx responses are
// discarded — REGISTER never sends them in practice, but we tolerate.
func (s *SIPServer) do(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	tx, err := s.client.TransactionRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("transaction: %w", err)
	}
	defer tx.Terminate()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case resp, ok := <-tx.Responses():
			if !ok {
				return nil, errors.New("response channel closed")
			}
			if resp.StatusCode < 200 {
				continue
			}
			return resp, nil
		}
	}
}

// sourceHost returns the IP (without port) the request arrived from.
func sourceHost(req *sip.Request) string {
	src := req.Source()
	host, _, err := net.SplitHostPort(src)
	if err != nil {
		return src
	}
	return host
}

func portFromBind(bind string) (int, error) {
	_, p, err := net.SplitHostPort(bind)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// detectLocalIPv4 returns the first non-loopback IPv4 address bound to a
// running interface. Good enough for typical single-NIC LAN deployments;
// users on multi-homed hosts should set sip.contact_host explicitly.
func detectLocalIPv4() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			return ip4.String(), nil
		}
	}
	return "", errors.New("no non-loopback IPv4 interface found")
}

func randomTag() string {
	// Reuse digest.go's cnonce generator — it gives us hex-encoded random
	// bytes, which is a perfectly valid SIP tag.
	return randomCnonce()
}
