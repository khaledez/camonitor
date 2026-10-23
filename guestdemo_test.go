package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testDemoCode = "review-demo-0123456789"

func newDemoAPI(t *testing.T) (*GuestStore, *fakeActions, *GuestAPI) {
	t.Helper()
	store, err := OpenGuestStore(filepath.Join(t.TempDir(), "guests.json"))
	if err != nil {
		t.Fatal(err)
	}
	actions := &fakeActions{}
	api := NewGuestAPI(store, actions, nil)
	api.DemoCode = testDemoCode
	return store, actions, api
}

type enrollReply struct {
	Token   string `json:"token"`
	Label   string `json:"label"`
	Created int64  `json:"created"`
	Demo    bool   `json:"demo"`
}

func enrollWith(t *testing.T, api *GuestAPI, code string) (int, enrollReply) {
	t.Helper()
	rec := do(api.Routes(), "/guest/enroll", `{"code":"`+code+`"}`, "")
	var reply enrollReply
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, reply
}

func TestEnrollDemoCodeIsReusable(t *testing.T) {
	store, _, api := newDemoAPI(t)
	status1, first := enrollWith(t, api, testDemoCode)
	status2, second := enrollWith(t, api, testDemoCode)
	if status1 != 200 || status2 != 200 {
		t.Fatalf("statuses %d %d", status1, status2)
	}
	if first.Token == second.Token {
		t.Fatal("demo enrolls share a token")
	}
	for _, reply := range []enrollReply{first, second} {
		guest, ok := store.Authenticate(reply.Token)
		if !ok || !guest.Demo {
			t.Fatalf("token does not authenticate as demo: %+v", guest)
		}
		if !reply.Demo || reply.Label != "App Review (demo)" {
			t.Fatalf("reply %+v", reply)
		}
	}
}

func TestEnrollDemoCodeExactMatchOnly(t *testing.T) {
	_, _, api := newDemoAPI(t)
	for _, code := range []string{" " + testDemoCode, strings.ToUpper(testDemoCode), testDemoCode[:10]} {
		if status, _ := enrollWith(t, api, code); status != 400 {
			t.Errorf("code %q: got %d", code, status)
		}
	}
}

func TestDemoCodeUnsetNeverMatchesEmpty(t *testing.T) {
	_, _, api := newDemoAPI(t)
	api.DemoCode = ""
	if status, _ := enrollWith(t, api, ""); status != 400 {
		t.Fatalf("got %d", status)
	}
}

func TestDemoGuestDoorForbidden(t *testing.T) {
	_, actions, api := newDemoAPI(t)
	_, reply := enrollWith(t, api, testDemoCode)
	for _, path := range []string{"/guest/gate/open", "/guest/outdoor/open"} {
		if rec := do(api.Routes(), path, "", reply.Token); rec.Code != 403 {
			t.Errorf("%s: got %d", path, rec.Code)
		}
	}
	if actions.gateCalls+actions.outdoorCalls != 0 {
		t.Fatal("opener called for demo guest")
	}
}

func TestRevokedDemoGuestDoorUnauthorized(t *testing.T) {
	store, _, api := newDemoAPI(t)
	_, reply := enrollWith(t, api, testDemoCode)
	guest, _ := store.Authenticate(reply.Token)
	if err := store.Revoke(guest.ID); err != nil {
		t.Fatal(err)
	}
	if rec := do(api.Routes(), "/guest/gate/open", "", reply.Token); rec.Code != 401 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestEnrollResponseFields(t *testing.T) {
	store, _, api := newDemoAPI(t)
	guest, code, _ := store.Create("Ahmed")
	status, reply := enrollWith(t, api, code)
	if status != 200 || reply.Label != "Ahmed" || reply.Demo || reply.Created != guest.Created.Unix() {
		t.Fatalf("%d %+v", status, reply)
	}
}

func TestGuestDemoCodeMinLength(t *testing.T) {
	load := func(guest string) error {
		path := filepath.Join(t.TempDir(), "c.json")
		body := `{"guest": ` + guest + `, "streams": [{"id": "vto1", "host": "192.168.88.200"}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfig(path)
		return err
	}
	const base = `"public_base": "https://m.ts.net:8443"`
	if load(`{`+base+`, "demo_code": "short"}`) == nil {
		t.Error("accepted short demo_code")
	}
	if err := load(`{` + base + `, "demo_code": "` + testDemoCode + `"}`); err != nil {
		t.Errorf("rejected valid demo_code: %v", err)
	}
}
