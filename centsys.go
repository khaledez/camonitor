// Centsys SMART Wi-Fi gate control over the Centurion cloud MQTT broker.
//
// A SMART operator never listens on the LAN; it holds one outbound
// connection to Centurion's broker and is reached only through it. We do
// the same: a one-time OTP login (see centsysLogin) fetches a client
// certificate plus the gate's serial and MAC, after which open/status need
// no account token — only mutual-TLS to the broker and the per-operator
// trigger packets in centsys_packets.go.
//
// This is a port of the protocol the MyCentsys app uses. It is South-Africa
// region only (the only broker we can reach) and gate-only (never the
// garage RUN id). shortcut: single operator, ZA region; generalise if a
// second gate or the AU broker is ever needed.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

const (
	centsysGatewayZA     = "https://centsys.southafricanorth.cloudapp.azure.com:4445"
	centsysMQTTHostZA    = "20.87.192.195"
	centsysMQTTPort      = 8880
	centsysTLSServerName = "CentsysQA"
	centsysClientPrefix  = "mcr:"
	centsysUserAgent     = "MyCentsysRemoteMaui/2.1.0.35 CFNetwork/3860.600.12 Darwin/25.5.0"
	centsysOpenTimeout   = 8 * time.Second
	centsysRewake        = 5 * time.Minute

	// Public service-level bearer the app presents for SendOtp/ValidateOtp
	// before a user token exists. Not a secret — it is shipped in the app.
	centsysServiceJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJodHRwOi8vc2NoZW1hcy54bWxzb2FwLm9yZy93cy8yMDA1LzA1L2lkZW50aXR5L2NsYWltcy9z" +
		"eXN0ZW0iOiJHYXRld2F5QXBpIiwiaHR0cDovL3NjaGVtYXMueG1sc29hcC5vcmcvd3MvMjAwNS8w" +
		"NS9pZGVudGl0eS9jbGFpbXMvaGFzaCI6InJzZmEhJDZWMzJHTlpWS29zJnglIiwiZXhwIjoyNzA0" +
		"MDIxMzYxLCJpc3MiOiJHYXRlV2F5QXBpLmNvbSIsImF1ZCI6IkdhdGVXYXlBcGkuY29tIn0." +
		"ZhCrEe6WUCkI-uxlcZlyvPQ5FfAe8P3EiVMswFbquLE"
)

// Centurion's self-signed root. The broker presents a leaf (CN=CentsysQA)
// signed by it; we pin this so only the real broker is trusted.
const centsysCAPEM = `-----BEGIN CERTIFICATE-----
MIIBTDCB86ADAgECAhAAlehwTzsEGZ6Pn0lUC637MAoGCCqGSM49BAMCMBUxEzAR
BgNVBAMMCkNlbnRzeXMtQ0EwIBcNMjUwNzE2MDgwNzQzWhgPMjEyNTA3MTcwODA3
NDNaMBUxEzARBgNVBAMMCkNlbnRzeXMtQ0EwWTATBgcqhkjOPQIBBggqhkjOPQMB
BwNCAATvJX+WpkqtnVm+JA+TdzYSFnheR7Cg7VdCVaX/xPIP84jklT7/O7iP3YN1
uHzyVCnsrERzeqMvNUVrjFgqnMcsoyMwITAPBgNVHRMBAf8EBTADAQH/MA4GA1Ud
DwEB/wQEAwIBBjAKBggqhkjOPQQDAgNIADBFAiEAgsMNud0PC9ZcBb3lgmgsl/xr
IupvqrsK4919dH0x1/cCIDEKKzZbusGc4+KiiUbhiwJAed7f/PrOzFWKjvmz8y0Q
-----END CERTIFICATE-----`

// CentsysConfig is the on-disk config block.
type CentsysConfig struct {
	Phone   string `json:"phone"`             // account number, e.g. +27831234567
	Name    string `json:"name,omitempty"`    // HomeKit tile name; defaults to "Gate"
	Session string `json:"session,omitempty"` // path to the login file written by -centsys-login
}

func (c CentsysConfig) sessionPath() string {
	if c.Session != "" {
		return c.Session
	}
	return "/var/lib/camonitor/centsys.json"
}

func (c CentsysConfig) name() string {
	if c.Name != "" {
		return c.Name
	}
	return "Gate"
}

// centsysSession is everything the runtime needs, written once at login.
// No account token: the open/status handshake authenticates with the
// client certificate and the phone number carried in the trigger packet.
type centsysSession struct {
	Phone   string `json:"phone"`
	Serial  string `json:"serial"`
	MAC     string `json:"mac"`
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
}

// centsysCreds is the immutable material one login yields. The controller
// holds it behind an atomic pointer so a fresh web login can swap it in
// while Open/follow are running — no restart, no lock dance.
type centsysCreds struct {
	phone, serial string
	mac4          []byte
	cert          tls.Certificate
}

// CentsysController opens the gate and tracks its live open/closed state.
// It exists whenever a gate is configured, even before login: creds is nil
// until a session is loaded (at startup or after a web login), and the
// reload channel wakes the status listener the moment one appears.
type CentsysController struct {
	cfg    CentsysConfig
	caPool *x509.CertPool
	creds  atomic.Pointer[centsysCreds]
	reload chan struct{}

	mu       sync.Mutex
	status   gateStatus
	onUpdate func(gateStatus)
}

// NewCentsysController always returns a controller. It attempts to load a
// saved session; a missing one is not an error — the gate simply stays
// dormant (and its HomeKit tile shows "No Response") until a login lands.
func NewCentsysController(cfg CentsysConfig) *CentsysController {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(centsysCAPEM))
	c := &CentsysController{
		cfg:    cfg,
		caPool: pool,
		reload: make(chan struct{}, 1),
		status: gateUnknown,
	}
	if err := c.load(); err != nil {
		log.Printf("centsys: %v (sign in from the web UI or -centsys-login)", err)
	}
	return c
}

// load reads and parses the session file into fresh creds. Called at
// startup and after a web login.
func (c *CentsysController) load() error {
	raw, err := os.ReadFile(c.cfg.sessionPath())
	if err != nil {
		return fmt.Errorf("not signed in (%s)", c.cfg.sessionPath())
	}
	var s centsysSession
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("bad session file: %w", err)
	}
	cert, err := tls.X509KeyPair([]byte(s.CertPEM), []byte(s.KeyPEM))
	if err != nil {
		return fmt.Errorf("bad client cert: %w", err)
	}
	mac4, err := parseMAC4(s.Serial, s.MAC)
	if err != nil {
		return err
	}
	c.creds.Store(&centsysCreds{phone: s.Phone, serial: s.Serial, mac4: mac4, cert: cert})
	return nil
}

// Reload re-reads the session after a login and wakes the status listener,
// so the gate goes live without a restart.
func (c *CentsysController) Reload() error {
	if err := c.load(); err != nil {
		return err
	}
	select {
	case c.reload <- struct{}{}:
	default:
	}
	log.Printf("centsys: session loaded, gate is live")
	return nil
}

// parseMAC4 returns the 4-byte key from a MAC string ("AA:BB:.." or hex).
func parseMAC4(serial, mac string) ([]byte, error) {
	clean := strings.NewReplacer(":", "", "-", "", " ", "").Replace(mac)
	b := make([]byte, 0, len(clean)/2)
	for i := 0; i+1 < len(clean); i += 2 {
		var v int
		if _, err := fmt.Sscanf(clean[i:i+2], "%02x", &v); err != nil {
			return nil, fmt.Errorf("centsys: bad MAC %q for %s: %w", mac, serial, err)
		}
		b = append(b, byte(v))
	}
	if len(b) < 4 {
		return nil, fmt.Errorf("centsys: MAC %q too short for %s", mac, serial)
	}
	return b[:4], nil
}

func (c *CentsysController) Name() string { return c.cfg.name() }

func (c *CentsysController) OnUpdate(fn func(gateStatus)) { c.onUpdate = fn }

func (c *CentsysController) CurrentStatus() gateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *CentsysController) setStatus(s gateStatus) {
	if s == gateUnknown {
		return
	}
	c.mu.Lock()
	changed := c.status != s
	c.status = s
	fn := c.onUpdate
	c.mu.Unlock()
	if changed && fn != nil {
		fn(s)
	}
}

func (c *CentsysController) tlsConfig(cr *centsysCreds) *tls.Config {
	return &tls.Config{
		RootCAs:      c.caPool,
		ServerName:   centsysTLSServerName,
		Certificates: []tls.Certificate{cr.cert},
	}
}

func centsysClientID(phone, suffix string) string {
	return centsysClientPrefix + strings.TrimPrefix(phone, "+") + suffix
}

// mqttConnect dials the broker (mutual TLS) and brings up an MQTT v5 client
// whose inbound PUBLISHes are handed to onMsg. The returned stop() closes
// everything. A distinct clientID suffix keeps us off the phone app's
// session on the broker.
func (c *CentsysController) mqttConnect(ctx context.Context, cr *centsysCreds, suffix string, onMsg func(topic string, payload []byte)) (*paho.Client, func(), error) {
	dialer := tls.Dialer{Config: c.tlsConfig(cr)}
	conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", centsysMQTTHostZA, centsysMQTTPort))
	if err != nil {
		return nil, nil, fmt.Errorf("centsys: broker dial: %w", err)
	}

	cli := paho.NewClient(paho.ClientConfig{
		// tls.Conn isn't safe for concurrent writes; the pinger and our
		// publishes both write, so wrap it.
		Conn: packets.NewThreadSafeConn(conn),
		OnPublishReceived: []func(paho.PublishReceived) (bool, error){
			func(pr paho.PublishReceived) (bool, error) {
				onMsg(pr.Packet.Topic, pr.Packet.Payload)
				return true, nil
			},
		},
	})
	if _, err := cli.Connect(ctx, &paho.Connect{
		ClientID:   centsysClientID(cr.phone, suffix),
		KeepAlive:  30,
		CleanStart: true,
	}); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("centsys: broker connect: %w", err)
	}
	stop := func() { _ = cli.Disconnect(&paho.Disconnect{ReasonCode: 0}) }
	return cli, stop, nil
}

// pubProps builds the PUBLISH properties every message carries: a response
// topic and the ClientId user-property the broker routes replies by.
func (c *CentsysController) pubProps(responseTopic, clientID string) *paho.PublishProperties {
	p := &paho.PublishProperties{ResponseTopic: responseTopic}
	p.User.Add("ClientId", clientID)
	return p
}

// Open runs the full open handshake and returns nil only if the gate
// accepted the trigger. Both HomeKit "open" and "close" call this — a
// sliding gate has one toggle input (TRG), so there is only one command.
func (c *CentsysController) Open(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, centsysOpenTimeout)
	defer cancel()

	cr := c.creds.Load()
	if cr == nil {
		return errors.New("centsys: gate not signed in yet")
	}
	serial := cr.serial
	tReq := serial + "/connectionRequest"
	tReqResp := serial + "/connectionRequestResponse"
	tTrig := serial + "/userRemoteTrigger"
	tTrigResp := serial + "/userRemoteTriggerResponse"
	tDisc := serial + "/disconnect"
	cid := centsysClientID(cr.phone, ":cam")

	reqResp := make(chan []byte, 1)
	trigResp := make(chan []byte, 4)
	onMsg := func(topic string, payload []byte) {
		switch topic {
		case tReqResp:
			select {
			case reqResp <- payload:
			default:
			}
		case tTrigResp:
			select {
			case trigResp <- payload:
			default:
			}
		}
	}

	cli, stop, err := c.mqttConnect(ctx, cr, ":cam", onMsg)
	if err != nil {
		return err
	}
	defer stop()

	sub := func(topic string) {
		_, _ = cli.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: topic, QoS: 0}}})
	}
	sub(tReqResp)
	sub(tTrigResp)

	pub := func(topic string, payload []byte, qos byte, respTopic string) error {
		_, err := cli.Publish(ctx, &paho.Publish{
			Topic: topic, QoS: qos, Payload: payload,
			Properties: c.pubProps(respTopic, cid),
		})
		return err
	}

	// Release our broker slot on the way out so the phone app reconnects cleanly.
	defer func() { _ = pub(tDisc, nil, 0, tDisc) }()

	// Wake the operator.
	if err := pub(tReq, nil, 2, tReqResp); err != nil {
		return fmt.Errorf("centsys: connectionRequest: %w", err)
	}
	if _, err := recvCtx(ctx, reqResp); err != nil {
		return errors.New("centsys: gate offline (no connectionRequestResponse)")
	}

	// Identity -> challenge.
	if err := pub(tTrig, buildCmd01(cr.phone, cr.mac4), 0, tTrigResp); err != nil {
		return fmt.Errorf("centsys: cmd01: %w", err)
	}
	cmd02, err := recvCtx(ctx, trigResp)
	if err != nil || len(cmd02) < 4 {
		return errors.New("centsys: no challenge (cmd02)")
	}
	challenge := cmd02[len(cmd02)-4:]

	// Time sync (reply ignored).
	if err := pub(tTrig, buildCmd05(cr.mac4, time.Now()), 0, tTrigResp); err != nil {
		return fmt.Errorf("centsys: cmd05: %w", err)
	}
	_, _ = recvCtx(ctx, trigResp)

	// Activation. A stricter gate rejects config version 0 and reports the
	// one it wants; retry once with that value.
	cv := 0
	for attempt := 0; attempt < 2; attempt++ {
		packet := append(buildCmd03Prefix(cr.mac4, cv), challenge...)
		if err := pub(tTrig, packet, 0, tTrigResp); err != nil {
			return fmt.Errorf("centsys: cmd03: %w", err)
		}
		cmd04, err := recvCtx(ctx, trigResp)
		if err != nil {
			return errors.New("centsys: no activation response (cmd04)")
		}
		code, gateCV, ok := decodeCmd04(cr.mac4, cmd04)
		if !ok {
			return errors.New("centsys: short activation response")
		}
		if code != activationConfigurationMismatch || gateCV == cv {
			if code == activationOK {
				return nil
			}
			return fmt.Errorf("centsys: gate rejected trigger (code %d)", code)
		}
		cv = gateCV
	}
	return errors.New("centsys: gate kept rejecting config version")
}

// recvCtx waits for one message or ctx expiry.
func recvCtx(ctx context.Context, ch <-chan []byte) ([]byte, error) {
	select {
	case p := <-ch:
		return p, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Run keeps a persistent listener on the gate's telemetry so the HomeKit
// tile shows real open/closed state. It reconnects on drop and re-wakes the
// gate periodically so state stays fresh. Mirrors GreeController.Run.
func (c *CentsysController) Run(ctx context.Context) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		// Nothing to listen to until a login exists; wait for one.
		cr := c.creds.Load()
		if cr == nil {
			select {
			case <-c.reload:
			case <-ctx.Done():
				return
			}
			continue
		}
		if err := c.follow(ctx, cr); err != nil && ctx.Err() == nil {
			log.Printf("centsys: listener: %v (retrying in %s)", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 2*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second
	}
}

// follow stays subscribed to deviceOverview until ctx or the connection
// ends, decoding each frame into the gate's status. A fresh login (reload)
// also ends it, so Run reconnects with the new creds.
func (c *CentsysController) follow(ctx context.Context, cr *centsysCreds) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.reload:
			cancel()
		case <-ctx.Done():
		}
	}()

	serial := cr.serial
	tOverview := serial + "/deviceOverview"
	tReq := serial + "/connectionRequest"
	tReqResp := serial + "/connectionRequestResponse"
	cid := centsysClientID(cr.phone, ":camobs")

	onMsg := func(topic string, payload []byte) {
		if topic == tOverview {
			c.setStatus(decodeOverview(payload))
		}
	}
	cli, stop, err := c.mqttConnect(ctx, cr, ":camobs", onMsg)
	if err != nil {
		return err
	}
	defer stop()

	for _, t := range []string{tOverview, tReqResp} {
		if _, err := cli.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: t, QoS: 0}}}); err != nil {
			return fmt.Errorf("subscribe %s: %w", t, err)
		}
	}

	wake := func() {
		_, _ = cli.Publish(ctx, &paho.Publish{Topic: tReq, QoS: 0, Properties: c.pubProps(tReqResp, cid)})
	}
	wake()

	ticker := time.NewTicker(centsysRewake)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			wake()
		}
	}
}

// --- One-time OTP login ---------------------------------------------------

// centsysHTTP is the client for the gateway API. It trusts the system roots
// plus Centurion's CA, so it works whether the gateway uses a public cert or
// one from the same private CA as the broker.
func centsysHTTP() *http.Client {
	pool, _ := x509.SystemCertPool()
	if pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM([]byte(centsysCAPEM))
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
}

func centsysPost(ctx context.Context, cli *http.Client, url, bearer string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("User-Agent", centsysUserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return data, nil
}

// centsysSendOTP asks Centurion to send a one-time code to the account
// phone. Stateless on our side: ValidateOtp later needs only phone + code,
// so the send and the submit can come from separate requests (the web UI).
func centsysSendOTP(ctx context.Context, cfg CentsysConfig) error {
	if cfg.Phone == "" {
		return errors.New("centsys: config has no phone number")
	}
	_, err := centsysPost(ctx, centsysHTTP(), centsysGatewayZA+"/SendOtp", centsysServiceJWT, map[string]any{
		"MobileNumber": cfg.Phone, "OtpPlatform": 1, "ThreeLetterIsoLanguageName": "eng",
	})
	if err != nil {
		return fmt.Errorf("centsys: SendOtp: %w", err)
	}
	log.Printf("centsys: OTP sent to %s (WhatsApp/SMS)", cfg.Phone)
	return nil
}

// centsysLogin runs the interactive CLI login: send the OTP, prompt for the
// code, then complete and persist the session.
func centsysLogin(ctx context.Context, cfg CentsysConfig, promptCode func() (string, error)) error {
	if err := centsysSendOTP(ctx, cfg); err != nil {
		return err
	}
	code, err := promptCode()
	if err != nil {
		return err
	}
	return centsysCompleteLogin(ctx, cfg, code)
}

// centsysCompleteLogin validates the code, fetches the device and
// certificate, and writes the session file. Safe to call after an OTP was
// sent by either the CLI or the web UI.
func centsysCompleteLogin(ctx context.Context, cfg CentsysConfig, code string) error {
	if cfg.Phone == "" {
		return errors.New("centsys: config has no phone number")
	}
	cli := centsysHTTP()

	// 2. Validate OTP -> session token.
	data, err := centsysPost(ctx, cli, centsysGatewayZA+"/ValidateOtp", centsysServiceJWT, map[string]any{
		"MobileNumber": cfg.Phone, "Otp": strings.TrimSpace(code),
	})
	if err != nil {
		return fmt.Errorf("centsys: ValidateOtp: %w", err)
	}
	var vr struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(data, &vr); err != nil || vr.Response == "" {
		return errors.New("centsys: OTP rejected")
	}
	token := vr.Response

	// 3. List devices -> serial + MAC.
	data, err = centsysPost(ctx, cli, centsysGatewayZA+"/GetDevicesByRemoteUserNumber?remoteUserNumber="+url.QueryEscape(cfg.Phone), token, nil)
	if err != nil {
		return fmt.Errorf("centsys: GetDevices: %w", err)
	}
	var devices []struct {
		SerialNumber string `json:"serialNumber"`
		MACAddress   string `json:"macAddress"`
	}
	if err := json.Unmarshal(data, &devices); err != nil || len(devices) == 0 {
		return errors.New("centsys: no gate operators on this account")
	}
	gate := devices[0]
	if len(devices) > 1 {
		log.Printf("centsys: %d operators found; using the first (%s). shortcut: first-only, add a selector if wrong.", len(devices), gate.SerialNumber)
	}
	if gate.SerialNumber == "" || gate.MACAddress == "" {
		return errors.New("centsys: operator missing serial or MAC")
	}

	// 4. Certificate (PKCS#12) -> PEM cert + key.
	data, err = centsysPost(ctx, cli, centsysGatewayZA+"/GetCertificate", token, map[string]any{})
	if err != nil {
		return fmt.Errorf("centsys: GetCertificate: %w", err)
	}
	certPEM, keyPEM, err := centsysPFXToPEM(data)
	if err != nil {
		return err
	}

	sess := centsysSession{
		Phone: cfg.Phone, Serial: gate.SerialNumber, MAC: gate.MACAddress,
		CertPEM: string(certPEM), KeyPEM: string(keyPEM),
	}
	if _, err := parseMAC4(sess.Serial, sess.MAC); err != nil {
		return err
	}
	out, _ := json.MarshalIndent(sess, "", "  ")
	if err := os.WriteFile(cfg.sessionPath(), out, 0o600); err != nil {
		return fmt.Errorf("centsys: write %s: %w", cfg.sessionPath(), err)
	}
	log.Printf("centsys: logged in, gate %s saved to %s", gate.SerialNumber, cfg.sessionPath())
	return nil
}

// centsysPFXToPEM decodes the GetCertificate response (a JSON object with a
// base64 PKCS#12 blob and its password) into PEM cert and key.
func centsysPFXToPEM(body []byte) (certPEM, keyPEM []byte, err error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, fmt.Errorf("centsys: GetCertificate body: %w", err)
	}
	pick := func(names ...string) string {
		for k, v := range raw {
			for _, n := range names {
				if strings.EqualFold(k, n) {
					var s string
					_ = json.Unmarshal(v, &s)
					return s
				}
			}
		}
		return ""
	}
	pfxB64 := pick("certificatePfxBase64", "pfxBase64")
	password := pick("certificatePassword", "password")
	if pfxB64 == "" {
		return nil, nil, errors.New("centsys: GetCertificate returned no certificate")
	}
	pfx, err := base64.StdEncoding.DecodeString(pfxB64)
	if err != nil {
		return nil, nil, fmt.Errorf("centsys: certificate base64: %w", err)
	}
	key, cert, err := pkcs12.Decode(pfx, password)
	if err != nil {
		return nil, nil, fmt.Errorf("centsys: certificate decode: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("centsys: marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// --- Web login UI ---------------------------------------------------------

// CentsysWeb backs the browser login flow: send the OTP, submit the code,
// and persist the session to the state volume. It works before any login
// exists (ctrl is nil then); the gate becomes a live HomeKit tile on the
// next restart, once the session file is present at startup.
type CentsysWeb struct {
	cfg  CentsysConfig
	ctrl *CentsysController // always set when a gate is configured
}

func (s *CentsysWeb) signedIn() bool {
	return s.ctrl != nil && s.ctrl.creds.Load() != nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// HandleStatus reports whether the gate is configured, logged in, and (when
// live) its current open/closed state.
func (s *CentsysWeb) HandleStatus(w http.ResponseWriter, r *http.Request) {
	status := ""
	if s.signedIn() {
		switch s.ctrl.CurrentStatus() {
		case gateOpen:
			status = "open"
		case gateClosed:
			status = "closed"
		case gateOpening:
			status = "opening"
		case gateClosing:
			status = "closing"
		case gateStopped:
			status = "stopped"
		}
	}
	writeJSON(w, map[string]any{
		"configured": true,
		"phone":      s.cfg.Phone,
		"loggedIn":   s.signedIn(),
		"gateStatus": status,
	})
}

// HandleSend triggers the OTP to the account phone.
func (s *CentsysWeb) HandleSend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if err := centsysSendOTP(ctx, s.cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// HandleLogin validates the submitted code and writes the session file.
// The gate goes live after the next restart (HomeKit accessories are fixed
// at startup), so the response says so.
func (s *CentsysWeb) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.Code) == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := centsysCompleteLogin(ctx, s.cfg, body.Code); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Pick up the just-written session so the gate goes live immediately —
	// no restart.
	if s.ctrl != nil {
		if err := s.ctrl.Reload(); err != nil {
			http.Error(w, "saved but failed to activate: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}
