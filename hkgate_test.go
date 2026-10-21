package main

import (
	"context"
	"errors"
	"testing"

	"github.com/brutella/hap/characteristic"
)

type fakeGate struct {
	status  gateStatus
	openErr error
	opens   int
}

func (f *fakeGate) Open(context.Context) error { f.opens++; return f.openErr }
func (f *fakeGate) CurrentStatus() gateStatus  { return f.status }

func TestHKGateTriggerAndStatus(t *testing.T) {
	f := &fakeGate{status: gateClosed}
	g := newHKGate("Gate", f)

	if got := g.svc.CurrentDoorState.Value(); got != characteristic.CurrentDoorStateClosed {
		t.Fatalf("initial current = %d, want closed", got)
	}

	// Opening fires the trigger and shows motion immediately.
	if err := g.setTarget(characteristic.TargetDoorStateOpen); err != nil {
		t.Fatalf("setTarget open: %v", err)
	}
	if f.opens != 1 {
		t.Errorf("Open called %d times, want 1", f.opens)
	}
	if got := g.svc.CurrentDoorState.Value(); got != characteristic.CurrentDoorStateOpening {
		t.Errorf("current after open = %d, want opening", got)
	}

	// Telemetry settles the tile.
	g.Update(gateOpen)
	if got := g.svc.CurrentDoorState.Value(); got != characteristic.CurrentDoorStateOpen {
		t.Errorf("current after telemetry = %d, want open", got)
	}
	if got := g.svc.TargetDoorState.Value(); got != characteristic.TargetDoorStateOpen {
		t.Errorf("target after telemetry = %d, want open", got)
	}

	// "Close" also just pulses the one trigger.
	if err := g.setTarget(characteristic.TargetDoorStateClosed); err != nil {
		t.Fatalf("setTarget close: %v", err)
	}
	if f.opens != 2 {
		t.Errorf("Open called %d times, want 2", f.opens)
	}

	// A failed open surfaces as a write error (Home shows "No Response").
	f.openErr = errors.New("gate offline")
	if err := g.setTarget(characteristic.TargetDoorStateOpen); err == nil {
		t.Error("setTarget should return the open error")
	}
}
