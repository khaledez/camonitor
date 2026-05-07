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

	// Optional overrides — empty means "use the Dahua default".
	RTSPURLOverride string `json:"rtsp_url,omitempty"`
	DoorURLOverride string `json:"door_url,omitempty"`
}

// RTSPURLForSubtype returns the RTSP URL for a given Dahua subtype:
// 0 = main stream (HD), 1 = sub stream (SD). If rtsp_url is set in config
// we use it verbatim and the subtype is ignored.
func (s StreamConfig) RTSPURLForSubtype(subtype int) string {
	if s.RTSPURLOverride != "" {
		return s.RTSPURLOverride
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
	Listen  string         `json:"listen"`
	Streams []StreamConfig `json:"streams"`
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
	if len(cfg.Streams) == 0 {
		return nil, fmt.Errorf("%s: no streams configured", path)
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

	udp, err := ts.ListenPacket("udp", "0.0.0.0:0")
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
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
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

	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/webrtc/offer", hub.HandleOffer)
	mux.HandleFunc("/webrtc/answer", hub.HandleAnswer)
	mux.HandleFunc("/stream/quality", hub.HandleSetQuality)
	mux.HandleFunc("/door/open", doors.HandleOpen)

	// The plain HTTP listener stays even when tailnet is enabled — it
	// serves liveness/readiness probes from kubelet and any in-cluster
	// access. Tailnet clients use the HTTPS listener below.
	plainSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	tsSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("camonitor HTTP listening on %s with %d stream(s)", cfg.Listen, len(cfg.Streams))
		if err := plainSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("plain http: %w", err)
		}
	}()

	if tsListener != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("camonitor HTTPS listening on %s.<tailnet>.ts.net", *tsHostname)
			if err := tsSrv.Serve(tsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- fmt.Errorf("tsnet http: %w", err)
			}
		}()
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
	cancel()
	wg.Wait()
}
