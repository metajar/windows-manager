//go:build windows

package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// runFace is the per-session UI. It owns no secrets: it connects to the brain's
// pipe, mirrors the brain's lock state into the overlay, and forwards PIN
// attempts back to the brain. It starts LOCKED and stays locked whenever the
// pipe is down, so killing the brain or racing it at logon never reveals the
// desktop.
func runFace(configPath string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctrl := &LockController{}
	ctrl.setLocked(true)

	fc := &faceClient{ctrl: ctrl}
	ctrl.submit = fc.submitPIN

	go fc.run(ctx)

	log.Println("face up: connecting to brain over the pipe")
	runOverlay(ctx, ctrl)
}

// faceClient maintains the pipe connection and bridges it to the controller.
type faceClient struct {
	ctrl *LockController

	connMu sync.Mutex
	enc    *json.Encoder // writer for the live connection, nil when disconnected

	resMu  sync.Mutex
	result chan bool // set for the duration of one in-flight PIN attempt
}

// run keeps a connection to the brain alive, reconnecting with backoff.
func (fc *faceClient) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := dialPipe(5 * time.Second)
		if err != nil {
			// Brain not up yet (or restarting): stay locked and retry.
			fc.ctrl.setLocked(true)
			fc.ctrl.online.Store(false)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}

		fc.connMu.Lock()
		fc.enc = json.NewEncoder(conn)
		fc.connMu.Unlock()

		mr := newMessageReader(conn)
		for {
			m, err := mr.read()
			if err != nil {
				break
			}
			switch m.Type {
			case msgState:
				fc.ctrl.setRemaining(m.Remaining)
				fc.ctrl.setLocked(m.Locked)
				fc.ctrl.online.Store(m.Online)
			case msgPINResult:
				fc.deliverResult(m.OK)
			}
		}

		// Connection dropped: clear it and fail closed until we reconnect.
		fc.connMu.Lock()
		fc.enc = nil
		fc.connMu.Unlock()
		conn.Close()
		fc.ctrl.setLocked(true)
		fc.ctrl.online.Store(false)

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// submitPIN sends one PIN attempt to the brain and waits for its verdict. The
// overlay calls this synchronously, so only one attempt is in flight at a time.
func (fc *faceClient) submitPIN(pin string) bool {
	fc.connMu.Lock()
	enc := fc.enc
	fc.connMu.Unlock()
	if enc == nil {
		return false // not connected; cannot validate
	}

	res := make(chan bool, 1)
	fc.resMu.Lock()
	fc.result = res
	fc.resMu.Unlock()
	defer func() {
		fc.resMu.Lock()
		fc.result = nil
		fc.resMu.Unlock()
	}()

	if err := enc.Encode(pinMessage(pin)); err != nil {
		return false
	}
	select {
	case ok := <-res:
		return ok
	case <-time.After(5 * time.Second):
		return false
	}
}

func (fc *faceClient) deliverResult(ok bool) {
	fc.resMu.Lock()
	res := fc.result
	fc.resMu.Unlock()
	if res != nil {
		select {
		case res <- ok:
		default:
		}
	}
}
