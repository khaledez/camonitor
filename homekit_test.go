package main

import (
	"strings"
	"testing"
	"time"

	"github.com/brutella/hap/accessory"
)

func TestNormalizePin(t *testing.T) {
	tests := []struct {
		name    string
		pin     string
		want    string
		wantErr string
	}{
		{name: "dashed", pin: "031-45-154", want: "03145154"},
		{name: "bare digits", pin: "03145154", want: "03145154"},
		{name: "spaces and dashes", pin: " 031 - 45 - 154 ", want: "03145154"},
		{name: "too short", pin: "031-45-15", wantErr: "8 digits"},
		{name: "too long", pin: "031-45-1544", wantErr: "8 digits"},
		{name: "sequential is rejected", pin: "123-45-678", wantErr: "guessed"},
		{name: "repeated is rejected", pin: "111-11-111", wantErr: "guessed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizePin(tt.pin)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("normalizePin(%q) = %q, want error containing %q", tt.pin, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePin(%q): %v", tt.pin, err)
			}
			if got != tt.want {
				t.Errorf("normalizePin(%q) = %q, want %q", tt.pin, got, tt.want)
			}
		})
	}
}

func TestFormatPin(t *testing.T) {
	if got := formatPin("03145154"); got != "031-45-154" {
		t.Errorf("formatPin = %q, want 031-45-154", got)
	}
}

func TestSetupPayloadURI(t *testing.T) {
	// Hand-computed against HAP-R2 §5.7 so this checks the implementation
	// rather than agreeing with it. Payload bits, most significant first:
	// version 0 (3b), reserved 0 (4b), category 2 (8b), flags 2 (4b),
	// code 3145154 (27b) = 4566547906, which is 23ISYWY in base 36.
	got, err := setupPayloadURI("03145154", accessory.TypeBridge, "ABCD")
	if err != nil {
		t.Fatalf("setupPayloadURI: %v", err)
	}
	const want = "X-HM://0023ISYWYABCD"
	if got != want {
		t.Errorf("setupPayloadURI = %q, want %q", got, want)
	}
}

func TestSetupPayloadURIIsAlwaysNineDigitsPlusSetupID(t *testing.T) {
	// A tiny code must still left-pad to nine characters, or the Home app
	// reads the payload off by one position.
	got, err := setupPayloadURI("00000001", accessory.TypeBridge, "WXYZ")
	if err != nil {
		t.Fatalf("setupPayloadURI: %v", err)
	}
	payload := strings.TrimPrefix(got, "X-HM://")
	if len(payload) != 13 {
		t.Errorf("payload %q is %d chars, want 9 + 4 setup id", payload, len(payload))
	}
	if !strings.HasSuffix(payload, "WXYZ") {
		t.Errorf("payload %q does not end with the setup id", payload)
	}
}

func TestSetupIDIsStableAndWellFormed(t *testing.T) {
	first := setupIDFor("03145154", bridgeName)
	if first != setupIDFor("03145154", bridgeName) {
		t.Error("setupIDFor is not deterministic")
	}
	if len(first) != 4 {
		t.Fatalf("setup id %q is %d chars, want 4", first, len(first))
	}
	for _, r := range first {
		isUpperAlnum := (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z')
		if !isUpperAlnum {
			t.Errorf("setup id %q contains %q, want uppercase alphanumerics only", first, r)
		}
	}
	if setupIDFor("03145154", "FrontDoor") == first {
		t.Error("different accessories should get different setup ids")
	}
}

func TestRelockAfter(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty uses the default", value: "", want: defaultRelockAfter},
		{name: "parsed", value: "12s", want: 12 * time.Second},
		{name: "zero is rejected", value: "0s", wantErr: true},
		{name: "negative is rejected", value: "-3s", wantErr: true},
		{name: "garbage is rejected", value: "soon", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HomeKitConfig{RelockAfter: tt.value}.relockAfter()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("relockAfter(%q) = %v, want error", tt.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("relockAfter(%q): %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("relockAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestHomeKitConfigDefaults(t *testing.T) {
	var cfg HomeKitConfig
	if cfg.port() != defaultHomeKitPort {
		t.Errorf("port = %d, want %d", cfg.port(), defaultHomeKitPort)
	}
	if cfg.store() != defaultHomeKitStore {
		t.Errorf("store = %q, want %q", cfg.store(), defaultHomeKitStore)
	}

	set := HomeKitConfig{Port: 60000, Store: "/tmp/hk"}
	if set.port() != 60000 || set.store() != "/tmp/hk" {
		t.Errorf("explicit values not honoured: port=%d store=%q", set.port(), set.store())
	}
}
