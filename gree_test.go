package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeGreeKey is the 16-byte AES key a fake unit hands out at bind time.
const fakeGreeKey = "testkeytestkey12"

// fakeGreeUnit is a minimal in-process Gree unit: it answers scan with a
// generic-key pack, bind with a device key, and status with a fixed set
// of columns. It exists so discovery can be exercised over real UDP
// sockets without an air conditioner on the LAN.
//
// When broadcastOnly is set the unit binds 0.0.0.0 (so it also sees
// broadcast traffic) and ignores the first bind request it receives. That
// mimics a Gree firmware that never answers a unicast bind: the client's
// unicast attempt times out, its broadcast fallback is the second bind
// and succeeds. The unit still answers every scan and everything after
// the bind.
type fakeGreeUnit struct {
	cid           string
	addr          string // "127.0.0.1:<port>" (or "0.0.0.0:<port>" when broadcastOnly)
	conn          *net.UDPConn
	broadcastOnly bool
	bindsSeen     int
}

func startFakeGreeUnit(t *testing.T, cid string) *fakeGreeUnit {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeGreeUnit{cid: cid, addr: conn.LocalAddr().String(), conn: conn}
	t.Cleanup(f.close)
	go f.serve()
	return f
}

// startBroadcastOnlyGreeUnit starts a unit that only answers a bind sent
// to the broadcast address (see fakeGreeUnit.broadcastOnly). It binds
// 0.0.0.0 so it also sees broadcast traffic on the returned port.
func startBroadcastOnlyGreeUnit(t *testing.T, cid string) *fakeGreeUnit {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeGreeUnit{cid: cid, addr: conn.LocalAddr().String(), conn: conn, broadcastOnly: true}
	t.Cleanup(f.close)
	go f.serve()
	return f
}

func (f *fakeGreeUnit) close() { f.conn.Close() }

func (f *fakeGreeUnit) serve() {
	buf := make([]byte, 65535)
	for {
		n, src, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if reply := f.replyTo(buf[:n]); reply != nil {
			f.conn.WriteToUDP(reply, src)
		}
	}
}

func (f *fakeGreeUnit) replyTo(req []byte) []byte {
	var outer map[string]any
	if err := json.Unmarshal(req, &outer); err != nil {
		return nil
	}
	if outer["t"] == "scan" {
		return f.pack(map[string]any{"t": "dev", "cid": f.cid, "mac": f.cid}, greeGenericKey)
	}
	pack, _ := outer["pack"].(string)

	// Bind arrives encrypted with the generic key, everything after it
	// with the key this unit handed out.
	if inner := unpack(pack, greeGenericKey); inner["t"] == "bind" {
		if f.broadcastOnly {
			f.bindsSeen++
			if f.bindsSeen == 1 {
				// First bind is the client's unicast attempt; a
				// broadcast-only unit never answers that.
				return nil
			}
		}
		return f.pack(map[string]any{"t": "bindok", "mac": f.cid, "key": fakeGreeKey}, greeGenericKey)
	}
	inner := unpack(pack, fakeGreeKey)
	switch inner["t"] {
	case "status":
		vals := map[string]int{"Pow": 1, "Mod": greeModeCool, "SetTem": 22, "WdSpd": 3, "TemSen": 66}
		dat := make([]int, len(statusColumns))
		for i, col := range statusColumns {
			dat[i] = vals[col]
		}
		return f.pack(map[string]any{"t": "dat", "cols": statusColumns, "dat": dat}, fakeGreeKey)
	case "cmd":
		return f.pack(map[string]any{"t": "res", "r": 200}, fakeGreeKey)
	}
	return nil
}

func (f *fakeGreeUnit) pack(inner map[string]any, key string) []byte {
	plain, _ := json.Marshal(inner)
	enc, _ := greeEncrypt(plain, []byte(key))
	out, _ := json.Marshal(map[string]any{"t": "pack", "i": 1, "uid": 0, "cid": f.cid, "pack": enc})
	return out
}

// unpack decrypts a pack, returning nil when key is the wrong one (ECB
// decryption with a bad key yields garbage rather than an error).
func unpack(pack, key string) map[string]any {
	plain, err := greeDecrypt(pack, []byte(key))
	if err != nil {
		return nil
	}
	var inner map[string]any
	if err := json.Unmarshal(plain, &inner); err != nil {
		return nil
	}
	return inner
}

func TestGreeDiscoversTheUnitOwningTheConfiguredMAC(t *testing.T) {
	other := startFakeGreeUnit(t, "aaaaaaaaaaaa")
	ac := startFakeGreeUnit(t, "502cc67b52eb")

	// Colons and case in config must not matter; the wire cid is bare hex.
	c := newGreeClient(GreeConfig{MAC: "50:2C:C6:7B:52:EB"})
	c.scanTargets = []string{other.addr, ac.addr}

	st := c.Status(context.Background())

	if !st.Online {
		t.Fatal("unit reported offline, want a status from the matching unit")
	}
	if st.SetTemp != 22 || st.RoomTemp != 26 {
		t.Errorf("got set %d room %d, want set 22 room 26", st.SetTemp, st.RoomTemp)
	}
	if got := c.addr.String(); got != ac.addr {
		t.Errorf("bound to %s, want the unit owning the MAC at %s", got, ac.addr)
	}
}

func TestGreeIgnoresAUnitWithADifferentMAC(t *testing.T) {
	other := startFakeGreeUnit(t, "aaaaaaaaaaaa")

	c := newGreeClient(GreeConfig{MAC: "502cc67b52eb"})
	c.scanTargets = []string{other.addr}

	if st := c.Status(context.Background()); st.Online {
		t.Fatal("bound to a unit whose MAC does not match the config")
	}
}

func TestGreeRediscoversTheUnitAfterItsAddressChanges(t *testing.T) {
	const cid = "502cc67b52eb"
	old := startFakeGreeUnit(t, cid)

	c := newGreeClient(GreeConfig{MAC: cid})
	c.scanTargets = []string{old.addr}
	if st := c.Status(context.Background()); !st.Online {
		t.Fatal("first status failed, want the unit online at its original address")
	}

	// A new DHCP lease: the old address goes dark and the unit starts
	// answering from a new one. A broadcast scan reaches both.
	old.close()
	moved := startFakeGreeUnit(t, cid)
	c.scanTargets = []string{old.addr, moved.addr}

	if st := c.Status(context.Background()); st.Online {
		t.Fatal("status succeeded against the dead address, want a failure that clears the learned address")
	}
	st := c.Status(context.Background())

	if !st.Online {
		t.Fatal("unit still offline, want it re-discovered at its new address")
	}
	if got := c.addr.String(); got != moved.addr {
		t.Errorf("bound to %s, want the new address %s", got, moved.addr)
	}
}

func TestGreeAcceptsAnyUnitWhenNoMACIsConfigured(t *testing.T) {
	ac := startFakeGreeUnit(t, "502cc67b52eb")
	host, port, err := net.SplitHostPort(ac.addr)
	if err != nil {
		t.Fatalf("split %s: %v", ac.addr, err)
	}

	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse port %q: %v", port, err)
	}
	c := newGreeClient(GreeConfig{Host: host, Port: portNum})

	if st := c.Status(context.Background()); !st.Online {
		t.Fatal("unit reported offline, want a host-only config to work as before")
	}
}

// broadcastReachable reports whether a UDP broadcast to the given port is
// delivered on this host. The broadcast-only test needs real broadcast
// delivery (a fake unit bound to 0.0.0.0 must hear a scan sent to
// 255.255.255.255), which some CI containers lack; it skips rather than
// fails there.
func broadcastReachable(t *testing.T, port int) bool {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := conn.WriteToUDP([]byte(`{"t":"scan"}`), &net.UDPAddr{IP: net.IPv4(255, 255, 255, 255), Port: port}); err != nil {
		return false
	}
	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return false
		}
		if bytes.Contains(buf[:n], []byte(`"pack"`)) {
			return true
		}
	}
}

// TestGreeFallsBackToBroadcastForBroadcastOnlyUnit exercises the unit
// whose firmware only answers a bind sent to the broadcast address: the
// unicast bind must time out, the broadcast fallback must succeed, and
// the client must remember to keep sending status/commands to broadcast.
func TestGreeFallsBackToBroadcastForBroadcastOnlyUnit(t *testing.T) {
	// A cid that does not collide with the real unit on this LAN, so the
	// broadcast scan can only match the fake.
	const cid = "aabbccddeeff"

	// No host configured, so discovery scans the LAN broadcast and the
	// fake (bound 0.0.0.0) sees it. The client must use the fake's port.
	ac := startBroadcastOnlyGreeUnit(t, cid)
	_, portStr, err := net.SplitHostPort(ac.addr)
	if err != nil {
		t.Fatalf("split %s: %v", ac.addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	if !broadcastReachable(t, port) {
		t.Skip("host cannot deliver UDP broadcast; skipping broadcast-only unit test")
	}
	c := newGreeClient(GreeConfig{MAC: cid, Port: port})

	st := c.Status(context.Background())
	if !st.Online {
		t.Fatal("broadcast-only unit reported offline, want the broadcast fallback to bind and read status")
	}
	if !c.broadcastOnly {
		t.Fatal("broadcastOnly not set, want the client to remember the unit only answers broadcast")
	}
	if st.SetTemp != 22 || st.RoomTemp != 26 {
		t.Errorf("got set %d room %d, want set 22 room 26", st.SetTemp, st.RoomTemp)
	}

	// A second poll must keep working through the broadcast path (the
	// bind is already done, so this exercises request()'s routing).
	if st := c.Status(context.Background()); !st.Online {
		t.Fatal("second status failed, want broadcast routing to persist")
	}
}

// TestWorkCtxKeepsCancellationDropsDeadline pins down workCtx's contract:
// the caller's deadline must not leak into the client's work (the client
// bounds each protocol step itself), and a deadline expiry must not cancel
// the work — only an explicit cancel does.
func TestWorkCtxKeepsCancellationDropsDeadline(t *testing.T) {
	// Explicit cancellation propagates.
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := workCtx(parent)
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("workCtx must drop the parent's deadline")
	}
	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("workCtx must propagate the parent's cancellation")
	}
	cancel()

	// A deadline expiry must NOT cancel the work context.
	parent, cancelParent = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelParent()
	ctx, cancel = workCtx(parent)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("workCtx must not propagate a deadline expiry")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestGreeBroadcastOnlyUnitSurvivesCallerDeadline is the regression test
// for the "still unreachable after deploy" bug: the poll loop hands Status
// a short deadline (greeStatusTimeout), and a broadcast-only handshake
// (scan + 4s unicast timeout + broadcast bind) outlives it. A deadline
// expiry must not cancel the handshake, or the unit never comes online.
func TestGreeBroadcastOnlyUnitSurvivesCallerDeadline(t *testing.T) {
	const cid = "aabbccddeeff"
	ac := startBroadcastOnlyGreeUnit(t, cid)
	_, portStr, err := net.SplitHostPort(ac.addr)
	if err != nil {
		t.Fatalf("split %s: %v", ac.addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	if !broadcastReachable(t, port) {
		t.Skip("host cannot deliver UDP broadcast; skipping broadcast-only unit test")
	}
	c := newGreeClient(GreeConfig{MAC: cid, Port: port})

	// A deadline far shorter than the handshake, mirroring the poll
	// loop's greeStatusTimeout. The handshake must still complete.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if st := c.Status(ctx); !st.Online {
		t.Fatal("broadcast-only unit reported offline under a short caller deadline; the deadline must not cancel the handshake")
	}
}

func TestLoadConfigRejectsGreeWithoutHostOrMAC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
	  "gree": {"name": "Living Room AC"},
	  "streams": [{"id": "vto1", "host": "192.168.88.200"}]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := loadConfig(path); err == nil {
		t.Fatal("accepted a gree block with neither host nor mac, want an error")
	}
}
