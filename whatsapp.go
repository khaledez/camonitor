// WhatsApp client wrapper around whatsmeow. The wrapper hides the library's
// shape — callers just need to know "SendImage to all recipients" — and
// drives first-time pairing via a QR code surfaced two ways:
//
//   - As a PNG on the web UI at GET /wa/qr.png, plus status JSON at
//     GET /wa/status, so the user can pair without leaving the browser.
//   - As a half-block QR on stdout (visible via `docker logs`), as a
//     terminal-only fallback for headless setups.
//
// Storage uses modernc.org/sqlite (pure Go, cgo-free) registered under the
// driver name "sqlite3" so whatsmeow's sqlstore (which keys both its Go SQL
// driver lookup and its SQL-dialect branch on the same string) picks it up.
//
// First boot: client.Store.ID is nil → we open a QR channel, expose each
// new code via both surfaces, and finalise pairing when the user scans
// from WhatsApp → Linked Devices. The session is persisted to the SQLite
// file so subsequent restarts skip pairing.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"rsc.io/qr"

	// Pure-Go SQLite driver; the alias-import below re-registers it as
	// "sqlite3" so whatsmeow's sqlstore picks it without cgo.
	_ "modernc.org/sqlite"
	sqlite "modernc.org/sqlite"
)

func init() {
	// Register modernc's driver under whatsmeow's expected name. Calling
	// Register a second time with the same name would panic, so guard
	// against accidental double-registration if another file imports a
	// "sqlite3" driver in the future.
	if !driverRegistered("sqlite3") {
		sql.Register("sqlite3", &sqlite.Driver{})
	}
}

func driverRegistered(name string) bool {
	for _, n := range sql.Drivers() {
		if n == name {
			return true
		}
	}
	return false
}

type WhatsAppConfig struct {
	Session    string   `json:"session"`
	Recipients []string `json:"recipients"`
	// IntroMessage is the text broadcast to every recipient on each
	// successful pairing (initial scan and re-pair). Empty falls back to
	// a default Arabic announcement, so the recipients know the new
	// sender phone is the doorbell.
	IntroMessage string `json:"intro_message,omitempty"`
}

// defaultIntroMessage is the announcement sent to every recipient on
// each successful pairing. Plain-text Arabic: "Peace be upon you! From
// now on, doorbell notifications will arrive from this number."
const defaultIntroMessage = "السلام عليكم! من الآن فصاعداً، ستصلكم إشعارات جرس الباب من هذا الرقم 🔔"

// WhatsAppClient wraps a whatsmeow client + the recipient JIDs we'll send
// to. SendImage is safe to call before pairing completes — it returns an
// error explaining that the user needs to scan the QR.
type WhatsAppClient struct {
	// container outlives any specific whatsmeow.Client; re-pair creates
	// a fresh Device from this container after the previous device is
	// logged out (whatsmeow marks the old Device as "deleted" and won't
	// let it be reused).
	container     *sqlstore.Container
	rawRecipients []string // as configured, for display
	recipients    []types.JID
	introMessage  string
	clientLog     waLog.Logger
	ready         atomic.Bool

	mu     sync.Mutex
	client *whatsmeow.Client // replaced on Repair; always non-nil after construction
	qrCode string            // empty once paired; populated on each rotation
	qrAt   time.Time         // when the current QR was issued
	qrPNG  []byte            // pre-rendered PNG of qrCode, regenerated on rotation
}

// currentClient returns the live whatsmeow client under mu. Use this
// instead of touching w.client directly: Repair replaces the pointer
// when re-pairing.
func (w *WhatsAppClient) currentClient() *whatsmeow.Client {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.client
}

// WhatsAppStatus is the JSON returned by GET /wa/status.
type WhatsAppStatus struct {
	Configured  bool      `json:"configured"`
	Paired      bool      `json:"paired"`
	JID         string    `json:"jid,omitempty"`
	Recipients  []string  `json:"recipients"`
	HasQR       bool      `json:"hasQR"`
	QRIssuedAt  time.Time `json:"qrIssuedAt,omitempty"`
}

// NewWhatsAppClient opens (or creates) the session DB, builds a whatsmeow
// client, and either resumes the persisted session or starts the QR-code
// pairing flow on a background goroutine. The returned client is usable
// immediately; SendImage will fail with a clear error until pairing
// completes on first run.
func NewWhatsAppClient(ctx context.Context, cfg WhatsAppConfig) (*WhatsAppClient, error) {
	if cfg.Session == "" {
		return nil, errors.New("whatsapp.session path is required")
	}
	if len(cfg.Recipients) == 0 {
		return nil, errors.New("whatsapp.recipients must list at least one number")
	}

	recipients := make([]types.JID, 0, len(cfg.Recipients))
	for _, r := range cfg.Recipients {
		jid, err := parseRecipient(r)
		if err != nil {
			return nil, fmt.Errorf("recipient %q: %w", r, err)
		}
		recipients = append(recipients, jid)
	}

	// _pragma=foreign_keys(1) matches what whatsmeow's docs recommend so
	// device-store cascade deletes behave correctly.
	dsn := "file:" + cfg.Session + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open session db: %w", err)
	}
	// SQLite isn't happy with many concurrent writers; one is plenty for
	// our workload (the whatsmeow goroutines serialise themselves anyway).
	db.SetMaxOpenConns(1)

	dbLog := waLog.Stdout("WA-DB", "WARN", true)
	container := sqlstore.NewWithDB(db, "sqlite3", dbLog)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade session schema: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("load device: %w", err)
	}

	clientLog := waLog.Stdout("WA", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	introMsg := cfg.IntroMessage
	if introMsg == "" {
		introMsg = defaultIntroMessage
	}
	wa := &WhatsAppClient{
		container:     container,
		client:        client,
		clientLog:     clientLog,
		rawRecipients: append([]string(nil), cfg.Recipients...),
		recipients:    recipients,
		introMessage:  introMsg,
	}

	if client.Store.ID == nil {
		// First boot — pair via QR. Connect first so whatsmeow generates
		// the QR codes; we print each new code to stdout until we're paired
		// (or the user scans a previous code, which makes whatsmeow rotate).
		qrChan, err := client.GetQRChannel(ctx)
		if err != nil {
			return nil, fmt.Errorf("qr channel: %w", err)
		}
		if err := client.Connect(); err != nil {
			return nil, fmt.Errorf("whatsapp connect: %w", err)
		}
		go wa.watchQR(qrChan)
	} else {
		if err := client.Connect(); err != nil {
			return nil, fmt.Errorf("whatsapp connect: %w", err)
		}
		wa.ready.Store(true)
		log.Printf("whatsapp: resumed session as %s", client.Store.ID.String())
	}

	return wa, nil
}

func (w *WhatsAppClient) watchQR(qrChan <-chan whatsmeow.QRChannelItem) {
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			log.Println("whatsapp: scan this QR from WhatsApp → Linked Devices (or open the web UI)")
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			w.setQR(evt.Code)
		case "success":
			log.Println("whatsapp: pairing successful")
			w.clearQR()
			w.ready.Store(true)
			// Announce the (possibly new) sender to recipients. Done on
			// a fresh goroutine so we can return from watchQR; the actual
			// send is bounded by its own context timeout.
			go w.sendIntro()
			return
		case "timeout":
			log.Println("whatsapp: pairing timed out — restart camonitor to try again")
			w.clearQR()
			return
		default:
			log.Printf("whatsapp: pairing event %q", evt.Event)
		}
	}
}

func (w *WhatsAppClient) setQR(code string) {
	// Pre-render the PNG once per rotation so /wa/qr.png is a cheap memcpy
	// rather than re-encoding on every browser refresh.
	png := renderQRPNG(code)
	w.mu.Lock()
	w.qrCode = code
	w.qrAt = time.Now()
	w.qrPNG = png
	w.mu.Unlock()
}

func (w *WhatsAppClient) clearQR() {
	w.mu.Lock()
	w.qrCode = ""
	w.qrPNG = nil
	w.qrAt = time.Time{}
	w.mu.Unlock()
}

// renderQRPNG encodes the pairing code as a QR PNG. Error-correction level
// M is plenty for a short pairing string and keeps the matrix compact; the
// browser scales it up via CSS image-rendering: pixelated.
func renderQRPNG(text string) []byte {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		log.Printf("whatsapp: qr encode failed: %v", err)
		return nil
	}
	return code.PNG()
}

// Status snapshots the current pairing state for the HTTP handler.
func (w *WhatsAppClient) Status() WhatsAppStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := WhatsAppStatus{
		Configured: true,
		Paired:     w.ready.Load(),
		Recipients: w.rawRecipients,
		HasQR:      len(w.qrPNG) > 0,
		QRIssuedAt: w.qrAt,
	}
	// w.client is also protected by w.mu.
	if w.client != nil && w.client.Store.ID != nil {
		st.JID = w.client.Store.ID.String()
	}
	return st
}

// HandleStatus serves GET /wa/status. Always 200 — the body's `configured`
// field tells the browser whether WhatsApp is even in use.
func (w *WhatsAppClient) HandleStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(rw).Encode(w.Status())
}

// HandleQR serves GET /wa/qr.png. Returns 404 once pairing has completed
// so the browser knows to stop showing the QR panel.
func (w *WhatsAppClient) HandleQR(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	png := w.qrPNG
	w.mu.Unlock()
	if len(png) == 0 {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "image/png")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("Content-Length", fmt.Sprintf("%d", len(png)))
	_, _ = rw.Write(png)
}

// sendIntro broadcasts introMessage to every recipient. Called on every
// successful pairing so the recipients know the (possibly new) sender
// account is the doorbell. Errors are logged per recipient and do not
// propagate; the bell pipeline keeps working even if one phone is offline.
func (w *WhatsAppClient) sendIntro() {
	if w.introMessage == "" {
		return
	}
	client := w.currentClient()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// whatsmeow's QR "success" fires before the websocket has reconnected
	// as the newly paired device — wait for the TCP + LOGIN to land first.
	if err := waitClientReady(ctx, client); err != nil {
		log.Printf("whatsapp: intro skipped: %v", err)
		return
	}
	// IsLoggedIn alone is not enough: whatsmeow still has to fetch the
	// recipients' device lists and establish per-device Signal sessions
	// before encryption succeeds. Sending too soon makes the first send
	// drop the recipient's primary phone (":0" device) with a
	// "no signal session established" warning, so the message never lands
	// on the actual phone. AppStateSyncComplete fires after the post-
	// pairing app-state pull and is a reliable "now I can encrypt" gate.
	if err := waitAppStateSync(ctx, client, 15*time.Second); err != nil {
		log.Printf("whatsapp: app-state sync wait: %v (sending intro anyway)", err)
	}

	for _, to := range w.recipients {
		msg := &waProto.Message{Conversation: proto.String(w.introMessage)}
		if _, err := client.SendMessage(ctx, to, msg); err != nil {
			log.Printf("whatsapp: intro send to +%s failed: %v", to.User, err)
			continue
		}
		log.Printf("whatsapp: intro sent to +%s", to.User)
	}
}

// waitAppStateSync blocks until whatsmeow emits its first
// AppStateSyncComplete event, ctx expires, or the optional fallback
// duration elapses (whichever comes first). The fallback exists so a
// re-pair onto an already-warm session (no app-state sync triggered)
// still proceeds to send rather than hanging until ctx times out.
// Returns nil only when the event arrived in time.
func waitAppStateSync(ctx context.Context, c *whatsmeow.Client, fallback time.Duration) error {
	synced := make(chan struct{})
	var once sync.Once
	handlerID := c.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.AppStateSyncComplete); ok {
			once.Do(func() { close(synced) })
		}
	})
	defer c.RemoveEventHandler(handlerID)

	var fallbackCh <-chan time.Time
	if fallback > 0 {
		t := time.NewTimer(fallback)
		defer t.Stop()
		fallbackCh = t.C
	}
	select {
	case <-synced:
		return nil
	case <-fallbackCh:
		return fmt.Errorf("AppStateSyncComplete not received within %s", fallback)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitClientReady blocks until the whatsmeow client is connected AND
// logged in, or ctx is cancelled. Returns ctx.Err() on timeout. Uses a
// small polling interval because whatsmeow doesn't expose a single
// "ready" channel/event covering both states.
func waitClientReady(ctx context.Context, c *whatsmeow.Client) error {
	if c.IsConnected() && c.IsLoggedIn() {
		return nil
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if c.IsConnected() && c.IsLoggedIn() {
				return nil
			}
		}
	}
}

// SendImage uploads the JPEG once and sends it (with caption) to every
// configured recipient. We deliberately don't fan out concurrently —
// whatsmeow's send path holds locks, so parallel sends just queue up.
func (w *WhatsAppClient) SendImage(ctx context.Context, jpeg []byte, caption string) error {
	if !w.ready.Load() {
		return errors.New("whatsapp not paired yet (open the web UI and scan the QR, or check `docker logs`)")
	}
	if len(jpeg) == 0 {
		return errors.New("empty image")
	}
	client := w.currentClient()
	if client == nil {
		return errors.New("whatsapp client not initialized")
	}

	uploaded, err := client.Upload(ctx, jpeg, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	msg := &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			Caption:       proto.String(caption),
			Mimetype:      proto.String("image/jpeg"),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(jpeg))),
		},
	}

	var errs []string
	for _, to := range w.recipients {
		if _, err := client.SendMessage(ctx, to, msg); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", to.User, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("send to some recipients failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Close disconnects the whatsmeow client. The session DB stays on disk so
// the next run can resume without re-pairing.
func (w *WhatsAppClient) Close() {
	if c := w.currentClient(); c != nil {
		c.Disconnect()
	}
}

// Repair unlinks the currently paired WhatsApp account and starts a fresh
// pairing flow. After this returns successfully, the web UI will start
// surfacing a new QR via /wa/qr.png; the user scans it from a (possibly
// different) phone's WhatsApp → Linked Devices to relink. The recipient
// list is unaffected — it's controlled by config.json.
//
// Implementation note: whatsmeow's Logout marks the underlying Device as
// "deleted" and refuses to reuse it for a subsequent Connect. So we
// allocate a fresh Device on the same SQL container and swap the active
// client pointer under mu; concurrent readers (SendImage, HandleStatus)
// fetch the live client via currentClient().
func (w *WhatsAppClient) Repair(ctx context.Context) error {
	old := w.currentClient()
	if old == nil {
		return errors.New("whatsapp client not initialized")
	}

	// Best-effort server-side logout. If the network's flaky or we're
	// already unpaired, fall through to local cleanup so the QR refresh
	// still happens.
	if old.Store.ID != nil {
		if err := old.Logout(ctx); err != nil {
			log.Printf("whatsapp: logout error: %v (continuing)", err)
			old.Disconnect()
		}
	} else {
		old.Disconnect()
	}
	w.ready.Store(false)
	w.clearQR()

	// Fresh Device from the same container. NewDevice is in-memory until
	// the first pairing-success write hits the SQL store.
	newDevice := w.container.NewDevice()
	newClient := whatsmeow.NewClient(newDevice, w.clientLog)

	qrChan, err := newClient.GetQRChannel(context.Background())
	if err != nil {
		return fmt.Errorf("qr channel: %w", err)
	}
	if err := newClient.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	w.mu.Lock()
	w.client = newClient
	w.mu.Unlock()

	go w.watchQR(qrChan)
	return nil
}

// HandleUnpair serves POST /wa/unpair. Triggers a re-pair flow; the
// browser's existing /wa/status polling picks up the new QR within a
// few seconds.
func (w *WhatsAppClient) HandleUnpair(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := w.Repair(ctx); err != nil {
		log.Printf("whatsapp: repair failed: %v", err)
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("whatsapp: unpaired; awaiting fresh QR scan")
	rw.WriteHeader(http.StatusNoContent)
}

// parseRecipient turns "+15551234567" (or "15551234567") into the matching
// WhatsApp user JID. WhatsApp identifies users by their phone number in
// international format with no '+' and no separators.
func parseRecipient(raw string) (types.JID, error) {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, raw)
	if digits == "" {
		return types.JID{}, errors.New("no digits in number")
	}
	return types.JID{User: digits, Server: types.DefaultUserServer}, nil
}
