// HomeKit lock accessory for a Dahua VTO door station.
//
// The VTO exposes a momentary relay and reports no lock state at all —
// there is no magnetic contact to read back. So LockCurrentState here is
// inferred rather than sensed: Unsecured immediately after a successful
// open, back to Secured once relockAfter elapses.
//
// A failed open is reported by failing the write, which is all HomeKit
// gives us. Setting LockCurrentState to Jammed alongside it looks
// tempting, but hap maps any handler error to SERVICE_COMMUNICATION_
// FAILURE and the Home app renders that as "No Response" — the Jammed
// value never surfaces, so it would be decoration that also lies about
// why the command failed.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

// doorOpener is the slice of DoorClient this accessory needs. Narrowing it
// keeps the tests free of HTTP and cameras.
type doorOpener interface {
	Open(ctx context.Context, streamID string) error
}

// timerFunc schedules f to run after d and returns a cancel function.
// Injected so the relock delay is testable without sleeping.
type timerFunc func(d time.Duration, f func()) (cancel func())

func realTimer(d time.Duration, f func()) func() {
	t := time.AfterFunc(d, f)
	return func() { t.Stop() }
}

type hkLock struct {
	streamID string
	doors    doorOpener
	relock   time.Duration
	timer    timerFunc

	svc *service.LockMechanism

	// mu guards cancelPending. hap dispatches writes on the goroutine of
	// whichever connection made them, and a household normally has several
	// paired controllers (iPhone, iPad, Home hub) each on their own
	// connection — so two unlocks really can land at once.
	mu sync.Mutex
	// cancelPending stops the in-flight relock timer. The timer callback
	// deliberately does not clear it, so cancelling an already-fired timer
	// stays a harmless no-op.
	cancelPending func()
}

func newHKLock(streamID, name string, doors doorOpener, relock time.Duration, timer timerFunc) *hkLock {
	l := &hkLock{
		streamID: streamID,
		doors:    doors,
		relock:   relock,
		timer:    timer,
		svc:      service.NewLockMechanism(),
	}

	n := characteristic.NewName()
	n.SetValue(name)
	l.svc.AddC(n.C)

	l.svc.LockCurrentState.SetValue(characteristic.LockCurrentStateSecured)
	l.svc.LockTargetState.SetValue(characteristic.LockTargetStateSecured)
	l.svc.LockTargetState.OnSetRemoteValue(l.setTarget)
	return l
}

func (l *hkLock) setTarget(v int) error {
	if v == characteristic.LockTargetStateSecured {
		l.secure()
		return nil
	}
	return l.unlock()
}

// unlock pulses the relay. The returned error reaches HomeKit as a failed
// write, which is what makes the Home app show the command didn't land.
func (l *hkLock) unlock() error {
	ctx, cancel := context.WithTimeout(context.Background(), doorOpenTimeout)
	defer cancel()

	if err := l.doors.Open(ctx, l.streamID); err != nil {
		return err
	}

	l.svc.LockCurrentState.SetValue(characteristic.LockCurrentStateUnsecured)
	l.scheduleRelock(l.relock)
	return nil
}

// secure has no command to send — the relay only opens. It cancels any
// pending relock and settles the tile immediately.
func (l *hkLock) secure() {
	l.cancel()
	l.svc.LockCurrentState.SetValue(characteristic.LockCurrentStateSecured)
}

// scheduleRelock restarts the timer rather than stacking, so concurrent
// unlocks extend the window instead of one of them slamming the tile back
// to Secured while another's relay window is still open.
func (l *hkLock) scheduleRelock(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancelLocked()
	l.cancelPending = l.timer(d, func() {
		l.svc.LockCurrentState.SetValue(characteristic.LockCurrentStateSecured)
		l.svc.LockTargetState.SetValue(characteristic.LockTargetStateSecured)
	})
}

func (l *hkLock) cancel() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancelLocked()
}

func (l *hkLock) cancelLocked() {
	if l.cancelPending != nil {
		l.cancelPending()
		l.cancelPending = nil
	}
}
