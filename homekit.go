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

	// restartBackoff paces retries when the server cannot start — the port
	// is taken, the interface is gone. Retrying rather than exiting is
	// deliberate: under hostNetwork any other process on the node can hold
	// 51826, and HomeKit going missing must not take doors and cameras
	// down with it.
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
//
// There is deliberately no "unpair" here. Removing the accessory in the
// Home app is the supported path and hap handles it properly, including
// re-advertising as pairable. Doing it ourselves would mean either
// bouncing the hap.Server — which cannot be restarted, it closes the
// http.Server it was built with and never rebuilds it — or constructing a
// second one over the same accessories, which double-registers hap's
// notification callbacks and duplicates every event to the controller.
// Recovering a wedged pairing store is an operator job: delete the store
// directory and restart.
type hkServer struct {
	name  string
	addr  string
	uri   string // X-HM:// setup payload, pre-rendered for the QR
	srv   *hap.Server
	store hap.Store
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

// run serves until ctx is cancelled. A bind failure (something else on the
// host already holds the port) is retried rather than fatal: HomeKit going
// missing is much cheaper than taking doors and cameras down with it.
func (s *hkServer) run(ctx context.Context) {
	for ctx.Err() == nil {
		log.Printf("homekit [%s]: listening on %s", s.name, s.addr)
		err := s.srv.ListenAndServe(ctx)
		if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		log.Printf("homekit [%s]: %v — retrying in %v", s.name, err, restartBackoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(restartBackoff):
		}
	}
}

// HomeKitManager owns every hap.Server camonitor publishes plus the HTTP
// surface the web UI pairs through.
type HomeKitManager struct {
	pin     string // eight digits, no dashes
	servers []*hkServer
	qrPNG   []byte
	climate *hkClimate
	cameras []*hkCamera
	bell    *BellBus
}

// NewHomeKitManager builds the bridge — one lock per door station plus the
// air conditioner — and one standalone video-doorbell accessory per door
// station. Returns an error for any misconfiguration that would otherwise
// surface as a mystifying pairing failure later.
//
// bell and snapshot may be nil, in which case the camera accessories are
// skipped and only the bridge is published.
func NewHomeKitManager(cfg HomeKitConfig, streams []StreamConfig, doors doorOpener, gree greeDevice, bell *BellBus, snapshot snapshotFetcher) (*HomeKitManager, error) {
	pin, err := normalizePin(cfg.Pin)
	if err != nil {
		return nil, err
	}
	relock, err := cfg.relockAfter()
	if err != nil {
		return nil, err
	}

	bridge, children, climate := buildBridgeAccessories(streams, doors, gree, relock)
	bridgeSrv, err := newHKServer(cfg, pin, bridgeName, "bridge", cfg.port(),
		accessory.TypeBridge, bridge, children...)
	if err != nil {
		return nil, err
	}

	m := &HomeKitManager{
		pin:     pin,
		servers: []*hkServer{bridgeSrv},
		qrPNG:   renderQRPNG(bridgeSrv.uri),
		climate: climate,
		bell:    bell,
	}

	if bell == nil || snapshot == nil {
		return m, nil
	}

	// Cameras cannot ride the bridge, so each gets its own server on the
	// port above it, sharing the one setup code. The Home app surfaces
	// them as nearby accessories once the bridge is paired.
	port := cfg.port()
	for _, s := range doorStreams(streams) {
		port++
		cam := newHKCamera(s, bell, snapshot)
		srv, err := newHKServer(cfg, pin, cam.a.Name(), "camera-"+s.ID, port,
			accessory.TypeVideoDoorbell, cam.a)
		if err != nil {
			return nil, err
		}
		// HAP has no snapshot characteristic; the Home app fetches stills
		// over the encrypted session at this path.
		srv.srv.ServeMux().HandleFunc("/resource", cam.handleSnapshot(srv.srv))

		m.cameras = append(m.cameras, cam)
		m.servers = append(m.servers, srv)
	}
	return m, nil
}

// newHKServer wires one pairable accessory tree onto its own port and
// pairing store.
func newHKServer(cfg HomeKitConfig, pin, name, storeName string, port int, category byte, a *accessory.A, children ...*accessory.A) (*hkServer, error) {
	store, err := openPairingStore(filepath.Join(cfg.store(), storeName))
	if err != nil {
		return nil, err
	}

	// Exactly one NewServer per accessory tree, ever: it appends a
	// notification callback to every characteristic, so a second call over
	// the same accessories makes hap emit each event twice.
	srv, err := hap.NewServer(store, a, children...)
	if err != nil {
		return nil, fmt.Errorf("build accessories for %s: %w", name, err)
	}

	setupID := setupIDFor(pin, name)
	srv.Pin = pin
	srv.Addr = fmt.Sprintf(":%d", port)
	srv.SetupId = setupID

	uri, err := setupPayloadURI(pin, category, setupID)
	if err != nil {
		return nil, err
	}

	return &hkServer{
		name:  name,
		addr:  srv.Addr,
		uri:   uri,
		srv:   srv,
		store: store,
	}, nil
}

// openPairingStore creates the store directory and proves it is writable,
// so an unusable volume is reported as such rather than surfacing later as
// an opaque hap error that gets misattributed.
func openPairingStore(dir string) (hap.Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errHomeKitStore, dir, err)
	}
	store := hap.NewFsStore(dir)
	const probe = "camonitor.probe"
	if err := store.Set(probe, []byte("ok")); err != nil {
		return nil, fmt.Errorf("%w: %s not writable: %v", errHomeKitStore, dir, err)
	}
	if err := store.Delete(probe); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errHomeKitStore, dir, err)
	}
	return store, nil
}

// newHomeKit builds the bridge when configured, returning nil when
// HomeKit is off or its pairing store is unusable. Misconfiguration — a
// bad pin — is fatal instead, because it would otherwise surface as an
// accessory that silently never appears.
//
// The caller starts Run and is expected to join it on shutdown: hap's
// dnssd responder sends goodbye records when its context is cancelled,
// and exiting before it does leaves a ghost accessory advertised on the
// network until the record ages out. That is not hypothetical here — the
// deployment uses the Recreate strategy, so every redeploy would leave
// one behind.
func newHomeKit(cfg *Config, doors doorOpener, gree *GreeController, bell *BellBus, snapshot snapshotFetcher) *HomeKitManager {
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

	m, err := NewHomeKitManager(*cfg.HomeKit, cfg.Streams, doors, greeDev, bell, snapshot)
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
	return m
}

// Climate exposes the climate accessory so main can wire it to the Gree
// poll. Nil when no air conditioner is configured.
func (m *HomeKitManager) Climate() *hkClimate { return m.climate }

// Run serves every accessory until ctx is cancelled.
func (m *HomeKitManager) Run(ctx context.Context) {
	m.printPairingCode()

	var wg sync.WaitGroup

	// One subscription feeds every camera. BellBus fans out to each
	// subscriber, and a per-camera subscription would just mean more
	// channels carrying the same events.
	if len(m.cameras) > 0 {
		events, unsubscribe := m.bell.Subscribe()
		wg.Go(func() {
			defer unsubscribe()
			m.dispatchBell(ctx, events)
		})
	}

	for _, s := range m.servers {
		wg.Go(func() { s.run(ctx) })
	}

	wg.Wait()

	for _, cam := range m.cameras {
		cam.stop()
	}
}

// dispatchBell hands each ring to the camera it belongs to.
func (m *HomeKitManager) dispatchBell(ctx context.Context, events <-chan BellEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			for _, cam := range m.cameras {
				cam.ring(ev)
			}
		}
	}
}

// printPairingCode writes the setup code to stdout when nothing is paired
// yet, so a headless setup can pair from `docker logs` alone.
func (m *HomeKitManager) printPairingCode() {
	var unpaired []string
	for _, s := range m.servers {
		if s.pairedControllers() == 0 {
			unpaired = append(unpaired, s.name)
		}
	}
	if len(unpaired) == 0 {
		return
	}

	log.Printf("homekit: setup code %s — not yet paired: %s",
		formatPin(m.pin), strings.Join(unpaired, ", "))
	// One QR only. Every accessory shares the code, and the Home app finds
	// the cameras as nearby accessories once the bridge is in.
	qrterminal.GenerateHalfBlock(m.servers[0].uri, qrterminal.L, os.Stdout)
}

// HomeKitStatus is the JSON shape served to the web UI. Pin is populated
// only while the bridge is unpaired — see HandleStatus.
type HomeKitStatus struct {
	Configured  bool                  `json:"configured"`
	Pin         string                `json:"pin,omitempty"`
	Paired      bool                  `json:"paired"`
	Accessories []HomeKitAccessoryDTO `json:"accessories"`
}

type HomeKitAccessoryDTO struct {
	Name        string `json:"name"`
	Controllers int    `json:"controllers"`
}

// fullyPaired reports whether every accessory has a controller — i.e.
// setup is finished and the code is no longer needed.
//
// Deliberately "every", not "any": the cameras are separate accessories
// that each need the code at Add Accessory time, so withholding it the
// moment the bridge pairs would strand the user halfway through setup.
func (m *HomeKitManager) fullyPaired() bool {
	for _, s := range m.servers {
		if s.pairedControllers() == 0 {
			return false
		}
	}
	return true
}

// HandleStatus serves GET /homekit/status.
//
// The setup code is withheld once paired. It is a permanent credential
// that grants control of the door locks, and this mux has no
// authentication — the same reasoning that makes /wa/qr.png 404 after
// pairing, except that here the code never rotates, so withholding it
// matters more.
func (m *HomeKitManager) HandleStatus(w http.ResponseWriter, r *http.Request) {
	st := HomeKitStatus{Configured: true, Paired: m.fullyPaired()}
	if !st.Paired {
		st.Pin = formatPin(m.pin)
	}
	for _, s := range m.servers {
		st.Accessories = append(st.Accessories, HomeKitAccessoryDTO{
			Name:        s.name,
			Controllers: s.pairedControllers(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(st)
}

// HandleQR serves GET /homekit/qr.png — the bridge's setup payload, and
// only while unpaired. The QR encodes the setup code, so serving it after
// pairing would hand out the same credential HandleStatus withholds.
func (m *HomeKitManager) HandleQR(w http.ResponseWriter, r *http.Request) {
	if len(m.qrPNG) == 0 || m.fullyPaired() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(m.qrPNG)))
	_, _ = w.Write(m.qrPNG)
}

// handleHomeKitDisabled answers the status endpoint when HomeKit is off,
// so the web UI can render its panel without a conditional fetch — the
// same pattern /gree/status and /wa/status use.
func handleHomeKitDisabled(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"configured":false,"paired":false,"accessories":[]}`))
}
