// Camonitor bridges multiple Dahua VTO RTSP streams to WebRTC and serves a
// single web page that plays them all and exposes a per-stream door-open
// button. Configuration is read from a JSON file whose path is passed via
// the -config flag.
//
// When -tailscale <hostname> is supplied, camonitor registers itself on
// the user's tailnet under that hostname. HTTPS and pion's ICE UDP mux
// bind on the tsnet interface, so WebRTC media flows directly across the
// tailnet — no Tailscale operator / Ingress / per-port TCP-relay needed.
// The TS_AUTHKEY env var is required for the first auth; subsequent
// restarts reuse persisted state from $TS_STATE_DIR (default ./tsnet).
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"tailscale.com/tsnet"

	// Embed Go's tzdata so time.LoadLocation works in the FROM-scratch
	// image (which has no system zoneinfo).
	_ "time/tzdata"
)

//go:embed web
var webFS embed.FS

// StreamConfig is the on-disk shape for a single device. We deliberately
// model the parts of the Dahua VTO API we use (host + credentials) rather
// than asking the user to hand-craft RTSP and door-control URLs. If a user
// later needs a different RTSP path or a non-standard door endpoint, they
// can populate the explicit overrides.
type StreamConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	User string `json:"user"`
	Pass string `json:"pass"`

	// Door marks a stream as a door station (VTO) that exposes a magnetic-
	// lock relay and a bell/event stream. Cameras without a door lock (e.g.
	// plain IP cameras) leave this false, which hides the "open door"
	// button in the UI and skips the Dahua event-attach subscription.
	Door bool `json:"door,omitempty"`

	// RTSPPathTemplate is an optional printf-style path template with a
	// single %d placeholder that receives subtype+1 (1 = main/HD, 2 =
	// sub/SD). It lets non-Dahua cameras (e.g. Tiandy, whose RTSP paths are
	// /stream1 and /stream2) reuse the same HD/SD toggle machinery.
	RTSPPathTemplate string `json:"rtsp_path_template,omitempty"`

	// Codec names the video codec this stream delivers over RTSP. "h264"
	// (default) and "h265" are supported. The WebRTC track is created with
	// the matching codec so the browser negotiates it. Tiandy 4K cameras
	// commonly stream H.265/HEVC on both main and sub streams.
	Codec string `json:"codec,omitempty"`

	// SIP credentials. When SIPExt is set, camonitor registers as that
	// extension on the VTO's built-in SIP server and receives an INVITE
	// when the call button is pressed. Empty means "no SIP for this
	// stream" — door/RTSP/snapshot still work without these.
	SIPExt  string `json:"sip_ext,omitempty"`
	SIPPass string `json:"sip_pass,omitempty"`

	// Optional overrides — empty means "use the Dahua default".
	RTSPURLOverride string `json:"rtsp_url,omitempty"`
	DoorURLOverride string `json:"door_url,omitempty"`
}

// doorStreams returns only the streams flagged as door stations (VTO). The
// Dahua event-attach / bell machinery only makes sense for those; plain
// cameras have no eventManager.cgi endpoint and would otherwise spam the
// log with reconnect attempts.
func doorStreams(streams []StreamConfig) []StreamConfig {
	out := make([]StreamConfig, 0, len(streams))
	for _, s := range streams {
		if s.Door {
			out = append(out, s)
		}
	}
	return out
}

// RTSPURLForSubtype returns the RTSP URL for a given Dahua subtype:
// 0 = main stream (HD), 1 = sub stream (SD). If rtsp_url is set in config
// we use it verbatim and the subtype is ignored.
func (s StreamConfig) RTSPURLForSubtype(subtype int) string {
	if s.RTSPURLOverride != "" {
		return s.RTSPURLOverride
	}
	if s.RTSPPathTemplate != "" {
		u := &url.URL{
			Scheme: "rtsp",
			User:   url.UserPassword(s.User, s.Pass),
			Host:   s.Host,
			Path:   fmt.Sprintf(s.RTSPPathTemplate, subtype+1),
		}
		return u.String()
	}
	u := &url.URL{
		Scheme:   "rtsp",
		User:     url.UserPassword(s.User, s.Pass),
		Host:     s.Host,
		Path:     "/cam/realmonitor",
		RawQuery: fmt.Sprintf("channel=1&subtype=%d", subtype),
	}
	return u.String()
}

// DoorOpenURL returns the HTTP endpoint that triggers the magnetic-lock
// relay on the device. The default path is the documented Dahua VTO
// "openDoor" action, which is HTTP-Digest authenticated.
func (s StreamConfig) DoorOpenURL() string {
	if s.DoorURLOverride != "" {
		return s.DoorURLOverride
	}
	return fmt.Sprintf(
		"http://%s/cgi-bin/accessControl.cgi?action=openDoor&channel=1&UserID=101&Type=Remote",
		s.Host,
	)
}

type Config struct {
	Listen   string          `json:"listen"`
	SIP      SIPConfig       `json:"sip"`
	WhatsApp *WhatsAppConfig `json:"whatsapp,omitempty"`
	Gree     *GreeConfig     `json:"gree,omitempty"`
	Centsys  *CentsysConfig  `json:"centsys,omitempty"`
	HomeKit  *HomeKitConfig  `json:"homekit,omitempty"`
	Guest    *GuestConfig    `json:"guest,omitempty"`
	Streams  []StreamConfig  `json:"streams"`
	// Timezone names the IANA zone used when rendering wall-clock times
	// for outbound notifications (e.g. the WhatsApp caption). The browser
	// UI continues to use the viewer's local timezone via JS Date. Empty
	// defaults to UTC. Examples: "Asia/Hebron", "Europe/Berlin".
	Timezone string `json:"timezone,omitempty"`
	// BellHistoryDB is the path to a SQLite file used to persist the
	// ring history (events + snapshots) across pod restarts. Empty keeps
	// the existing in-memory-only behaviour. Recommended location is
	// the same PVC that holds the WhatsApp session.
	BellHistoryDB string `json:"bell_history_db,omitempty"`
}

// GuestConfig enables guest access (Funnel-exposed door buttons plus a
// tailnet-only admin API). Requires the -tailscale flag.
type GuestConfig struct {
	// StorePath defaults to /var/lib/camonitor/guests.json.
	StorePath string `json:"store_path,omitempty"`
	// PublicBase is the guest Funnel URL handed out in enroll links,
	// e.g. https://monitor.<tailnet>.ts.net:8443.
	PublicBase string `json:"public_base,omitempty"`
}

// guestDoorActions binds the guest API's two buttons to the real openers.
type guestDoorActions struct {
	centsys *CentsysController
	doors   *DoorClient
}

func (g guestDoorActions) OpenGate(ctx context.Context) error {
	if g.centsys == nil {
		return errors.New("centsys not configured")
	}
	return g.centsys.Open(ctx)
}

func (g guestDoorActions) OpenOutdoor(ctx context.Context) error {
	return g.doors.Open(ctx, "vto2")
}

func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.Guest != nil && cfg.Guest.PublicBase == "" {
		return nil, fmt.Errorf("%s: guest.public_base is required (e.g. https://monitor.<tailnet>.ts.net:8443)", path)
	}
	if cfg.Guest != nil && cfg.Guest.StorePath == "" {
		cfg.Guest.StorePath = "/var/lib/camonitor/guests.json"
	}
	if len(cfg.Streams) == 0 {
		return nil, fmt.Errorf("%s: no streams configured", path)
	}

	if cfg.Gree != nil && cfg.Gree.Host == "" && cfg.Gree.MAC == "" {
		return nil, fmt.Errorf("%s: gree needs a host or a mac", path)
	}

	seen := map[string]bool{}
	for i, s := range cfg.Streams {
		if s.ID == "" {
			return nil, fmt.Errorf("stream #%d: id is required", i)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("stream %q: duplicate id", s.ID)
		}
		seen[s.ID] = true
		if s.Host == "" && s.RTSPURLOverride == "" {
			return nil, fmt.Errorf("stream %q: host is required (unless rtsp_url is set)", s.ID)
		}
	}
	return &cfg, nil
}

// startTailscale brings up a tsnet node and returns the HTTPS listener and
// the UDP packet conn pion should use for ICE. Both are bound on the
// tailnet interface so browser-side ICE candidates are reachable from any
// other tailnet device.
//
// hostname becomes the tailnet machine name (and the host part of the
// HTTPS URL). TS_AUTHKEY is consulted on first registration; subsequent
// restarts reuse the persisted identity in TS_STATE_DIR.
func startTailscale(hostname string) (*tsnet.Server, net.Listener, net.PacketConn, error) {
	authKey := os.Getenv("TS_AUTHKEY")
	stateDir := os.Getenv("TS_STATE_DIR")
	if stateDir == "" {
		stateDir = "tsnet"
	}

	ts := &tsnet.Server{
		Hostname: hostname,
		AuthKey:  authKey, // empty is fine after first run; tsnet uses persisted identity
		Dir:      stateDir,
	}
	if err := ts.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("tsnet start: %w", err)
	}

	// Start() returns before authentication completes. Wait for the node
	// to be Running so its tailnet IP exists — without this, listeners
	// bound below will fail because the interface has no address yet.
	upCtx, upCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer upCancel()
	if _, err := ts.Up(upCtx); err != nil {
		ts.Close()
		return nil, nil, nil, fmt.Errorf("tsnet up: %w", err)
	}

	// tsnet's ListenPacket validates the address as a non-zero, non-
	// unspecified IP — so ":0" and "0.0.0.0:0" both fail. Bind on the
	// IPv4 address the control plane assigned to this node.
	ip4, _ := ts.TailscaleIPs()
	if !ip4.IsValid() {
		ts.Close()
		return nil, nil, nil, fmt.Errorf("tsnet has no IPv4 address after Up")
	}
	udp, err := ts.ListenPacket("udp", net.JoinHostPort(ip4.String(), "0"))
	if err != nil {
		ts.Close()
		return nil, nil, nil, fmt.Errorf("tsnet udp: %w", err)
	}

	tls, err := ts.ListenTLS("tcp", ":443")
	if err != nil {
		udp.Close()
		ts.Close()
		return nil, nil, nil, fmt.Errorf("tsnet listenTLS: %w", err)
	}

	return ts, tls, udp, nil
}

// newWebRTCAPI returns a *webrtc.API. If iceUDP is non-nil, all
// PeerConnections share it as their ICE-UDP transport — pion advertises
// the conn's local address as a host candidate, demuxes per-peer by ICE
// ufrag, and never needs the cluster's own ephemeral UDP ports.
func newWebRTCAPI(iceUDP net.PacketConn) *webrtc.API {
	if iceUDP == nil {
		return webrtc.NewAPI()
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, iceUDP))
	return webrtc.NewAPI(webrtc.WithSettingEngine(se))
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	configPath := flag.String("config", "config.json", "path to JSON config file")
	tsHostname := flag.String("tailscale", "",
		"if set, register on the tailnet with this hostname and serve HTTPS / "+
			"WebRTC media over the tailnet. Requires TS_AUTHKEY env on first run.")
	centsysLoginFlag := flag.Bool("centsys-login", false,
		"run the one-time Centsys gate login (sends an OTP, prompts for the code), "+
			"save the session, and exit")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if *centsysLoginFlag {
		if cfg.Centsys == nil {
			log.Fatalf("centsys-login: no \"centsys\" block in %s", *configPath)
		}
		err := centsysLogin(context.Background(), *cfg.Centsys, func() (string, error) {
			fmt.Print("Enter the OTP code you received: ")
			var code string
			_, err := fmt.Scanln(&code)
			return code, err
		})
		if err != nil {
			log.Fatalf("centsys-login: %v", err)
		}
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		ts         *tsnet.Server
		tsListener net.Listener
		iceUDP     net.PacketConn
	)
	if *tsHostname != "" {
		ts, tsListener, iceUDP, err = startTailscale(*tsHostname)
		if err != nil {
			log.Fatalf("tailscale: %v", err)
		}
		defer ts.Close()
		log.Printf("tsnet: hostname=%s ICE UDP=%s", *tsHostname, iceUDP.LocalAddr())
	}

	hub, err := NewHub(ctx, cfg.Streams, newWebRTCAPI(iceUDP))
	if err != nil {
		log.Fatalf("create hub: %v", err)
	}

	doors := NewDoorClient(cfg.Streams)

	// Built here rather than at mux-registration time because HomeKit
	// needs it too. Polling starts further down, once every observer has
	// registered.
	var greeCtrl *GreeController
	if cfg.Gree != nil {
		greeCtrl = NewGreeController(*cfg.Gree)
	}

	// Centsys gate is optional. The controller is built whenever a gate is
	// configured, even before the one-time login, so its HomeKit tile and
	// the web login endpoints are present; it stays dormant until a session
	// is loaded (at startup or via the web login) — never fatal.
	var centsysCtrl *CentsysController
	if cfg.Centsys != nil {
		centsysCtrl = NewCentsysController(*cfg.Centsys)
	}

	// WhatsApp is optional; nil sender means "skip the WA leg". The
	// snapshot fetcher is unconditional — every bell event tries to grab
	// a JPEG so the cached /snapshot endpoint always has the latest frame.
	var waSend whatsappSender
	var waClient *WhatsAppClient
	if cfg.WhatsApp != nil {
		waClient, err = NewWhatsAppClient(ctx, *cfg.WhatsApp)
		if err != nil {
			log.Fatalf("whatsapp: %v", err)
		}
		waSend = waClient.SendImage
		defer waClient.Close()
	} else {
		log.Printf("whatsapp: not configured (skipping)")
	}

	snapClient := newSnapshotClient()
	snapFetch := func(c context.Context, s StreamConfig) ([]byte, error) {
		return FetchSnapshot(c, snapClient, s)
	}
	tz := time.UTC
	if cfg.Timezone != "" {
		loc, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			log.Fatalf("invalid timezone %q: %v", cfg.Timezone, err)
		}
		tz = loc
	}
	// Optional persistent history. A failure to open the store is logged
	// but not fatal — the rest of the system works fine in-memory.
	var bellStore *BellStore
	if cfg.BellHistoryDB != "" {
		s, err := OpenBellStore(cfg.BellHistoryDB)
		if err != nil {
			log.Printf("bell history: open %q failed: %v (running in-memory only)",
				cfg.BellHistoryDB, err)
		} else {
			bellStore = s
			defer bellStore.Close()
		}
	}
	bell := NewBellBus(cfg.Streams, snapFetch, waSend, tz, bellStore)

	sipSrv, err := NewSIPServer(cfg.SIP, cfg.Streams, bell)
	if err != nil {
		log.Fatalf("sip: %v", err)
	}
	if sipSrv != nil {
		go func() {
			if err := sipSrv.Run(ctx); err != nil {
				log.Printf("sip: run exited: %v", err)
			}
		}()
	} else {
		log.Printf("sip: not configured (no stream has sip_ext)")
	}

	// Dahua's HTTP event-stream is the reliable bell trigger on every
	// VTO firmware we've tested — unlike the SIP path which depends on
	// IsMainVTO/registration topology. Run it for every door stream
	// alongside the SIP UA above; both feed the same BellBus and share
	// its per-stream debounce, so duplicate triggers are harmless. Plain
	// cameras (Door=false) have no eventManager.cgi and are skipped.
	events := NewEventAttachClient(doorStreams(cfg.Streams), bell)
	go events.Run(ctx)

	homekit := newHomeKit(cfg, doors, greeCtrl, centsysCtrl, bell, snapFetch)

	// Every observer is registered by now, so start polling the AC and
	// following the gate's status.
	if greeCtrl != nil {
		go greeCtrl.Run(ctx)
	}
	if centsysCtrl != nil {
		go centsysCtrl.Run(ctx)
	}

	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	base := http.NewServeMux()
	base.Handle("/", http.FileServer(http.FS(staticFS)))
	base.HandleFunc("/webrtc/offer", hub.HandleOffer)
	base.HandleFunc("/webrtc/answer", hub.HandleAnswer)
	base.HandleFunc("/stream/quality", hub.HandleSetQuality)
	base.HandleFunc("/door/open", doors.HandleOpen)
	base.HandleFunc("/events", bell.HandleEvents)
	base.HandleFunc("/snapshot", bell.HandleSnapshot)
	base.HandleFunc("/rings", bell.HandleHistory)

	// Gree climate control is optional; nil config means "skip". When
	// absent we still mount a static "not configured" status endpoint so
	// the web UI can hide the climate panel without a 404.
	if greeCtrl != nil {
		base.HandleFunc("/gree/status", greeCtrl.HandleStatus)
		base.HandleFunc("/gree/set", greeCtrl.HandleSet)
	} else {
		base.HandleFunc("/gree/status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(`{"configured":false}`))
		})
	}

	// Centsys gate login + status. The endpoints exist whenever a gate is
	// configured, even before the one-time login, so the browser can do it.
	if cfg.Centsys != nil {
		cw := &CentsysWeb{cfg: *cfg.Centsys, ctrl: centsysCtrl}
		base.HandleFunc("/centsys/status", cw.HandleStatus)
		base.HandleFunc("/centsys/send", cw.HandleSend)
		base.HandleFunc("/centsys/login", cw.HandleLogin)
	} else {
		base.HandleFunc("/centsys/status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(`{"configured":false}`))
		})
	}
	if homekit != nil {
		base.HandleFunc("/homekit/status", homekit.HandleStatus)
		base.HandleFunc("/homekit/qr.png", homekit.HandleQR)
	} else {
		base.HandleFunc("/homekit/status", handleHomeKitDisabled)
	}

	if waClient != nil {
		base.HandleFunc("/wa/status", waClient.HandleStatus)
		base.HandleFunc("/wa/qr.png", waClient.HandleQR)
		base.HandleFunc("/wa/unpair", waClient.HandleUnpair)
	} else {
		// Static "not configured" status so the web UI can render its
		// WhatsApp panel without conditionally fetching.
		base.HandleFunc("/wa/status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(`{"configured":false,"paired":false,"recipients":[],"hasQR":false}`))
		})
	}

	// The plain HTTP listener stays even when tailnet is enabled — it
	// serves liveness/readiness probes from kubelet and any in-cluster
	// access. Tailnet clients use the HTTPS listener below.
	var lanHandler, tsHandler http.Handler = base, base
	var guestFunnel net.Listener
	var guestSrv *http.Server
	switch {
	case cfg.Guest == nil:
	case ts == nil:
		log.Printf("guest: guest access needs -tailscale; guest access disabled")
	default:
		store, err := OpenGuestStore(cfg.Guest.StorePath)
		if err != nil {
			// A bad guests.json must not take cameras and doors down with it.
			log.Printf("guest: open store %q failed: %v; guest access disabled", cfg.Guest.StorePath, err)
			break
		}
		guestAPI := NewGuestAPI(store, guestDoorActions{centsys: centsysCtrl, doors: doors})
		admin := NewGuestAdmin(store, cfg.Guest.PublicBase)
		lanHandler, tsHandler = buildSurfaces(base, admin.Routes())

		// Funnel needs the "funnel" node attribute in the tailnet ACL; without
		// it we keep running, just without guest access.
		guestFunnel, err = ts.ListenFunnel("tcp", ":8443")
		if err != nil {
			log.Printf("guest: Funnel unavailable — enable the funnel node attribute in the tailnet ACL; guest access disabled: %v", err)
			guestFunnel = nil
		} else {
			guestSrv = &http.Server{Handler: guestAPI.Routes(), ReadHeaderTimeout: 10 * time.Second}
		}
	}

	plainSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           lanHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	tsSrv := &http.Server{
		Handler:           tsHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 3)
	var wg sync.WaitGroup

	// Joined rather than fired and forgotten: shutdown has to outlive
	// hap's dnssd goodbye records, or the accessory lingers in the Home
	// app until its mDNS record ages out.
	if homekit != nil {
		wg.Go(func() { homekit.Run(ctx) })
	}

	wg.Go(func() {
		log.Printf("camonitor HTTP listening on %s with %d stream(s)", cfg.Listen, len(cfg.Streams))
		if err := plainSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("plain http: %w", err)
		}
	})

	if tsListener != nil {
		wg.Go(func() {
			log.Printf("camonitor HTTPS listening on %s.<tailnet>.ts.net", *tsHostname)
			if err := tsSrv.Serve(tsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- fmt.Errorf("tsnet http: %w", err)
			}
		})
	}

	if guestSrv != nil {
		wg.Go(func() {
			log.Printf("camonitor guest Funnel listening on :8443")
			if err := guestSrv.Serve(guestFunnel); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- fmt.Errorf("guest funnel: %w", err)
			}
		})
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sig:
		log.Println("shutting down")
	case err := <-serverErr:
		log.Fatalf("%v", err)
	}

	shutCtx, cancelShut := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShut()
	_ = plainSrv.Shutdown(shutCtx)
	_ = tsSrv.Shutdown(shutCtx)
	if guestSrv != nil {
		_ = guestSrv.Shutdown(shutCtx)
	}
	cancel()
	wg.Wait()
}
