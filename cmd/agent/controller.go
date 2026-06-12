package main

import "sync/atomic"

// LockController is the thread-safe bridge between the networking goroutine
// (which decides locked/unlocked and remaining time) and the platform overlay
// (which reads those values and submits PIN attempts). The Windows overlay
// runs on its own OS-locked thread, so every field crossing that boundary is
// either atomic or a function the overlay calls.
type LockController struct {
	locked    atomic.Bool
	remaining atomic.Int64 // seconds left, for display
	online    atomic.Bool  // last heartbeat reached the server
	submit    func(pin string) bool
}

func (c *LockController) Locked() bool         { return c.locked.Load() }
func (c *LockController) Remaining() int64     { return c.remaining.Load() }
func (c *LockController) Online() bool         { return c.online.Load() }
func (c *LockController) setLocked(v bool)     { c.locked.Store(v) }
func (c *LockController) setRemaining(s int64) { c.remaining.Store(s) }

// SubmitPIN is called by the overlay when the parent enters a code.
func (c *LockController) SubmitPIN(pin string) bool {
	if c.submit == nil {
		return false
	}
	return c.submit(pin)
}
