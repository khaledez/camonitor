// Gree Wi-Fi air-conditioner monitoring & control over the "Gree Smart"
// UDP protocol (port 7000). This is the newer JSON-over-UDP variant used
// by most Gree Home / Gree Smart units.
//
// Protocol summary (reverse-engineered, see the gree-remote project):
//
//  1. SCAN:   broadcast/unicast {"t":"scan"} → device replies with a
//     "pack" whose inner JSON (device info) is AES-128-ECB
//     encrypted with a well-known GENERIC key.
//  2. BIND:   send {"mac":<cid>,"t":"bind","uid":0} (encrypted with the
//     generic key) → device replies "bindok" with a per-device
//     AES key.
//  3. STATUS: send {"cols":[...],"mac":<cid>,"t":"status"} encrypted with
//     the device key → device replies "dat" with cols + dat arrays.
//  4. CMD:    send {"opt":[...],"p":[...],"t":"cmd"} encrypted with the
//     device key → device replies "res" echoing the applied values.
//
// All payloads are JSON, AES-128-ECB + PKCS7 padded, then Base64 encoded
// inside a "pack" field. The outer envelope is plain JSON over UDP.
package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	greeDefaultPort = 7000
	greeGenericKey  = "a3K8Bx%2r8Y7#xDh"
	greeUDPTimeout  = 4 * time.Second
	// greeBroadcastAddr is where discovery scans go, so the unit is found
	// wherever DHCP has put it rather than at a hard-coded address.
	greeBroadcastAddr = "255.255.255.255"
	// greeScanTimeout bounds a discovery scan. Every Gree unit on the LAN
	// answers a broadcast scan, so we keep reading replies until this
	// expires or the one we are looking for turns up.
	greeScanTimeout = 2 * time.Second
	// greePollInterval is how often we re-read the unit's status so the
	// UI reflects changes made from the physical remote too.
	greePollInterval = 10 * time.Second
	// greeStatusTimeout caps a single status read; a missed poll just
	// marks the unit offline until the next attempt.
	greeStatusTimeout = 4 * time.Second
	// greeHandshakeTimeout bounds the whole discovery + bind handshake
	// (scan + up to two binds). It is deliberately longer than a single
	// status read so the broadcast fallback has room to complete on a
	// cold start, even when the caller's status-read context is short.
	greeHandshakeTimeout = 12 * time.Second
)

// GreeConfig is the on-disk shape for a single Gree Wi-Fi unit. Either
// Host or MAC must be set; MAC is preferred because it survives a DHCP
// lease change.
type GreeConfig struct {
	// Host is the unit's address. With MAC set it is only a first guess:
	// discovery falls back to a LAN broadcast when it does not answer.
	Host string `json:"host,omitempty"`
	// MAC is the unit's Wi-Fi MAC, which the Gree protocol also uses as
	// the device id ("cid"). Separators and case are ignored. Setting it
	// makes the connection independent of the unit's current IP.
	MAC  string `json:"mac,omitempty"`
	Port int    `json:"port,omitempty"`
	// Name is shown in the web UI; falls back to the host, then the MAC.
	Name string `json:"name,omitempty"`
}

// greeNormalizeMAC strips the separators people write MACs with, so a
// config value can be compared against the bare lowercase hex cid that
// arrives on the wire.
func greeNormalizeMAC(mac string) string {
	return strings.ToLower(strings.NewReplacer(":", "", "-", "", ".", "").Replace(mac))
}

func (g GreeConfig) port() int {
	if g.Port == 0 {
		return greeDefaultPort
	}
	return g.Port
}

// Operating modes as encoded by the Gree protocol's "Mod" column.
const (
	greeModeAuto = 0
	greeModeCool = 1
	greeModeDry  = 2
	greeModeFan  = 3
	greeModeHeat = 4
)

// greeFanAuto is the "Wd Spd" value meaning "let the unit pick"; 1..5 are
// the explicit speeds from lowest to highest.
const greeFanAuto = 0

// GreeStatus is the decoded, human-friendly view of a unit's state. Values
// follow the Gree protocol (see README): Power 0/1, Mode 0=auto 1=cool
// 2=dry 3=fan 4=heat, SetTemp in the unit's TemUn scale, FanSpeed 0=auto
// 1..5, RoomTemp = TemSen-40 (0 when the unit has no sensor / is off).
type GreeStatus struct {
	Configured  bool      `json:"configured"`
	Online      bool      `json:"online"`
	Name        string    `json:"name"`
	Power       int       `json:"power"`
	Mode        int       `json:"mode"`
	SetTemp     int       `json:"set_temp"`
	FanSpeed    int       `json:"fan_speed"`
	Air         int       `json:"air"`
	Health      int       `json:"health"`
	Sleep       int       `json:"sleep"`
	Light       int       `json:"light"`
	SwingV      int       `json:"swing_v"`
	Quiet       int       `json:"quiet"`
	Turbo       int       `json:"turbo"`
	TempUnit    int       `json:"temp_unit"`
	EnergySave  int       `json:"energy_save"`
	RoomTemp    int       `json:"room_temp"`
	LastUpdated time.Time `json:"last_updated"`
}

// greeClient owns the UDP socket and the per-device AES key. All methods
// are safe for concurrent use; the socket, address and key are guarded
// by mu.
type greeClient struct {
	// mac is the device id a scan reply must carry to be accepted. Empty
	// means "trust whoever answers", which is the host-only config.
	mac string
	// port is the unit's UDP port (default 7000), used to build the
	// broadcast address for units that only answer broadcast.
	port int
	// scanTargets are the addresses a discovery scan is sent to, in order:
	// the configured host first (answering directly saves waiting on the
	// broadcast), then the LAN broadcast address.
	scanTargets []string
	// broadcastOnly is set once we learn the unit only answers packets
	// sent to the broadcast address (a unicast bind timed out but a
	// broadcast bind succeeded). Some Gree firmwares behave this way.
	broadcastOnly bool

	mu   sync.Mutex
	conn *net.UDPConn
	// addr is where the unit answered from, learned during discovery.
	addr *net.UDPAddr
	cid  string
	key  []byte
}

func newGreeClient(cfg GreeConfig) *greeClient {
	port := cfg.port()
	portStr := strconv.Itoa(port)
	var targets []string
	if cfg.Host != "" {
		targets = append(targets, net.JoinHostPort(cfg.Host, portStr))
	}
	targets = append(targets, net.JoinHostPort(greeBroadcastAddr, portStr))
	return &greeClient{
		mac:         greeNormalizeMAC(cfg.MAC),
		port:        port,
		scanTargets: targets,
	}
}

// broadcastAddr is the LAN broadcast address on the unit's port. Units
// whose firmware only answers broadcast (never unicast) need requests
// sent here.
func (c *greeClient) broadcastAddr() *net.UDPAddr {
	addr, _ := net.ResolveUDPAddr("udp4", net.JoinHostPort(greeBroadcastAddr, strconv.Itoa(c.port)))
	return addr
}

// sendTarget is where requests to the unit go: its unicast address
// normally, or the LAN broadcast for units that only answer broadcast.
// Both the bind handshake and every status/command use it, so the routing
// decision lives in exactly one place.
func (c *greeClient) sendTarget() *net.UDPAddr {
	if c.broadcastOnly {
		return c.broadcastAddr()
	}
	return c.addr
}

// workCtx returns a context that keeps the caller's explicit cancellation
// but drops its deadline, so the client's own per-step timeouts (handshake
// vs status read) apply. The caller's deadline is a UI-level bound and is
// too short to cover a cold start that needs a full handshake, so a
// deadline expiry must not cancel the work — only an explicit cancel does.
func workCtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stop := context.AfterFunc(parent, func() {
		if parent.Err() == context.Canceled {
			cancel()
		}
	})
	return ctx, func() { stop(); cancel() }
}

// isTimeout reports whether err is a network timeout (e.g. a UDP read
// that hit its deadline). Only a timeout means "the unit ignored the
// request", which is what triggers the broadcast fallback.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// reset forgets the unit's address and key so the next call rediscovers
// it. A failed exchange most often means the unit picked up a new DHCP
// lease; the next poll then heals the connection on its own.
func (c *greeClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addr, c.cid, c.key = nil, "", nil
}

// ensureKey performs the SCAN + BIND handshake once so subsequent status /
// command packets can be encrypted with the device key. It re-runs if the
// socket was dropped or the key is missing.
func (c *greeClient) ensureKey(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != nil {
		return nil
	}
	// The handshake (scan + up to two binds) can outlive the caller's
	// status-read budget; give it its own, longer deadline.
	hsCtx, cancel := context.WithTimeout(ctx, greeHandshakeTimeout)
	defer cancel()
	return c.scanAndBindLocked(hsCtx)
}

// scanAndBindLocked runs the discovery handshake. Caller must hold mu.
func (c *greeClient) scanAndBindLocked(ctx context.Context) error {
	// 1. SCAN — locates the unit and fills in c.addr / c.cid.
	if err := c.discoverLocked(ctx); err != nil {
		return err
	}
	conn, err := c.connLocked()
	if err != nil {
		return err
	}

	// 2. BIND — encrypted with the generic key. Most units answer a
	// unicast bind, but some firmwares (e.g. this unit's V1.2.1) only
	// answer when the request is sent to the broadcast address. A known
	// broadcast-only unit skips straight to broadcast; otherwise try
	// unicast first and fall back to broadcast on a timeout. The mode is
	// remembered so status/commands keep going the same way, and it is
	// re-learned if the unit turns out to have been replaced.
	bindInner, _ := json.Marshal(map[string]any{"mac": c.cid, "t": "bind", "uid": 0})
	enc, err := greeEncrypt(bindInner, []byte(greeGenericKey))
	if err != nil {
		return err
	}
	bindReq, _ := json.Marshal(map[string]any{
		"cid": "app", "i": 1, "t": "pack", "uid": 0, "tcid": c.cid, "pack": enc,
	})
	keyStr, err := c.bindLocked(ctx, conn, bindReq, c.sendTarget())
	if err != nil {
		if c.broadcastOnly {
			// Sticky mode but the unit stopped answering broadcast: it
			// may have been replaced by one that answers unicast. Try
			// unicast, and forget the broadcast-only mode if it answers.
			keyStr, err = c.bindLocked(ctx, conn, bindReq, c.addr)
			if err != nil {
				return err
			}
			c.broadcastOnly = false
		} else if isTimeout(err) {
			// The unit may only accept broadcast. Try that before giving
			// up; a timeout (not any error) is what tells us it ignored
			// the unicast request.
			keyStr, err = c.bindLocked(ctx, conn, bindReq, c.broadcastAddr())
			if err != nil {
				return err
			}
			c.broadcastOnly = true
		} else {
			return err
		}
	}

	c.key = []byte(keyStr)
	return nil
}

// bindLocked sends a bind request to target and returns the device key
// from the reply. Caller must hold mu.
func (c *greeClient) bindLocked(ctx context.Context, conn *net.UDPConn, bindReq []byte, target *net.UDPAddr) (string, error) {
	if err := c.sendLocked(ctx, conn, target, bindReq); err != nil {
		return "", fmt.Errorf("bind: %w", err)
	}
	bindResp, err := c.readReplyLocked(ctx, conn, []byte(greeGenericKey))
	if err != nil {
		return "", fmt.Errorf("bind response: %w", err)
	}
	keyStr, _ := bindResp["key"].(string)
	if keyStr == "" {
		return "", errors.New("bind: no key in response")
	}
	return keyStr, nil
}

// discoverLocked finds the unit and records the address it answered from,
// so a changed DHCP lease costs one scan instead of a config edit. The
// scan is raw JSON, not a pack. Caller must hold mu.
func (c *greeClient) discoverLocked(ctx context.Context) error {
	conn, err := c.connLocked()
	if err != nil {
		return err
	}
	for _, target := range c.scanTargets {
		addr, err := net.ResolveUDPAddr("udp4", target)
		if err != nil {
			return fmt.Errorf("scan target %q: %w", target, err)
		}
		if err := c.sendLocked(ctx, conn, addr, []byte(`{"t":"scan"}`)); err != nil {
			return fmt.Errorf("scan %s: %w", target, err)
		}
	}

	// Every unit on the LAN answers a broadcast scan, so keep reading
	// until ours replies or the deadline passes.
	deadline := time.Now().Add(greeScanTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			break
		}
		// The scan response's pack is encrypted with the generic key.
		resp, src, err := c.readPackLocked(ctx, conn, []byte(greeGenericKey), deadline)
		if err != nil {
			break
		}
		cid, _ := resp["cid"].(string)
		if cid == "" {
			cid, _ = resp["mac"].(string)
		}
		isOurUnit := cid != "" && (c.mac == "" || greeNormalizeMAC(cid) == c.mac)
		if !isOurUnit {
			continue
		}
		c.addr, c.cid = src, cid
		return nil
	}
	if c.mac != "" {
		return fmt.Errorf("scan: no unit with mac %s answered", c.mac)
	}
	return errors.New("scan: no unit answered")
}

// connLocked returns the shared UDP socket, creating it if needed. It is
// an IPv4 socket with SO_BROADCAST set, which is what lets discovery
// reach a unit whose address we do not know yet.
func (c *greeClient) connLocked() (*net.UDPConn, error) {
	if c.conn != nil {
		return c.conn, nil
	}
	lc := net.ListenConfig{
		Control: func(_, _ string, rc syscall.RawConn) error {
			var setErr error
			if err := rc.Control(func(fd uintptr) {
				setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			}); err != nil {
				return err
			}
			return setErr
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":0")
	if err != nil {
		return nil, err
	}
	c.conn = pc.(*net.UDPConn)
	return c.conn, nil
}

// sendLocked writes payload to addr. The caller reads the response with
// readPackLocked (or a raw read) afterwards.
func (c *greeClient) sendLocked(ctx context.Context, conn *net.UDPConn, addr *net.UDPAddr, payload []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(greeUDPTimeout)); err != nil {
		return err
	}
	_, err := conn.WriteToUDP(payload, addr)
	return err
}

// readReplyLocked reads until the unit we discovered answers, discarding
// datagrams from anywhere else: a broadcast scan makes every other Gree
// unit on the LAN reply too, and those replies land in the same socket.
// Caller must hold mu.
func (c *greeClient) readReplyLocked(ctx context.Context, conn *net.UDPConn, key []byte) (map[string]any, error) {
	deadline := time.Now().Add(greeUDPTimeout)
	lastErr := errors.New("no reply from the unit")
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, src, err := c.readPackLocked(ctx, conn, key, deadline)
		if err != nil {
			lastErr = err
			continue
		}
		if !src.IP.Equal(c.addr.IP) || src.Port != c.addr.Port {
			lastErr = fmt.Errorf("ignored a reply from %s", src)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// readPackLocked reads one datagram before deadline, parses the outer
// JSON envelope and decrypts its "pack" field with the supplied key. It
// returns the inner JSON object and the address it came from — during
// discovery that address is the unit's current one.
func (c *greeClient) readPackLocked(ctx context.Context, conn *net.UDPConn, key []byte, deadline time.Time) (map[string]any, *net.UDPAddr, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	buf := make([]byte, 65535)
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, nil, err
	}
	n, src, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, nil, err
	}
	var outer map[string]any
	if err := json.Unmarshal(buf[:n], &outer); err != nil {
		return nil, nil, fmt.Errorf("bad envelope: %w", err)
	}
	pack, _ := outer["pack"].(string)
	if pack == "" {
		return nil, nil, errors.New("envelope missing pack")
	}
	dec, err := greeDecrypt(pack, key)
	if err != nil {
		return nil, nil, err
	}
	var inner map[string]any
	if err := json.Unmarshal(dec, &inner); err != nil {
		return nil, nil, fmt.Errorf("bad inner json: %w", err)
	}
	return inner, src, nil
}

// request sends an inner JSON object as a pack encrypted with the device
// key and returns the decrypted inner response. It transparently re-runs
// the bind handshake once if the device key is missing or stale.
func (c *greeClient) request(ctx context.Context, inner map[string]any) (map[string]any, error) {
	if err := c.ensureKey(ctx); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	enc, err := greeEncrypt(innerJSON, c.key)
	if err != nil {
		return nil, err
	}
	req, _ := json.Marshal(map[string]any{
		"cid": "app", "i": 0, "t": "pack", "uid": 0, "tcid": c.cid, "pack": enc,
	})
	conn, err := c.connLocked()
	if err != nil {
		return nil, err
	}
	if err := c.sendLocked(ctx, conn, c.sendTarget(), req); err != nil {
		return nil, err
	}
	return c.readReplyLocked(ctx, conn, c.key)
}

// statusColumns lists every parameter we care about, in the order the
// device echoes them back in "dat".
var statusColumns = []string{
	"Pow", "Mod", "SetTem", "WdSpd", "Air", "Blo", "Health", "SwhSlp",
	"Lig", "SwingLfRig", "SwUpDn", "Quiet", "Tur", "StHt", "TemUn",
	"HeatCoolType", "TemRec", "SvSt", "TemSen",
}

// Status reads the unit's current state. Returns a GreeStatus with Online
// set false if the unit is unreachable.
func (c *greeClient) Status(ctx context.Context) GreeStatus {
	st := GreeStatus{Configured: true}
	// The caller's deadline is a UI-level bound; the client bounds each
	// protocol step itself (handshake vs status read). Keep the caller's
	// cancellation but not its deadline, so a cold start that needs a
	// handshake isn't truncated mid-bind.
	ctx, cancel := workCtx(ctx)
	defer cancel()
	if err := c.ensureKey(ctx); err != nil {
		log.Printf("gree: status failed: %v", err)
		return st
	}
	c.mu.Lock()
	cid := c.cid
	c.mu.Unlock()
	inner := map[string]any{
		"cols": statusColumns,
		"mac":  cid,
		"t":    "status",
	}
	resp, err := c.request(ctx, inner)
	if err != nil {
		log.Printf("gree: status failed: %v", err)
		c.reset()
		return st
	}
	cols, _ := resp["cols"].([]any)
	dat, _ := resp["dat"].([]any)
	vals := make(map[string]int, len(cols))
	for i, col := range cols {
		name, _ := col.(string)
		if i < len(dat) {
			if f, ok := dat[i].(float64); ok {
				vals[name] = int(f)
			}
		}
	}
	st.Online = true
	st.Power = vals["Pow"]
	st.Mode = vals["Mod"]
	st.SetTemp = vals["SetTem"]
	st.FanSpeed = vals["WdSpd"]
	st.Air = vals["Air"]
	st.Health = vals["Health"]
	st.Sleep = vals["SwhSlp"]
	st.Light = vals["Lig"]
	st.SwingV = vals["SwUpDn"]
	st.Quiet = vals["Quiet"]
	st.Turbo = vals["Tur"]
	st.TempUnit = vals["TemUn"]
	st.EnergySave = vals["SvSt"]
	if ts := vals["TemSen"]; ts > 0 {
		st.RoomTemp = ts - 40
	}
	st.LastUpdated = time.Now()
	return st
}

// Set applies one or more parameters. The map keys are the friendly names
// used by the HTTP API (power, mode, temp, fan, air, health, sleep, light,
// swing, quiet, turbo, energy) and are translated to Gree protocol columns.
func (c *greeClient) Set(ctx context.Context, params map[string]int) error {
	// Same as Status: the caller's deadline is a UI-level bound; the
	// client bounds the handshake and the command read itself.
	ctx, cancel := workCtx(ctx)
	defer cancel()

	opt := make([]string, 0, len(params))
	p := make([]int, 0, len(params))
	for k, v := range params {
		col, ok := greeColumnFor(k)
		if !ok {
			return fmt.Errorf("unknown gree parameter %q", k)
		}
		opt = append(opt, col)
		p = append(p, v)
	}
	if len(opt) == 0 {
		return errors.New("no parameters to set")
	}
	inner := map[string]any{"opt": opt, "p": p, "t": "cmd"}
	resp, err := c.request(ctx, inner)
	if err != nil {
		c.reset()
		return err
	}
	if code, _ := resp["r"].(float64); code != 0 && code != 200 {
		return fmt.Errorf("gree rejected command (r=%v)", code)
	}
	return nil
}

// greeColumnFor maps friendly HTTP names to Gree protocol columns.
func greeColumnFor(name string) (string, bool) {
	switch name {
	case "power":
		return "Pow", true
	case "mode":
		return "Mod", true
	case "temp":
		return "SetTem", true
	case "fan":
		return "WdSpd", true
	case "air":
		return "Air", true
	case "health":
		return "Health", true
	case "sleep":
		return "SwhSlp", true
	case "light":
		return "Lig", true
	case "swing":
		return "SwUpDn", true
	case "quiet":
		return "Quiet", true
	case "turbo":
		return "Tur", true
	case "energy":
		return "SvSt", true
	case "temp_unit":
		return "TemUn", true
	}
	return "", false
}

// ---- AES helpers -------------------------------------------------------

// greeEncrypt AES-128-ECB encrypts plaintext with PKCS7 padding and
// returns the Base64-encoded ciphertext.
func greeEncrypt(plaintext, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	data := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// greeDecrypt Base64-decodes and AES-128-ECB decrypts a pack, trimming
// PKCS7 padding by truncating at the final '}' of the JSON payload.
func greeDecrypt(encoded string, key []byte) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, errors.New("ciphertext not block-aligned")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	if i := bytes.LastIndexByte(out, '}'); i >= 0 {
		return out[:i+1], nil
	}
	return out, nil
}

// ---- HTTP + polling ----------------------------------------------------

// GreeController owns a greeClient and a cached status, serving the
// /gree/status and /gree/set endpoints and polling the unit on a timer.
type GreeController struct {
	client *greeClient
	name   string

	mu        sync.Mutex
	status    GreeStatus
	observers []func(GreeStatus)
}

func NewGreeController(cfg GreeConfig) *GreeController {
	name := cmp.Or(cfg.Name, cfg.Host, cfg.MAC)
	return &GreeController{
		client: newGreeClient(cfg),
		name:   name,
	}
}

// Run polls the unit's status until ctx is cancelled. The first read
// happens immediately so the UI isn't blank for one poll interval.
func (g *GreeController) Run(ctx context.Context) {
	t := time.NewTicker(greePollInterval)
	defer t.Stop()
	g.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.refresh(ctx)
		}
	}
}

func (g *GreeController) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, greeStatusTimeout)
	defer cancel()
	st := g.client.Status(ctx)
	st.Name = g.name

	g.mu.Lock()
	g.status = st
	observers := slices.Clone(g.observers)
	g.mu.Unlock()

	for _, fn := range observers {
		fn(st)
	}
}

// Name is the unit's display label, falling back to its host.
func (g *GreeController) Name() string { return g.name }

// Status returns the most recent poll result.
func (g *GreeController) Status() GreeStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.status
}

// Set applies params and re-reads the unit so callers see the result
// rather than what they asked for. Params use the friendly names from
// greeColumnFor, e.g. {"power":1,"mode":1,"temp":24,"fan":3}.
func (g *GreeController) Set(ctx context.Context, params map[string]int) error {
	if err := g.client.Set(ctx, params); err != nil {
		return err
	}
	g.refresh(context.WithoutCancel(ctx))
	return nil
}

// OnUpdate registers fn to be called after every poll. HomeKit uses this
// to push characteristic events, so Home app tiles reflect changes made
// from the physical remote or the web UI.
func (g *GreeController) OnUpdate(fn func(GreeStatus)) {
	g.mu.Lock()
	g.observers = append(g.observers, fn)
	g.mu.Unlock()
}

// HandleStatus serves GET /gree/status.
func (g *GreeController) HandleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(g.Status())
}

// HandleSet serves POST /gree/set with a JSON body of friendly params,
// e.g. {"power":1,"mode":1,"temp":24,"fan":3}.
func (g *GreeController) HandleSet(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	var params map[string]int
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), greeStatusTimeout)
	defer cancel()
	if err := g.Set(ctx, params); err != nil {
		log.Printf("gree set %v: %v", params, err)
		http.Error(w, "command failed", http.StatusBadGateway)
		return
	}
	log.Printf("gree set: %v", params)
	w.WriteHeader(http.StatusNoContent)
}
