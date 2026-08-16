package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// fakeTimer records scheduled callbacks instead of running them, so the
// relock delay is exercised without any sleeping.
type fakeTimer struct {
	durations []time.Duration
	pending   []func() // nil once cancelled or fired
	cancels   int
}

func (f *fakeTimer) schedule(d time.Duration, fn func()) func() {
	f.durations = append(f.durations, d)
	f.pending = append(f.pending, fn)
	i := len(f.pending) - 1
	return func() {
		f.cancels++
		f.pending[i] = nil
	}
}

// fireLast runs the most recently scheduled callback if it is still live.
func (f *fakeTimer) fireLast(t *testing.T) {
	t.Helper()
	if len(f.pending) == 0 {
		t.Fatal("no timer was scheduled")
	}
	fn := f.pending[len(f.pending)-1]
	if fn == nil {
		t.Fatal("most recent timer was cancelled")
	}
	fn()
}

type fakeDoors struct {
	opened []string
	err    error
}

func (d *fakeDoors) Open(_ context.Context, streamID string) error {
	d.opened = append(d.opened, streamID)
	return d.err
}

func newTestLock(doors doorOpener, timer *fakeTimer) *hkLock {
	return newHKLock("vto1", "FrontDoor", doors, 5*time.Second, timer.schedule)
}

func lockStates(l *hkLock) (current, target int) {
	return l.svc.LockCurrentState.Value(), l.svc.LockTargetState.Value()
}

func TestLockStartsSecured(t *testing.T) {
	l := newTestLock(&fakeDoors{}, &fakeTimer{})
	current, target := lockStates(l)
	if current != characteristic.LockCurrentStateSecured {
		t.Errorf("current = %d, want Secured", current)
	}
	if target != characteristic.LockTargetStateSecured {
		t.Errorf("target = %d, want Secured", target)
	}
}

func TestUnlockPulsesRelayAndRelocks(t *testing.T) {
	doors := &fakeDoors{}
	timer := &fakeTimer{}
	l := newTestLock(doors, timer)

	if err := l.setTarget(characteristic.LockTargetStateUnsecured); err != nil {
		t.Fatalf("setTarget: %v", err)
	}

	if len(doors.opened) != 1 || doors.opened[0] != "vto1" {
		t.Fatalf("opened = %v, want [vto1]", doors.opened)
	}
	if current, _ := lockStates(l); current != characteristic.LockCurrentStateUnsecured {
		t.Errorf("current = %d, want Unsecured", current)
	}
	if len(timer.durations) != 1 || timer.durations[0] != 5*time.Second {
		t.Fatalf("scheduled %v, want one 5s relock", timer.durations)
	}

	timer.fireLast(t)

	current, target := lockStates(l)
	if current != characteristic.LockCurrentStateSecured {
		t.Errorf("after relock current = %d, want Secured", current)
	}
	if target != characteristic.LockTargetStateSecured {
		t.Errorf("after relock target = %d, want Secured", target)
	}
}

func TestFailedUnlockReportsJam(t *testing.T) {
	wantErr := errors.New("camera did not respond")
	doors := &fakeDoors{err: wantErr}
	timer := &fakeTimer{}
	l := newTestLock(doors, timer)

	err := l.setTarget(characteristic.LockTargetStateUnsecured)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if current, _ := lockStates(l); current != characteristic.LockCurrentStateJammed {
		t.Errorf("current = %d, want Jammed", current)
	}
	if len(timer.durations) != 1 || timer.durations[0] != jamDisplayTime {
		t.Fatalf("scheduled %v, want one %v jam display", timer.durations, jamDisplayTime)
	}

	timer.fireLast(t)

	if current, _ := lockStates(l); current != characteristic.LockCurrentStateSecured {
		t.Errorf("after jam display current = %d, want Secured", current)
	}
}

func TestExplicitSecureCancelsPendingRelock(t *testing.T) {
	timer := &fakeTimer{}
	l := newTestLock(&fakeDoors{}, timer)

	if err := l.setTarget(characteristic.LockTargetStateUnsecured); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := l.setTarget(characteristic.LockTargetStateSecured); err != nil {
		t.Fatalf("secure: %v", err)
	}

	if timer.cancels != 1 {
		t.Errorf("cancels = %d, want 1", timer.cancels)
	}
	if current, _ := lockStates(l); current != characteristic.LockCurrentStateSecured {
		t.Errorf("current = %d, want Secured", current)
	}
}

func TestRepeatedUnlockRestartsTimerRatherThanStacking(t *testing.T) {
	timer := &fakeTimer{}
	l := newTestLock(&fakeDoors{}, timer)

	for range 3 {
		if err := l.setTarget(characteristic.LockTargetStateUnsecured); err != nil {
			t.Fatalf("unlock: %v", err)
		}
	}

	// Three unlocks, two of which superseded a live timer.
	if timer.cancels != 2 {
		t.Errorf("cancels = %d, want 2", timer.cancels)
	}
	live := 0
	for _, fn := range timer.pending {
		if fn != nil {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d live timers, want exactly 1", live)
	}
	if current, _ := lockStates(l); current != characteristic.LockCurrentStateUnsecured {
		t.Errorf("current = %d, want Unsecured", current)
	}
}

func TestSecureWithNoPendingTimerIsHarmless(t *testing.T) {
	timer := &fakeTimer{}
	l := newTestLock(&fakeDoors{}, timer)

	if err := l.setTarget(characteristic.LockTargetStateSecured); err != nil {
		t.Fatalf("secure: %v", err)
	}
	if timer.cancels != 0 {
		t.Errorf("cancels = %d, want 0", timer.cancels)
	}
}
