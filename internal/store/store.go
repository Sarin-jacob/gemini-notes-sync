// Package store keeps sync state in SQLite: which Drive note maps to which
// Outline documents, and a hash of what was last written to each of them.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS notes (
	drive_id       TEXT PRIMARY KEY,
	drive_modified TEXT NOT NULL,
	title          TEXT NOT NULL,
	synced_at      TEXT NOT NULL
);
-- One row per Outline document written for a note: role is "main" or a tab key.
CREATE TABLE IF NOT EXISTS docs (
	drive_id     TEXT NOT NULL,
	role         TEXT NOT NULL,
	outline_id   TEXT NOT NULL,
	outline_url  TEXT NOT NULL,
	written_hash TEXT NOT NULL,
	PRIMARY KEY (drive_id, role)
);
-- Container (folder) documents whose index we maintain.
CREATE TABLE IF NOT EXISTS containers (
	outline_id TEXT PRIMARY KEY,
	index_hash TEXT NOT NULL
);
-- Uploaded images by content hash, so re-syncs don't upload duplicates.
CREATE TABLE IF NOT EXISTS attachments (
	sha256 TEXT PRIMARY KEY,
	url    TEXT NOT NULL
);
`

type Store struct{ db *sql.DB }

type DocRef struct {
	OutlineID   string
	URL         string
	WrittenHash string
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// Columns added after the first release; the error is ignored when one exists.
	for _, col := range []string{
		`render_key TEXT NOT NULL DEFAULT ''`,   // fingerprint of renderer + output settings
		`meeting_time TEXT NOT NULL DEFAULT ''`, // RFC 3339, for ordering indexes
	} {
		if _, err := db.Exec(`ALTER TABLE notes ADD COLUMN ` + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// NoteState returns the Drive modifiedTime and render key recorded at the last
// successful sync of a note.
func (s *Store) NoteState(driveID string) (modified, renderKey string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT drive_modified, render_key FROM notes WHERE drive_id = ?`, driveID).Scan(&modified, &renderKey)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return modified, renderKey, err == nil, err
}

func (s *Store) MarkSynced(driveID, modified, title, renderKey string, meeting time.Time) error {
	_, err := s.db.Exec(`INSERT INTO notes (drive_id, drive_modified, title, synced_at, render_key, meeting_time) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (drive_id) DO UPDATE SET drive_modified = excluded.drive_modified, title = excluded.title,
			synced_at = excluded.synced_at, render_key = excluded.render_key, meeting_time = excluded.meeting_time`,
		driveID, modified, title, time.Now().UTC().Format(time.RFC3339), renderKey, meeting.Format(time.RFC3339))
	return err
}

// MeetingTimes maps the Outline ID of each meeting document to the meeting's start.
func (s *Store) MeetingTimes() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT d.outline_id, n.meeting_time FROM docs d JOIN notes n USING (drive_id)
		WHERE d.role = 'main' AND n.meeting_time != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, ts string
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			out[id] = t
		}
	}
	return out, rows.Err()
}

func (s *Store) Docs(driveID string) (map[string]DocRef, error) {
	rows, err := s.db.Query(`SELECT role, outline_id, outline_url, written_hash FROM docs WHERE drive_id = ?`, driveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DocRef{}
	for rows.Next() {
		var role string
		var r DocRef
		if err := rows.Scan(&role, &r.OutlineID, &r.URL, &r.WrittenHash); err != nil {
			return nil, err
		}
		out[role] = r
	}
	return out, rows.Err()
}

func (s *Store) PutDoc(driveID, role string, r DocRef) error {
	_, err := s.db.Exec(`INSERT INTO docs (drive_id, role, outline_id, outline_url, written_hash) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (drive_id, role) DO UPDATE SET outline_id = excluded.outline_id, outline_url = excluded.outline_url, written_hash = excluded.written_hash`,
		driveID, role, r.OutlineID, r.URL, r.WrittenHash)
	return err
}

func (s *Store) IndexHash(outlineID string) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT index_hash FROM containers WHERE outline_id = ?`, outlineID).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return h, err
}

func (s *Store) PutIndexHash(outlineID, hash string) error {
	_, err := s.db.Exec(`INSERT INTO containers (outline_id, index_hash) VALUES (?, ?)
		ON CONFLICT (outline_id) DO UPDATE SET index_hash = excluded.index_hash`, outlineID, hash)
	return err
}

func (s *Store) Attachment(sha string) (string, error) {
	var url string
	err := s.db.QueryRow(`SELECT url FROM attachments WHERE sha256 = ?`, sha).Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return url, err
}

func (s *Store) PutAttachment(sha, url string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO attachments (sha256, url) VALUES (?, ?)`, sha, url)
	return err
}
