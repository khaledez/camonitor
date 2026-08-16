package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

type fakeGree struct {
	name   string
	status GreeStatus
	sets   []map[string]int
	err    error
}

func (f *fakeGree) Name() string       { return f.name }
func (f *fakeGree) Status() GreeStatus { return f.status }
func (f *fakeGree) Set(_ context.Context, params map[string]int) error {
	if f.err != nil {
		return f.err
	}
	f.sets = append(f.sets, params)
	return nil
}

func (f *fakeGree) lastSet(t *testing.T) map[string]int {
	t.Helper()
	if len(f.sets) == 0 {
		t.Fatal("nothing was sent to the unit")
	}
	return f.sets[len(f.sets)-1]
}

var testStreams = []StreamConfig{
	{ID: "vto1", Name: "FrontDoor", Door: true},
	{ID: "vto2", Name: "Gate", Door: true},
	{ID: "tiandy1", Name: "Front", Codec: "h265"}, // camera, no door
}

func TestBridgeExposesOneLockPerDoorPlusClimate(t *testing.T) {
	gree := &fakeGree{name: "Living Room AC"}
	bridge, children, climate := buildBridgeAccessories(testStreams, &fakeDoors{}, gree, 5*time.Second)

	if bridge.Id != bridgeAID {
		t.Errorf("bridge AID = %d, want %d", bridge.Id, bridgeAID)
	}
	if climate == nil {
		t.Fatal("climate accessory is nil despite a configured unit")
	}
	if len(children) != 3 {
		t.Fatalf("%d children, want 2 locks + 1 climate", len(children))
	}

	wantNames := []string{"FrontDoor", "Gate", "Living Room AC"}
	for i, want := range wantNames {
		if got := children[i].Name(); got != want {
			t.Errorf("children[%d] = %q, want %q", i, got, want)
		}
	}

	// The H.265 camera must not appear: it has no door, and HomeKit will
	// not bridge cameras regardless.
	for _, a := range children {
		if a.Name() == "Front" {
			t.Error("camera stream leaked onto the bridge")
		}
	}

	if children[0].Id != firstLockAID || children[1].Id != firstLockAID+1 {
		t.Errorf("lock AIDs = %d, %d, want %d, %d",
			children[0].Id, children[1].Id, firstLockAID, firstLockAID+1)
	}
	if children[2].Id != climateAID {
		t.Errorf("climate AID = %d, want %d", children[2].Id, climateAID)
	}
}

func TestBridgeWithoutAirConditioner(t *testing.T) {
	_, children, climate := buildBridgeAccessories(testStreams, &fakeDoors{}, nil, 5*time.Second)
	if climate != nil {
		t.Error("climate accessory built without a configured unit")
	}
	if len(children) != 2 {
		t.Fatalf("%d children, want 2 locks", len(children))
	}
}

func TestClimateAccessoryCarriesThermostatAndBothModeSwitches(t *testing.T) {
	gree := &fakeGree{name: "Living Room AC"}
	_, children, _ := buildBridgeAccessories(testStreams, &fakeDoors{}, gree, 5*time.Second)

	ac := children[2]
	types := map[string]int{}
	for _, s := range ac.Ss {
		types[s.Type]++
	}
	if types[service.TypeHeaterCooler] != 1 {
		t.Errorf("%d HeaterCooler services, want 1", types[service.TypeHeaterCooler])
	}
	if types[service.TypeSwitch] != 2 {
		t.Errorf("%d Switch services, want 2 (dry, fan-only)", types[service.TypeSwitch])
	}
}

// controllerWrite drives a characteristic exactly as a paired controller
// does. hap only invokes the write handler for requests that carry an HTTP
// request, and skips writes whose value already matches, so every case
// below seeds a state the write actually changes.
func controllerWrite(c *characteristic.C, v any) (any, int) {
	return c.SetValueRequest(v, httptest.NewRequest(http.MethodPut, "/characteristics", nil))
}

func write(t *testing.T, c *characteristic.C, v any) {
	t.Helper()
	if _, code := controllerWrite(c, v); code != 0 {
		t.Fatalf("write %v rejected with HAP status %d", v, code)
	}
}

// off is the resting state most write cases start from: powered down,
// cooling to 25, fan speed 3, no swing.
var off = GreeStatus{Online: true, Power: 0, Mode: greeModeCool, SetTemp: 25, FanSpeed: 3}

func on(mutate func(*GreeStatus)) GreeStatus {
	st := off
	st.Power = 1
	mutate(&st)
	return st
}

func TestClimateWritesReachTheUnit(t *testing.T) {
	tests := []struct {
		name  string
		seed  GreeStatus
		write func(t *testing.T, c *hkClimate)
		want  map[string]int
	}{
		{
			name:  "power on",
			seed:  off,
			write: func(t *testing.T, c *hkClimate) { write(t, c.heaterCooler.Active.C, characteristic.ActiveActive) },
			want:  map[string]int{"power": 1},
		},
		{
			name:  "power off",
			seed:  on(func(*GreeStatus) {}),
			write: func(t *testing.T, c *hkClimate) { write(t, c.heaterCooler.Active.C, characteristic.ActiveInactive) },
			want:  map[string]int{"power": 0},
		},
		{
			name: "target heat",
			seed: on(func(s *GreeStatus) { s.Mode = greeModeCool }),
			write: func(t *testing.T, c *hkClimate) {
				write(t, c.heaterCooler.TargetHeaterCoolerState.C, characteristic.TargetHeaterCoolerStateHeat)
			},
			want: map[string]int{"power": 1, "mode": greeModeHeat},
		},
		{
			name: "target cool",
			seed: on(func(s *GreeStatus) { s.Mode = greeModeHeat }),
			write: func(t *testing.T, c *hkClimate) {
				write(t, c.heaterCooler.TargetHeaterCoolerState.C, characteristic.TargetHeaterCoolerStateCool)
			},
			want: map[string]int{"power": 1, "mode": greeModeCool},
		},
		{
			name:  "cooling threshold sets the single set point",
			seed:  on(func(s *GreeStatus) { s.SetTemp = 25 }),
			write: func(t *testing.T, c *hkClimate) { write(t, c.cooling.C, 23.0) },
			want:  map[string]int{"temp": 23},
		},
		{
			name:  "heating threshold sets the same set point",
			seed:  on(func(s *GreeStatus) { s.SetTemp = 25 }),
			write: func(t *testing.T, c *hkClimate) { write(t, c.heating.C, 26.0) },
			want:  map[string]int{"temp": 26},
		},
		{
			name:  "fan speed",
			seed:  on(func(s *GreeStatus) { s.FanSpeed = 3 }),
			write: func(t *testing.T, c *hkClimate) { write(t, c.speed.C, 80.0) },
			want:  map[string]int{"fan": 4},
		},
		{
			name:  "swing on",
			seed:  on(func(s *GreeStatus) { s.SwingV = 0 }),
			write: func(t *testing.T, c *hkClimate) { write(t, c.swing.C, characteristic.SwingModeSwingEnabled) },
			want:  map[string]int{"swing_v": greeSwingFull},
		},
		{
			name:  "dry switch on",
			seed:  on(func(s *GreeStatus) { s.Mode = greeModeCool }),
			write: func(t *testing.T, c *hkClimate) { write(t, c.dry.On.C, true) },
			want:  map[string]int{"power": 1, "mode": greeModeDry},
		},
		{
			name:  "fan-only switch on",
			seed:  on(func(s *GreeStatus) { s.Mode = greeModeCool }),
			write: func(t *testing.T, c *hkClimate) { write(t, c.fanOnly.On.C, true) },
			want:  map[string]int{"power": 1, "mode": greeModeFan},
		},
		{
			// Turning a mode switch off has to land somewhere; the least
			// surprising destination is whatever the thermostat tile shows,
			// which for a unit that was cooling before is cool.
			name:  "dry switch off returns to the thermostat's mode",
			seed:  on(func(s *GreeStatus) { s.Mode = greeModeDry }),
			write: func(t *testing.T, c *hkClimate) { write(t, c.dry.On.C, false) },
			want:  map[string]int{"mode": greeModeCool},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gree := &fakeGree{name: "AC", status: tt.seed}
			c := newHKClimate(gree)
			tt.write(t, c)

			got := gree.lastSet(t)
			if len(got) != len(tt.want) {
				t.Fatalf("sent %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("sent %v, want %s=%d", got, k, v)
				}
			}
		})
	}
}

func TestClimateWriteFailureIsReportedToHomeKit(t *testing.T) {
	wantErr := errors.New("unit offline")
	gree := &fakeGree{name: "AC", status: off, err: wantErr}
	c := newHKClimate(gree)

	// A unit that does not answer must surface as a failed write, so the
	// Home app shows "No Response" rather than accepting the command.
	if _, code := controllerWrite(c.heaterCooler.Active.C, characteristic.ActiveActive); code == 0 {
		t.Error("write succeeded despite the unit being unreachable")
	}
	if err := c.apply(map[string]int{"power": 1}); !errors.Is(err, wantErr) {
		t.Errorf("apply err = %v, want %v", err, wantErr)
	}
}
