package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rewardd/internal/store"
)

const testToken = "test-token-please-ignore"

func newTestServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	st, err := store.NewJSONStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(serverConfig{
		token:         testToken,
		parentPIN:     "4242",
		unlockSeconds: 30 * 60,
		heartbeatSec:  20,
		maxGap:        120 * time.Second,
	}, st)
	return s, s.routes()
}

func do(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return m
}

func TestHealthAndVersion(t *testing.T) {
	_, h := newTestServer(t)

	w := do(t, h, http.MethodGet, "/healthz", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("healthz: code=%d body=%q", w.Code, w.Body.String())
	}

	w = do(t, h, http.MethodGet, "/version", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("version: code=%d", w.Code)
	}
	m := decodeMap(t, w)
	if _, ok := m["version"]; !ok {
		t.Fatalf("version payload missing version field: %v", m)
	}
}

func TestAuthRequired(t *testing.T) {
	_, h := newTestServer(t)

	for _, tc := range []struct{ token string }{{""}, {"wrong"}, {testToken + "x"}} {
		w := do(t, h, http.MethodGet, "/api/v1/users", tc.token, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("token=%q: code=%d, want 401", tc.token, w.Code)
		}
	}
	w := do(t, h, http.MethodGet, "/api/v1/users", testToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("valid token: code=%d, want 200", w.Code)
	}
}

func TestGrantThenStatus(t *testing.T) {
	_, h := newTestServer(t)

	w := do(t, h, http.MethodPost, "/api/v1/grant", testToken,
		map[string]any{"user": "leo", "minutes": 30, "reason": "chores"})
	if w.Code != http.StatusOK {
		t.Fatalf("grant: code=%d body=%q", w.Code, w.Body.String())
	}
	if got := decodeMap(t, w)["remaining_seconds"].(float64); got != 1800 {
		t.Fatalf("grant remaining=%v, want 1800", got)
	}

	w = do(t, h, http.MethodGet, "/api/v1/status?user=leo", testToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: code=%d", w.Code)
	}
	m := decodeMap(t, w)
	if m["remaining_seconds"].(float64) != 1800 || m["allowed"] != true {
		t.Fatalf("status=%v, want 1800/allowed", m)
	}
}

func TestGrantSecondsField(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, http.MethodPost, "/api/v1/grant", testToken,
		map[string]any{"user": "leo", "seconds": 90})
	if got := decodeMap(t, w)["remaining_seconds"].(float64); got != 90 {
		t.Fatalf("seconds grant remaining=%v, want 90", got)
	}
}

func TestSetIsAbsolute(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, http.MethodPost, "/api/v1/grant", testToken, map[string]any{"user": "leo", "minutes": 30})
	w := do(t, h, http.MethodPost, "/api/v1/set", testToken, map[string]any{"user": "leo", "minutes": 5})
	if got := decodeMap(t, w)["remaining_seconds"].(float64); got != 300 {
		t.Fatalf("set remaining=%v, want 300", got)
	}
}

func TestHeartbeatFlow(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, http.MethodPost, "/api/v1/grant", testToken, map[string]any{"user": "leo", "seconds": 100})

	w := do(t, h, http.MethodPost, "/api/v1/heartbeat", testToken,
		map[string]any{"user": "leo", "machine": "GAMEPC"})
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: code=%d body=%q", w.Code, w.Body.String())
	}
	m := decodeMap(t, w)
	if m["allowed"] != true {
		t.Fatalf("first beat not allowed: %v", m)
	}
	if m["heartbeat_interval"].(float64) != 20 {
		t.Fatalf("interval=%v, want 20", m["heartbeat_interval"])
	}
}

func TestUnlockPIN(t *testing.T) {
	_, h := newTestServer(t)

	// Wrong PIN -> 403, ok:false.
	w := do(t, h, http.MethodPost, "/api/v1/unlock", testToken,
		map[string]any{"user": "leo", "pin": "0000"})
	if w.Code != http.StatusForbidden || decodeMap(t, w)["ok"] != false {
		t.Fatalf("wrong pin: code=%d body=%q", w.Code, w.Body.String())
	}

	// Right PIN -> grants default unlock minutes.
	w = do(t, h, http.MethodPost, "/api/v1/unlock", testToken,
		map[string]any{"user": "leo", "pin": "4242"})
	if w.Code != http.StatusOK {
		t.Fatalf("right pin: code=%d body=%q", w.Code, w.Body.String())
	}
	m := decodeMap(t, w)
	if m["ok"] != true || m["remaining_seconds"].(float64) != 1800 {
		t.Fatalf("unlock=%v, want ok/1800", m)
	}
}

func TestUnlockWithoutConfiguredPIN(t *testing.T) {
	st, _ := store.NewJSONStore(filepath.Join(t.TempDir(), "s.json"))
	s := newServer(serverConfig{token: testToken, unlockSeconds: 1800, heartbeatSec: 20, maxGap: time.Minute}, st)
	h := s.routes()
	w := do(t, h, http.MethodPost, "/api/v1/unlock", testToken, map[string]any{"user": "leo", "pin": "x"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no pin configured: code=%d, want 503", w.Code)
	}
}

func TestBadRequests(t *testing.T) {
	_, h := newTestServer(t)

	// Missing user.
	w := do(t, h, http.MethodPost, "/api/v1/grant", testToken, map[string]any{"minutes": 5})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing user: code=%d, want 400", w.Code)
	}
	// Malformed JSON.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/grant", strings.NewReader("{bad"))
	r.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: code=%d, want 400", w.Code)
	}
	// Unknown user status.
	w = do(t, h, http.MethodGet, "/api/v1/status?user=ghost", testToken, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown user: code=%d, want 404", w.Code)
	}
}

func TestDashboardServedAtRootOnly(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, http.MethodGet, "/", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("dashboard: code=%d", w.Code)
	}
	w = do(t, h, http.MethodGet, "/nope", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown path: code=%d, want 404", w.Code)
	}
}
