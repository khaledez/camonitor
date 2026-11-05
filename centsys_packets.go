// Centsys SMART Wi-Fi trigger-packet construction and telemetry decode.
//
// SMART operators are driven over the Centurion cloud MQTT broker. The
// command packets are a 4-byte header plus a body obfuscated (not
// encrypted) with a key derived from the operator's MAC. This is a direct
// port of the protocol the MyCentsys app uses; byte-for-byte vectors are
// pinned in centsys_packets_test.go.
package main

import (
	"encoding/binary"
	"time"
)

// Activation ids (cmd 03). A sliding/swing gate opens with TRG. The garage
// RUN id (1) must never be sent to a gate — the operator reads id 1 as
// Holiday Lock and would lock itself out — so camonitor only ever sends TRG.
const activationTRG = 34

var centsysBaseKey = []byte{0x38, 0x98, 0x3f, 0xba, 0x4d, 0xbf, 0xab, 0x9c}

// centsysKey derives the 8-byte obfuscation key from the first 4 MAC bytes.
func centsysKey(mac4 []byte) []byte {
	k := make([]byte, 8)
	for i := 0; i < 8; i++ {
		k[i] = centsysBaseKey[i]
		if i < 4 {
			k[i] ^= mac4[i]
		}
	}
	return k
}

func centsysXOR(data, key []byte) []byte {
	out := make([]byte, len(data))
	for i := range data {
		out[i] = data[i] ^ key[i%len(key)]
	}
	return out
}

func centsysHeader(packetType byte) []byte {
	return []byte{1, 1, packetType, 0}
}

// phoneParts packs the phone number's decimal digits as BCD into three
// uint32 words, rightmost 8 digits in `upper`, exactly as the app does.
func phoneParts(phone string) (lower, mid, upper uint32) {
	s := ""
	for _, r := range phone {
		if r != '+' {
			s += string(r)
		}
	}
	n := len(s)
	digit := func(idx int) uint32 { return uint32(s[idx] - '0') }
	for i := 16; i < 24; i++ {
		if i < n {
			lower |= digit(n-i-1) << uint(((23-i)*4)&31)
		}
	}
	for i := 8; i < 16; i++ {
		if i < n {
			mid |= digit(n-i-1) << uint(((15-i)*4)&31)
		}
	}
	for i := 0; i < 8; i++ {
		if i < n {
			upper |= digit(n-i-1) << uint(((7-i)*4)&31)
		}
	}
	return
}

// buildCmd01 is the identity packet carrying the account phone number.
func buildCmd01(phone string, mac4 []byte) []byte {
	lo, mid, up := phoneParts(phone)
	body := make([]byte, 12)
	binary.LittleEndian.PutUint32(body[0:], lo)
	binary.LittleEndian.PutUint32(body[4:], mid)
	binary.LittleEndian.PutUint32(body[8:], up)
	return append(centsysHeader(1), centsysXOR(body, centsysKey(mac4))...)
}

// buildCmd05 is the time-sync packet (local date/time).
func buildCmd05(mac4 []byte, t time.Time) []byte {
	// Python's weekday() is Mon=0..Sun=6; Go's is Sun=0..Sat=6.
	pyWeekday := (int(t.Weekday()) + 6) % 7
	body := []byte{
		byte(t.Second()), byte(t.Minute()), byte(t.Hour()), byte(pyWeekday),
		byte(t.Day()), byte(int(t.Month()) - 1), byte(t.Year() - 2000), 0,
	}
	return append(centsysHeader(5), centsysXOR(body, centsysKey(mac4))...)
}

// buildCmd03Prefix is the activation packet without the live challenge; the
// caller appends the 4-byte challenge from the cmd 02 response.
func buildCmd03Prefix(mac4 []byte, configVersion int) []byte {
	body := []byte{byte(configVersion), 0, 0, 0}
	binary.LittleEndian.PutUint16(body[2:], uint16(activationTRG))
	return append(centsysHeader(3), centsysXOR(body, centsysKey(mac4))...)
}

// Activation response codes in cmd 04.
const (
	activationOK                    = 1
	activationConfigurationMismatch = 7
)

// decodeCmd04 returns (responseCode, configVersion) from a cmd 04 payload.
// On a config mismatch the gate reports the version it expects.
func decodeCmd04(mac4, payload []byte) (code, configVersion int, ok bool) {
	if len(payload) < 6 {
		return 0, 0, false
	}
	body := centsysXOR(payload[4:], centsysKey(mac4))
	return int(body[1]), int(body[0]), true
}

// gateStatus is the operator's own open/closed state from a deviceOverview
// message. Values match HomeKit's garage-door CurrentDoorState so the
// accessory maps without a second table.
type gateStatus int

const (
	gateOpen    gateStatus = 0
	gateClosed  gateStatus = 1
	gateOpening gateStatus = 2
	gateClosing gateStatus = 3
	gateStopped gateStatus = 4 // partly open/closed
	gateUnknown gateStatus = -1
)

// sliderStatus maps the slider/swing status enum; garageStatus the garage
// one. Only the position states matter here.
var sliderStatus = map[byte]gateStatus{
	0: gateOpen, 1: gateClosed, 2: gateStopped, 3: gateStopped,
	4: gateOpening, 5: gateClosing,
}
var garageStatus = map[byte]gateStatus{
	1: gateOpen, 2: gateOpening, 3: gateClosed, 4: gateClosing, 5: gateStopped,
}

// decodeOverview reads the gate's current state from a raw deviceOverview
// MQTT payload (4-byte header + packed body). The status byte's offset and
// enum depend on the operator family, detected by body length. Returns
// gateUnknown for a frame we can't place.
func decodeOverview(payload []byte) gateStatus {
	if len(payload) < 4 {
		return gateUnknown
	}
	body := payload[4:]
	switch n := len(body); {
	case n == 24: // garage: status byte at 12, garage enum
		return garageStatus[body[12]]
	case n >= 36: // slider (36/64) and swing (38/52): status byte at 22, gate enum
		return sliderStatus[body[22]]
	default:
		return gateUnknown
	}
}
