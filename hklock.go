// HomeKit lock accessory for a Dahua VTO door station.
//
// The VTO exposes a momentary relay and reports no lock state at all —
// there is no magnetic contact to read back. So LockCurrentState here is
// inferred rather than sensed: Unsecured immediately after a successful
// open, back to Secured once relockAfter elapses. A failed open reports
// Jammed before settling back to Secured, because a tile that always
// claims success is worse than no tile.
package main

import (
	"context"
	"time"

	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

// jamDisplayTime is how long a failed unlock shows as Jammed before the
// tile settles back to Secured. Long enough to notice, short enough that
// the accessory does not look stuck.
const jamDisplayTime = 3 * time.Second

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

	// cancelPending stops the in-flight relock timer. Only ever touched
	// from HomeKit write handlers, which the HAP server serialises per
	// connection; the timer callback deliberately does not clear it, so
	// cancelling an already-fired timer stays a harmless no-op.
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
		l.svc.LockCurrentState.SetValue(characteristic.LockCurrentStateJammed)
		l.scheduleRelock(jamDisplayTime)
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

// scheduleRelock restarts the timer rather than stacking, so repeated
// unlocks extend the window instead of racing each other back to Secured.
func (l *hkLock) scheduleRelock(d time.Duration) {
	l.cancel()
	l.cancelPending = l.timer(d, func() {
		l.svc.LockCurrentState.SetValue(characteristic.LockCurrentStateSecured)
		l.svc.LockTargetState.SetValue(characteristic.LockTargetStateSecured)
	})
}

func (l *hkLock) cancel() {
	if l.cancelPending != nil {
		l.cancelPending()
		l.cancelPending = nil
	}
}
