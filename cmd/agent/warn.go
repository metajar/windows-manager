package main

import "time"

// Low-time warning: when an unlocked session drops to five minutes remaining,
// the overlay shows a one-shot banner so the kid isn't blindsided by the lock.

const (
	lowTimeWarnSeconds = 5 * 60
	lowTimeWarnText    = "5 minutes remaining please purchase more time in choremore.org"

	// lowTimeWarnShowFor is how long the banner stays up. It must auto-hide:
	// nothing in a fullscreen game session can be relied on to dismiss it, and
	// a stale warning lingering under the lock overlay is worse than none.
	lowTimeWarnShowFor = 30 * time.Second
)

// warnTracker decides when to fire the low-time warning. It fires at most once
// per descent into the low-time zone: crossing back above the threshold (a
// parent granted time) or locking re-arms it, so the next descent warns again.
// Pure and single-threaded by design — the overlay tick is its only caller —
// so the fire-once transition is unit-testable without a UI.
type warnTracker struct {
	fired bool
}

// check observes the current lock state and remaining seconds, and reports
// whether the warning should be shown right now.
func (w *warnTracker) check(locked bool, remaining int64) bool {
	if locked || remaining > lowTimeWarnSeconds {
		w.fired = false
		return false
	}
	if remaining <= 0 || w.fired {
		return false
	}
	w.fired = true
	return true
}
