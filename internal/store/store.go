// Package store holds the reward/time-bank domain logic and a dead-simple
// persistence layer. All time accounting lives here so it is unit testable
// without HTTP. Swap JSONStore for a SQLite-backed Store later without
// touching the server: the only contract is the Store interface.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrUnknownUser is returned for operations on a user that does not exist and
// was not auto-created.
var ErrUnknownUser = errors.New("unknown user")

// User is a kid (or anyone) with a time balance and optional active session.
type User struct {
	Name            string    `json:"name"`
	BalanceSeconds  int64     `json:"balance_seconds"`
	SessionMachine  string    `json:"session_machine,omitempty"`
	SessionLastSeen time.Time `json:"session_last_seen,omitempty"`
	SessionActive   bool      `json:"session_active"`
}

// GrantRecord is an audit entry for time added or removed.
type GrantRecord struct {
	User    string    `json:"user"`
	Seconds int64     `json:"seconds"`
	Reason  string    `json:"reason"`
	When    time.Time `json:"when"`
}

// Status is the read model returned to dashboards and clients.
type Status struct {
	User             string `json:"user"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	Allowed          bool   `json:"allowed"`
	PlayingNow       bool   `json:"playing_now"`
	Machine          string `json:"machine,omitempty"`
}

// Store is the persistence + domain contract the server depends on.
type Store interface {
	EnsureUser(name string) error
	Grant(name string, seconds int64, reason string) (remaining int64, err error)
	SetBalance(name string, seconds int64, reason string) (remaining int64, err error)
	// Heartbeat charges elapsed time against the balance and reports whether
	// the user may keep using the machine. The very first beat of a session is
	// free (no charge) so logging in never costs time before play starts.
	Heartbeat(name, machine string, now time.Time, maxGap time.Duration) (allowed bool, remaining int64, err error)
	Status(name string, now time.Time) (Status, error)
	List(now time.Time) ([]Status, error)
}

// playingWindow decides whether a session counts as "live" for the dashboard,
// based on how recently the last heartbeat arrived.
const playingWindow = 90 * time.Second

type persisted struct {
	Users  map[string]*User `json:"users"`
	Grants []GrantRecord    `json:"grants"`
}

// JSONStore persists the whole world to a single JSON file with an atomic
// temp-file-and-rename write. Plenty for a single household and one PC.
type JSONStore struct {
	mu        sync.Mutex
	path      string
	state     persisted
	maxGrants int
}

// NewJSONStore loads existing state from path or starts empty.
func NewJSONStore(path string) (*JSONStore, error) {
	s := &JSONStore{
		path:      path,
		maxGrants: 500,
		state:     persisted{Users: map[string]*User{}},
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.flush()
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.state); err != nil {
			return nil, err
		}
	}
	if s.state.Users == nil {
		s.state.Users = map[string]*User{}
	}
	return s, nil
}

// flush assumes the caller holds s.mu.
func (s *JSONStore) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(&s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *JSONStore) getOrCreate(name string) *User {
	u := s.state.Users[name]
	if u == nil {
		u = &User{Name: name}
		s.state.Users[name] = u
	}
	return u
}

func (s *JSONStore) logGrant(name string, seconds int64, reason string, now time.Time) {
	s.state.Grants = append(s.state.Grants, GrantRecord{
		User: name, Seconds: seconds, Reason: reason, When: now,
	})
	if len(s.state.Grants) > s.maxGrants {
		s.state.Grants = s.state.Grants[len(s.state.Grants)-s.maxGrants:]
	}
}

// EnsureUser creates the user if missing.
func (s *JSONStore) EnsureUser(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getOrCreate(name)
	return s.flush()
}

// Grant adds (or removes, if negative) seconds to a balance.
func (s *JSONStore) Grant(name string, seconds int64, reason string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.getOrCreate(name)
	applyGrant(u, seconds)
	s.logGrant(name, seconds, reason, time.Now())
	return u.BalanceSeconds, s.flush()
}

// SetBalance hard-sets the balance to an absolute value.
func (s *JSONStore) SetBalance(name string, seconds int64, reason string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.getOrCreate(name)
	delta := applySetBalance(u, seconds)
	s.logGrant(name, delta, "set: "+reason, time.Now())
	return u.BalanceSeconds, s.flush()
}

// Heartbeat is the heart of the time bank. See applyHeartbeat for the rules.
func (s *JSONStore) Heartbeat(name, machine string, now time.Time, maxGap time.Duration) (bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.getOrCreate(name)
	allowed := applyHeartbeat(u, machine, now, maxGap)
	return allowed, u.BalanceSeconds, s.flush()
}

// Status returns the read model for one user.
func (s *JSONStore) Status(name string, now time.Time) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.state.Users[name]
	if u == nil {
		return Status{}, ErrUnknownUser
	}
	return snapshot(u, now), nil
}

// List returns the read model for everyone, sorted by name.
func (s *JSONStore) List(now time.Time) ([]Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.state.Users))
	for _, u := range s.state.Users {
		out = append(out, snapshot(u, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, nil
}
