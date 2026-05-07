// Camonitor bridges multiple Dahua VTO RTSP streams to WebRTC and serves a
// single web page that plays them all and exposes a per-stream door-open
// button. Configuration is read from a JSON file whose path is passed via
// the -config flag.
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
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
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

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	configPath := flag.String("config", "config.json", "path to JSON config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The hub starts and supervises one RTSP reader per stream — passing
	// it ctx here lets a SIGTERM cascade through to every reader.
	hub, err := NewHub(ctx, cfg.Streams)
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

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("camonitor listening on %s with %d stream(s)", cfg.Listen, len(cfg.Streams))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sig:
		log.Println("shutting down")
	case err := <-serverErr:
		log.Fatalf("http server: %v", err)
	}

	shutCtx, cancelShut := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShut()
	_ = srv.Shutdown(shutCtx)
	cancel()
}
