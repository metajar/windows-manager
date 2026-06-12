// Command agent runs on the Windows gaming PC. It heartbeats the reward server,
// counts the kid's remaining time down locally for a smooth display, and when
// time runs out it raises a fullscreen lock overlay (see lock_windows.go) that
// only clears when the server grants more time or a parent enters the PIN.
//
// Flags (env var in parens) override defaults:
//
//	-server   (REWARDD_SERVER)  server base URL, e.g. http://192.168.1.10:8080
//	-token    (REWARDD_TOKEN)   API bearer token, must match the server
//	-user     (REWARDD_USER)    kid name, must match a server user
//	-machine  (REWARDD_MACHINE) machine label (default: hostname)
//	-pin      (REWARDD_PIN)     local emergency PIN, used only when offline
//	-interval (REWARDD_INTERVAL) heartbeat seconds (default 20; server may override)
//	-grace    (REWARDD_GRACE)   minutes granted by a local emergency unlock (default 30)
//	-install                    register a logon scheduled task and exit (Windows)
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"rewardd/internal/buildinfo"
)

type client struct {
	http *http.Client
	cfg  config
}

type heartbeatResp struct {
	Allowed   bool  `json:"allowed"`
	Remaining int64 `json:"remaining_seconds"`
	Interval  int   `json:"heartbeat_interval"`
}

func (cl *client) post(path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, cl.cfg.server+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cl.cfg.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server %s: %s", path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (cl *client) heartbeat() (heartbeatResp, error) {
	var r heartbeatResp
	err := cl.post("/api/v1/heartbeat", map[string]string{
		"user": cl.cfg.user, "machine": cl.cfg.machine,
	}, &r)
	return r, err
}

func (cl *client) unlock(pin string) (remaining int64, ok bool, err error) {
	var r struct {
		OK        bool  `json:"ok"`
		Remaining int64 `json:"remaining_seconds"`
	}
	err = cl.post("/api/v1/unlock", map[string]any{
		"user": cl.cfg.user, "pin": pin,
	}, &r)
	return r.Remaining, r.OK, err
}

func main() {
	log.SetFlags(log.LstdFlags)
	res, err := resolveConfig(os.Args[1:], os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg := res.cfg

	switch res.action {
	case actionVersion:
		fmt.Println(buildinfo.String())
		return
	case actionWriteConfig:
		if err := writeConfigFile(res.configPath, cfg); err != nil {
			log.Fatalf("writeconfig: %v", err)
		}
		log.Printf("wrote config to %s", res.configPath)
		return
	case actionInstall:
		if err := installAutostart(cfg, res.configPath); err != nil {
			log.Fatalf("install: %v", err)
		}
		log.Println("autostart task installed")
		return
	case actionUninstall:
		if err := removeAutostart(res.configPath); err != nil {
			log.Fatalf("uninstall: %v", err)
		}
		log.Println("autostart task removed")
		return
	case actionInstallService:
		if err := installService(cfg, res.configPath); err != nil {
			log.Fatalf("install-service: %v", err)
		}
		log.Println("brain service installed")
		return
	case actionUninstallService:
		if err := uninstallService(res.configPath); err != nil {
			log.Fatalf("uninstall-service: %v", err)
		}
		log.Println("brain service removed")
		return
	case actionFace:
		// The face needs no server/token: it mirrors the brain over the pipe.
		defer logToFile(res.configPath, "face.log")()
		log.Printf("%s", buildinfo.String())
		runFace(res.configPath)
		return
	case actionService:
		if err := cfg.validate(); err != nil {
			log.Fatal(err)
		}
		defer logToFile(res.configPath, "brain.log")()
		log.Printf("%s", buildinfo.String())
		runService(cfg, res.configPath)
		return
	}

	if err := cfg.validate(); err != nil {
		log.Fatal(err)
	}
	log.Printf("%s", buildinfo.String())
	runStandalone(cfg)
}

// logToFile mirrors the standard logger into a file next to the config file,
// returning a close func. The brain runs as a session-0 service and the face
// runs windowless in the kid's session, so stderr for both is a black hole;
// these files are the only way to see why something fails in the field.
func logToFile(configPath, name string) func() {
	if configPath == "" {
		return func() {}
	}
	p := filepath.Join(filepath.Dir(configPath), name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("file log unavailable at %s: %v", p, err)
		return func() {}
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	return func() { f.Close() }
}

// newEngine builds the heartbeat client and the lock controller shared by the
// standalone agent and the brain service. The controller starts LOCKED and
// fail-closed: nothing unlocks until the server confirms time remains.
func newEngine(cfg config) (*client, *LockController, *atomic.Int64) {
	cl := &client{http: &http.Client{Timeout: 8 * time.Second}, cfg: cfg}
	ctrl := &LockController{}
	ctrl.setLocked(true)
	wireSubmit(cl, ctrl, cfg)
	interval := new(atomic.Int64)
	interval.Store(int64(cfg.interval))
	return cl, ctrl, interval
}

// wireSubmit installs the PIN-handling logic: try the server first (so grants
// bank centrally), then fall back to the local emergency PIN when offline.
func wireSubmit(cl *client, ctrl *LockController, cfg config) {
	ctrl.submit = func(pin string) bool {
		if rem, ok, err := cl.unlock(pin); err == nil {
			if ok {
				ctrl.setRemaining(rem)
				ctrl.setLocked(false)
				log.Printf("parent unlock via server, remaining=%ds", rem)
			}
			return ok
		}
		if cfg.pin != "" && subtle.ConstantTimeCompare([]byte(pin), []byte(cfg.pin)) == 1 {
			grant := int64(cfg.graceMin) * 60
			ctrl.setRemaining(grant)
			ctrl.setLocked(false)
			log.Printf("offline emergency unlock, granting %dm", cfg.graceMin)
			return true
		}
		return false
	}
}

// startEngine launches the heartbeat and local-countdown goroutines.
func startEngine(ctx context.Context, cl *client, ctrl *LockController, interval *atomic.Int64) {
	go heartbeatLoop(ctx, cl, ctrl, interval)
	go localCountdown(ctx, ctrl)
}

// runStandalone is the v1 deployment: a single per-session agent that both
// talks to the server and draws the overlay. A savvy kid can kill it; the
// service/face split (see -service / -face) closes that gap.
func runStandalone(cfg config) {
	cl, ctrl, interval := newEngine(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startEngine(ctx, cl, ctrl, interval)
	log.Printf("agent up (standalone): user=%s machine=%s server=%s", cfg.user, cfg.machine, cfg.server)
	// Blocks on Windows running the message loop; on other OSes logs state.
	runOverlay(ctx, ctrl)
}

// heartbeatLoop reconciles with the server. On success the server's remaining
// count and allowed flag are authoritative. On failure we stay on the last
// known values and let localCountdown keep ticking, so an outage still spends
// time and eventually locks rather than granting free play.
func heartbeatLoop(ctx context.Context, cl *client, ctrl *LockController, interval *atomic.Int64) {
	beat := func() {
		r, err := cl.heartbeat()
		if err != nil {
			ctrl.online.Store(false)
			log.Printf("heartbeat failed (offline mode): %v", err)
			return
		}
		ctrl.online.Store(true)
		ctrl.setRemaining(r.Remaining)
		ctrl.setLocked(!r.Allowed)
		if r.Interval > 0 && int64(r.Interval) != interval.Load() {
			interval.Store(int64(r.Interval))
		}
	}
	beat() // immediate first beat
	for {
		d := time.Duration(interval.Load()) * time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
			beat()
		}
	}
}

// localCountdown ticks once a second for a responsive display and prompt lock
// between heartbeats. Successful heartbeats overwrite remaining with the
// server's truth, so this never drifts while online.
func localCountdown(ctx context.Context, ctrl *LockController) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ctrl.Locked() {
				continue
			}
			rem, lock := tickCountdown(ctrl.Remaining())
			ctrl.setRemaining(rem)
			if lock {
				ctrl.setLocked(true)
				log.Println("time expired locally, locking")
			}
		}
	}
}

// tickCountdown advances the local clock by one second. It returns the new
// remaining value (clamped at zero) and whether the screen should lock. Pure so
// the spend-to-zero transition is unit testable without timers.
func tickCountdown(rem int64) (newRem int64, lock bool) {
	rem--
	if rem <= 0 {
		return 0, true
	}
	return rem, false
}
