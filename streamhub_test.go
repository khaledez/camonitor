package main

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource connects, then idles until cancelled. If failFirst
// is set, the first Run returns an error immediately.
type fakeSource struct {
	opens, closes *atomic.Int32
	failFirst     bool
}

func (f *fakeSource) Run(ctx context.Context, onAU accessUnitFunc) error {
	n := f.opens.Add(1)
	defer f.closes.Add(1)
	if f.failFirst && n == 1 {
		return errors.New("boom")
	}
	<-ctx.Done()
	return nil
}

type fakeCounters struct{ opens, closes atomic.Int32 }

func newTestHub(idle time.Duration, failFirst bool) (*StreamHub, *fakeCounters) {
	fc := &fakeCounters{}
	h := NewStreamHub(map[string]string{"10": "rtsp://fake/10"}, idle)
	h.backoff = 10 * time.Millisecond
	h.newSource = func(string) streamSource {
		return &fakeSource{opens: &fc.opens, closes: &fc.closes, failFirst: failFirst}
	}
	return h, fc
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestOnePullManyViewers(t *testing.T) {
	h, fc := newTestHub(time.Hour, false)
	defer h.Close()
	var wg sync.WaitGroup
	var releases []func()
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := h.acquire("10")
			if !ok {
				t.Error("acquire failed")
				return
			}
			mu.Lock()
			releases = append(releases, release)
			mu.Unlock()
		}()
	}
	wg.Wait()
	waitFor(t, "first open", func() bool { return fc.opens.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := fc.opens.Load(); got != 1 {
		t.Fatalf("source opened %d times, want 1", got)
	}
	// releasing two of three viewers must not stop the pull
	releases[0]()
	releases[1]()
	time.Sleep(30 * time.Millisecond)
	if got := fc.closes.Load(); got != 0 {
		t.Fatalf("source closed with a viewer left")
	}
}

func TestIdleStopsPull(t *testing.T) {
	h, fc := newTestHub(50*time.Millisecond, false)
	defer h.Close()
	release, _ := h.acquire("10")
	waitFor(t, "open", func() bool { return fc.opens.Load() == 1 })
	release()
	waitFor(t, "idle close", func() bool { return fc.closes.Load() == 1 })

	// a new viewer after idle starts a fresh pull
	release, _ = h.acquire("10")
	defer release()
	waitFor(t, "reopen", func() bool { return fc.opens.Load() == 2 })
}

func TestHandlerStartsPullAndIdles(t *testing.T) {
	h, fc := newTestHub(50*time.Millisecond, false)
	defer h.Close()
	if _, ok := h.Handler("10"); !ok {
		t.Fatal("Handler(10) not found")
	}
	waitFor(t, "open", func() bool { return fc.opens.Load() == 1 })
	waitFor(t, "idle close", func() bool { return fc.closes.Load() == 1 })
}

func TestReconnectOnUpstreamError(t *testing.T) {
	before := runtime.NumGoroutine()
	h, fc := newTestHub(time.Hour, true)
	release, _ := h.acquire("10")
	waitFor(t, "reopen after error", func() bool { return fc.opens.Load() >= 2 })
	release()
	h.Close()
	waitFor(t, "goroutines to drain", func() bool { return runtime.NumGoroutine() <= before })
}

func TestUnknownCamera(t *testing.T) {
	h, _ := newTestHub(time.Hour, false)
	defer h.Close()
	if _, ok := h.Handler("99"); ok {
		t.Fatal("Handler(99) should be unknown")
	}
	var nilHub *StreamHub
	if _, ok := nilHub.Handler("10"); ok {
		t.Fatal("nil hub should miss")
	}
	empty := NewStreamHub(nil, time.Second)
	if _, ok := empty.Handler("10"); ok {
		t.Fatal("empty hub should miss")
	}
}
