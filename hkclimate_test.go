package main

import (
	"testing"

	"github.com/brutella/hap/characteristic"
)

func TestClimateViewOf(t *testing.T) {
	// A unit cooling to 22 with the room at 26, fan speed 3, swing on.
	cooling := GreeStatus{
		Online: true, Power: 1, Mode: greeModeCool,
		SetTemp: 22, FanSpeed: 3, SwingV: 1, RoomTemp: 26,
	}

	tests := []struct {
		name       string
		status     GreeStatus
		lastTarget int
		want       climateView
	}{
		{
			name:       "cooling",
			status:     cooling,
			lastTarget: characteristic.TargetHeaterCoolerStateCool,
			want: climateView{
				Active:             characteristic.ActiveActive,
				CurrentState:       characteristic.CurrentHeaterCoolerStateCooling,
				TargetState:        characteristic.TargetHeaterCoolerStateCool,
				CurrentTemperature: 26,
				TargetTemperature:  22,
				RotationSpeed:      60,
				SwingMode:          characteristic.SwingModeSwingEnabled,
				DisplayUnits:       characteristic.TemperatureDisplayUnitsCelsius,
			},
		},
		{
			name: "powered off reports inactive",
			status: GreeStatus{
				Online: true, Power: 0, Mode: greeModeCool,
				SetTemp: 22, FanSpeed: 3, RoomTemp: 26,
			},
			lastTarget: characteristic.TargetHeaterCoolerStateCool,
			want: climateView{
				Active:             characteristic.ActiveInactive,
				CurrentState:       characteristic.CurrentHeaterCoolerStateInactive,
				TargetState:        characteristic.TargetHeaterCoolerStateCool,
				CurrentTemperature: 26,
				TargetTemperature:  22,
				RotationSpeed:      60,
				SwingMode:          characteristic.SwingModeSwingDisabled,
			},
		},
		{
			name: "heating",
			status: GreeStatus{
				Online: true, Power: 1, Mode: greeModeHeat,
				SetTemp: 24, FanSpeed: 5, RoomTemp: 18,
			},
			lastTarget: characteristic.TargetHeaterCoolerStateCool,
			want: climateView{
				Active:             characteristic.ActiveActive,
				CurrentState:       characteristic.CurrentHeaterCoolerStateHeating,
				TargetState:        characteristic.TargetHeaterCoolerStateHeat,
				CurrentTemperature: 18,
				TargetTemperature:  24,
				RotationSpeed:      100,
			},
		},
		{
			// HomeKit's HeaterCooler has no dry mode, so the tile keeps
			// showing the last heat/cool/auto choice and the Dry switch
			// carries the truth.
			name: "dry keeps last target state and flips the dry switch",
			status: GreeStatus{
				Online: true, Power: 1, Mode: greeModeDry,
				SetTemp: 25, FanSpeed: 1, RoomTemp: 27,
			},
			lastTarget: characteristic.TargetHeaterCoolerStateHeat,
			want: climateView{
				Active:             characteristic.ActiveActive,
				CurrentState:       characteristic.CurrentHeaterCoolerStateIdle,
				TargetState:        characteristic.TargetHeaterCoolerStateHeat,
				CurrentTemperature: 27,
				TargetTemperature:  25,
				RotationSpeed:      20,
				DryOn:              true,
			},
		},
		{
			name: "fan only flips the fan switch",
			status: GreeStatus{
				Online: true, Power: 1, Mode: greeModeFan,
				SetTemp: 25, FanSpeed: 2, RoomTemp: 27,
			},
			lastTarget: characteristic.TargetHeaterCoolerStateAuto,
			want: climateView{
				Active:             characteristic.ActiveActive,
				CurrentState:       characteristic.CurrentHeaterCoolerStateIdle,
				TargetState:        characteristic.TargetHeaterCoolerStateAuto,
				CurrentTemperature: 27,
				TargetTemperature:  25,
				RotationSpeed:      40,
				FanOnlyOn:          true,
			},
		},
		{
			name: "auto mode derives current state from the room temperature",
			status: GreeStatus{
				Online: true, Power: 1, Mode: greeModeAuto,
				SetTemp: 24, FanSpeed: 0, RoomTemp: 28,
			},
			lastTarget: characteristic.TargetHeaterCoolerStateCool,
			want: climateView{
				Active:             characteristic.ActiveActive,
				CurrentState:       characteristic.CurrentHeaterCoolerStateCooling,
				TargetState:        characteristic.TargetHeaterCoolerStateAuto,
				CurrentTemperature: 28,
				TargetTemperature:  24,
				RotationSpeed:      0,
			},
		},
		{
			// HomeKit refuses to render a 0 °C current temperature as
			// "unknown" — it just shows 0. The set point is a far more
			// plausible stand-in.
			name: "missing room sensor falls back to the set point",
			status: GreeStatus{
				Online: true, Power: 1, Mode: greeModeCool,
				SetTemp: 23, FanSpeed: 4, RoomTemp: 0,
			},
			lastTarget: characteristic.TargetHeaterCoolerStateCool,
			want: climateView{
				Active:             characteristic.ActiveActive,
				CurrentState:       characteristic.CurrentHeaterCoolerStateCooling,
				TargetState:        characteristic.TargetHeaterCoolerStateCool,
				CurrentTemperature: 23,
				TargetTemperature:  23,
				RotationSpeed:      80,
			},
		},
		{
			// HAP carries every temperature in Celsius; the display-units
			// characteristic is only a rendering hint. A unit set to °F
			// reports Fahrenheit numbers that must be converted, not
			// relabelled — 72 °F published as "72" would be read as 72 °C.
			name: "fahrenheit unit is converted to celsius",
			status: GreeStatus{
				Online: true, Power: 1, Mode: greeModeCool,
				SetTemp: 72, FanSpeed: 3, RoomTemp: 78, TempUnit: 1,
			},
			lastTarget: characteristic.TargetHeaterCoolerStateCool,
			want: climateView{
				Active:             characteristic.ActiveActive,
				CurrentState:       characteristic.CurrentHeaterCoolerStateCooling,
				TargetState:        characteristic.TargetHeaterCoolerStateCool,
				CurrentTemperature: (78 - 32) * 5.0 / 9,
				TargetTemperature:  (72 - 32) * 5.0 / 9,
				RotationSpeed:      60,
				DisplayUnits:       characteristic.TemperatureDisplayUnitsFahrenheit,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := climateViewOf(tt.status, tt.lastTarget)
			if got != tt.want {
				t.Errorf("climateViewOf()\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestTemperatureRoundTripsThroughBothUnits(t *testing.T) {
	for _, unit := range []int{0, 1} {
		for raw := 16; raw <= 86; raw++ {
			if unit == 0 && raw > 30 {
				continue // Celsius units only span 16-30
			}
			c := celsius(raw, unit)
			if got := greeTemp(c, unit); got != raw {
				t.Errorf("unit=%d: %d -> %.2f°C -> %d, want %d", unit, raw, c, got, raw)
			}
		}
	}
}

// An unreachable unit must not publish anything: greeClient.Status returns
// a zero struct on failure, and pushing that would tell every controller
// the AC just switched off at 0 °C on every dropped UDP packet.
func TestOfflineStatusIsNotPublished(t *testing.T) {
	live := GreeStatus{Online: true, Power: 1, Mode: greeModeCool, SetTemp: 22, FanSpeed: 3, RoomTemp: 26}
	gree := &fakeGree{name: "AC", status: live}
	c := newHKClimate(gree)

	if got := c.heaterCooler.Active.Value(); got != characteristic.ActiveActive {
		t.Fatalf("Active = %d before the drop, want Active", got)
	}

	c.Update(GreeStatus{}) // what a timed-out poll hands us

	if got := c.heaterCooler.Active.Value(); got != characteristic.ActiveActive {
		t.Errorf("Active = %d after an offline poll, want the last known Active", got)
	}
	if got := c.heaterCooler.CurrentTemperature.Value(); got != 26 {
		t.Errorf("CurrentTemperature = %v after an offline poll, want the last known 26", got)
	}
}

func TestGreeModeForTargetState(t *testing.T) {
	tests := []struct {
		target int
		want   int
	}{
		{characteristic.TargetHeaterCoolerStateAuto, greeModeAuto},
		{characteristic.TargetHeaterCoolerStateHeat, greeModeHeat},
		{characteristic.TargetHeaterCoolerStateCool, greeModeCool},
	}
	for _, tt := range tests {
		if got := greeModeForTargetState(tt.target); got != tt.want {
			t.Errorf("greeModeForTargetState(%d) = %d, want %d", tt.target, got, tt.want)
		}
	}
}

func TestFanSpeedRoundTrip(t *testing.T) {
	for fan := greeFanAuto; fan <= 5; fan++ {
		pct := rotationSpeedForFan(fan)
		if got := fanForRotationSpeed(pct); got != fan {
			t.Errorf("fan %d -> %.0f%% -> %d, want %d", fan, pct, got, fan)
		}
	}
}

func TestFanForRotationSpeedClamps(t *testing.T) {
	tests := []struct {
		pct  float64
		want int
	}{
		{-10, greeFanAuto},
		{0, greeFanAuto},
		{9, greeFanAuto}, // rounds down to auto
		{11, 1},          // rounds up to speed 1
		{55, 3},          // nearest step
		{100, 5},
		{140, 5},
	}
	for _, tt := range tests {
		if got := fanForRotationSpeed(tt.pct); got != tt.want {
			t.Errorf("fanForRotationSpeed(%.0f) = %d, want %d", tt.pct, got, tt.want)
		}
	}
}
