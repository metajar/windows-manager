//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceName is the Windows service key for the brain.
const serviceName = "rewardd-agent-svc"

// runService runs the brain. Launched by the Service Control Manager it speaks
// the SCM protocol; launched from a console (for debugging) it runs in the
// foreground until Ctrl+C.
func runService(cfg config, configPath string) {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		log.Fatalf("could not determine service context: %v", err)
	}
	b := &brain{cfg: cfg, configPath: configPath}
	if isSvc {
		if err := svc.Run(serviceName, b); err != nil {
			log.Fatalf("service failed: %v", err)
		}
		return
	}
	log.Println("running brain in the foreground (not under the SCM); Ctrl+C to stop")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.serve(ctx)
}

// brain is the SYSTEM-side accounting + lock authority. It owns the server
// token, reconciles the time bank, decides locked/unlocked, serves that state
// to per-session faces over the pipe, and keeps a face alive in the active
// session so killing the UI never buys free play.
type brain struct {
	cfg        config
	configPath string
}

// Execute implements svc.Handler.
func (b *brain) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { b.serve(ctx); close(done) }()

	status <- svc.Status{State: svc.Running, Accepts: accepts}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		case <-done:
			status <- svc.Status{State: svc.Stopped}
			return false, 0
		}
	}
}

// serve runs the heartbeat engine, the pipe server, and the face watchdog until
// the context is cancelled.
func (b *brain) serve(ctx context.Context) {
	cl, ctrl, interval := newEngine(b.cfg)
	startEngine(ctx, cl, ctrl, interval)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.servePipe(ctx, ctrl) }()
	go func() { defer wg.Done(); b.watchFace(ctx) }()
	wg.Wait()
}

// servePipe accepts face connections and serves each one until it disconnects.
func (b *brain) servePipe(ctx context.Context, ctrl *LockController) {
	l, err := listenPipe()
	if err != nil {
		log.Printf("pipe listen failed: %v", err)
		return
	}
	go func() { <-ctx.Done(); l.Close() }()

	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("pipe accept: %v", err)
				return
			}
		}
		go serveFaceConn(ctx, conn, ctrl)
	}
}

// serveFaceConn pushes lock-state changes to one face and applies its PIN
// attempts via the shared controller (which talks to the server / local PIN).
func serveFaceConn(ctx context.Context, conn net.Conn, ctrl *LockController) {
	defer conn.Close()

	// Inbound: PIN attempts.
	go func() {
		mr := newMessageReader(conn)
		for {
			m, err := mr.read()
			if err != nil {
				return
			}
			if m.Type == msgPIN {
				ok := ctrl.SubmitPIN(m.PIN)
				if err := writeMessage(conn, pinResultMessage(ok)); err != nil {
					return
				}
			}
		}
	}()

	// Outbound: push state whenever it changes (polled at 250ms).
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	var last ipcMessage
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := stateMessage(ctrl.Locked(), ctrl.Remaining(), ctrl.Online())
			if first || cur != last {
				if err := writeMessage(conn, cur); err != nil {
					return
				}
				last, first = cur, false
			}
		}
	}
}

// watchFace keeps a face process alive in the active console session. It blocks
// while a face runs and relaunches it after it exits, so terminating the UI
// only blanks the screen for a moment before it returns.
func (b *brain) watchFace(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := launchFaceInActiveSession(ctx, b.configPath); err != nil {
			log.Printf("launch face: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// installService writes the config and registers the brain as an auto-start
// service running as LocalSystem, then starts it.
func installService(cfg config, configPath string) error {
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	// Idempotent: a reinstall (or a service left behind by an older installer)
	// just refreshes the config and bounces the existing service.
	if s, err := m.OpenService(serviceName); err == nil {
		defer s.Close()
		log.Printf("service %q already exists, refreshing config and restarting", serviceName)
		stopService(s)
		if err := s.Start(); err != nil {
			return fmt.Errorf("restart service: %w", err)
		}
		log.Printf("service %q running with config at %s", serviceName, configPath)
		return nil
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName:  "rewardd agent (brain)",
		Description:  "Owns the rewardd time bank, decides screen lock state, and keeps the per-session lock UI alive.",
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}, "-service", "-config", configPath)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	log.Printf("service %q installed and started; config at %s", serviceName, configPath)
	return nil
}

// stopService asks the SCM to stop a service and waits (up to ~15s) until it
// actually reaches Stopped. A bare Control(Stop) followed by Start/Delete races
// the shutdown: the SCM rejects both while the service is still StopPending.
func stopService(s *mgr.Service) {
	status, err := s.Control(svc.Stop)
	if err != nil {
		log.Printf("stop service (ignored): %v", err)
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for status.State != svc.Stopped {
		if time.Now().After(deadline) {
			log.Printf("service did not stop within 15s, continuing anyway")
			return
		}
		time.Sleep(300 * time.Millisecond)
		if status, err = s.Query(); err != nil {
			log.Printf("query service while stopping (ignored): %v", err)
			return
		}
	}
}

// uninstallService stops and deletes the brain service and removes the config.
func uninstallService(configPath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		log.Printf("service %q not installed (ignored)", serviceName)
	} else {
		defer s.Close()
		stopService(s)
		if err := s.Delete(); err != nil {
			return fmt.Errorf("delete service: %w", err)
		}
	}
	if configPath != "" {
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove config: %w", err)
		}
	}
	return nil
}
