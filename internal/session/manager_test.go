package session

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"hypercode/internal/adapter"
	"hypercode/internal/store"
)

type fakeAdapter struct {
	mu      sync.Mutex
	last    *fakeSession
	resumed string
}
type fakeSession struct {
	events    chan adapter.Event
	once      sync.Once
	responses int
	send      func(context.Context) error
}

func (f *fakeAdapter) Open(context.Context, string, adapter.PermissionMode) (adapter.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeSession{events: make(chan adapter.Event, 1024)}
	f.last = s
	return s, nil
}
func (f *fakeAdapter) Resume(ctx context.Context, dir, ref string, mode adapter.PermissionMode) (adapter.Session, error) {
	f.resumed = ref
	return f.Open(ctx, dir, mode)
}
func (f *fakeSession) Ref() string { return "native-ref" }
func (f *fakeSession) Send(ctx context.Context, _ string) error {
	if f.send != nil {
		return f.send(ctx)
	}
	return nil
}
func (f *fakeSession) Stop(context.Context) error {
	f.events <- adapter.Event{Kind: "turn_done", Status: "interrupted"}
	return nil
}
func (f *fakeSession) Respond(context.Context, string, adapter.Answer) error {
	f.responses++
	return nil
}
func (f *fakeSession) Events() <-chan adapter.Event { return f.events }
func (f *fakeSession) Close() error                 { f.once.Do(func() { close(f.events) }); return nil }
func setup(t *testing.T) (*Manager, *fakeAdapter) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeAdapter{}
	m, err := New(db, map[string]adapter.Adapter{"codex": a})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(); _ = db.Close() })
	return m, a
}
func awaitChat(t *testing.T, m *Manager, id string, predicate func(*store.Chat) bool) *store.Chat {
	t.Helper()
	wake, cancel := m.Subscribe()
	defer cancel()
	for {
		_, c := m.View(id)
		if predicate(c) {
			return c
		}
		select {
		case <-wake:
		case <-t.Context().Done():
			t.Fatal("test canceled")
		}
	}
}
func TestStreamApprovalStopAndReplay(t *testing.T) {
	m, a := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.WorkspaceWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Send(t.Context(), id, "Build this"); err != nil {
		t.Fatal(err)
	}
	if err = m.Send(t.Context(), id, "second"); err == nil {
		t.Fatal("concurrent turn accepted")
	}
	a.last.events <- adapter.Event{Kind: "text_delta", ID: "msg", Text: "partial "}
	a.last.events <- adapter.Event{Kind: "text_delta", ID: "msg", Text: "output"}
	a.last.events <- adapter.Event{Kind: "approval_requested", Prompt: &adapter.Prompt{RequestID: "1", Title: "Allow?", Decisions: []string{"accept"}}}
	c := awaitChat(t, m, id, func(c *store.Chat) bool { return len(c.Items) == 3 })
	if c.Items[1].Body != "partial output" {
		t.Fatalf("partial=%q", c.Items[1].Body)
	}
	gen, seq, snapshot, _, _, snap := m.Read(id, "", 0)
	if !snapshot || snap.Items[1].Body != "partial output" || snap.Items[2].Status != "pending" {
		t.Fatal("snapshot lost live state")
	}
	prompt := c.Items[2].ID
	if err = m.Respond(t.Context(), id, prompt, adapter.Answer{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	if err = m.Respond(t.Context(), id, prompt, adapter.Answer{Decision: "accept"}); err == nil {
		t.Fatal("duplicate answer accepted")
	}
	_, _, snapshot, updates, _, _ := m.Read(id, gen, seq)
	if snapshot || len(updates) != 1 || updates[0].Item.Status != "answered" {
		t.Fatalf("replay=%+v", updates)
	}
	if err = m.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	awaitChat(t, m, id, func(c *store.Chat) bool { return c.Status == "interrupted" })
	if err = m.Resume(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	_, c = m.View(id)
	if c.Status != "idle" {
		t.Fatal("resume not ready")
	}
}
func TestSlowSubscriberSnapshotFallback(t *testing.T) {
	m, a := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	wake, cancel := m.Subscribe()
	defer cancel()
	gen, seq, _, _, _, _ := m.Read(id, "", 0)
	for range ReplayLimit + 20 {
		a.last.events <- adapter.Event{Kind: "text_delta", ID: "msg", Text: "x"}
	}
	awaitChat(t, m, id, func(c *store.Chat) bool { return len(c.Items) > 0 && len(c.Items[0].Body) == ReplayLimit+20 })
	if len(wake) != 1 {
		t.Fatal("notifications should coalesce")
	}
	_, _, snapshot, _, _, c := m.Read(id, gen, seq)
	if !snapshot || len(c.Items[0].Body) != ReplayLimit+20 {
		t.Fatal("stale sequence did not receive full snapshot")
	}
}
func TestProcessLossAndColdResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeAdapter{}
	m, err := New(db, map[string]adapter.Adapter{"codex": a})
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Send(t.Context(), id, "hello"); err != nil {
		t.Fatal(err)
	}
	a.last.events <- adapter.Event{Kind: "approval_requested", Prompt: &adapter.Prompt{RequestID: "old-request"}}
	c := awaitChat(t, m, id, func(c *store.Chat) bool { return len(c.Items) == 2 })
	promptID := c.Items[1].ID
	_ = a.last.Close()
	c = awaitChat(t, m, id, func(c *store.Chat) bool { return !c.Live })
	if c.Items[1].Status != "interrupted" {
		t.Fatal("stale prompt is still pending")
	}
	if err = m.Respond(t.Context(), id, promptID, adapter.Answer{Decision: "accept"}); err == nil {
		t.Fatal("accepted stale answer")
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	m, err = New(db, map[string]adapter.Adapter{"codex": a})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	_, c = m.View(id)
	if c.Live {
		t.Fatal("restart automatically resumed")
	}
	if err = m.Resume(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if a.resumed != "native-ref" {
		t.Fatal("cold resume did not use stored reference")
	}
}
func TestCreateValidation(t *testing.T) {
	m, _ := setup(t)
	for _, tt := range []struct {
		agent, dir string
		mode       adapter.PermissionMode
	}{{"claude", t.TempDir(), adapter.ReadOnly}, {"codex", "relative", adapter.ReadOnly}, {"codex", t.TempDir(), "bad"}} {
		if _, err := m.Create(t.Context(), tt.agent, tt.dir, tt.mode); err == nil {
			t.Fatal(errors.New("invalid chat accepted"))
		}
	}
}

func TestDisconnectDuringSendDoesNotCancelTurn(t *testing.T) {
	m, a := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	a.last.send = func(ctx context.Context) error { close(entered); <-release; return ctx.Err() }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- m.Send(ctx, id, "keep working") }()
	<-entered
	cancel()
	close(release)
	if err = <-done; err != nil {
		t.Fatalf("browser cancellation stopped agent: %v", err)
	}
	_, c := m.View(id)
	if !c.Live || c.Status != "running" {
		t.Fatalf("turn did not survive: %+v", c)
	}
}

func TestResolvedRequestBecomesInterrupted(t *testing.T) {
	m, a := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	a.last.events <- adapter.Event{Kind: "question_asked", Prompt: &adapter.Prompt{RequestID: "expired"}}
	awaitChat(t, m, id, func(c *store.Chat) bool { return len(c.Items) == 1 })
	a.last.events <- adapter.Event{Kind: "status", ID: "expired", Status: "request_resolved"}
	c := awaitChat(t, m, id, func(c *store.Chat) bool { return c.Items[0].Status == "interrupted" })
	if err = m.Respond(t.Context(), id, c.Items[0].ID, adapter.Answer{}); err == nil {
		t.Fatal("expired request accepted")
	}
}
