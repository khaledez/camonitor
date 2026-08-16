// HomeKit support: publishes camonitor's devices as HAP accessories so
// they can be paired from the iPhone Home app.
//
// Topology is dictated by HomeKit itself. Locks and the air conditioner
// ride a single bridge; cameras cannot be bridged and each need their own
// hap.Server (phase 2). Every server shares one setup code, so pairing is
// "scan once, then Add Accessory for each camera" — the same shape
// Homebridge uses for its external accessories.
//
// The setup code is surfaced two ways, mirroring the WhatsApp pairing
// flow: a QR in the web UI at /homekit/qr.png and a half-block ASCII
// rendering on stdout at startup.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/mdp/qrterminal/v3"
)

const (
	defaultHomeKitPort  = 51826
	defaultHomeKitStore = "/var/lib/camonitor/homekit"
	defaultRelockAfter  = 5 * time.Second

	// restartBackoff keeps a server that fails to start (port taken,
	// interface gone) from spinning. Startup already fails fast on a bound
	// port, so this only covers failures that appear later.
	restartBackoff = 5 * time.Second
)

// errHomeKitStore marks a failure to open or write the pairing store.
// Callers downgrade it to "HomeKit disabled" rather than exiting: pairing
// without persistence is worthless, but doors and the web UI are the more
// important function and they do not need it.
var errHomeKitStore = errors.New("homekit pairing store")

// HomeKitConfig is the on-disk shape for the optional `homekit` block.
// Its absence disables HomeKit entirely, the same way `gree` and
// `whatsapp` already behave.
type HomeKitConfig struct {
	// Pin is the setup code, with or without dashes ("031-45-154").
	// Fixed in config rather than generated so wiping the pairing store
	// does not change the code you have written down.
	Pin string `json:"pin"`
	// Port is the bridge's TCP port. Camera accessories take the ports
	// immediately above it. Defaults to 51826.
	Port int `json:"port,omitempty"`
	// Store is the directory holding HAP pairing state.
	Store string `json:"store,omitempty"`
	// RelockAfter is how long a lock reports Unsecured after a successful
	// open. Go duration string; defaults to 5s.
	RelockAfter string `json:"relock_after,omitempty"`
}

func (c HomeKitConfig) port() int {
	if c.Port == 0 {
		return defaultHomeKitPort
	}
	return c.Port
}

func (c HomeKitConfig) store() string {
	if c.Store == "" {
		return defaultHomeKitStore
	}
	return c.Store
}

// relockAfter parses RelockAfter, falling back to the default when empty.
func (c HomeKitConfig) relockAfter() (time.Duration, error) {
	if c.RelockAfter == "" {
		return defaultRelockAfter, nil
	}
	d, err := time.ParseDuration(c.RelockAfter)
	if err != nil {
		return 0, fmt.Errorf("relock_after %q: %w", c.RelockAfter, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("relock_after %q: must be positive", c.RelockAfter)
	}
	return d, nil
}

// normalizePin strips formatting and validates the setup code. HomeKit
// codes are eight digits; Apple rejects a handful of trivial ones, and so
// does hap, so we catch them at config load with a clearer message than a
// pairing failure would give.
func normalizePin(pin string) (string, error) {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, pin)

	if len(digits) != 8 {
		return "", fmt.Errorf("pin %q: need exactly 8 digits, got %d", pin, len(digits))
	}
	if hap.InvalidPins[digits] {
		return "", fmt.Errorf("pin %q: too easily guessed, Apple rejects it", pin)
	}
	return digits, nil
}

// formatPin renders eight digits as the XXX-XX-XXX form Apple displays.
func formatPin(digits string) string {
	return digits[:3] + "-" + digits[3:5] + "-" + digits[5:]
}

// setupPayloadURI builds the X-HM:// string an iPhone camera scans.
//
// Layout per HAP-R2 §5.7, most significant bits first: 3 version bits,
// 4 reserved, 8 category, 4 transport flags, then the 27-bit setup code.
// The result is base-36 encoded, upper-cased, left-padded to 9 characters,
// and suffixed with the 4-character setup ID.
func setupPayloadURI(pin string, category byte, setupID string) (string, error) {
	code, err := strconv.ParseUint(pin, 10, 32)
	if err != nil {
		return "", fmt.Errorf("pin %q: %w", pin, err)
	}

	const (
		version    = 0
		reserved   = 0
		flagsIP    = 2 // bit 1 = supports IP transport
		codeBits   = 27
		payloadFmt = 36
	)

	var payload uint64
	payload = version & 0x7
	payload = payload<<4 | reserved&0xF
	payload = payload<<8 | uint64(category)
	payload = payload<<4 | flagsIP
	payload = payload<<codeBits | (code & 0x7FFFFFF)

	encoded := strings.ToUpper(strconv.FormatUint(payload, payloadFmt))
	return "X-HM://" + strings.Repeat("0", max(0, 9-len(encoded))) + encoded + setupID, nil
}

// setupIDFor derives a stable 4-character setup ID. HomeKit uses it to
// match a scanned code against the accessory advertising it. Deriving it
// from the pin and accessory name rather than storing it keeps the state
// directory to exactly what hap owns, and keeps the QR stable across a
// wiped pairing store.
func setupIDFor(pin, name string) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	sum := sha256.Sum256([]byte(pin + "\x00" + name))
	id := make([]byte, 4)
	for i := range id {
		id[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return string(id)
}

// hkServer is one pairable HomeKit endpoint: the bridge, or (phase 2) a
// single camera accessory.
type hkServer struct {
	name  string
	addr  string
	uri   string // X-HM:// setup payload, pre-rendered for the QR
	srv   *hap.Server
	store hap.Store

	mu      sync.Mutex
	restart context.CancelFunc
}

// pairedControllers counts stored pairings. hap keeps one file per paired
// controller and exposes the count only on its unexported storer, so we
// read it back through the Store interface.
func (s *hkServer) pairedControllers() int {
	keys, err := s.store.KeysWithSuffix(".pairing")
	if err != nil {
		return 0
	}
	return len(keys)
}

// unpair forgets every controller and bounces the server so it
// re-advertises as pairable. hap updates its discovery flag only while
// starting up, so a restart is the only way to make the accessory visible
// again without restarting the whole process.
func (s *hkServer) unpair() error {
	keys, err := s.store.KeysWithSuffix(".pairing")
	if err != nil {
		return fmt.Errorf("list pairings: %w", err)
	}
	for _, k := range keys {
		if err := s.store.Delete(k); err != nil {
			return fmt.Errorf("delete pairing %s: %w", k, err)
		}
	}

	s.mu.Lock()
	restart := s.restart
	s.mu.Unlock()
	if restart != nil {
		restart()
	}
	return nil
}

// run serves until ctx is cancelled, restarting after an unpair.
func (s *hkServer) run(ctx context.Context) {
	for ctx.Err() == nil {
		runCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.restart = cancel
		s.mu.Unlock()

		err := s.srv.ListenAndServe(runCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("homekit [%s]: serve: %v", s.name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(restartBackoff):
			}
			continue
		}
		log.Printf("homekit [%s]: restarting (pairings cleared)", s.name)
	}
}

// HomeKitManager owns every hap.Server camonitor publishes plus the HTTP
// surface the web UI pairs through.
type HomeKitManager struct {
	pin     string // eight digits, no dashes
	servers []*hkServer
	qrPNG   []byte
	climate *hkClimate
}

// NewHomeKitManager builds the phase-1 bridge: one lock per door station
// and, when configured, the air conditioner. Returns an error for any
// misconfiguration that would otherwise surface as a mystifying pairing
// failure later.
func NewHomeKitManager(cfg HomeKitConfig, streams []StreamConfig, doors doorOpener, gree greeDevice) (*HomeKitManager, error) {
	pin, err := normalizePin(cfg.Pin)
	if err != nil {
		return nil, err
	}
	relock, err := cfg.relockAfter()
	if err != nil {
		return nil, err
	}

	bridge, children, climate := buildBridgeAccessories(streams, doors, gree, relock)

	storeDir := filepath.Join(cfg.store(), "bridge")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errHomeKitStore, storeDir, err)
	}
	store := hap.NewFsStore(storeDir)

	// NewServer writes the accessory's uuid and keypair, so a store that
	// is present but unwritable surfaces here rather than above.
	srv, err := hap.NewServer(store, bridge, children...)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errHomeKitStore, err)
	}

	addr := fmt.Sprintf(":%d", cfg.port())
	setupID := setupIDFor(pin, bridgeName)
	srv.Pin = pin
	srv.Addr = addr
	srv.SetupId = setupID

	uri, err := setupPayloadURI(pin, accessory.TypeBridge, setupID)
	if err != nil {
		return nil, err
	}

	if err := checkPortFree(addr); err != nil {
		return nil, err
	}

	return &HomeKitManager{
		pin: pin,
		servers: []*hkServer{{
			name:  bridgeName,
			addr:  addr,
			uri:   uri,
			srv:   srv,
			store: store,
		}},
		qrPNG:   renderQRPNG(uri),
		climate: climate,
	}, nil
}

// startHomeKit builds and starts the bridge when configured, returning nil
// when HomeKit is off or its pairing store is unusable. Misconfiguration —
// a bad pin, a taken port — is fatal instead, because those would
// otherwise surface as an accessory that silently never appears.
func startHomeKit(ctx context.Context, cfg *Config, doors doorOpener, gree *GreeController) *HomeKitManager {
	if cfg.HomeKit == nil {
		log.Printf("homekit: not configured (skipping)")
		return nil
	}

	// A nil *GreeController held in an interface is not a nil interface,
	// so the conversion has to be explicit.
	var greeDev greeDevice
	if gree != nil {
		greeDev = gree
	}

	m, err := NewHomeKitManager(*cfg.HomeKit, cfg.Streams, doors, greeDev)
	if err != nil {
		if errors.Is(err, errHomeKitStore) {
			log.Printf("homekit: %v — disabled; doors and web UI unaffected", err)
			return nil
		}
		log.Fatalf("homekit: %v", err)
	}

	if climate := m.Climate(); climate != nil {
		gree.OnUpdate(climate.Update)
	}
	go m.Run(ctx)
	return m
}

// checkPortFree fails fast on a port conflict. Half-advertising an
// accessory that cannot accept connections is worse than not starting.
func checkPortFree(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("homekit port %s: %w", addr, err)
	}
	return ln.Close()
}

// Climate exposes the climate accessory so main can wire it to the Gree
// poll. Nil when no air conditioner is configured.
func (m *HomeKitManager) Climate() *hkClimate { return m.climate }

// Run serves every accessory until ctx is cancelled.
func (m *HomeKitManager) Run(ctx context.Context) {
	m.printPairingCode()

	var wg sync.WaitGroup
	for _, s := range m.servers {
		wg.Go(func() {
			log.Printf("homekit [%s]: listening on %s", s.name, s.addr)
			s.run(ctx)
		})
	}
	wg.Wait()
}

// printPairingCode writes the setup code to stdout when nothing is paired
// yet, so a headless setup can pair from `docker logs` alone.
func (m *HomeKitManager) printPairingCode() {
	for _, s := range m.servers {
		if s.pairedControllers() > 0 {
			continue
		}
		log.Printf("homekit [%s]: not paired — setup code %s", s.name, formatPin(m.pin))
		qrterminal.GenerateHalfBlock(s.uri, qrterminal.L, os.Stdout)
	}
}

// HomeKitStatus is the JSON shape served to the web UI.
type HomeKitStatus struct {
	Configured  bool                  `json:"configured"`
	Pin         string                `json:"pin"`
	Paired      bool                  `json:"paired"`
	Accessories []HomeKitAccessoryDTO `json:"accessories"`
}

type HomeKitAccessoryDTO struct {
	Name        string `json:"name"`
	Controllers int    `json:"controllers"`
}

// HandleStatus serves GET /homekit/status.
func (m *HomeKitManager) HandleStatus(w http.ResponseWriter, r *http.Request) {
	st := HomeKitStatus{Configured: true, Pin: formatPin(m.pin)}
	for _, s := range m.servers {
		n := s.pairedControllers()
		if n > 0 {
			st.Paired = true
		}
		st.Accessories = append(st.Accessories, HomeKitAccessoryDTO{Name: s.name, Controllers: n})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(st)
}

// HandleQR serves GET /homekit/qr.png — the bridge's setup payload. The
// camera accessories (phase 2) share this code; the Home app finds them as
// nearby accessories once the bridge is paired.
func (m *HomeKitManager) HandleQR(w http.ResponseWriter, r *http.Request) {
	if len(m.qrPNG) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(m.qrPNG)))
	_, _ = w.Write(m.qrPNG)
}

// HandleUnpair serves POST /homekit/unpair.
func (m *HomeKitManager) HandleUnpair(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	for _, s := range m.servers {
		if err := s.unpair(); err != nil {
			log.Printf("homekit [%s]: unpair: %v", s.name, err)
			http.Error(w, "unpair failed", http.StatusInternalServerError)
			return
		}
	}
	log.Printf("homekit: all pairings cleared")
	w.WriteHeader(http.StatusNoContent)
}

// handleHomeKitDisabled answers the status endpoint when HomeKit is off,
// so the web UI can render its panel without a conditional fetch — the
// same pattern /gree/status and /wa/status use.
func handleHomeKitDisabled(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"configured":false,"paired":false,"accessories":[]}`))
}
