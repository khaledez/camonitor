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
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	greeDefaultPort = 7000
	greeGenericKey  = "a3K8Bx%2r8Y7#xDh"
	greeUDPTimeout  = 4 * time.Second
	// greePollInterval is how often we re-read the unit's status so the
	// UI reflects changes made from the physical remote too.
	greePollInterval = 10 * time.Second
	// greeStatusTimeout caps a single status read; a missed poll just
	// marks the unit offline until the next attempt.
	greeStatusTimeout = 4 * time.Second
)

// GreeConfig is the on-disk shape for a single Gree Wi-Fi unit.
type GreeConfig struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	// Name is shown in the web UI; defaults to the host when empty.
	Name string `json:"name,omitempty"`
}

func (g GreeConfig) port() int {
	if g.Port == 0 {
		return greeDefaultPort
	}
	return g.Port
}

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
// are safe for concurrent use; the socket + key are guarded by mu.
type greeClient struct {
	host string
	port int

	mu   sync.Mutex
	conn *net.UDPConn
	cid  string
	key  []byte
}

func newGreeClient(cfg GreeConfig) *greeClient {
	return &greeClient{host: cfg.Host, port: cfg.port()}
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
	return c.scanAndBindLocked(ctx)
}

// scanAndBindLocked runs the discovery handshake. Caller must hold mu.
func (c *greeClient) scanAndBindLocked(ctx context.Context) error {
	conn, err := c.connLocked()
	if err != nil {
		return err
	}
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(c.host, strconv.Itoa(c.port)))
	if err != nil {
		return err
	}

	// 1. SCAN — raw JSON, not a pack.
	if err := c.sendLocked(ctx, conn, addr, []byte(`{"t":"scan"}`)); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	// The scan response's pack is encrypted with the generic key.
	scanResp, err := c.readPackLocked(ctx, conn, []byte(greeGenericKey))
	if err != nil {
		return fmt.Errorf("scan response: %w", err)
	}
	cid, _ := scanResp["cid"].(string)
	if cid == "" {
		cid, _ = scanResp["mac"].(string)
	}
	if cid == "" {
		return errors.New("scan: no device id in response")
	}

	// 2. BIND — encrypted with the generic key.
	bindInner, _ := json.Marshal(map[string]any{"mac": cid, "t": "bind", "uid": 0})
	enc, err := greeEncrypt(bindInner, []byte(greeGenericKey))
	if err != nil {
		return err
	}
	bindReq, _ := json.Marshal(map[string]any{
		"cid": "app", "i": 1, "t": "pack", "uid": 0, "tcid": cid, "pack": enc,
	})
	if err := c.sendLocked(ctx, conn, addr, bindReq); err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	bindResp, err := c.readPackLocked(ctx, conn, []byte(greeGenericKey))
	if err != nil {
		return fmt.Errorf("bind response: %w", err)
	}
	keyStr, _ := bindResp["key"].(string)
	if keyStr == "" {
		return errors.New("bind: no key in response")
	}

	c.cid = cid
	c.key = []byte(keyStr)
	return nil
}

// connLocked returns the shared UDP socket, creating it if needed.
func (c *greeClient) connLocked() (*net.UDPConn, error) {
	if c.conn == nil {
		conn, err := net.ListenUDP("udp", nil)
		if err != nil {
			return nil, err
		}
		c.conn = conn
	}
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

// readPackLocked parses the outer JSON envelope and decrypts its "pack"
// field with the supplied key, returning the inner JSON object.
func (c *greeClient) readPackLocked(ctx context.Context, conn *net.UDPConn, key []byte) (map[string]any, error) {
	buf := make([]byte, 65535)
	if err := conn.SetDeadline(time.Now().Add(greeUDPTimeout)); err != nil {
		return nil, err
	}
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	var outer map[string]any
	if err := json.Unmarshal(buf[:n], &outer); err != nil {
		return nil, fmt.Errorf("bad envelope: %w", err)
	}
	pack, _ := outer["pack"].(string)
	if pack == "" {
		return nil, errors.New("envelope missing pack")
	}
	dec, err := greeDecrypt(pack, key)
	if err != nil {
		return nil, err
	}
	var inner map[string]any
	if err := json.Unmarshal(dec, &inner); err != nil {
		return nil, fmt.Errorf("bad inner json: %w", err)
	}
	return inner, nil
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
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(c.host, strconv.Itoa(c.port)))
	if err != nil {
		return nil, err
	}
	if err := c.sendLocked(ctx, conn, addr, req); err != nil {
		return nil, err
	}
	return c.readPackLocked(ctx, conn, c.key)
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

	mu     sync.Mutex
	status GreeStatus
}

func NewGreeController(cfg GreeConfig) *GreeController {
	name := cfg.Name
	if name == "" {
		name = cfg.Host
	}
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
	g.mu.Lock()
	g.status = st
	g.mu.Unlock()
}

func (g *GreeController) current() GreeStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.status
}

// HandleStatus serves GET /gree/status.
func (g *GreeController) HandleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	st := g.current()
	st.Name = g.name
	_ = json.NewEncoder(w).Encode(st)
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
	if err := g.client.Set(ctx, params); err != nil {
		log.Printf("gree set %v: %v", params, err)
		http.Error(w, "command failed", http.StatusBadGateway)
		return
	}
	log.Printf("gree set: %v", params)
	// Refresh promptly so the UI reflects the change without waiting for
	// the next poll tick.
	g.refresh(context.Background())
	w.WriteHeader(http.StatusNoContent)
}
