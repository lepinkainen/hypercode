package store

import (
	"github.com/lepinkainen/hypercode/internal/adapter"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistenceAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := Chat{ID: ID(), ProjectID: ID(), Directory: "/tmp/project", Project: "project", Harness: "codex", NativeRef: "native-thread", Title: "Test", Mode: adapter.WorkspaceWrite, Model: "fixture-large", Status: "running", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	c.Items = []Item{{ID: ID(), ChatID: c.ID, Ordinal: 0, Kind: "assistant", Status: "streaming", Body: "partial text", Attachments: []adapter.Attachment{{ID: ID(), Name: "screenshot.png", MediaType: "image/png", Size: 42, Path: "/host/private/path"}}, CreatedAt: time.Now()}, {ID: ID(), ChatID: c.ID, Ordinal: 1, Kind: "approval", Status: "pending", Prompt: &adapter.Prompt{RequestID: "42", Title: "Allow?"}, CreatedAt: time.Now()}}
	if err = db.Save(c); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	chats, err := db.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 {
		t.Fatalf("chats=%d", len(chats))
	}
	got := chats[0]
	if got.Status != "interrupted" || got.NativeRef != "native-thread" {
		t.Fatalf("chat=%+v", got)
	}
	if got.Items[0].Body != "partial text" || got.Items[0].Status != "interrupted" || got.Items[1].Status != "interrupted" || got.Items[1].Prompt.RequestID != "42" {
		t.Fatalf("items=%+v", got.Items)
	}
	if len(got.Items[0].Attachments) != 1 || got.Items[0].Attachments[0].Name != "screenshot.png" || got.Items[0].Attachments[0].Path != "" || len(got.Items[1].Attachments) != 0 {
		t.Fatalf("attachment roundtrip: %+v", got.Items)
	}
	c.ID = ID()
	c.ProjectID = ID()
	c.Items = nil
	if err = db.Save(c); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.db.QueryRow(`SELECT count(*) FROM projects`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("project dedup: %d %v", count, err)
	}
}
