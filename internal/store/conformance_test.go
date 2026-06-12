package store

import (
	"path/filepath"
	"testing"
	"time"
)

// storeFactory builds a fresh, empty Store for a subtest.
type storeFactory struct {
	name string
	make func(t *testing.T) Store
}

func backends(t *testing.T) []storeFactory {
	return []storeFactory{
		{"json", func(t *testing.T) Store {
			s, err := NewJSONStore(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		}},
		{"sqlite", func(t *testing.T) Store {
			s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			return s
		}},
	}
}

// TestConformance runs identical behavioral checks against every backend so the
// JSON and SQLite engines are guaranteed to agree.
func TestConformance(t *testing.T) {
	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			t.Run("HeartbeatAccounting", func(t *testing.T) { conformHeartbeat(t, b.make(t)) })
			t.Run("SwitchMachineFree", func(t *testing.T) { conformSwitchMachine(t, b.make(t)) })
			t.Run("GrantNeverNegative", func(t *testing.T) { conformGrantClamp(t, b.make(t)) })
			t.Run("SetIsAbsolute", func(t *testing.T) { conformSet(t, b.make(t)) })
			t.Run("UnknownUser", func(t *testing.T) { conformUnknownUser(t, b.make(t)) })
			t.Run("ListSorted", func(t *testing.T) { conformListSorted(t, b.make(t)) })
			t.Run("PlayingWindow", func(t *testing.T) { conformPlayingWindow(t, b.make(t)) })
			t.Run("ClockSkew", func(t *testing.T) { conformClockSkew(t, b.make(t)) })
		})
	}
}

func conformHeartbeat(t *testing.T, s Store) {
	const maxGap = 60 * time.Second
	if _, err := s.Grant("kid", 120, "chores"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)

	if a, rem, _ := s.Heartbeat("kid", "GAMEPC", t0, maxGap); !a || rem != 120 {
		t.Fatalf("first beat: allowed=%v rem=%d, want true/120", a, rem)
	}
	if a, rem, _ := s.Heartbeat("kid", "GAMEPC", t0.Add(30*time.Second), maxGap); !a || rem != 90 {
		t.Fatalf("beat+30: allowed=%v rem=%d, want true/90", a, rem)
	}
	// PC off for an hour: charge capped at maxGap.
	if a, rem, _ := s.Heartbeat("kid", "GAMEPC", t0.Add(time.Hour), maxGap); !a || rem != 30 {
		t.Fatalf("capped beat: allowed=%v rem=%d, want true/30", a, rem)
	}
	if a, rem, _ := s.Heartbeat("kid", "GAMEPC", t0.Add(time.Hour+40*time.Second), maxGap); a || rem != 0 {
		t.Fatalf("drain: allowed=%v rem=%d, want false/0", a, rem)
	}
}

func conformSwitchMachine(t *testing.T, s Store) {
	const maxGap = 60 * time.Second
	s.Grant("kid", 100, "x")
	t0 := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	s.Heartbeat("kid", "PC-A", t0, maxGap)
	if _, rem, _ := s.Heartbeat("kid", "PC-B", t0.Add(30*time.Second), maxGap); rem != 100 {
		t.Fatalf("machine switch charged: rem=%d, want 100", rem)
	}
}

func conformGrantClamp(t *testing.T, s Store) {
	s.Grant("kid", 60, "x")
	if rem, _ := s.Grant("kid", -600, "penalty"); rem != 0 {
		t.Fatalf("balance went negative: %d", rem)
	}
}

func conformSet(t *testing.T, s Store) {
	s.Grant("kid", 600, "x")
	if rem, _ := s.SetBalance("kid", 120, "reset"); rem != 120 {
		t.Fatalf("set: rem=%d, want 120", rem)
	}
	if rem, _ := s.SetBalance("kid", -5, "lock"); rem != 0 {
		t.Fatalf("negative set: rem=%d, want 0", rem)
	}
}

func conformUnknownUser(t *testing.T, s Store) {
	if _, err := s.Status("ghost", time.Now()); err != ErrUnknownUser {
		t.Fatalf("got %v, want ErrUnknownUser", err)
	}
}

func conformListSorted(t *testing.T, s Store) {
	s.EnsureUser("zoe")
	s.EnsureUser("alice")
	s.EnsureUser("mike")
	list, err := s.List(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alice", "mike", "zoe"}
	if len(list) != len(want) {
		t.Fatalf("List len=%d, want %d", len(list), len(want))
	}
	for i := range want {
		if list[i].User != want[i] {
			t.Fatalf("List[%d]=%q, want %q", i, list[i].User, want[i])
		}
	}
}

func conformPlayingWindow(t *testing.T, s Store) {
	const maxGap = 60 * time.Second
	s.Grant("kid", 600, "x")
	t0 := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	s.Heartbeat("kid", "PC", t0, maxGap)
	if st, _ := s.Status("kid", t0.Add(10*time.Second)); !st.PlayingNow || st.Machine != "PC" {
		t.Fatalf("expected playing on PC, got %+v", st)
	}
	if st, _ := s.Status("kid", t0.Add(5*time.Minute)); st.PlayingNow || st.Machine != "" {
		t.Fatalf("expected idle, got %+v", st)
	}
}

func conformClockSkew(t *testing.T, s Store) {
	const maxGap = 60 * time.Second
	s.Grant("kid", 100, "x")
	t0 := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	s.Heartbeat("kid", "PC", t0, maxGap)
	if _, rem, _ := s.Heartbeat("kid", "PC", t0.Add(-30*time.Second), maxGap); rem != 100 {
		t.Fatalf("clock skew charged: rem=%d, want 100", rem)
	}
}

func TestSQLitePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Grant("kid", 300, "chores")
	s1.Close()

	s2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	st, err := s2.Status("kid", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st.RemainingSeconds != 300 {
		t.Fatalf("reopen: rem=%d, want 300", st.RemainingSeconds)
	}
}

func TestMigrateBalancesJSONToSQLite(t *testing.T) {
	src, err := NewJSONStore(filepath.Join(t.TempDir(), "src.json"))
	if err != nil {
		t.Fatal(err)
	}
	src.Grant("leo", 600, "x")
	src.Grant("mia", 1200, "y")

	dst, err := NewSQLiteStore(filepath.Join(t.TempDir(), "dst.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	n, err := MigrateBalances(dst, src)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("migrated %d users, want 2", n)
	}
	if st, _ := dst.Status("leo", time.Now()); st.RemainingSeconds != 600 {
		t.Fatalf("leo: rem=%d, want 600", st.RemainingSeconds)
	}
	if st, _ := dst.Status("mia", time.Now()); st.RemainingSeconds != 1200 {
		t.Fatalf("mia: rem=%d, want 1200", st.RemainingSeconds)
	}
}

func TestKindInference(t *testing.T) {
	cases := map[string]string{
		"rewardd.json": "json",
		"data.db":      "sqlite",
		"x.sqlite":     "sqlite",
		"y.sqlite3":    "sqlite",
		"noext":        "json",
	}
	for path, want := range cases {
		if got := Kind("", path); got != want {
			t.Fatalf("Kind(%q)=%q, want %q", path, got, want)
		}
	}
	if got := Kind("sqlite", "whatever.json"); got != "sqlite" {
		t.Fatalf("explicit kind ignored: got %q", got)
	}
}
