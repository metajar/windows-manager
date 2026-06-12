package store

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go (CGO-free) SQLite driver
)

// SQLiteStore is a Store backed by a single SQLite file via the pure-Go
// modernc driver, so it keeps the project's CGO-free static builds. It mirrors
// JSONStore's semantics exactly by delegating all accounting to the shared
// apply* helpers; the only difference is durability and concurrent-reader
// friendliness (WAL).
type SQLiteStore struct {
	mu        sync.Mutex
	db        *sql.DB
	maxGrants int
}

// NewSQLiteStore opens (creating if needed) a SQLite database at path and
// ensures the schema exists.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	// WAL + a busy timeout keep the single-writer model smooth; foreign_keys is
	// harmless here but good hygiene.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer at a time matches our mutex and avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	s := &SQLiteStore{db: db, maxGrants: 500}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS users (
  name              TEXT    PRIMARY KEY,
  balance_seconds   INTEGER NOT NULL DEFAULT 0,
  session_machine   TEXT    NOT NULL DEFAULT '',
  session_last_seen INTEGER NOT NULL DEFAULT 0,  -- unix nanoseconds, 0 = never
  session_active    INTEGER NOT NULL DEFAULT 0   -- 0/1
);
CREATE TABLE IF NOT EXISTS grants (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  user      TEXT    NOT NULL,
  seconds   INTEGER NOT NULL,
  reason    TEXT    NOT NULL,
  when_unix INTEGER NOT NULL                      -- unix nanoseconds
);
CREATE INDEX IF NOT EXISTS idx_grants_id ON grants(id);
`)
	return err
}

// loadUser reads a user row inside tx, returning a zero-balance User (not yet
// persisted) when the row is absent so callers can treat create and update
// uniformly.
func loadUser(tx *sql.Tx, name string) (*User, error) {
	u := &User{Name: name}
	var lastSeen int64
	var active int
	err := tx.QueryRow(
		`SELECT balance_seconds, session_machine, session_last_seen, session_active FROM users WHERE name = ?`,
		name,
	).Scan(&u.BalanceSeconds, &u.SessionMachine, &lastSeen, &active)
	switch {
	case err == sql.ErrNoRows:
		return u, nil
	case err != nil:
		return nil, err
	}
	if lastSeen != 0 {
		u.SessionLastSeen = time.Unix(0, lastSeen).UTC()
	}
	u.SessionActive = active != 0
	return u, nil
}

func saveUser(tx *sql.Tx, u *User) error {
	var lastSeen int64
	if !u.SessionLastSeen.IsZero() {
		lastSeen = u.SessionLastSeen.UnixNano()
	}
	active := 0
	if u.SessionActive {
		active = 1
	}
	_, err := tx.Exec(`
INSERT INTO users (name, balance_seconds, session_machine, session_last_seen, session_active)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  balance_seconds   = excluded.balance_seconds,
  session_machine   = excluded.session_machine,
  session_last_seen = excluded.session_last_seen,
  session_active    = excluded.session_active`,
		u.Name, u.BalanceSeconds, u.SessionMachine, lastSeen, active)
	return err
}

func (s *SQLiteStore) logGrant(tx *sql.Tx, name string, seconds int64, reason string, now time.Time) error {
	if _, err := tx.Exec(
		`INSERT INTO grants (user, seconds, reason, when_unix) VALUES (?, ?, ?, ?)`,
		name, seconds, reason, now.UnixNano(),
	); err != nil {
		return err
	}
	// Trim the audit log to the most recent maxGrants rows.
	_, err := tx.Exec(
		`DELETE FROM grants WHERE id NOT IN (SELECT id FROM grants ORDER BY id DESC LIMIT ?)`,
		s.maxGrants)
	return err
}

// withTx runs fn in an immediate transaction, committing on success.
func (s *SQLiteStore) withTx(fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// EnsureUser creates the user if missing.
func (s *SQLiteStore) EnsureUser(name string) error {
	return s.withTx(func(tx *sql.Tx) error {
		u, err := loadUser(tx, name)
		if err != nil {
			return err
		}
		return saveUser(tx, u)
	})
}

// Grant adds (or removes) seconds to a balance.
func (s *SQLiteStore) Grant(name string, seconds int64, reason string) (int64, error) {
	var rem int64
	err := s.withTx(func(tx *sql.Tx) error {
		u, err := loadUser(tx, name)
		if err != nil {
			return err
		}
		applyGrant(u, seconds)
		if err := saveUser(tx, u); err != nil {
			return err
		}
		rem = u.BalanceSeconds
		return s.logGrant(tx, name, seconds, reason, time.Now())
	})
	return rem, err
}

// SetBalance hard-sets the balance to an absolute value.
func (s *SQLiteStore) SetBalance(name string, seconds int64, reason string) (int64, error) {
	var rem int64
	err := s.withTx(func(tx *sql.Tx) error {
		u, err := loadUser(tx, name)
		if err != nil {
			return err
		}
		delta := applySetBalance(u, seconds)
		if err := saveUser(tx, u); err != nil {
			return err
		}
		rem = u.BalanceSeconds
		return s.logGrant(tx, name, delta, "set: "+reason, time.Now())
	})
	return rem, err
}

// Heartbeat charges elapsed time and reports whether play may continue.
func (s *SQLiteStore) Heartbeat(name, machine string, now time.Time, maxGap time.Duration) (bool, int64, error) {
	var (
		allowed bool
		rem     int64
	)
	err := s.withTx(func(tx *sql.Tx) error {
		u, err := loadUser(tx, name)
		if err != nil {
			return err
		}
		allowed = applyHeartbeat(u, machine, now, maxGap)
		rem = u.BalanceSeconds
		return saveUser(tx, u)
	})
	return allowed, rem, err
}

// Status returns the read model for one user.
func (s *SQLiteStore) Status(name string, now time.Time) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &User{Name: name}
	var lastSeen int64
	var active int
	err := s.db.QueryRow(
		`SELECT balance_seconds, session_machine, session_last_seen, session_active FROM users WHERE name = ?`,
		name,
	).Scan(&u.BalanceSeconds, &u.SessionMachine, &lastSeen, &active)
	if err == sql.ErrNoRows {
		return Status{}, ErrUnknownUser
	}
	if err != nil {
		return Status{}, err
	}
	if lastSeen != 0 {
		u.SessionLastSeen = time.Unix(0, lastSeen).UTC()
	}
	u.SessionActive = active != 0
	return snapshot(u, now), nil
}

// List returns the read model for everyone, sorted by name.
func (s *SQLiteStore) List(now time.Time) ([]Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT name, balance_seconds, session_machine, session_last_seen, session_active FROM users ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		u := &User{}
		var lastSeen int64
		var active int
		if err := rows.Scan(&u.Name, &u.BalanceSeconds, &u.SessionMachine, &lastSeen, &active); err != nil {
			return nil, err
		}
		if lastSeen != 0 {
			u.SessionLastSeen = time.Unix(0, lastSeen).UTC()
		}
		u.SessionActive = active != 0
		out = append(out, snapshot(u, now))
	}
	if out == nil {
		out = []Status{}
	}
	return out, rows.Err()
}
