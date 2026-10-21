package main

import (
	"encoding/hex"
	"testing"
	"time"
)

// Vectors generated independently from the protocol description
// (scratchpad/gatetest/vectors.py). phone=+27831234567, mac4=acebe625,
// cmd05 time = 2026-10-21 14:30:45 (a Wednesday).
func TestCentsysPackets(t *testing.T) {
	mac4, _ := hex.DecodeString("acebe625")

	if got := hex.EncodeToString(centsysKey(mac4)); got != "9473d99f4dbfab9c" {
		t.Fatalf("key = %s", got)
	}

	cases := []struct {
		name, want string
		got        []byte
	}{
		{"cmd01", "010101009473d99f4dbf8b1b87418de9", buildCmd01("+27831234567", mac4)},
		{"cmd05", "01010500b96dd79d58b6b19c",
			buildCmd05(mac4, time.Date(2026, 10, 21, 14, 30, 45, 0, time.UTC))},
		{"cmd03_prefix", "010103009473fb9f", buildCmd03Prefix(mac4, 0)},
	}
	for _, c := range cases {
		if got := hex.EncodeToString(c.got); got != c.want {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}

	// cmd 04 "OK" response decodes to (code=OK, cv=0).
	cmd04, _ := hex.DecodeString("010104009472")
	code, cv, ok := decodeCmd04(mac4, cmd04)
	if !ok || code != activationOK || cv != 0 {
		t.Errorf("decodeCmd04 = (%d,%d,%v), want (1,0,true)", code, cv, ok)
	}
}

func TestDecodeOverview(t *testing.T) {
	// Slider frame: 4-byte header + 36-byte body, status byte at body[22].
	frame := make([]byte, 4+36)
	frame[4+22] = 1 // closed
	if got := decodeOverview(frame); got != gateClosed {
		t.Errorf("slider closed = %d", got)
	}
	frame[4+22] = 4 // opening
	if got := decodeOverview(frame); got != gateOpening {
		t.Errorf("slider opening = %d", got)
	}

	// Garage frame: 24-byte body, status byte at body[12].
	g := make([]byte, 4+24)
	g[4+12] = 3 // garage "closed"
	if got := decodeOverview(g); got != gateClosed {
		t.Errorf("garage closed = %d", got)
	}

	if got := decodeOverview([]byte{1, 2, 3}); got != gateUnknown {
		t.Errorf("short frame = %d, want unknown", got)
	}
}
