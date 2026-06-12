// Command server is the reward/time-bank API. It owns the balances and is the
// source of truth for how much screen time each kid has left.
//
// Config via environment:
//
//	REWARDD_ADDR        listen address            (default ":8080")
//	REWARDD_DB          state file path           (default "rewardd.json")
//	REWARDD_TOKEN       bearer token for the API  (required)
//	REWARDD_PARENT_PIN  PIN for /unlock overrides (required for unlock)
//	REWARDD_UNLOCK_MIN  minutes granted per PIN unlock (default 30)
//	REWARDD_HEARTBEAT   client heartbeat seconds  (default 20)
//	REWARDD_MAXGAP      max seconds charged per beat (default 120)
//	REWARDD_SHUTDOWN    graceful shutdown timeout seconds (default 10)
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rewardd/internal/buildinfo"
	"rewardd/internal/store"
)

type serverConfig struct {
	addr          string
	db            string
	storeKind     string
	token         string
	parentPIN     string
	unlockSeconds int64
	heartbeatSec  int
	maxGap        time.Duration
	shutdown      time.Duration
}

type server struct {
	st            store.Store
	token         string
	parentPIN     string
	unlockSeconds int64
	heartbeatSec  int
	maxGap        time.Duration
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// loadServerConfig reads configuration from the environment and validates it.
func loadServerConfig() (serverConfig, error) {
	token := os.Getenv("REWARDD_TOKEN")
	if token == "" {
		return serverConfig{}, errors.New("REWARDD_TOKEN is required (the API bearer token)")
	}
	return serverConfig{
		addr:          env("REWARDD_ADDR", ":8080"),
		db:            env("REWARDD_DB", "rewardd.json"),
		storeKind:     os.Getenv("REWARDD_STORE"),
		token:         token,
		parentPIN:     os.Getenv("REWARDD_PARENT_PIN"),
		unlockSeconds: int64(envInt("REWARDD_UNLOCK_MIN", 30)) * 60,
		heartbeatSec:  envInt("REWARDD_HEARTBEAT", 20),
		maxGap:        time.Duration(envInt("REWARDD_MAXGAP", 120)) * time.Second,
		shutdown:      time.Duration(envInt("REWARDD_SHUTDOWN", 10)) * time.Second,
	}, nil
}

func newServer(cfg serverConfig, st store.Store) *server {
	return &server{
		st:            st,
		token:         cfg.token,
		parentPIN:     cfg.parentPIN,
		unlockSeconds: cfg.unlockSeconds,
		heartbeatSec:  cfg.heartbeatSec,
		maxGap:        cfg.maxGap,
	}
}

// routes builds the HTTP handler. Split out from main so tests can exercise the
// full routing/auth stack with httptest.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, buildinfo.Get())
	})
	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("POST /api/v1/heartbeat", s.auth(s.handleHeartbeat))
	mux.HandleFunc("POST /api/v1/grant", s.auth(s.handleGrant))
	mux.HandleFunc("POST /api/v1/set", s.auth(s.handleSet))
	mux.HandleFunc("POST /api/v1/unlock", s.auth(s.handleUnlock))
	mux.HandleFunc("GET /api/v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /api/v1/users", s.auth(s.handleList))
	return mux
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	migrateJSON := flag.String("migrate-from-json", "", "import balances from a JSON db into the configured store, then exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.String())
		return
	}

	cfg, err := loadServerConfig()
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(cfg.storeKind, cfg.db)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	if *migrateJSON != "" {
		src, err := store.Open("json", *migrateJSON)
		if err != nil {
			log.Fatalf("open source json store: %v", err)
		}
		n, err := store.MigrateBalances(st, src)
		if err != nil {
			log.Fatalf("migrate: %v", err)
		}
		log.Printf("migrated %d users from %s into %s", n, *migrateJSON, cfg.db)
		return
	}

	s := newServer(cfg, st)

	srv := &http.Server{
		Addr:         cfg.addr,
		Handler:      s.routes(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Printf("%s", buildinfo.String())
		log.Printf("listening on %s (heartbeat=%ds, maxgap=%s, store=%s db=%s)",
			cfg.addr, s.heartbeatSec, s.maxGap, store.Kind(cfg.storeKind, cfg.db), cfg.db)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		log.Fatalf("server error: %v", err)
	case <-ctx.Done():
		log.Println("shutdown requested, draining connections...")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdown)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Fatalf("graceful shutdown failed: %v", err)
		}
		log.Println("stopped cleanly")
	}
}

// auth enforces a constant-time bearer token on API routes.
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + s.token)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// POST /api/v1/heartbeat  {user, machine}
func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User    string `json:"user"`
		Machine string `json:"machine"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.User == "" {
		http.Error(w, "user required", http.StatusBadRequest)
		return
	}
	allowed, rem, err := s.st.Heartbeat(req.User, req.Machine, time.Now(), s.maxGap)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"allowed":            allowed,
		"remaining_seconds":  rem,
		"heartbeat_interval": s.heartbeatSec,
	})
}

// POST /api/v1/grant  {user, minutes, reason}
func (s *server) handleGrant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User    string  `json:"user"`
		Minutes float64 `json:"minutes"`
		Seconds int64   `json:"seconds"`
		Reason  string  `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.User == "" {
		http.Error(w, "user required", http.StatusBadRequest)
		return
	}
	secs := req.Seconds + int64(req.Minutes*60)
	rem, err := s.st.Grant(req.User, secs, req.Reason)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": req.User, "remaining_seconds": rem})
}

// POST /api/v1/set  {user, minutes}  (absolute)
func (s *server) handleSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User    string  `json:"user"`
		Minutes float64 `json:"minutes"`
		Reason  string  `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.User == "" {
		http.Error(w, "user required", http.StatusBadRequest)
		return
	}
	rem, err := s.st.SetBalance(req.User, int64(req.Minutes*60), req.Reason)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": req.User, "remaining_seconds": rem})
}

// POST /api/v1/unlock  {user, pin, minutes?}  parent override
func (s *server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User    string  `json:"user"`
		PIN     string  `json:"pin"`
		Minutes float64 `json:"minutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.User == "" {
		http.Error(w, "user required", http.StatusBadRequest)
		return
	}
	if s.parentPIN == "" {
		http.Error(w, "parent pin not configured", http.StatusServiceUnavailable)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.PIN), []byte(s.parentPIN)) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false})
		return
	}
	secs := s.unlockSeconds
	if req.Minutes > 0 {
		secs = int64(req.Minutes * 60)
	}
	rem, err := s.st.Grant(req.User, secs, "parent unlock")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remaining_seconds": rem})
}

// GET /api/v1/status?user=NAME
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("user")
	if name == "" {
		http.Error(w, "user required", http.StatusBadRequest)
		return
	}
	st, err := s.st.Status(name, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// GET /api/v1/users
func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.List(time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(strings.TrimSpace(dashboardHTML)))
}
