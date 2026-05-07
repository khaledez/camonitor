// Minimal RTSP-over-TCP client tailored to forwarding H.264 from a Dahua VTO
// (or similar IP camera) into a pion track.
//
// Why hand-rolled: pion does not ship RTSP, and the alternative library
// (gortsplib) brings a sizeable transitive dependency tree. RTSP-over-TCP
// with digest authentication and a single H.264 media is a small, well-
// defined slice of the protocol that fits in one file.
//
// What it supports:
//   - RTSP/1.0 OPTIONS / DESCRIBE / SETUP / PLAY
//   - HTTP Digest authentication (Dahua's default for cameras)
//   - TCP-interleaved RTP transport on the same socket as RTSP control
//   - One H.264 video media per stream (audio is intentionally out of scope)
//
// What it does NOT support: UDP transport, RTP/SAVP, multicast, redirects,
// keepalive (Dahua resets its session timer on inbound RTP, so a continuous
// stream is its own keepalive — if the link goes idle long enough to drop,
// runRTSPReader will reconnect).
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pion/rtp"
)

const (
	rtspDefaultPort = "554"
	rtspIOTimeout   = 10 * time.Second
	rtpReadIdle     = 30 * time.Second // packet-to-packet read deadline once PLAYing
	minBackoff      = 1 * time.Second
	maxBackoff      = 30 * time.Second

	// TCP-interleaved channel numbers we ask for in SETUP. Channel 0 carries
	// RTP packets we forward; channel 1 carries RTCP which we discard.
	rtpChannel  byte = 0
	rtcpChannel byte = 1
)

// trackWriter is the slice of *webrtc.TrackLocalStaticRTP we depend on. The
// narrow interface keeps this file independent of the WebRTC layer and makes
// the unit testable in principle.
type trackWriter interface {
	WriteRTP(*rtp.Packet) error
}

// runRTSPReader keeps a stream connected with bounded exponential backoff
// and forwards every received H.264 RTP packet into the supplied track.
// The subtype selects the Dahua stream variant (0 = main / HD, 1 = sub /
// SD). It exits only when ctx is cancelled.
func runRTSPReader(ctx context.Context, s StreamConfig, track trackWriter, subtype int) {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		err := streamOnce(ctx, s, subtype, track)
		if ctx.Err() != nil {
			return
		}
		log.Printf("[%s] %v — retry in %s", s.ID, err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func streamOnce(ctx context.Context, s StreamConfig, subtype int, track trackWriter) error {
	u, err := url.Parse(s.RTSPURLForSubtype(subtype))
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "rtsp" {
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	host := u.Host
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		host = net.JoinHostPort(host, rtspDefaultPort)
	}

	dialer := net.Dialer{Timeout: rtspIOTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Cancel-aware close: if the parent context is cancelled, force the
	// blocking read in readPackets to return.
	closeOnDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-closeOnDone:
		}
	}()
	defer close(closeOnDone)

	rc := newRTSPConn(conn, u)
	if err := rc.handshake(); err != nil {
		return err
	}

	log.Printf("[%s] connected to %s; forwarding H.264", s.ID, redact(u))
	return rc.readPackets(track)
}

// redact returns the URL string without any embedded user:password.
func redact(u *url.URL) string {
	clean := *u
	clean.User = nil
	return clean.String()
}

// rtspConn carries the state of a single RTSP control session over a TCP
// connection. After handshake() returns, the same socket is used to read
// interleaved RTP frames in readPackets.
type rtspConn struct {
	conn net.Conn
	br   *bufio.Reader

	// requestURI is the original RTSP URL with credentials stripped — it's
	// what we send on the request line for OPTIONS / DESCRIBE / PLAY.
	requestURI *url.URL
	user, pass string

	cseq    int
	session string // returned in SETUP, sent on subsequent requests

	// readBuf is reused across every interleaved frame on this connection.
	// 64 KiB matches the maximum length expressible in the 16-bit length
	// field; in practice frames are MTU-bounded and far smaller. Per-conn
	// scratch saves a per-packet allocation on the hot path.
	readBuf [64 * 1024]byte
}

func newRTSPConn(conn net.Conn, source *url.URL) *rtspConn {
	clean := *source
	clean.User = nil
	user := source.User.Username()
	pass, _ := source.User.Password()
	return &rtspConn{
		conn:       conn,
		br:         bufio.NewReaderSize(conn, 64*1024),
		requestURI: &clean,
		user:       user,
		pass:       pass,
	}
}

// handshake walks OPTIONS → DESCRIBE → SETUP → PLAY. After it returns the
// connection is ready to deliver interleaved RTP frames.
func (rc *rtspConn) handshake() error {
	if _, err := rc.do("OPTIONS", rc.requestURI.String(), nil); err != nil {
		return fmt.Errorf("OPTIONS: %w", err)
	}

	desc, err := rc.do("DESCRIBE", rc.requestURI.String(), map[string]string{
		"Accept": "application/sdp",
	})
	if err != nil {
		return fmt.Errorf("DESCRIBE: %w", err)
	}

	// One-shot SDP dump for v0.3.6 — used to discover what audio media the
	// camera advertises before we write the real audio path.
	log.Printf("=== SDP from %s ===\n%s=== end SDP ===", redact(rc.requestURI), string(desc.body))

	// PLAY targets the session URI, which is Content-Base when present and
	// the original request URI otherwise (RFC 2326 C.1.1).
	sessionURI := rc.requestURI.String()
	if cb := desc.header("Content-Base"); cb != "" {
		sessionURI = cb
	}

	controlURL, err := findH264ControlURL(string(desc.body), sessionURI)
	if err != nil {
		return fmt.Errorf("SDP: %w", err)
	}

	setup, err := rc.do("SETUP", controlURL, map[string]string{
		"Transport": fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", rtpChannel, rtcpChannel),
	})
	if err != nil {
		return fmt.Errorf("SETUP: %w", err)
	}
	rc.session = sessionIDOnly(setup.header("Session"))
	if rc.session == "" {
		return errors.New("SETUP: missing Session header")
	}

	if _, err := rc.do("PLAY", sessionURI, map[string]string{
		"Range": "npt=0-",
	}); err != nil {
		return fmt.Errorf("PLAY: %w", err)
	}
	return nil
}

// readPackets forwards every RTP packet received on rtpChannel into track,
// blocking until the connection breaks. RTCP and any other interleaved
// channel numbers are silently discarded.
func (rc *rtspConn) readPackets(track trackWriter) error {
	pkt := &rtp.Packet{}
	for {
		if err := rc.conn.SetReadDeadline(time.Now().Add(rtpReadIdle)); err != nil {
			return err
		}
		ch, payload, err := rc.readInterleaved()
		if err != nil {
			return err
		}
		if ch != rtpChannel {
			continue
		}
		if err := pkt.Unmarshal(payload); err != nil {
			log.Printf("rtp unmarshal: %v", err)
			continue
		}
		if err := track.WriteRTP(pkt); err != nil {
			return fmt.Errorf("write track: %w", err)
		}
	}
}

// readInterleaved reads the next inbound message from the RTSP socket and
// returns either an interleaved RTP/RTCP frame (channel + payload) or skips
// past any inline RTSP response (e.g. an unsolicited server message).
//
// Wire format of an interleaved frame (RFC 2326 §10.12):
//
//	'$' (1) | channel (1) | length (uint16 BE) | payload
//
// An inline RTSP response begins with "RTSP/1.0", which is handled by
// pushing the byte back and consuming a full response.
func (rc *rtspConn) readInterleaved() (channel byte, payload []byte, err error) {
	for {
		first, err := rc.br.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		if first == '$' {
			var hdr [3]byte
			if _, err := io.ReadFull(rc.br, hdr[:]); err != nil {
				return 0, nil, err
			}
			length := binary.BigEndian.Uint16(hdr[1:3])
			buf := rc.readBuf[:length]
			if _, err := io.ReadFull(rc.br, buf); err != nil {
				return 0, nil, err
			}
			return hdr[0], buf, nil
		}
		if err := rc.br.UnreadByte(); err != nil {
			return 0, nil, err
		}
		if _, err := readResponse(rc.br); err != nil {
			return 0, nil, err
		}
	}
}

// rtspResponse is a parsed RTSP response. Header lookups are case-insensitive
// because RTSP, like HTTP, treats header names that way.
type rtspResponse struct {
	statusCode int
	headers    map[string]string // keys are lowercased
	body       []byte
}

func (r *rtspResponse) header(name string) string {
	return r.headers[strings.ToLower(name)]
}

// do sends a request and returns the parsed response. On 401 with a digest
// challenge and credentials present, the request is retried once with the
// computed Authorization header.
func (rc *rtspConn) do(method, uri string, headers map[string]string) (*rtspResponse, error) {
	resp, err := rc.send(method, uri, headers)
	if err != nil {
		return nil, err
	}

	if resp.statusCode == 401 && rc.user != "" {
		chal, err := parseDigestChallenge(resp.header("WWW-Authenticate"))
		if err != nil {
			return resp, fmt.Errorf("parse challenge: %w", err)
		}
		authHeaders := make(map[string]string, len(headers)+1)
		maps.Copy(authHeaders, headers)
		authHeaders["Authorization"] = buildDigestResponse(method, uri, rc.user, rc.pass, chal)
		resp, err = rc.send(method, uri, authHeaders)
		if err != nil {
			return nil, err
		}
	}

	if resp.statusCode != 200 {
		return resp, fmt.Errorf("status %d: %s", resp.statusCode, strings.TrimSpace(string(resp.body)))
	}
	return resp, nil
}

func (rc *rtspConn) send(method, uri string, headers map[string]string) (*rtspResponse, error) {
	rc.cseq++

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\n", method, uri)
	fmt.Fprintf(&b, "CSeq: %d\r\n", rc.cseq)
	b.WriteString("User-Agent: camonitor/1\r\n")
	if rc.session != "" && headers["Session"] == "" {
		fmt.Fprintf(&b, "Session: %s\r\n", rc.session)
	}
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")

	if err := rc.conn.SetWriteDeadline(time.Now().Add(rtspIOTimeout)); err != nil {
		return nil, err
	}
	if _, err := rc.conn.Write([]byte(b.String())); err != nil {
		return nil, err
	}

	if err := rc.conn.SetReadDeadline(time.Now().Add(rtspIOTimeout)); err != nil {
		return nil, err
	}
	return readResponse(rc.br)
}

func readResponse(br *bufio.Reader) (*rtspResponse, error) {
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	statusLine = strings.TrimRight(statusLine, "\r\n")
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "RTSP/") {
		return nil, fmt.Errorf("bad status line: %q", statusLine)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("bad status code: %w", err)
	}

	resp := &rtspResponse{statusCode: code, headers: map[string]string{}}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		before, after, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(before))
		value := strings.TrimSpace(after)
		resp.headers[name] = value
	}

	if cl := resp.header("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil {
			return nil, fmt.Errorf("bad Content-Length: %w", err)
		}
		if n > 0 {
			body := make([]byte, n)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, err
			}
			resp.body = body
		}
	}
	return resp, nil
}

// sessionIDOnly returns the bare session ID from a Session header value,
// stripping any "timeout=NN" parameter that follows.
func sessionIDOnly(value string) string {
	id, _, _ := strings.Cut(value, ";")
	return strings.TrimSpace(id)
}

// ---- SDP --------------------------------------------------------------

// findH264ControlURL parses the SDP body, locates the first H.264 video
// media, and resolves its control URL relative to baseURI. The control URL
// is what we send to SETUP.
//
// SDP control resolution per RFC 2326 C.1.1:
//   - "*" means "use the base URL"
//   - an absolute URL is used verbatim
//   - a relative value is appended to the session base, treating the base as
//     if it ended with '/' (this differs from generic RFC 3986 resolution,
//     which would replace the last path segment).
func findH264ControlURL(sdp, baseURI string) (string, error) {
	type media struct {
		kind    string
		pts     []string
		codecs  map[string]string // payload type → codec name (uppercased)
		control string
	}
	var medias []*media
	var current *media
	inMedia := false

	for raw := range strings.SplitSeq(sdp, "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(line, "m="):
			if current != nil {
				medias = append(medias, current)
			}
			fields := strings.Fields(strings.TrimPrefix(line, "m="))
			current = &media{codecs: map[string]string{}}
			if len(fields) >= 1 {
				current.kind = fields[0]
			}
			if len(fields) >= 4 {
				current.pts = fields[3:]
			}
			inMedia = true
		case strings.HasPrefix(line, "a=control:") && inMedia:
			current.control = strings.TrimPrefix(line, "a=control:")
		case strings.HasPrefix(line, "a=rtpmap:") && inMedia:
			pt, rest, ok := strings.Cut(strings.TrimPrefix(line, "a=rtpmap:"), " ")
			if !ok {
				continue
			}
			codecName, _, _ := strings.Cut(rest, "/")
			current.codecs[pt] = strings.ToUpper(codecName)
		}
	}
	if current != nil {
		medias = append(medias, current)
	}

	for _, m := range medias {
		if m.kind != "video" {
			continue
		}
		isH264 := false
		for _, pt := range m.pts {
			if m.codecs[pt] == "H264" {
				isH264 = true
				break
			}
		}
		if !isH264 {
			continue
		}
		if m.control == "" {
			return "", errors.New("H.264 media has no a=control attribute")
		}
		return resolveControl(baseURI, m.control), nil
	}
	return "", errors.New("no H.264 media found in SDP")
}

func resolveControl(base, control string) string {
	if control == "" || control == "*" {
		return base
	}
	if u, err := url.Parse(control); err == nil && u.IsAbs() {
		return control
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + control
}

// HTTP-Digest auth helpers live in digest.go — RTSP and the door-open HTTP
// client share the exact same scheme.
