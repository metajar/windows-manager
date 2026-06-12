//go:build !windows

package main

import (
	"context"
	"errors"
	"log"
	"time"
)

// runOverlay on non-Windows has no real UI. It logs lock-state transitions so
// the networking can be exercised during development on Linux or macOS.
func runOverlay(ctx context.Context, ctrl *LockController) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastLocked = !ctrl.Locked() // force first log
	for {
		select {
		case <-ctx.Done():
			log.Println("overlay stub exiting")
			return
		case <-t.C:
			locked := ctrl.Locked()
			if locked != lastLocked {
				if locked {
					log.Printf("[LOCK] screen would be covered (online=%v)", ctrl.Online())
				} else {
					log.Printf("[UNLOCK] play allowed, remaining=%ds", ctrl.Remaining())
				}
				lastLocked = locked
			}
		}
	}
}

func installAutostart(cfg config, configPath string) error {
	return errors.New("install is only supported on Windows")
}

func removeAutostart(configPath string) error {
	return errors.New("uninstall is only supported on Windows")
}
