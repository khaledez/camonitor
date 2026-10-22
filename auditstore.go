// Audit log of guest actions (gate/door opens, enrollments, denials),
// kept in SQLite on the monitor-state PVC. Mirrors bellstore.go: same
// driver, nil-safe receiver, no background goroutine.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

const (
	auditRetention   = 60 * 24 * time.Hour
	auditDefaultList = 200 // also the maximum
)

const auditSchema = `
CREATE TABLE IF NOT EXISTS audit (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	ts            INTEGER NOT NULL,
	guest_id      TEXT NOT NULL,
	guest_label   TEXT NOT NULL,
	action        TEXT NOT NULL,
	result        TEXT NOT NULL,
	source_ip     TEXT NOT NULL,
	source_device TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit(ts DESC);
`

type AuditEvent struct {
	Time         time.Time `json:"time"`
	GuestID      string    `json:"guest_id"`
	GuestLabel   string    `json:"guest_label"`
	Action       string    `json:"action"`
	Result       string    `json:"result"`
	SourceIP     string    `json:"source_ip"`
	SourceDevice string    `json:"source_device"`
}

// AuditFilter fields are optional; zero values mean "no filter".
type AuditFilter struct {
	GuestID string
	Action  string
	Limit   int
}

// AuditStore wraps the SQLite handle. nil is valid: every method no-ops.
type AuditStore struct {
	db *sql.DB
}

// OpenAuditStore opens (creating if missing) the DB at path and prunes
// rows older than the retention window.
func OpenAuditStore(path string) (*AuditStore, error) {
	if path == "" {
		return nil, errors.New("empty path")
	}
	dsn := "file:" + filepath.Clean(path) +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(auditSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	s := &AuditStore{db: db}
	if _, err := s.Prune(time.Now().Add(-auditRetention)); err != nil {
		db.Close()
		return nil, fmt.Errorf("prune: %w", err)
	}
	return s, nil
}

func (s *AuditStore) Log(e AuditEvent) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO audit (ts, guest_id, guest_label, action, result, source_ip, source_device)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.Time.Unix(), e.GuestID, e.GuestLabel, e.Action, e.Result, e.SourceIP, e.SourceDevice,
	)
	return err
}

// List returns events most-recent-first; never nil.
func (s *AuditStore) List(f AuditFilter) ([]AuditEvent, error) {
	out := []AuditEvent{}
	if s == nil || s.db == nil {
		return out, nil
	}
	limit := f.Limit
	if limit <= 0 || limit > auditDefaultList {
		limit = auditDefaultList
	}
	// Empty filter string matches everything, so one static query suffices.
	rows, err := s.db.Query(
		`SELECT ts, guest_id, guest_label, action, result, source_ip, source_device
		 FROM audit
		 WHERE (? = '' OR guest_id = ?) AND (? = '' OR action = ?)
		 ORDER BY ts DESC, id DESC
		 LIMIT ?`,
		f.GuestID, f.GuestID, f.Action, f.Action, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e AuditEvent
		var ts int64
		if err := rows.Scan(&ts, &e.GuestID, &e.GuestLabel, &e.Action, &e.Result, &e.SourceIP, &e.SourceDevice); err != nil {
			return nil, err
		}
		e.Time = time.Unix(ts, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// Prune deletes rows with ts < before and returns how many were removed.
func (s *AuditStore) Prune(before time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	res, err := s.db.Exec(`DELETE FROM audit WHERE ts < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
