package store

import "time"

// This file holds the pure time-bank rules as functions over a *User. Both the
// JSON and SQLite backends call these so the accounting is provably identical
// across storage engines: a backend's only job is to load a user, apply one of
// these, and persist the result (plus an audit grant where relevant).

// applyGrant adds (or removes, if negative) seconds and clamps at zero.
func applyGrant(u *User, seconds int64) {
	u.BalanceSeconds += seconds
	if u.BalanceSeconds < 0 {
		u.BalanceSeconds = 0
	}
}

// applySetBalance hard-sets the balance to an absolute (non-negative) value and
// returns the signed delta from the previous balance, for the audit log.
func applySetBalance(u *User, seconds int64) (delta int64) {
	if seconds < 0 {
		seconds = 0
	}
	delta = seconds - u.BalanceSeconds
	u.BalanceSeconds = seconds
	return delta
}

// applyHeartbeat charges wall-clock time elapsed since the previous beat of the
// same session, capped at maxGap so a machine that was powered off (or an agent
// that crashed) does not drain the bank. The first beat of a session is free.
// It mutates the session fields and returns whether play may continue.
func applyHeartbeat(u *User, machine string, now time.Time, maxGap time.Duration) (allowed bool) {
	resuming := u.SessionActive && u.SessionMachine == machine
	if resuming {
		elapsed := now.Sub(u.SessionLastSeen)
		if elapsed < 0 {
			elapsed = 0 // clock skew, charge nothing
		}
		if elapsed > maxGap {
			elapsed = maxGap // PC was off / agent gap, charge at most one window
		}
		applyGrant(u, -int64(elapsed.Seconds()))
	} else {
		// New session on this machine. First beat is free.
		u.SessionActive = true
		u.SessionMachine = machine
	}
	u.SessionLastSeen = now

	allowed = u.BalanceSeconds > 0
	if !allowed {
		u.SessionActive = false // stop the clock while locked
	}
	return allowed
}

// snapshot builds the read model for a user at time now.
func snapshot(u *User, now time.Time) Status {
	playing := u.SessionActive && now.Sub(u.SessionLastSeen) <= playingWindow
	st := Status{
		User:             u.Name,
		RemainingSeconds: u.BalanceSeconds,
		Allowed:          u.BalanceSeconds > 0,
		PlayingNow:       playing,
	}
	if playing {
		st.Machine = u.SessionMachine
	}
	return st
}
