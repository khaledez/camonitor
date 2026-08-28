package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeGreeKey is the 16-byte AES key a fake unit hands out at bind time.
const fakeGreeKey = "testkeytestkey12"

// fakeGreeUnit is a minimal in-process Gree unit: it answers scan with a
// generic-key pack, bind with a device key, and status with a fixed set
// of columns. It exists so discovery can be exercised over real UDP
// sockets without an air conditioner on the LAN.
type fakeGreeUnit struct {
	cid  string
	addr string // "127.0.0.1:<port>"
	conn *net.UDPConn
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
