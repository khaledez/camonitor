// HomeKit climate accessory: the Gree unit as a HeaterCooler service plus
// two switches for the modes HeaterCooler cannot express.
//
// HomeKit's HeaterCooler understands heat, cool, and auto. Gree also has
// dry and fan-only. Rather than distort the thermostat tile to carry five
// modes, the extra two live on their own switches and the tile keeps
// showing the last heat/cool/auto choice while they are active. `Active`
// still reports power truthfully, which is what the tile is judged on.
package main

import (
	"context"
	"math"
	"sync"

	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

// greeSwingFull is the "SwUpDn" value for a full-range vertical swing.
// Values 2..6 park the louvre at a fixed angle, which HomeKit's binary
// SwingMode has no way to express, so only 1 counts as "swinging".
const greeSwingFull = 1

// climateView is the HomeKit-facing projection of a Gree unit's state.
// Keeping it a plain comparable struct is what makes the mapping — the
// part most likely to harbour bugs — testable without a device.
type climateView struct {
	Active             int
	CurrentState       int
	TargetState        int
	CurrentTemperature float64
	TargetTemperature  float64
	RotationSpeed      float64
	SwingMode          int
	DisplayUnits       int
	DryOn              bool
	FanOnlyOn          bool
}

// climateViewOf projects a Gree status onto HomeKit characteristics.
// lastTarget carries the heat/cool/auto choice forward while the unit sits
// in dry or fan-only mode.
func climateViewOf(st GreeStatus, lastTarget int) climateView {
	view := climateView{
		Active:             characteristic.ActiveInactive,
		CurrentState:       characteristic.CurrentHeaterCoolerStateInactive,
		TargetState:        homekitTargetState(st.Mode, lastTarget),
		CurrentTemperature: float64(st.RoomTemp),
		TargetTemperature:  float64(st.SetTemp),
		RotationSpeed:      rotationSpeedForFan(st.FanSpeed),
		SwingMode:          characteristic.SwingModeSwingDisabled,
		DisplayUnits:       characteristic.TemperatureDisplayUnitsCelsius,
	}

	// Gree reports 0 when the unit is off or has no room sensor. HomeKit
	// renders that as a literal 0 °C rather than "unknown", so the set
	// point is the more plausible stand-in.
	if st.RoomTemp == 0 {
		view.CurrentTemperature = float64(st.SetTemp)
	}
	if st.SwingV == greeSwingFull {
		view.SwingMode = characteristic.SwingModeSwingEnabled
	}
	if st.TempUnit == 1 {
		view.DisplayUnits = characteristic.TemperatureDisplayUnitsFahrenheit
	}

	if st.Power != 1 {
		return view
	}

	view.Active = characteristic.ActiveActive
	view.CurrentState = currentHeaterCoolerState(st)
	view.DryOn = st.Mode == greeModeDry
	view.FanOnlyOn = st.Mode == greeModeFan
	return view
}

func homekitTargetState(greeMode, lastTarget int) int {
	switch greeMode {
	case greeModeCool:
		return characteristic.TargetHeaterCoolerStateCool
	case greeModeHeat:
		return characteristic.TargetHeaterCoolerStateHeat
	case greeModeAuto:
		return characteristic.TargetHeaterCoolerStateAuto
	}
	return lastTarget // dry and fan-only have no HeaterCooler equivalent
}

func greeModeForTargetState(target int) int {
	switch target {
	case characteristic.TargetHeaterCoolerStateHeat:
		return greeModeHeat
	case characteristic.TargetHeaterCoolerStateCool:
		return greeModeCool
	}
	return greeModeAuto
}

// currentHeaterCoolerState describes what the unit is doing right now.
// Only called when the unit is powered on.
func currentHeaterCoolerState(st GreeStatus) int {
	switch st.Mode {
	case greeModeCool:
		return characteristic.CurrentHeaterCoolerStateCooling
	case greeModeHeat:
		return characteristic.CurrentHeaterCoolerStateHeating
	case greeModeAuto:
		return autoHeaterCoolerState(st)
	}
	return characteristic.CurrentHeaterCoolerStateIdle
}

// autoHeaterCoolerState infers heating vs cooling in auto mode, which the
// unit does not report directly, by comparing the room to the set point.
func autoHeaterCoolerState(st GreeStatus) int {
	noRoomSensor := st.RoomTemp == 0
	if noRoomSensor || st.RoomTemp == st.SetTemp {
		return characteristic.CurrentHeaterCoolerStateIdle
	}
	if st.RoomTemp > st.SetTemp {
		return characteristic.CurrentHeaterCoolerStateCooling
	}
	return characteristic.CurrentHeaterCoolerStateHeating
}

// rotationSpeedForFan maps Gree's fan speed onto HomeKit's 0-100 percent
// scale in 20-point steps. 0 is Gree's "auto" and stays 0 here: HomeKit
// has no fan-auto for a HeaterCooler, and inventing a percentage for it
// would round-trip into an explicit speed the user never chose.
func rotationSpeedForFan(fan int) float64 {
	return float64(fan) * 20
}

func fanForRotationSpeed(pct float64) int {
	clamped := math.Min(math.Max(pct, 0), 100)
	return int(math.Round(clamped / 20))
}

// greeDevice is the slice of GreeController the climate accessory needs.
// Narrowing it here keeps the tests free of UDP sockets.
type greeDevice interface {
	Name() string
	Status() GreeStatus
	Set(ctx context.Context, params map[string]int) error
}

// hkClimate owns the HeaterCooler service and the two mode switches, and
// keeps them in sync with the unit.
type hkClimate struct {
	gree greeDevice

	heaterCooler *service.HeaterCooler
	cooling      *characteristic.CoolingThresholdTemperature
	heating      *characteristic.HeatingThresholdTemperature
	speed        *characteristic.RotationSpeed
	swing        *characteristic.SwingMode
	units        *characteristic.TemperatureDisplayUnits
	dry          *service.Switch
	fanOnly      *service.Switch

	mu         sync.Mutex
	lastTarget int
}

func newHKClimate(gree greeDevice) *hkClimate {
	c := &hkClimate{
		gree:         gree,
		heaterCooler: service.NewHeaterCooler(),
		cooling:      characteristic.NewCoolingThresholdTemperature(),
		heating:      characteristic.NewHeatingThresholdTemperature(),
		speed:        characteristic.NewRotationSpeed(),
		swing:        characteristic.NewSwingMode(),
		units:        characteristic.NewTemperatureDisplayUnits(),
		dry:          service.NewSwitch(),
		fanOnly:      service.NewSwitch(),
		lastTarget:   characteristic.TargetHeaterCoolerStateCool,
	}

	// Gree units accept 16-30 °C in whole degrees on both thresholds.
	for _, t := range []*characteristic.Float{c.cooling.Float, c.heating.Float} {
		t.SetMinValue(16)
		t.SetMaxValue(30)
		t.SetStepValue(1)
	}
	c.speed.SetStepValue(20)

	hc := c.heaterCooler
	hc.AddC(c.cooling.C)
	hc.AddC(c.heating.C)
	hc.AddC(c.speed.C)
	hc.AddC(c.swing.C)
	hc.AddC(c.units.C)

	nameSwitch(c.dry, "AC Dry")
	nameSwitch(c.fanOnly, "AC Fan Only")

	c.bindWrites()
	c.Update(gree.Status())
	return c
}

func nameSwitch(s *service.Switch, name string) {
	n := characteristic.NewName()
	n.SetValue(name)
	s.AddC(n.C)
}

// services returns everything this accessory contributes, in tile order.
func (c *hkClimate) services() []*service.S {
	return []*service.S{c.heaterCooler.S, c.dry.S, c.fanOnly.S}
}

// bindWrites hooks every writable characteristic to the unit. Each handler
// returns an error on failure so HomeKit surfaces "No Response" instead of
// silently accepting a command the unit never received.
func (c *hkClimate) bindWrites() {
	c.heaterCooler.Active.OnSetRemoteValue(func(v int) error {
		power := 0
		if v == characteristic.ActiveActive {
			power = 1
		}
		return c.apply(map[string]int{"power": power})
	})

	c.heaterCooler.TargetHeaterCoolerState.OnSetRemoteValue(func(v int) error {
		c.setLastTarget(v)
		return c.apply(map[string]int{"power": 1, "mode": greeModeForTargetState(v)})
	})

	// Gree has a single set point, so both HomeKit thresholds drive it.
	setTemp := func(v float64) error {
		return c.apply(map[string]int{"temp": int(math.Round(v))})
	}
	c.cooling.OnSetRemoteValue(setTemp)
	c.heating.OnSetRemoteValue(setTemp)

	c.speed.OnSetRemoteValue(func(v float64) error {
		return c.apply(map[string]int{"fan": fanForRotationSpeed(v)})
	})

	c.swing.OnSetRemoteValue(func(v int) error {
		swing := 0
		if v == characteristic.SwingModeSwingEnabled {
			swing = greeSwingFull
		}
		return c.apply(map[string]int{"swing_v": swing})
	})

	// Switching a mode switch off returns the unit to whatever heat/cool/
	// auto mode the thermostat tile is showing, which is the least
	// surprising destination.
	c.dry.On.OnSetRemoteValue(func(on bool) error {
		return c.applyModeSwitch(on, greeModeDry)
	})
	c.fanOnly.On.OnSetRemoteValue(func(on bool) error {
		return c.applyModeSwitch(on, greeModeFan)
	})
}

func (c *hkClimate) applyModeSwitch(on bool, mode int) error {
	if !on {
		return c.apply(map[string]int{"mode": greeModeForTargetState(c.getLastTarget())})
	}
	return c.apply(map[string]int{"power": 1, "mode": mode})
}

// apply sends params to the unit and immediately republishes the state it
// reports back, so the Home app settles on the truth rather than on what
// it optimistically assumed.
func (c *hkClimate) apply(params map[string]int) error {
	if err := c.gree.Set(context.Background(), params); err != nil {
		return err
	}
	c.Update(c.gree.Status())
	return nil
}

// Update republishes every characteristic from a fresh status. Wired to
// GreeController's poll so changes made from the physical remote or the
// web UI reach HomeKit as events.
func (c *hkClimate) Update(st GreeStatus) {
	view := climateViewOf(st, c.getLastTarget())
	c.setLastTarget(view.TargetState)

	c.heaterCooler.Active.SetValue(view.Active)
	c.heaterCooler.CurrentHeaterCoolerState.SetValue(view.CurrentState)
	c.heaterCooler.TargetHeaterCoolerState.SetValue(view.TargetState)
	c.heaterCooler.CurrentTemperature.SetValue(view.CurrentTemperature)
	c.cooling.SetValue(view.TargetTemperature)
	c.heating.SetValue(view.TargetTemperature)
	c.speed.SetValue(view.RotationSpeed)
	c.swing.SetValue(view.SwingMode)
	c.units.SetValue(view.DisplayUnits)
	c.dry.On.SetValue(view.DryOn)
	c.fanOnly.On.SetValue(view.FanOnlyOn)
}

func (c *hkClimate) getLastTarget() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastTarget
}

func (c *hkClimate) setLastTarget(v int) {
	c.mu.Lock()
	c.lastTarget = v
	c.mu.Unlock()
}
