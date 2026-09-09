package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"testing"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/store"
)

type fakeAdapter struct {
	mu      sync.Mutex
	last    *fakeSession
	resumed string
	model   string
	openErr error
}
type fakeSession struct {
	model     string
	events    chan adapter.Event
	once      sync.Once
	responses int
	send      func(context.Context) error
}

func (f *fakeAdapter) Models(context.Context) ([]adapter.Model, error) {
	return []adapter.Model{{ID: "", Name: "Default", Default: true}, {ID: "fake-fast", Name: "Fake Fast", Description: "Quick answers"}}, nil
}
func (f *fakeAdapter) Open(_ context.Context, _ string, _ adapter.PermissionMode, model string) (adapter.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.model = model
	if f.openErr != nil {
		return nil, f.openErr
	}
	if model == "" {
		model = "fake-resolved"
	}
	s := &fakeSession{model: model, events: make(chan adapter.Event, 1024)}
	f.last = s
	return s, nil
}
func (f *fakeAdapter) Resume(ctx context.Context, dir, ref string, mode adapter.PermissionMode, model string) (adapter.Session, error) {
	f.resumed = ref
	return f.Open(ctx, dir, mode, model)
}
func (f *fakeSession) Ref() string   { return "native-ref" }
func (f *fakeSession) Model() string { return f.model }
func (f *fakeSession) Send(ctx context.Context, _ adapter.Input) error {
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
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.WorkspaceWrite, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Send(t.Context(), id, adapter.Input{Text: "Build this"}); err != nil {
		t.Fatal(err)
	}
	if err = m.Send(t.Context(), id, adapter.Input{Text: "second"}); err == nil {
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
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
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
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Send(t.Context(), id, adapter.Input{Text: "hello"}); err != nil {
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
		if _, err := m.Create(t.Context(), tt.agent, tt.dir, tt.mode, ""); err == nil {
			t.Fatal(errors.New("invalid chat accepted"))
		}
	}
}

func TestDisconnectDuringSendDoesNotCancelTurn(t *testing.T) {
	m, a := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	a.last.send = func(ctx context.Context) error { close(entered); <-release; return ctx.Err() }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- m.Send(ctx, id, adapter.Input{Text: "keep working"}) }()
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
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
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

func TestSendFailureKeepsLiveSession(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		status string
	}{{"rejected", &adapter.RejectedError{Err: errors.New("request rejected")}, "idle"}, {"timeout", context.DeadlineExceeded, "running"}} {
		t.Run(tt.name, func(t *testing.T) {
			m, a := setup(t)
			id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
			if err != nil {
				t.Fatal(err)
			}
			native := a.last
			native.send = func(context.Context) error { return tt.err }
			if err = m.Send(t.Context(), id, adapter.Input{Text: "keep this session"}); !errors.Is(err, tt.err) {
				t.Fatalf("error=%v", err)
			}
			_, c := m.View(id)
			if !c.Live || c.Status != tt.status {
				t.Fatalf("live=%v status=%s; want live %s", c.Live, c.Status, tt.status)
			}
			if tt.name == "timeout" {
				if err = m.Send(t.Context(), id, adapter.Input{Text: "duplicate"}); err == nil {
					t.Fatal("allowed a second turn before resolving uncertain acceptance")
				}
				native.events <- adapter.Event{Kind: "text_done", ID: "late", Text: "The timed-out request did start."}
				native.events <- adapter.Event{Kind: "turn_done", Status: "completed"}
				awaitChat(t, m, id, func(c *store.Chat) bool { return c.Status == "idle" })
			} else {
				if c.Items[0].Status != "rejected" {
					t.Fatalf("user item status=%s", c.Items[0].Status)
				}
				native.send = nil
				if err = m.Send(t.Context(), id, adapter.Input{Text: "retry"}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestEmptyChatRetryIgnoresErrorHistory(t *testing.T) {
	m, a := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = a.last.Close()
	awaitChat(t, m, id, func(c *store.Chat) bool { return !c.Live })
	a.openErr = errors.New("temporary launch failure")
	if err = m.Resume(t.Context(), id); err == nil {
		t.Fatal("expected failed launch")
	}
	a.openErr = nil
	a.resumed = ""
	if err = m.Resume(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if a.resumed != "" {
		t.Fatalf("error-only history caused native resume of %q", a.resumed)
	}
}

func TestMessageLimitMatchesBrowserUTF16(t *testing.T) {
	for _, tt := range []struct {
		name, text string
		valid      bool
	}{{"ASCII", strings.Repeat("+", 100000), true}, {"BMP", strings.Repeat("界", 100000), true}, {"astral", strings.Repeat("😀", 50000), true}, {"too long", strings.Repeat("😀", 50001), false}} {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := setup(t)
			id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
			if err != nil {
				t.Fatal(err)
			}
			err = m.Send(t.Context(), id, adapter.Input{Text: tt.text})
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
		})
	}
}

type observedStore struct {
	*store.Store
	observe func(store.Chat) error
}

func (s *observedStore) Save(c store.Chat) error {
	if s.observe != nil {
		if err := s.observe(c); err != nil {
			return err
		}
	}
	return s.Store.Save(c)
}

func TestPersistenceIsIncrementalAndDoesNotBlockViews(t *testing.T) {
	m, _ := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := m.getRuntime(id)
	for range 12 {
		if err := m.Send(t.Context(), id, adapter.Input{Text: "hello"}); err != nil {
			t.Fatal(err)
		}
		m.apply(id, rt, adapter.Event{Kind: "text_done", ID: store.ID(), Text: "reply"})
		m.apply(id, rt, adapter.Event{Kind: "turn_done"})
	}
	entered, release := make(chan store.Chat, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	m.mu.Lock()
	m.db = &observedStore{Store: m.db.(*store.Store), observe: func(c store.Chat) error {
		select {
		case entered <- c:
		default:
		}
		<-release
		return nil
	}}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.apply(id, rt, adapter.Event{Kind: "text_done", ID: "latest", Text: "new output"})
		close(done)
	}()
	saved := <-entered
	if len(saved.Items) != 1 || saved.Items[0].Body != "new output" {
		t.Errorf("save rewrote history: %d items, want only the changed item", len(saved.Items))
	}
	viewed := make(chan struct{})
	go func() { m.View(id); m.Read(id, "", 0); close(viewed) }()
	select {
	case <-viewed:
	case <-time.After(time.Second):
		t.Error("history I/O blocked View/Read")
	}
	unblock()
	<-done
	<-viewed
	m.mu.Lock()
	m.db = m.db.(*observedStore).Store
	m.mu.Unlock()
	chats, err := m.db.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 || len(chats[0].Items) != 25 {
		t.Fatalf("partial saves lost history: %+v", chats)
	}
}

func TestFailedSaveRetriesLatestItem(t *testing.T) {
	m, _ := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := m.getRuntime(id)
	m.apply(id, rt, adapter.Event{Kind: "text_delta", ID: "reply", Text: "first"})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	m.mu.Lock()
	db := m.db.(*store.Store)
	var first sync.Once
	m.db = &observedStore{Store: db, observe: func(store.Chat) error {
		var err error
		first.Do(func() { close(entered); <-release; err = errors.New("temporary write failure") })
		return err
	}}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.mu.Lock(); _ = m.save(m.chats[id]); m.mu.Unlock(); close(done) }()
	<-entered
	m.apply(id, rt, adapter.Event{Kind: "text_delta", ID: "reply", Text: " latest"})
	unblock()
	<-done
	m.mu.Lock()
	err = m.save(m.chats[id])
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	chats, err := db.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(chats[0].Items) != 1 || chats[0].Items[0].Body != "first latest" {
		t.Fatalf("retry overwrote a newer item: %+v", chats[0].Items)
	}
}

func TestCloseWaitsForChatBeingSaved(t *testing.T) {
	m, _ := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	m.mu.Lock()
	m.db = &observedStore{Store: m.db.(*store.Store), observe: func(store.Chat) error {
		close(entered)
		<-release
		return nil
	}}
	m.mu.Unlock()
	dir := t.TempDir()
	created := make(chan error, 1)
	go func() { _, err := m.Create(t.Context(), "codex", dir, adapter.ReadOnly, ""); created <- err }()
	<-entered
	closed := make(chan struct{})
	go func() { _ = m.Close(); close(closed) }()
	<-m.ctx.Done()
	select {
	case <-closed:
		t.Error("shutdown returned while a new chat still owned a process and a save")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	if err := <-created; err == nil {
		t.Error("created a live chat after shutdown started")
	}
	<-closed
}
func TestModelSelection(t *testing.T) {
	m, a := setup(t)
	defer func() { _ = m.Close() }()
	m.modelsReady.Wait()
	if len(m.Models("codex")) != 2 || m.ModelName("codex", "fake-fast") != "Fake Fast" || m.ModelName("codex", "") != "Default" || m.ModelName("codex", "gone") != "gone" {
		t.Fatalf("catalog=%+v", m.Models("codex"))
	}
	if _, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "not-a-model"); err == nil {
		t.Fatal("unknown model accepted")
	}
	pinnedID, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, c := m.View(pinnedID); c.Model != "fake-resolved" {
		t.Fatalf("default not pinned to the effective model: %q", c.Model)
	}
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "fake-fast")
	if err != nil {
		t.Fatal(err)
	}
	if a.model != "fake-fast" {
		t.Fatalf("adapter opened with model %q", a.model)
	}
	_, c := m.View(id)
	if c.Model != "fake-fast" {
		t.Fatalf("chat model=%q", c.Model)
	}
	a.model = ""
	_ = a.last.Close()
	awaitChat(t, m, id, func(c *store.Chat) bool { return !c.Live })
	if err = m.Resume(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if a.model != "fake-fast" {
		t.Fatalf("resume dropped model: %q", a.model)
	}
}

func TestAttachmentOnlySendAndSnapshot(t *testing.T) {
	m, a := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	file := adapter.Attachment{ID: store.ID(), Name: "screenshot.png", MediaType: "image/png", Size: 123, Path: "/host/image.png"}
	a.last.send = func(context.Context) error {
		chats, err := m.db.(*store.Store).Load()
		if err != nil || len(chats[0].Items[0].Attachments) != 1 {
			t.Errorf("dispatch preceded persistence: %v", err)
		}
		return &adapter.RejectedError{Err: errors.New("test rejection")}
	}
	if err := m.Send(t.Context(), id, adapter.Input{Attachments: []adapter.Attachment{file}}); err == nil {
		t.Fatal("expected rejection")
	}
	_, c := m.View(id)
	if c.Title != file.Name || c.Items[0].Status != "rejected" || len(c.Items[0].Attachments) != 1 {
		t.Fatalf("attachment-only message: %+v", c)
	}
	c.Items[0].Attachments[0].Name = "mutated"
	_, c = m.View(id)
	if c.Items[0].Attachments[0].Name != file.Name || c.Items[0].Attachments[0].Path != "" {
		t.Fatal("snapshot leaked mutable attachment or host path")
	}
}

func TestFailedAttachmentSaveDropsReferences(t *testing.T) {
	m, a := setup(t)
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	a.last.send = func(context.Context) error { t.Error("dispatched after failed save"); return nil }
	m.mu.Lock()
	m.db = &observedStore{Store: m.db.(*store.Store), observe: func(store.Chat) error { return errors.New("disk full") }}
	m.mu.Unlock()
	if err := m.Send(t.Context(), id, adapter.Input{Attachments: []adapter.Attachment{{ID: store.ID(), Name: "notes.txt"}}}); err == nil {
		t.Fatal("save succeeded")
	}
	_, c := m.View(id)
	if len(c.Items[0].Attachments) != 0 || c.Items[0].Status != "rejected" {
		t.Fatal("failed save retained attachment references")
	}
}
