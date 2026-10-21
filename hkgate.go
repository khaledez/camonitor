// HomeKit garage-door accessory for a Centsys SMART sliding gate.
//
// A sliding gate has one physical input (TRG) that toggles open/stop/close,
// so HomeKit's separate "open" and "close" both send that same trigger —
// see setTarget. The tile's displayed state is not guessed from the command
// but driven by the operator's own telemetry (Update), so it stays honest
// even when the gate is worked by a remote or the app.
package main

import (
	"context"

	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

// gateController is the slice of CentsysController this accessory needs,
// narrowed so the test needs no broker.
type gateController interface {
	Open(ctx context.Context) error
	CurrentStatus() gateStatus
}

type hkGate struct {
	gate gateController
	svc  *service.GarageDoorOpener
}

func newHKGate(name string, gate gateController) *hkGate {
	g := &hkGate{gate: gate, svc: service.NewGarageDoorOpener()}

	n := characteristic.NewName()
	n.SetValue(name)
	g.svc.AddC(n.C)

	// HomeKit needs a starting value; default closed until telemetry lands.
	g.svc.CurrentDoorState.SetValue(characteristic.CurrentDoorStateClosed)
	g.svc.TargetDoorState.SetValue(characteristic.TargetDoorStateClosed)
	g.apply(gate.CurrentStatus())

	g.svc.TargetDoorState.OnSetRemoteValue(g.setTarget)
	return g
}

// setTarget fires the one trigger the gate has. A failed open returns the
// error so the Home app shows the command didn't land.
func (g *hkGate) setTarget(target int) error {
	if err := g.gate.Open(context.Background()); err != nil {
		return err
	}
	// Show motion immediately; telemetry corrects it via Update.
	if target == characteristic.TargetDoorStateOpen {
		g.svc.CurrentDoorState.SetValue(characteristic.CurrentDoorStateOpening)
	} else {
		g.svc.CurrentDoorState.SetValue(characteristic.CurrentDoorStateClosing)
	}
	return nil
}

// Update reflects the operator's reported state onto the tile. Wired to
// CentsysController.OnUpdate.
func (g *hkGate) Update(s gateStatus) { g.apply(s) }

func (g *hkGate) apply(s gateStatus) {
	switch s {
	case gateOpen:
		g.svc.CurrentDoorState.SetValue(characteristic.CurrentDoorStateOpen)
		g.svc.TargetDoorState.SetValue(characteristic.TargetDoorStateOpen)
	case gateClosed:
		g.svc.CurrentDoorState.SetValue(characteristic.CurrentDoorStateClosed)
		g.svc.TargetDoorState.SetValue(characteristic.TargetDoorStateClosed)
	case gateOpening:
		g.svc.CurrentDoorState.SetValue(characteristic.CurrentDoorStateOpening)
		g.svc.TargetDoorState.SetValue(characteristic.TargetDoorStateOpen)
	case gateClosing:
		g.svc.CurrentDoorState.SetValue(characteristic.CurrentDoorStateClosing)
		g.svc.TargetDoorState.SetValue(characteristic.TargetDoorStateClosed)
	case gateStopped:
		g.svc.CurrentDoorState.SetValue(characteristic.CurrentDoorStateStopped)
	case gateUnknown:
		// Keep whatever is shown.
	}
}
