package store

import (
	"fmt"
	"strings"
	"time"
)

// Open returns a Store of the requested kind ("json" or "sqlite"). An empty
// kind is inferred from the path extension (.db/.sqlite/.sqlite3 -> sqlite,
// everything else -> json), so existing "rewardd.json" deployments keep working
// untouched.
func Open(kind, path string) (Store, error) {
	switch Kind(kind, path) {
	case "sqlite":
		return NewSQLiteStore(path)
	case "json":
		return NewJSONStore(path)
	default:
		return nil, fmt.Errorf("unknown store kind %q (want json or sqlite)", kind)
	}
}

// Kind resolves the effective backend name from an explicit kind or, when that
// is empty, the path extension.
func Kind(kind, path string) string {
	if k := strings.ToLower(strings.TrimSpace(kind)); k != "" {
		return k
	}
	lp := strings.ToLower(path)
	for _, ext := range []string{".db", ".sqlite", ".sqlite3"} {
		if strings.HasSuffix(lp, ext) {
			return "sqlite"
		}
	}
	return "json"
}

// MigrateBalances copies every user's current balance from src into dst using
// only the Store interface. Session state is intentionally not carried over
// (sessions are ephemeral and reset on the next heartbeat); the grant audit log
// is likewise not migrated. Returns the number of users copied.
func MigrateBalances(dst, src Store) (int, error) {
	now := time.Now()
	users, err := src.List(now)
	if err != nil {
		return 0, err
	}
	for _, u := range users {
		if _, err := dst.SetBalance(u.User, u.RemainingSeconds, "migration"); err != nil {
			return 0, fmt.Errorf("migrate %q: %w", u.User, err)
		}
	}
	return len(users), nil
}
