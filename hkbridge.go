// Assembly of the phase-1 HomeKit bridge: one lock accessory per door
// station, plus the air conditioner when one is configured.
//
// Cameras are deliberately absent. HomeKit refuses to bridge them, so
// they arrive in phase 2 as their own pairable accessories.
package main

import (
	"time"

	"github.com/brutella/hap/accessory"
)

const (
	bridgeName         = "camonitor"
	bridgeManufacturer = "camonitor"

	// HAP wants a major.minor.patch firmware revision. hap's placeholder
	// is "-", which the Home app renders as an empty Version field.
	accessoryFirmware = "1.0.0"

	// Accessory instance IDs. HomeKit identifies an accessory by this
	// number, so they are assigned explicitly rather than left to
	// insertion order: reordering `streams` in config would otherwise
	// re-identify every lock and orphan the user's scenes and automations.
	// Locks count up from firstLockAID in config order; the air
	// conditioner sits well clear so adding a door never displaces it.
	bridgeAID    = 1
	firstLockAID = 2
	climateAID   = 100
)

// buildBridgeAccessories returns the bridge, the accessories hanging off
// it, and the climate accessory (nil when no AC is configured) so the
// caller can wire it to the Gree poll.
func buildBridgeAccessories(
	streams []StreamConfig,
	doors doorOpener,
	gree greeDevice,
	relock time.Duration,
) (*accessory.A, []*accessory.A, *hkClimate) {
	bridge := accessory.NewBridge(accessory.Info{
		Name:         bridgeName,
		Manufacturer: bridgeManufacturer,
		Model:        "camonitor",
		SerialNumber: bridgeName,
		Firmware:     accessoryFirmware,
	})
	bridge.A.Id = bridgeAID

	var children []*accessory.A
	aid := uint64(firstLockAID)
	for _, s := range streams {
		if !s.Door {
			continue
		}
		children = append(children, newLockAccessory(s, doors, relock, aid))
		aid++
	}

	if gree == nil {
		return bridge.A, children, nil
	}

	climate := newHKClimate(gree)
	children = append(children, newClimateAccessory(gree.Name(), climate))
	return bridge.A, children, climate
}

func newLockAccessory(s StreamConfig, doors doorOpener, relock time.Duration, aid uint64) *accessory.A {
	name := s.Name
	if name == "" {
		name = s.ID
	}

	a := accessory.New(accessory.Info{
		Name:         name,
		Manufacturer: bridgeManufacturer,
		Model:        "Dahua VTO",
		SerialNumber: s.ID,
		Firmware:     accessoryFirmware,
	}, accessory.TypeDoorLock)
	a.Id = aid

	lock := newHKLock(s.ID, name, doors, relock, realTimer)
	lock.svc.Primary = true
	a.AddS(lock.svc.S)
	return a
}

func newClimateAccessory(name string, climate *hkClimate) *accessory.A {
	if name == "" {
		name = "Air Conditioner"
	}

	a := accessory.New(accessory.Info{
		Name:         name,
		Manufacturer: bridgeManufacturer,
		Model:        "Gree",
		SerialNumber: "gree",
		Firmware:     accessoryFirmware,
	}, accessory.TypeAirConditioner)
	a.Id = climateAID

	climate.heaterCooler.Primary = true
	for _, s := range climate.services() {
		a.AddS(s)
	}
	return a
}
