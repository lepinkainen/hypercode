// Package session owns agent lifetimes, persisted history, and reconnectable streams.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/limits"
	"github.com/lepinkainen/hypercode/internal/store"
)

const ReplayLimit = 256

type Update struct {
	Seq    uint64
	ChatID string
	Item   *store.Item
}
type runtime struct {
	native     adapter.Session
	op         sync.Mutex
	generation string
	itemIDs    map[string]string
}
type Manager struct {
	mu          sync.Mutex
	persistMu   sync.Mutex
	db          historyStore
	adapters    map[string]adapter.Adapter
	chats       map[string]*store.Chat
	runtimes    map[string]*runtime
	dirty       map[string]bool
	dirtyItems  map[string]map[string]store.Item
	generation  string
	seq         uint64
	ring        []Update
	subscribers map[chan struct{}]bool
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	closed      bool
}

type historyStore interface {
	Load() ([]store.Chat, error)
	Save(store.Chat) error
}

func New(db historyStore, adapters map[string]adapter.Adapter) (*Manager, error) {
	chats, err := db.Load()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{db: db, adapters: adapters, chats: map[string]*store.Chat{}, runtimes: map[string]*runtime{}, dirty: map[string]bool{}, dirtyItems: map[string]map[string]store.Item{}, generation: store.ID(), subscribers: map[chan struct{}]bool{}, ctx: ctx, cancel: cancel}
	for i := range chats {
		m.chats[chats[i].ID] = &chats[i]
	}
	m.wg.Add(1)
	go m.flushLoop()
	return m, nil
}
func (m *Manager) flushLoop() {
	defer m.wg.Done()
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.mu.Lock()
			ids := make([]string, 0, len(m.dirty))
			for id := range m.dirty {
				ids = append(ids, id)
			}
			for _, id := range ids {
				if m.chats[id] == nil {
					continue
				}
				if err := m.save(m.chats[id]); err != nil {
					slog.Error("persist partial output", "error", err)
				}
			}
			m.mu.Unlock()
		}
	}
}
func (m *Manager) save(c *store.Chat) error {
	// Enter and return with mu held. Serialize snapshots and writes together so
	// an older snapshot cannot overwrite a newer save, while reads remain free.
	m.mu.Unlock()
	m.persistMu.Lock()
	m.mu.Lock()
	snapshot := *c
	pending := m.dirtyItems[c.ID]
	snapshot.Items = make([]store.Item, 0, len(pending))
	for _, item := range pending {
		snapshot.Items = append(snapshot.Items, item)
	}
	delete(m.dirtyItems, c.ID)
	delete(m.dirty, c.ID)
	db := m.db
	m.mu.Unlock()
	err := db.Save(snapshot)
	m.mu.Lock()
	if err != nil {
		// Preserve changes that arrived during the failed write. Retry only the
		// newest version of each item, plus metadata, on the next flush.
		if m.dirtyItems[c.ID] == nil {
			m.dirtyItems[c.ID] = pending
		} else {
			for id, item := range pending {
				if _, newer := m.dirtyItems[c.ID][id]; !newer {
					m.dirtyItems[c.ID][id] = item
				}
			}
		}
		m.dirty[c.ID] = true
	}
	m.persistMu.Unlock()
	return err
}
func (m *Manager) publish(c *store.Chat, item *store.Item) {
	m.dirty[c.ID] = true
	m.seq++
	u := Update{Seq: m.seq, ChatID: c.ID}
	if item != nil {
		copy := *item
		u.Item = &copy
		if m.dirtyItems[c.ID] == nil {
			m.dirtyItems[c.ID] = map[string]store.Item{}
		}
		m.dirtyItems[c.ID][item.ID] = copy
	}
	m.ring = append(m.ring, u)
	if len(m.ring) > ReplayLimit {
		m.ring = m.ring[len(m.ring)-ReplayLimit:]
	}
	for ch := range m.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func clone(c *store.Chat) store.Chat {
	v := *c
	v.Items = append([]store.Item(nil), c.Items...)
	return v
}
func (m *Manager) listLocked() []store.Chat {
	out := make([]store.Chat, 0, len(m.chats))
	for _, c := range m.chats {
		v := *c
		v.Items = nil
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}
func (m *Manager) View(id string) ([]store.Chat, *store.Chat) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var c *store.Chat
	if current := m.chats[id]; current != nil {
		v := clone(current)
		c = &v
	}
	return m.listLocked(), c
}

// Subscribe and Read share the publication lock. A subscriber cannot miss the
// boundary between its snapshot and live updates. Notifications never block agents.
func (m *Manager) Subscribe() (chan struct{}, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan struct{}, 1)
	m.subscribers[ch] = true
	return ch, func() { m.mu.Lock(); delete(m.subscribers, ch); m.mu.Unlock() }
}
func (m *Manager) Read(id, generation string, after uint64) (string, uint64, bool, []Update, []store.Chat, *store.Chat) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := generation != m.generation || after > m.seq || (len(m.ring) > 0 && after < m.ring[0].Seq-1)
	var updates []Update
	if !snapshot {
		for _, u := range m.ring {
			if u.Seq > after {
				updates = append(updates, u)
			}
		}
	}
	var chat *store.Chat
	if c := m.chats[id]; c != nil {
		v := clone(c)
		chat = &v
	}
	return m.generation, m.seq, snapshot, updates, m.listLocked(), chat
}
func (m *Manager) Create(ctx context.Context, harness, dir string, mode adapter.PermissionMode) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New("application is shutting down")
	}
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()
	a := m.adapters[harness]
	if a == nil {
		return "", errors.New("unsupported agent")
	}
	if !mode.Valid() {
		return "", errors.New("invalid permission mode")
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("use an absolute working directory")
	}
	dir, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("working directory must be a directory")
	}
	native, err := a.Open(m.ctx, dir, mode)
	if err != nil {
		return "", err
	}
	now := time.Now()
	c := &store.Chat{ID: store.ID(), ProjectID: store.ID(), Project: filepath.Base(dir), Directory: dir, Harness: harness, NativeRef: native.Ref(), Title: "New chat", Mode: mode, Status: "idle", CreatedAt: now, UpdatedAt: now, Live: true}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		_ = native.Close()
		return "", errors.New("application is shutting down")
	}
	if err = m.save(c); err != nil {
		delete(m.dirty, c.ID)
		delete(m.dirtyItems, c.ID)
		_ = native.Close()
		return "", err
	}
	if m.closed {
		_ = native.Close()
		return "", errors.New("application is shutting down")
	}
	m.chats[c.ID] = c
	m.attach(c, native)
	m.publish(c, nil)
	return c.ID, nil
}
func (m *Manager) attach(c *store.Chat, native adapter.Session) {
	rt := &runtime{native: native, generation: store.ID(), itemIDs: map[string]string{}}
	m.runtimes[c.ID] = rt
	c.Live = true
	m.wg.Add(1)
	go m.consume(c.ID, rt)
}
func (m *Manager) getRuntime(id string) (*runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("application is shutting down")
	}
	rt := m.runtimes[id]
	if rt == nil {
		return nil, errors.New("resume this chat before sending a message")
	}
	return rt, nil
}
func (m *Manager) Send(ctx context.Context, id, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("write a message first")
	}
	if !limits.MessageFits(text) {
		return errors.New("message is too long")
	}
	rt, err := m.getRuntime(id)
	if err != nil {
		return err
	}
	rt.op.Lock()
	defer rt.op.Unlock()
	m.mu.Lock()
	c := m.chats[id]
	if m.runtimes[id] != rt {
		m.mu.Unlock()
		return errors.New("session disconnected")
	}
	if c.Status != "idle" {
		m.mu.Unlock()
		return errors.New("chat is not ready; stop or resume the current turn")
	}
	c.Status = "running"
	c.UpdatedAt = time.Now()
	if c.Title == "New chat" {
		r := []rune(strings.Join(strings.Fields(text), " "))
		if len(r) > 52 {
			r = append(r[:52], '…')
		}
		c.Title = string(r)
	}
	item := m.newItem(c, "user", "done", text)
	itemID := item.ID
	m.publish(c, item)
	if err = m.save(c); err != nil {
		c.Status = "error"
		m.publish(c, nil)
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	// Once accepted, work belongs to the application even if the POST disconnects.
	if err = rt.native.Send(m.ctx, text); err != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.runtimes[id] != rt {
			return err
		}
		var rejected *adapter.RejectedError
		message := "Could not confirm whether Codex accepted the message: " + err.Error()
		if errors.As(err, &rejected) {
			if c.Status == "running" {
				c.Status = "idle"
			}
			for n := range c.Items {
				if c.Items[n].ID == itemID {
					c.Items[n].Status = "rejected"
					m.publish(c, &c.Items[n])
					break
				}
			}
			message = "Codex rejected the message. You can edit it and retry: " + err.Error()
		}
		// A timeout is not proof of process loss or rejection. Keep the current
		// state until events report completion or the event stream closes.
		m.publish(c, m.newItem(c, "notice", "done", message))
		return errors.Join(err, m.save(c))
	}
	return nil
}
func (m *Manager) Stop(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rt, err := m.getRuntime(id)
	if err != nil {
		return err
	}
	rt.op.Lock()
	defer rt.op.Unlock()
	return rt.native.Stop(m.ctx)
}
func (m *Manager) Resume(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Serialize cold resumes without holding the manager lock during a process RPC.
	m.mu.Lock()
	c := m.chats[id]
	if c == nil {
		m.mu.Unlock()
		return errors.New("chat not found")
	}
	if m.closed {
		m.mu.Unlock()
		return errors.New("application is shutting down")
	}
	if rt := m.runtimes[id]; rt != nil {
		if c.Status == "running" {
			m.mu.Unlock()
			return errors.New("a turn is already running")
		}
		c.Status = "idle"
		err := m.save(c)
		m.publish(c, nil)
		m.mu.Unlock()
		return err
	}
	if c.Status == "running" {
		m.mu.Unlock()
		return errors.New("resume already in progress")
	}
	m.wg.Add(1)
	defer m.wg.Done()
	c.Status = "running"
	m.publish(c, nil)
	copy := clone(c)
	m.mu.Unlock()
	a := m.adapters[copy.Harness]
	if a == nil {
		return errors.New("agent is unavailable")
	}
	var native adapter.Session
	var err error
	hasConversation := false
	for _, item := range copy.Items {
		if item.Kind != "error" && item.Kind != "notice" && item.Status != "rejected" {
			hasConversation = true
			break
		}
	}
	if !hasConversation {
		native, err = a.Open(m.ctx, copy.Directory, copy.Mode)
	} else {
		native, err = a.Resume(m.ctx, copy.Directory, copy.NativeRef, copy.Mode)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c = m.chats[id]
	if err != nil {
		c.Status = "error"
		m.publish(c, m.newItem(c, "error", "done", err.Error()))
		saveErr := m.save(c)
		m.publish(c, nil)
		return errors.Join(err, saveErr)
	}
	if m.closed {
		_ = native.Close()
		return errors.New("application is shutting down")
	}
	c.NativeRef = native.Ref()
	// Keep the cold-resume reservation while persistence releases mu.
	if err = m.save(c); err != nil {
		_ = native.Close()
		c.Status = "error"
		m.publish(c, nil)
		return err
	}
	if m.closed {
		_ = native.Close()
		return errors.New("application is shutting down")
	}
	c.Status = "idle"
	m.attach(c, native)
	m.publish(c, nil)
	return nil
}
func (m *Manager) Respond(ctx context.Context, id, itemID string, answer adapter.Answer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rt, err := m.getRuntime(id)
	if err != nil {
		return err
	}
	rt.op.Lock()
	defer rt.op.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runtimes[id] != rt {
		return errors.New("session disconnected")
	}
	c := m.chats[id]
	var prompt *adapter.Prompt
	for _, i := range c.Items {
		if i.ID == itemID && i.Status == "pending" {
			prompt = i.Prompt
			break
		}
	}
	if prompt == nil {
		return errors.New("this request has expired or was already answered")
	}
	if err = rt.native.Respond(m.ctx, prompt.RequestID, answer); err != nil {
		return err
	}
	for n := range c.Items {
		if c.Items[n].ID == itemID && c.Items[n].Status == "pending" {
			c.Items[n].Status = "answered"
			m.publish(c, &c.Items[n])
			break
		}
	}
	return m.save(c)
}
func (m *Manager) newItem(c *store.Chat, kind, status, body string) *store.Item {
	c.Items = append(c.Items, store.Item{ID: store.ID(), ChatID: c.ID, Ordinal: len(c.Items), Kind: kind, Status: status, Body: body, CreatedAt: time.Now()})
	return &c.Items[len(c.Items)-1]
}
func (m *Manager) endItems(c *store.Chat) {
	for n := range c.Items {
		if c.Items[n].Status == "pending" || c.Items[n].Status == "streaming" {
			c.Items[n].Status = "interrupted"
			m.publish(c, &c.Items[n])
		}
	}
}
func (m *Manager) fail(id string, rt *runtime, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runtimes[id] != rt {
		return
	}
	c := m.chats[id]
	c.Status = "error"
	c.Live = false
	delete(m.runtimes, id)
	m.endItems(c)
	i := m.newItem(c, "error", "done", message)
	m.publish(c, i)
	if err := m.save(c); err != nil {
		slog.Error("save failed session", "error", err)
	}
}
func (m *Manager) consume(id string, rt *runtime) {
	defer m.wg.Done()
	for e := range rt.native.Events() {
		m.apply(id, rt, e)
	}
	m.mu.Lock()
	if m.closed && m.runtimes[id] == rt {
		c := m.chats[id]
		c.Live = false
		if c.Status == "running" {
			c.Status = "interrupted"
		}
		delete(m.runtimes, id)
		m.endItems(c)
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.fail(id, rt, "Codex disconnected. Resume this chat to continue.")
}
func (m *Manager) apply(id string, rt *runtime, e adapter.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runtimes[id] != rt {
		return
	}
	c := m.chats[id]
	c.UpdatedAt = time.Now()
	var item *store.Item
	switch e.Kind {
	case "text_delta", "text_done", "tool_started", "tool_updated", "tool_done":
		key := e.ID
		local := rt.itemIDs[key]
		for n := range c.Items {
			if c.Items[n].ID == local {
				item = &c.Items[n]
				break
			}
		}
		if item == nil {
			kind := "tool"
			if strings.HasPrefix(e.Kind, "text_") {
				kind = "assistant"
			}
			item = m.newItem(c, kind, "streaming", "")
			rt.itemIDs[key] = item.ID
		}
		if e.Kind == "text_delta" || e.Kind == "tool_updated" {
			item.Body += e.Text
		} else {
			item.Body = e.Text
		}
		if e.Kind == "text_done" || e.Kind == "tool_done" {
			item.Status = "done"
			if e.Status == "failed" || e.Status == "declined" {
				item.Status = e.Status
			}
		}
	case "approval_requested", "question_asked":
		kind := "approval"
		if e.Kind == "question_asked" {
			kind = "question"
		}
		item = m.newItem(c, kind, "pending", "")
		item.Prompt = e.Prompt
	case "turn_done":
		c.Status = "idle"
		if e.Status == "interrupted" {
			c.Status = "interrupted"
		}
		if e.Status == "failed" {
			c.Status = "error"
		}
		m.endItems(c)
		if e.Text != "" {
			item = m.newItem(c, "error", "done", e.Text)
		}
	case "error":
		c.Status = "error"
		m.endItems(c)
		item = m.newItem(c, "error", "done", e.Text)
	case "status":
		if e.Status == "request_resolved" {
			for n := range c.Items {
				i := &c.Items[n]
				if i.Prompt != nil && i.Prompt.RequestID == e.ID && i.Status == "pending" {
					i.Status = "interrupted"
					m.publish(c, i)
				}
			}
		}
		if e.Text != "" {
			item = m.newItem(c, "notice", "done", e.Text)
		}
	default:
		return
	}
	m.dirty[id] = true
	m.publish(c, item)
	if e.Kind != "text_delta" && e.Kind != "tool_updated" {
		if err := m.save(c); err != nil {
			slog.Error("persist event", "error", err)
			c.Status = "error"
			failure := m.newItem(c, "error", "done", "Could not save history: "+err.Error())
			m.publish(c, failure)
		}
	}
}
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	var natives []adapter.Session
	for _, rt := range m.runtimes {
		natives = append(natives, rt.native)
	}
	m.mu.Unlock()
	for _, native := range natives {
		_ = native.Close()
	}
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for _, c := range m.chats {
		if c.Status == "running" {
			c.Status = "interrupted"
		}
		m.endItems(c)
		errs = append(errs, m.save(c))
	}
	return errors.Join(errs...)
}
