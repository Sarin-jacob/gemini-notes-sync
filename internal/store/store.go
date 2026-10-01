// Package store keeps sync state in SQLite: which Drive note maps to which
// Outline documents, and a hash of what was last written to each of them.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// NoteModified returns the Drive modifiedTime recorded at the last successful sync.
func (s *Store) NoteModified(driveID string) (string, bool, error) {
	var m string
	err := s.db.QueryRow(`SELECT drive_modified FROM notes WHERE drive_id = ?`, driveID).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return m, err == nil, err
}

func (s *Store) MarkSynced(driveID, modified, title string) error {
	_, err := s.db.Exec(`INSERT INTO notes (drive_id, drive_modified, title, synced_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (drive_id) DO UPDATE SET drive_modified = excluded.drive_modified, title = excluded.title, synced_at = excluded.synced_at`,
		driveID, modified, title, time.Now().UTC().Format(time.RFC3339))
	return err
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
