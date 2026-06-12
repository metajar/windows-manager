package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *JSONStore {
	t.Helper()
	s, err := NewJSONStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHeartbeatAccounting(t *testing.T) {
	s := newTestStore(t)
	const maxGap = 60 * time.Second

	if _, err := s.Grant("kid", 120, "chores"); err != nil { // 2 minutes
		t.Fatal(err)
	}

	t0 := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)

	// First beat is free.
	allowed, rem, _ := s.Heartbeat("kid", "GAMEPC", t0, maxGap)
	if !allowed || rem != 120 {
		t.Fatalf("first beat: allowed=%v rem=%d, want true/120", allowed, rem)
	}

	// 30s later: charge 30.
	allowed, rem, _ = s.Heartbeat("kid", "GAMEPC", t0.Add(30*time.Second), maxGap)
	if !allowed || rem != 90 {
		t.Fatalf("beat+30: allowed=%v rem=%d, want true/90", allowed, rem)
	}

	// PC off for an hour, then a beat: capped at maxGap (60s), not 3600.
	allowed, rem, _ = s.Heartbeat("kid", "GAMEPC", t0.Add(time.Hour), maxGap)
	if !allowed || rem != 30 {
		t.Fatalf("beat after gap: allowed=%v rem=%d, want true/30 (capped)", allowed, rem)
	}

	// Drain past zero: locks and clamps at 0.
	allowed, rem, _ = s.Heartbeat("kid", "GAMEPC", t0.Add(time.Hour+40*time.Second), maxGap)
	if allowed || rem != 0 {
		t.Fatalf("drain: allowed=%v rem=%d, want false/0", allowed, rem)
	}
}

func TestSwitchMachineDoesNotCharge(t *testing.T) {
	s := newTestStore(t)
	const maxGap = 60 * time.Second
	s.Grant("kid", 100, "test")
	t0 := time.Now()

	s.Heartbeat("kid", "PC-A", t0, maxGap)
	// Beat from a different machine: treated as a new free session, no charge.
	_, rem, _ := s.Heartbeat("kid", "PC-B", t0.Add(30*time.Second), maxGap)
	if rem != 100 {
		t.Fatalf("machine switch charged time: rem=%d, want 100", rem)
	}
}

func TestGrantNeverNegative(t *testing.T) {
	s := newTestStore(t)
	s.Grant("kid", 60, "x")
	rem, _ := s.Grant("kid", -600, "penalty")
	if rem != 0 {
		t.Fatalf("balance went negative: %d", rem)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s1, _ := NewJSONStore(path)
	s1.Grant("kid", 300, "chores")

	s2, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s2.Status("kid", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st.RemainingSeconds != 300 {
		t.Fatalf("reload: rem=%d, want 300", st.RemainingSeconds)
	}
}

func TestSetBalanceIsAbsolute(t *testing.T) {
	s := newTestStore(t)
	s.Grant("kid", 600, "x")
	rem, err := s.SetBalance("kid", 120, "reset")
	if err != nil {
		t.Fatal(err)
	}
	if rem != 120 {
		t.Fatalf("set: rem=%d, want 120", rem)
	}
	// Negative set clamps to zero.
	rem, _ = s.SetBalance("kid", -50, "lock")
	if rem != 0 {
		t.Fatalf("negative set: rem=%d, want 0", rem)
	}
}

func TestStatusUnknownUser(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Status("ghost", time.Now()); err != ErrUnknownUser {
		t.Fatalf("got %v, want ErrUnknownUser", err)
	}
}

func TestEnsureUserCreatesZeroBalance(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnsureUser("newkid"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status("newkid", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st.RemainingSeconds != 0 || st.Allowed {
		t.Fatalf("new user: rem=%d allowed=%v, want 0/false", st.RemainingSeconds, st.Allowed)
	}
}

func TestListSortedByName(t *testing.T) {
	s := newTestStore(t)
	s.EnsureUser("zoe")
	s.EnsureUser("alice")
	s.EnsureUser("mike")
	list, err := s.List(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := []string{list[0].User, list[1].User, list[2].User}
	want := []string{"alice", "mike", "zoe"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List order = %v, want %v", got, want)
		}
	}
}

func TestPlayingWindow(t *testing.T) {
	s := newTestStore(t)
	const maxGap = 60 * time.Second
	s.Grant("kid", 600, "x")
	t0 := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	s.Heartbeat("kid", "PC", t0, maxGap)

	// Just after the beat: counts as playing.
	st, _ := s.Status("kid", t0.Add(10*time.Second))
	if !st.PlayingNow || st.Machine != "PC" {
		t.Fatalf("expected playing on PC, got playing=%v machine=%q", st.PlayingNow, st.Machine)
	}
	// Long after the last beat: idle, machine hidden.
	st, _ = s.Status("kid", t0.Add(5*time.Minute))
	if st.PlayingNow || st.Machine != "" {
		t.Fatalf("expected idle, got playing=%v machine=%q", st.PlayingNow, st.Machine)
	}
}

func TestClockSkewChargesNothing(t *testing.T) {
	s := newTestStore(t)
	const maxGap = 60 * time.Second
	s.Grant("kid", 100, "x")
	t0 := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	s.Heartbeat("kid", "PC", t0, maxGap)
	// A beat timestamped in the past (clock went backwards) must not credit time.
	_, rem, _ := s.Heartbeat("kid", "PC", t0.Add(-30*time.Second), maxGap)
	if rem != 100 {
		t.Fatalf("clock skew charged: rem=%d, want 100", rem)
	}
}

func TestGrantLogIsCapped(t *testing.T) {
	s := newTestStore(t)
	s.maxGrants = 10
	for i := 0; i < 50; i++ {
		if _, err := s.Grant("kid", 1, "tick"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.state.Grants); got != 10 {
		t.Fatalf("grant log not capped: len=%d, want 10", got)
	}
}

func TestCorruptStoreIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewJSONStore(path); err == nil {
		t.Fatal("expected error loading corrupt store, got nil")
	}
}
