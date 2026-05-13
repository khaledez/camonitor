// Persistent storage for ring history.
//
// Bell events (event-id, stream, time, snapshot JPEG) are kept both in
// the in-memory ring buffer that BellBus already manages and — when a
// path is configured — in a small SQLite database on disk. The on-disk
// copy is loaded back into the ring buffer on startup so a deploy or
// pod restart doesn't lose the recent history.
//
// The store is intentionally minimal: one table, one prepared statement
// per operation, all writes go through BellBus's existing mu so no
// extra coordination is needed. We reuse the modernc.org/sqlite driver
// that whatsapp.go already pulls in.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

const bellStoreSchema = `
CREATE TABLE IF NOT EXISTS bell_events (
	event_id           TEXT PRIMARY KEY,
	stream_id          TEXT NOT NULL,
	name               TEXT NOT NULL,
	fired_at_unix_nano INTEGER NOT NULL,
	snapshot           BLOB
);
CREATE INDEX IF NOT EXISTS idx_bell_events_fired_at
	ON bell_events(fired_at_unix_nano DESC);
`

// BellStore wraps the SQLite handle. nil is a valid value — every
// operation no-ops on a nil receiver so callers don't need to branch.
type BellStore struct {
	db *sql.DB
}

// OpenBellStore opens (creating if missing) the SQLite database at path
// and initialises the schema. The directory must already exist (it's
// mounted from the k8s PVC). On any error the caller can fall back to
// in-memory only by passing the returned (nil, err) — BellStore's
// methods tolerate a nil receiver.
func OpenBellStore(path string) (*BellStore, error) {
	if path == "" {
		return nil, errors.New("empty path")
	}
	// Same DSN flags as whatsapp.go so a misconfigured filesystem or a
	// lock contention fails fast rather than silently dropping rows.
	dsn := "file:" + filepath.Clean(path) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// SQLite + single-writer is plenty for this workload; cap to keep
	// behaviour predictable.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(bellStoreSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &BellStore{db: db}, nil
}

// Save records an event + its snapshot to the store. Errors are
// returned but the caller (BellBus.handle) only logs them — losing a
// persisted copy of a single ring is not worth crashing over.
func (s *BellStore) Save(ev BellEvent, jpeg []byte) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO bell_events
		 (event_id, stream_id, name, fired_at_unix_nano, snapshot)
		 VALUES (?, ?, ?, ?, ?)`,
		ev.EventID, ev.StreamID, ev.Name, ev.Time.UnixNano(), jpeg,
	)
	return err
}

// Prune deletes everything older than the most recent `keep` rows. We
// call this opportunistically after each Save so the DB stays bounded
// without a background goroutine.
func (s *BellStore) Prune(keep int) error {
	if s == nil || s.db == nil || keep <= 0 {
		return nil
	}
	// SQLite supports DELETE … WHERE NOT IN (subquery), so we can do
	// the trim in one statement.
	_, err := s.db.Exec(
		`DELETE FROM bell_events
		 WHERE event_id NOT IN (
		   SELECT event_id FROM bell_events
		   ORDER BY fired_at_unix_nano DESC
		   LIMIT ?
		 )`, keep,
	)
	return err
}

// loadedEntry is the wire-shape Load returns; BellBus reassembles a
// historyEntry from it. Keeping this type local to the store keeps the
// SQL-driven code out of bell.go.
type loadedEntry struct {
	Event BellEvent
	JPEG  []byte
}

// LoadRecent returns the most recent `limit` events from the store,
// oldest first (so appending in order rebuilds the ring buffer in the
// same chronological direction the in-memory variant uses).
func (s *BellStore) LoadRecent(limit int) ([]loadedEntry, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT event_id, stream_id, name, fired_at_unix_nano, snapshot
		 FROM bell_events
		 ORDER BY fired_at_unix_nano DESC
		 LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []loadedEntry
	for rows.Next() {
		var (
			id, stream, name string
			at               int64
			snap             []byte
		)
		if err := rows.Scan(&id, &stream, &name, &at, &snap); err != nil {
			return nil, err
		}
		out = append(out, loadedEntry{
			Event: BellEvent{
				EventID:     id,
				StreamID:    stream,
				Name:        name,
				Time:        time.Unix(0, at),
				HasSnapshot: len(snap) > 0,
			},
			JPEG: snap,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to oldest-first so the caller can naturally append.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Close releases the underlying connection. Safe to call on nil.
func (s *BellStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
