// Package store persists UI history; native agents retain their own context.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lepinkainen/hypercode/internal/adapter"
	_ "modernc.org/sqlite"
)

type Chat struct {
	ID        string
	ProjectID string
	Directory string
	Project   string
	Harness   string
	NativeRef string
	Title     string
	Mode      adapter.PermissionMode
	Model     string // Native model id; empty means the harness default.
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	Items     []Item
	Live      bool // Runtime state, never persisted.
}
type Item struct {
	ID          string
	ChatID      string
	Ordinal     int
	Kind        string
	Status      string
	Attachments []adapter.Attachment
	Body        string
	Prompt      *adapter.Prompt
	CreatedAt   time.Time
}
type Store struct{ db *sql.DB }

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY,name TEXT NOT NULL,directory TEXT NOT NULL UNIQUE);
 CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),harness TEXT NOT NULL,native_ref TEXT NOT NULL,title TEXT NOT NULL,permission_mode TEXT NOT NULL,status TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS items(id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES sessions(id),ordinal INTEGER NOT NULL,kind TEXT NOT NULL,status TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,UNIQUE(session_id,ordinal));
 UPDATE sessions SET status='interrupted' WHERE status='running';
 UPDATE items SET status='interrupted' WHERE status IN ('streaming','pending');`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumn(db, "sessions", "model", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }

// addColumn is the schema migration primitive: SQLite has no IF NOT EXISTS for columns.
func addColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

// Save upserts session metadata and the supplied items. Items omitted from c
// remain unchanged, so callers can persist only the items changed since a save.
func (s *Store) Save(c Chat) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`INSERT INTO projects(id,name,directory) VALUES(?,?,?) ON CONFLICT(directory) DO NOTHING`, c.ProjectID, c.Project, c.Directory); err != nil {
		return err
	}
	var projectID string
	if err = tx.QueryRow(`SELECT id FROM projects WHERE directory=?`, c.Directory).Scan(&projectID); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO sessions(id,project_id,harness,native_ref,title,permission_mode,status,created_at,updated_at,model) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET native_ref=excluded.native_ref,title=excluded.title,status=excluded.status,updated_at=excluded.updated_at`, c.ID, projectID, c.Harness, c.NativeRef, c.Title, c.Mode, c.Status, c.CreatedAt.Format(time.RFC3339Nano), c.UpdatedAt.Format(time.RFC3339Nano), c.Model)
	if err != nil {
		return err
	}
	for _, i := range c.Items {
		body, err := json.Marshal(struct {
			Attachments []adapter.Attachment `json:"attachments,omitempty"`
			Text        string               `json:"text"`
			Prompt      *adapter.Prompt      `json:"prompt,omitempty"`
		}{i.Attachments, i.Body, i.Prompt})
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO items VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,body=excluded.body`, i.ID, c.ID, i.Ordinal, i.Kind, i.Status, string(body), i.CreatedAt.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Load() ([]Chat, error) {
	rows, err := s.db.Query(`SELECT s.id,s.project_id,p.directory,p.name,s.harness,s.native_ref,s.title,s.permission_mode,s.status,s.created_at,s.updated_at,s.model FROM sessions s JOIN projects p ON p.id=s.project_id ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	var chats []Chat
	for rows.Next() {
		var c Chat
		var created, updated string
		if err = rows.Scan(&c.ID, &c.ProjectID, &c.Directory, &c.Project, &c.Harness, &c.NativeRef, &c.Title, &c.Mode, &c.Status, &created, &updated, &c.Model); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		chats = append(chats, c)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for n := range chats {
		items, err := s.items(chats[n].ID)
		if err != nil {
			return nil, err
		}
		chats[n].Items = items
	}
	return chats, nil
}
func (s *Store) items(id string) ([]Item, error) {
	rows, err := s.db.Query(`SELECT id,ordinal,kind,status,body,created_at FROM items WHERE session_id=? ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var i Item
		var body, created string
		i.ChatID = id
		if err = rows.Scan(&i.ID, &i.Ordinal, &i.Kind, &i.Status, &body, &created); err != nil {
			return nil, err
		}
		var data struct {
			Attachments []adapter.Attachment `json:"attachments,omitempty"`
			Text        string               `json:"text"`
			Prompt      *adapter.Prompt      `json:"prompt"`
		}
		if err = json.Unmarshal([]byte(body), &data); err != nil {
			return nil, fmt.Errorf("decode item %s: %w", i.ID, err)
		}
		i.Attachments = data.Attachments
		i.Body = data.Text
		i.Prompt = data.Prompt
		i.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		items = append(items, i)
	}
	return items, rows.Err()
}
