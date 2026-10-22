package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeHub struct{}

func (fakeHub) Handler(camera string) (http.Handler, bool) {
	if camera != "front" {
		return nil, false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Write([]byte(llhlsPlaylist))
			return
		}
		w.Write([]byte("hub:" + r.URL.Path))
	}), true
}

var testStreamSecret = []byte("test-secret-test-secret-test-secret!")

func newStreamTestAPI(t *testing.T) (*GuestStore, *AuditStore, http.Handler) {
	t.Helper()
	store, err := OpenGuestStore(filepath.Join(t.TempDir(), "guests.json"))
	if err != nil {
		t.Fatal(err)
	}
	audit, err := OpenAuditStore(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewStreamAPI(store, fakeHub{}, testStreamSecret, audit)
	if err != nil {
		t.Fatal(err)
	}
	return store, audit, api.Routes()
}

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestSessionRequiresToken(t *testing.T) {
	_, audit, h := newStreamTestAPI(t)
	if rec := do(h, "/guest/cam/front/session", "", ""); rec.Code != 401 {
		t.Fatalf("got %d", rec.Code)
	}
	if e := onlyEvent(t, audit); e.Action != "cam" || e.Result != "denied" {
		t.Fatalf("bad event %+v", e)
	}
}

func TestSessionRevokedTokenIs401(t *testing.T) {
	store, _, h := newStreamTestAPI(t)
	g, token := enrolledToken(t, store)
	store.Revoke(g.ID)
	if rec := do(h, "/guest/cam/front/session", "", token); rec.Code != 401 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestSessionUnknownCameraIs404(t *testing.T) {
	store, _, h := newStreamTestAPI(t)
	_, token := enrolledToken(t, store)
	if rec := do(h, "/guest/cam/nope/session", "", token); rec.Code != 404 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestSessionGrantsSignedURL(t *testing.T) {
	store, audit, h := newStreamTestAPI(t)
	g, token := enrolledToken(t, store)
	rec := do(h, "/guest/cam/front/session", "", token)
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	var resp struct {
		URL     string `json:"url"`
		Expires int64  `json:"expires"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(resp.URL)
	if err != nil || u.Path != "/guest/cam/front/hls/index.m3u8" {
		t.Fatalf("bad url %q", resp.URL)
	}
	if !verifyStream(testStreamSecret, "front", u.Query().Get("s"), time.Now()) {
		t.Fatal("signature not accepted")
	}
	if resp.Expires <= time.Now().Unix() {
		t.Fatalf("expires not in future: %d", resp.Expires)
	}
	if e := onlyEvent(t, audit); e.Action != "cam" || e.Result != "success" || e.GuestID != g.ID {
		t.Fatalf("bad event %+v", e)
	}
	// the returned URL works with no Authorization header
	if rec := get(h, resp.URL); rec.Code != 200 || !strings.Contains(rec.Body.String(), "seg7.mp4?s=") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestHLSSignatureChecks(t *testing.T) {
	_, _, h := newStreamTestAPI(t)
	good := signStream(testStreamSecret, "front", time.Now().Add(time.Minute))
	expired := signStream(testStreamSecret, "front", time.Now().Add(-time.Minute))
	cases := []struct {
		target string
		want   int
	}{
		{"/guest/cam/front/hls/seg1.mp4?s=" + good, 200},
		{"/guest/cam/front/hls/seg1.mp4?s=bad", 403},
		{"/guest/cam/front/hls/seg1.mp4", 403},
		{"/guest/cam/front/hls/seg1.mp4?s=" + expired, 403},
		{"/guest/cam/nope/hls/index.m3u8?s=" + signStream(testStreamSecret, "nope", time.Now().Add(time.Minute)), 404},
	}
	for _, c := range cases {
		if rec := get(h, c.target); rec.Code != c.want {
			t.Errorf("%s: got %d want %d", c.target, rec.Code, c.want)
		}
	}
}

const llhlsPlaylist = `#EXTM3U
#EXT-X-MAP:URI="init.mp4"
#EXT-X-PART:DURATION=0.5,URI="part1.mp4?x=1"
#EXTINF:2.0,
seg7.mp4
#EXT-X-PRELOAD-HINT:TYPE=PART,URI="part2.mp4"
`

func TestPlaylistChildURIsAreSigned(t *testing.T) {
	_, _, h := newStreamTestAPI(t)
	sig := signStream(testStreamSecret, "front", time.Now().Add(time.Minute))
	rec := get(h, "/guest/cam/front/hls/index.m3u8?s="+sig)
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`URI="init.mp4?s=`, `URI="part1.mp4?x=1&s=`, "\nseg7.mp4?s=", `URI="part2.mp4?s=`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	seg := "/guest/cam/front/hls/seg7.mp4?s=" + sig
	if rec := get(h, seg); rec.Code != 200 {
		t.Fatalf("signed segment got %d", rec.Code)
	}
	if rec := get(h, "/guest/cam/front/hls/seg7.mp4"); rec.Code != 403 {
		t.Fatalf("unsigned segment got %d", rec.Code)
	}
}

func TestShortSecretRejected(t *testing.T) {
	if _, err := NewStreamAPI(nil, fakeHub{}, []byte("short"), nil); err == nil {
		t.Fatal("short secret accepted")
	}
}
