package main

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestAudit(t *testing.T) *AuditStore {
	t.Helper()
	s, err := OpenAuditStore(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

func TestAuditLogAndListRoundTrip(t *testing.T) {
	s := openTestAudit(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i, action := range []string{"a", "b", "c"} {
		e := AuditEvent{Time: base.Add(time.Duration(i) * time.Minute), GuestID: "g1", GuestLabel: "Bob",
			Action: action, Result: "ok", SourceIP: "1.2.3.4", SourceDevice: "iphone"}
		if err := s.Log(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Action != "c" || got[2].Action != "a" {
		t.Fatalf("want c,b,a order, got %+v", got)
	}
	want := AuditEvent{Time: base.Add(2 * time.Minute), GuestID: "g1", GuestLabel: "Bob",
		Action: "c", Result: "ok", SourceIP: "1.2.3.4", SourceDevice: "iphone"}
	if !got[0].Time.Equal(want.Time) {
		t.Fatalf("time %v != %v", got[0].Time, want.Time)
	}
	got[0].Time = want.Time
	if got[0] != want {
		t.Fatalf("got %+v want %+v", got[0], want)
	}
}

func TestAuditFilterByGuestAndAction(t *testing.T) {
	s := openTestAudit(t)
	now := time.Now()
	s.Log(AuditEvent{Time: now, GuestID: "g1", Action: "gate"})
	s.Log(AuditEvent{Time: now, GuestID: "g1", Action: "door"})
	s.Log(AuditEvent{Time: now, GuestID: "g2", Action: "gate"})

	byGuest, _ := s.List(AuditFilter{GuestID: "g1"})
	byAction, _ := s.List(AuditFilter{Action: "gate"})
	both, _ := s.List(AuditFilter{GuestID: "g1", Action: "gate"})
	if len(byGuest) != 2 || len(byAction) != 2 || len(both) != 1 {
		t.Fatalf("got %d %d %d, want 2 2 1", len(byGuest), len(byAction), len(both))
	}
}

func TestAuditPruneDropsOld(t *testing.T) {
	s := openTestAudit(t)
	day := 24 * time.Hour
	s.Log(AuditEvent{Time: time.Now().Add(-61 * day), Action: "old"})
	s.Log(AuditEvent{Time: time.Now().Add(-59 * day), Action: "recent"})

	n, err := s.Prune(time.Now().Add(-60 * day))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, err %v; want 1", n, err)
	}
	got, _ := s.List(AuditFilter{})
	if len(got) != 1 || got[0].Action != "recent" {
		t.Fatalf("got %+v", got)
	}
}

func TestAuditNilStoreIsNoop(t *testing.T) {
	var s *AuditStore
	if err := s.Log(AuditEvent{}); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(AuditFilter{})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	if n, err := s.Prune(time.Now()); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

func TestAuditListEmptyReturnsEmptySlice(t *testing.T) {
	got, err := openTestAudit(t).List(AuditFilter{})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %#v, %v", got, err)
	}
}
