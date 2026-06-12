package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientHeartbeat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/heartbeat" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["user"] != "leo" || body["machine"] != "GAMEPC" {
			t.Errorf("bad body %v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"allowed": true, "remaining_seconds": 1234, "heartbeat_interval": 25,
		})
	}))
	defer srv.Close()

	cl := &client{http: srv.Client(), cfg: config{server: srv.URL, token: "secret", user: "leo", machine: "GAMEPC"}}
	r, err := cl.heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Allowed || r.Remaining != 1234 || r.Interval != 25 {
		t.Fatalf("heartbeat resp = %+v", r)
	}
}

func TestClientUnlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["pin"] == "4242" {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "remaining_seconds": 1800})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"ok": false})
	}))
	defer srv.Close()

	cl := &client{http: srv.Client(), cfg: config{server: srv.URL, token: "x", user: "leo"}}

	rem, ok, err := cl.unlock("4242")
	if err != nil || !ok || rem != 1800 {
		t.Fatalf("good pin: rem=%d ok=%v err=%v", rem, ok, err)
	}
	// 403 carries ok:false in the body; the client surfaces it as an error
	// (non-200) so the caller falls through to the local PIN path.
	_, ok, err = cl.unlock("0000")
	if err == nil {
		t.Fatalf("bad pin should be a transport error (non-200), got ok=%v", ok)
	}
}

func TestClientServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	cl := &client{http: srv.Client(), cfg: config{server: srv.URL, token: "x", user: "leo"}}
	if _, err := cl.heartbeat(); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestControllerConcurrency(t *testing.T) {
	c := &LockController{}
	c.setLocked(true)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.setRemaining(int64(n))
			c.setLocked(n%2 == 0)
			_ = c.Locked()
			_ = c.Remaining()
			c.online.Store(true)
			_ = c.Online()
		}(i)
	}
	wg.Wait()
}

func TestControllerSubmit(t *testing.T) {
	c := &LockController{}
	// No submit func wired: SubmitPIN must be safe and return false.
	if c.SubmitPIN("1234") {
		t.Fatal("nil submit should return false")
	}
	c.submit = func(pin string) bool { return pin == "open" }
	if !c.SubmitPIN("open") {
		t.Fatal("submit should accept matching pin")
	}
	if c.SubmitPIN("nope") {
		t.Fatal("submit should reject wrong pin")
	}
}

func TestTickCountdown(t *testing.T) {
	cases := []struct {
		rem     int64
		wantRem int64
		wantLk  bool
	}{
		{100, 99, false},
		{2, 1, false},
		{1, 0, true},
		{0, 0, true},
	}
	for _, c := range cases {
		gotRem, gotLk := tickCountdown(c.rem)
		if gotRem != c.wantRem || gotLk != c.wantLk {
			t.Fatalf("tickCountdown(%d) = (%d,%v), want (%d,%v)", c.rem, gotRem, gotLk, c.wantRem, c.wantLk)
		}
	}
}

func TestWarnTrackerFiresOnceOnCrossing(t *testing.T) {
	var w warnTracker
	// Plenty of time: no warning.
	if w.check(false, 600) {
		t.Fatal("should not warn above threshold")
	}
	// Drops to the threshold: fire exactly once.
	if !w.check(false, lowTimeWarnSeconds) {
		t.Fatal("should warn when crossing the threshold")
	}
	if w.check(false, 299) || w.check(false, 100) {
		t.Fatal("should not warn again while still low")
	}
	// Lock at zero: no warning, and the lock re-arms it.
	if w.check(true, 0) {
		t.Fatal("should not warn while locked")
	}
	// Unlocked again with low time (e.g. small grant): warn again.
	if !w.check(false, 240) {
		t.Fatal("should warn again after re-arm via lock")
	}
}

func TestWarnTrackerRearmsOnGrant(t *testing.T) {
	var w warnTracker
	if !w.check(false, 200) {
		t.Fatal("should warn on first low-time observation")
	}
	// Parent grants time, remaining jumps above the threshold: re-arm.
	if w.check(false, 1800) {
		t.Fatal("should not warn above threshold")
	}
	if !w.check(false, 300) {
		t.Fatal("should warn again on the next descent")
	}
}

func TestWarnTrackerSkipsZeroAndLocked(t *testing.T) {
	var w warnTracker
	if w.check(false, 0) {
		t.Fatal("zero remaining locks instead of warning")
	}
	if w.check(true, 120) {
		t.Fatal("should never warn while locked")
	}
}

func TestHeartbeatLoopRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"allowed": true, "remaining_seconds": 60, "heartbeat_interval": 1})
	}))
	defer srv.Close()
	cl := &client{http: srv.Client(), cfg: config{server: srv.URL, token: "x", user: "leo"}}
	ctrl := &LockController{}
	ctrl.setLocked(true)

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	interval := new(atomic.Int64)
	interval.Store(1)
	go func() {
		heartbeatLoop(ctx, cl, ctrl, interval)
		close(done)
	}()

	// The immediate first beat should reconcile from the server (unlock).
	deadline := time.After(2 * time.Second)
	for ctrl.Locked() {
		select {
		case <-deadline:
			t.Fatal("heartbeatLoop never unlocked from server response")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if ctrl.Remaining() != 60 {
		t.Fatalf("remaining=%d, want 60 from server", ctrl.Remaining())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeatLoop did not return after context cancel")
	}
}
