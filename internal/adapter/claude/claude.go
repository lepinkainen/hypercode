// Package claude implements the Claude Code stream-json stdio protocol.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lepinkainen/hypercode/internal/adapter"
)

type Adapter struct {
	Executable string
	Args       []string
	// Trace receives raw protocol envelopes for a dedicated diagnostic session.
	// Leave nil for normal application use.
	Trace io.Writer
}

func (a Adapter) Open(ctx context.Context, dir string, mode adapter.PermissionMode, model string) (adapter.Session, error) {
	return a.connect(ctx, dir, "", mode, model)
}
func (a Adapter) Resume(ctx context.Context, dir, ref string, mode adapter.PermissionMode, model string) (adapter.Session, error) {
	return a.connect(ctx, dir, ref, mode, model)
}

// Models reads the catalog the CLI reports in its initialize response. The
// CLI's own default entry becomes the empty model id.
func (a Adapter) Models(ctx context.Context) ([]adapter.Model, error) {
	s, init, err := a.start(ctx, "", nil)
	if err != nil {
		return nil, err
	}
	_ = s.Close()
	var result struct {
		Models []struct {
			Value       string `json:"value"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
		} `json:"models"`
	}
	if err = json.Unmarshal(init, &result); err != nil {
		return nil, fmt.Errorf("decode Claude Code model catalog: %w", err)
	}
	var models []adapter.Model
	for _, m := range result.Models {
		if m.Value == "" {
			continue
		}
		id := m.Value
		isDefault := m.Value == "default"
		if isDefault {
			id = ""
		}
		name := m.DisplayName
		if name == "" {
			name = m.Value
		}
		models = append(models, adapter.Model{ID: id, Name: name, Description: m.Description, Default: isDefault})
	}
	return models, nil
}

// frame is the loose shape of one stdout line. Fields not relevant to a given
// type stay empty; nested payloads remain raw so shape drift cannot drop a line.
type frame struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID json.RawMessage `json:"parent_tool_use_id"`
	Message         json.RawMessage `json:"message"`
	Event           json.RawMessage `json:"event"`
	RequestID       string          `json:"request_id"`
	Request         json.RawMessage `json:"request"`
	Response        json.RawMessage `json:"response"`
	Error           json.RawMessage `json:"error"`
	Errors          json.RawMessage `json:"errors"`
	IsError         bool            `json:"is_error"`
	TerminalReason  string          `json:"terminal_reason"`
	Result          string          `json:"result"`
}

type controlResponse struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Error     string          `json:"error"`
	Response  json.RawMessage `json:"response"`
}

type pendingRequest struct {
	toolName    string
	input       json.RawMessage
	suggestions json.RawMessage
	prompt      adapter.Prompt
}
type toolInfo struct {
	name    string
	summary string
}
type block struct {
	id   string
	text strings.Builder
}
type session struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	writeMu    sync.Mutex
	mu         sync.Mutex
	calls      map[string]chan controlResponse
	requests   map[string]pendingRequest
	tools      map[string]toolInfo
	blocks     map[int]*block
	messageID  string
	ref        string
	model      string
	turnActive bool
	next       atomic.Uint64
	events     chan adapter.Event
	done       chan struct{}
	closing    chan struct{}
	closeOnce  sync.Once
	stderr     *tailBuffer
	trace      io.Writer
	traceMu    sync.Mutex
}

type tailBuffer struct {
	mu   sync.Mutex
	text string
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text += string(p)
	if len(b.text) > 4096 {
		b.text = b.text[len(b.text)-4096:]
	}
	return len(p), nil
}
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.text)
}

func newSession(trace io.Writer) *session {
	return &session{calls: map[string]chan controlResponse{}, requests: map[string]pendingRequest{}, tools: map[string]toolInfo{}, blocks: map[int]*block{}, events: make(chan adapter.Event, 256), done: make(chan struct{}), closing: make(chan struct{}), stderr: &tailBuffer{}, trace: trace}
}

// newUUID returns a random RFC 4122 version 4 UUID, which --session-id requires.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func permissionArgs(mode adapter.PermissionMode) []string {
	switch mode {
	case adapter.ReadOnly:
		return []string{"--permission-mode", "default"}
	case adapter.WorkspaceWrite:
		return []string{"--permission-mode", "acceptEdits"}
	default:
		return []string{"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions"}
	}
}

// start spawns the CLI with the shared stream-json flags plus extra, and
// completes the initialize handshake, returning its raw response.
func (a Adapter) start(ctx context.Context, dir string, extra []string) (*session, json.RawMessage, error) {
	exe := a.Executable
	if exe == "" {
		exe = "claude"
	}
	args := append(append([]string{}, a.Args...), "--print", "--verbose", "--output-format", "stream-json", "--input-format", "stream-json", "--include-partial-messages", "--permission-prompt-tool", "stdio")
	args = append(args, extra...)
	cmd := exec.Command(exe, args...) // Lifetime belongs to the session, never an HTTP request.
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	// CLAUDECODE marks a nested launch and makes the CLI refuse to start when
	// Hypercode itself runs inside a Claude Code session.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLAUDECODE=") && !strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_ENTRYPOINT=sdk-go")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	s := newSession(a.Trace)
	s.cmd, s.stdin, s.stdout = cmd, stdin, stdout
	cmd.Stderr = s.stderr
	if err = cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start Claude Code: %w. Install Claude Code and run claude auth login on this host", err)
	}
	go s.read(stdout)
	// initialize proves the process speaks the protocol and surfaces a failed
	// --resume before the chat is considered live.
	init, err := s.control(ctx, map[string]any{"subtype": "initialize"}, 60*time.Second)
	if err != nil {
		_ = s.Close()
		return nil, nil, err
	}
	return s, init, nil
}
func (a Adapter) connect(ctx context.Context, dir, ref string, mode adapter.PermissionMode, model string) (adapter.Session, error) {
	if !mode.Valid() {
		return nil, errors.New("invalid permission mode")
	}
	args := permissionArgs(mode)
	if ref == "" {
		ref = newUUID()
		args = append(args, "--session-id", ref)
	} else {
		args = append(args, "--resume", ref)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	s, init, err := a.start(ctx, dir, args)
	if err != nil {
		return nil, err
	}
	if model == "" {
		// Pin the effective default so the chat keeps this model even after
		// the user's Claude settings or the CLI default change.
		model = s.effectiveModel(ctx, init)
	}
	s.mu.Lock()
	s.ref = ref
	s.model = model
	s.mu.Unlock()
	return s, nil
}

// effectiveModel asks the CLI which model its settings resolve to and maps
// it back onto a catalog value when one resolves to the same model.
func (s *session) effectiveModel(ctx context.Context, init json.RawMessage) string {
	var catalog struct {
		Models []struct {
			Value    string `json:"value"`
			Resolved string `json:"resolvedModel"`
		} `json:"models"`
	}
	_ = json.Unmarshal(init, &catalog)
	effective := ""
	if raw, err := s.control(ctx, map[string]any{"subtype": "get_settings"}, 30*time.Second); err == nil {
		var settings struct {
			Effective struct {
				Model string `json:"model"`
			} `json:"effective"`
		}
		_ = json.Unmarshal(raw, &settings)
		effective = settings.Effective.Model
	}
	if effective == "" {
		for _, m := range catalog.Models {
			if m.Value == "default" {
				effective = m.Resolved
			}
		}
	}
	for _, m := range catalog.Models {
		if m.Value != "default" && (m.Value == effective || m.Resolved == effective) {
			return m.Value
		}
	}
	return effective
}
func (s *session) Ref() string                  { s.mu.Lock(); defer s.mu.Unlock(); return s.ref }
func (s *session) Model() string                { s.mu.Lock(); defer s.mu.Unlock(); return s.model }
func (s *session) Events() <-chan adapter.Event { return s.events }
func (s *session) write(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.record("sent", v)
	if s.stdin == nil { // decoder-only sessions in fixture replay
		return nil
	}
	return json.NewEncoder(s.stdin).Encode(v)
}
func (s *session) record(direction string, message any) {
	if s.trace == nil {
		return
	}
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	_ = json.NewEncoder(s.trace).Encode(map[string]any{"direction": direction, "message": message})
}

// control sends a client-originated control request and waits for its reply.
func (s *session) control(ctx context.Context, request map[string]any, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var nonce [4]byte
	_, _ = rand.Read(nonce[:])
	id := fmt.Sprintf("req_%d_%s", s.next.Add(1), hex.EncodeToString(nonce[:]))
	ch := make(chan controlResponse, 1)
	s.mu.Lock()
	s.calls[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.calls, id); s.mu.Unlock() }()
	if err := s.write(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Subtype == "error" {
			return nil, &adapter.RejectedError{Err: fmt.Errorf("Claude Code %v: %s", request["subtype"], r.Error)}
		}
		return r.Response, nil
	case <-s.done:
		return nil, fmt.Errorf("Claude Code disconnected. %s", s.stderr.String())
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *session) Send(ctx context.Context, in adapter.Input) error {
	content := []map[string]any{}
	if text := in.Prompt(); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, a := range in.Attachments {
		if !a.Image() {
			continue
		}
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return &adapter.RejectedError{Err: err}
		}
		content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": a.MediaType, "data": base64.StdEncoding.EncodeToString(data)}})
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.turnActive = true
	ref := s.ref
	s.mu.Unlock()
	// The CLI does not acknowledge a prompt; the result frame is the only
	// completion signal, so a successful write is acceptance.
	return s.write(map[string]any{"type": "user", "session_id": ref, "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": content}})
}
func (s *session) Stop(ctx context.Context) error {
	s.mu.Lock()
	active := s.turnActive
	s.mu.Unlock()
	if !active {
		return errors.New("Claude Code has not started a turn yet; try Stop again")
	}
	_, err := s.control(ctx, map[string]any{"subtype": "interrupt"}, 45*time.Second)
	return err
}
func (s *session) Respond(ctx context.Context, id string, answer adapter.Answer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[id]
	if !ok {
		return errors.New("this request is no longer active")
	}
	var result map[string]any
	if req.toolName == "AskUserQuestion" {
		answers := map[string]string{}
		for _, q := range req.prompt.Questions {
			v := answer.Answers[q.ID]
			if len(v) == 0 || strings.TrimSpace(v[0]) == "" {
				return fmt.Errorf("answer required for %s", q.Header)
			}
			answers[q.ID] = strings.Join(v, ", ")
		}
		var input map[string]json.RawMessage
		_ = json.Unmarshal(req.input, &input)
		result = map[string]any{"behavior": "allow", "updatedInput": map[string]any{"questions": input["questions"], "answers": answers}}
	} else {
		valid := false
		for _, v := range req.prompt.Decisions {
			if v == answer.Decision {
				valid = true
			}
		}
		if !valid {
			return errors.New("invalid approval decision")
		}
		switch answer.Decision {
		case "accept":
			result = map[string]any{"behavior": "allow", "updatedInput": req.input}
		case "acceptForSession":
			result = map[string]any{"behavior": "allow", "updatedInput": req.input, "updatedPermissions": sessionPermissions(req.toolName, req.suggestions)}
		case "decline":
			result = map[string]any{"behavior": "deny", "message": "User declined tool execution."}
		case "cancel":
			result = map[string]any{"behavior": "deny", "message": "User cancelled tool execution.", "interrupt": true}
		}
	}
	if err := s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": result}}); err != nil {
		return err
	}
	delete(s.requests, id)
	return nil
}

// sessionPermissions rescopes the CLI's suggested rules to this session so
// "allow for session" never writes a permanent settings rule.
func sessionPermissions(toolName string, suggestions json.RawMessage) []map[string]any {
	var list []map[string]any
	_ = json.Unmarshal(suggestions, &list)
	var out []map[string]any
	for _, s := range list {
		if s == nil {
			continue
		}
		s["destination"] = "session"
		out = append(out, s)
	}
	if len(out) == 0 {
		out = []map[string]any{{"type": "addRules", "rules": []map[string]string{{"toolName": toolName}}, "behavior": "allow", "destination": "session"}}
	}
	return out
}
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closing)
		_ = s.stdin.Close()
		_ = s.cmd.Process.Kill()
		// Unblock the scanner even if a descendant inherited stdout. WaitDelay
		// separately bounds exec's stderr copier; only our captured process dies.
		_ = s.stdout.Close()
	})
	<-s.done
	return nil
}
func (s *session) emit(e adapter.Event) {
	select {
	case s.events <- e:
	case <-s.closing:
	}
}
func (s *session) read(stdout io.Reader) {
	defer close(s.events)
	defer close(s.done)
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64*1024), 40*1024*1024)
	for scan.Scan() {
		line := bytes.TrimSpace(scan.Bytes())
		if len(line) == 0 {
			continue
		}
		s.record("received", json.RawMessage(line))
		s.handle(line)
	}
	scanErr := scan.Err()
	if scanErr != nil {
		_ = s.cmd.Process.Kill()
	}
	err := s.cmd.Wait()
	select {
	case <-s.closing:
		return
	default:
	}
	detail := "Claude Code process exited. Resume the chat to reconnect."
	if err != nil {
		detail += " " + err.Error()
	}
	if scanErr != nil {
		detail += " " + scanErr.Error()
	}
	if tail := s.stderr.String(); tail != "" {
		detail += "\n" + tail
	}
	s.emit(adapter.Event{Kind: "error", Text: detail})
}

// handle decodes one stdout line and emits normalized events.
func (s *session) handle(line []byte) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			// Wrapper scripts and shims print plain text on stdout. The process
			// is still healthy, so surface the line without ending the turn.
			text := string(line)
			if len(text) > 200 {
				text = text[:200] + "…"
			}
			slog.Warn("ignoring non-protocol Claude Code output", "line", text, "error", err)
			s.emit(adapter.Event{Kind: "status", Text: "Ignored non-protocol output from Claude Code: " + text})
			return
		}
		slog.Warn("Claude Code frame field has unexpected shape", "type", f.Type, "field", mismatch.Field, "error", err)
	}
	subagent := len(f.ParentToolUseID) > 0 && string(f.ParentToolUseID) != "null"
	switch f.Type {
	case "system":
		if f.Subtype == "init" && f.SessionID != "" {
			s.mu.Lock()
			if s.ref != f.SessionID {
				slog.Info("Claude Code session id adopted", "was", s.ref, "now", f.SessionID)
				s.ref = f.SessionID
			}
			s.mu.Unlock()
		}
	case "stream_event":
		if !subagent {
			s.streamEvent(f.Event)
		}
	case "assistant":
		if !subagent {
			s.assistant(f)
		}
	case "user":
		if !subagent {
			s.toolResults(f.Message)
		}
	case "result":
		s.result(f)
	case "control_request":
		s.controlRequest(f)
	case "control_cancel_request":
		s.mu.Lock()
		delete(s.requests, f.RequestID)
		s.mu.Unlock()
		s.emit(adapter.Event{Kind: "status", ID: f.RequestID, Status: "request_resolved"})
	case "control_response":
		var r controlResponse
		if json.Unmarshal(f.Response, &r) != nil {
			return
		}
		s.mu.Lock()
		ch := s.calls[r.RequestID]
		s.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
}
func (s *session) streamEvent(raw json.RawMessage) {
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Type {
	case "message_start":
		s.messageID = ev.Message.ID
		s.flushBlocksLocked()
	case "content_block_start":
		if ev.ContentBlock.Type == "text" {
			s.blockLocked(ev.Index)
		}
	case "content_block_delta":
		if ev.Delta.Type != "text_delta" || ev.Delta.Text == "" {
			return
		}
		b := s.blockLocked(ev.Index)
		b.text.WriteString(ev.Delta.Text)
		s.emit(adapter.Event{Kind: "text_delta", ID: b.id, Text: ev.Delta.Text})
	case "content_block_stop":
		if b := s.blocks[ev.Index]; b != nil {
			delete(s.blocks, ev.Index)
			s.emit(adapter.Event{Kind: "text_done", ID: b.id, Text: b.text.String()})
		}
	}
}
func (s *session) blockLocked(index int) *block {
	b := s.blocks[index]
	if b == nil {
		id := s.messageID
		if id == "" {
			id = "msg"
		}
		b = &block{id: fmt.Sprintf("%s-%d", id, index)}
		s.blocks[index] = b
	}
	return b
}

// flushBlocksLocked completes text blocks whose stop event never arrived.
func (s *session) flushBlocksLocked() {
	for i, b := range s.blocks {
		delete(s.blocks, i)
		if b.text.Len() > 0 {
			s.emit(adapter.Event{Kind: "text_done", ID: b.id, Text: b.text.String()})
		}
	}
}
func (s *session) assistant(f frame) {
	var m struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	_ = json.Unmarshal(f.Message, &m)
	for _, c := range m.Content {
		if c.Type != "tool_use" || c.ID == "" {
			continue
		}
		info := toolInfo{name: c.Name, summary: summarize(c.Name, c.Input)}
		s.mu.Lock()
		s.tools[c.ID] = info
		s.mu.Unlock()
		s.emit(adapter.Event{Kind: "tool_started", ID: c.ID, Text: info.summary})
	}
	if e := looseText(f.Error); e != "" {
		s.emit(adapter.Event{Kind: "status", Text: "Claude Code reported: " + e})
	}
}
func (s *session) toolResults(raw json.RawMessage) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		s.mu.Lock()
		info, known := s.tools[b.ToolUseID]
		delete(s.tools, b.ToolUseID)
		s.mu.Unlock()
		if !known {
			continue
		}
		text := info.summary
		if out := strings.TrimSpace(contentText(b.Content)); out != "" {
			if len(out) > 8000 {
				out = out[:8000] + "…"
			}
			text += "\n" + out
		}
		status := "completed"
		if b.IsError {
			status = "failed"
		}
		s.emit(adapter.Event{Kind: "tool_done", ID: b.ToolUseID, Text: text, Status: status})
	}
}
func (s *session) result(f frame) {
	s.mu.Lock()
	s.flushBlocksLocked()
	s.turnActive = false
	clear(s.requests)
	clear(s.tools)
	s.mu.Unlock()
	var listed []string
	_ = json.Unmarshal(f.Errors, &listed)
	message := ""
	for _, e := range listed {
		if !strings.HasPrefix(e, "[ede_diagnostic]") {
			message = e
			break
		}
	}
	status := "completed"
	switch {
	case f.TerminalReason == "aborted_streaming" || f.TerminalReason == "aborted_tools":
		status = "interrupted"
		message = ""
	case f.Subtype != "success" && !f.IsError && (strings.Contains(strings.ToLower(message), "interrupt") || strings.Contains(strings.ToLower(message), "abort")):
		status = "interrupted"
		message = ""
	case f.Subtype != "success" || f.IsError:
		status = "failed"
		if message == "" {
			message = f.Result
		}
		if message == "" {
			message = "Claude Code turn ended with " + f.Subtype
		}
	}
	s.emit(adapter.Event{Kind: "turn_done", Status: status, Text: message})
}
func (s *session) controlRequest(f frame) {
	var req struct {
		Subtype        string          `json:"subtype"`
		ToolName       string          `json:"tool_name"`
		Input          json.RawMessage `json:"input"`
		Suggestions    json.RawMessage `json:"permission_suggestions"`
		DecisionReason string          `json:"decision_reason"`
		Description    string          `json:"description"`
	}
	_ = json.Unmarshal(f.Request, &req)
	if req.Subtype != "can_use_tool" {
		// Fail closed for capabilities this client does not implement.
		_ = s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": f.RequestID, "error": "Unsupported control request: " + req.Subtype}})
		s.emit(adapter.Event{Kind: "status", Text: "Unsupported Claude Code request: " + req.Subtype})
		return
	}
	prompt := adapter.Prompt{RequestID: f.RequestID}
	kind := "approval_requested"
	if req.ToolName == "AskUserQuestion" {
		kind = "question_asked"
		prompt.Title = "Claude Code needs your input"
		prompt.Questions = questions(req.Input)
	} else {
		prompt.Title = "Allow " + req.ToolName + "?"
		prompt.Decisions = []string{"accept", "acceptForSession", "decline", "cancel"}
		detail := summarize(req.ToolName, req.Input)
		if req.Description != "" {
			detail += "\n" + req.Description
		}
		if r := stripANSI(req.DecisionReason); r != "" {
			detail += "\n" + r
		}
		prompt.Detail = strings.TrimSpace(detail)
	}
	s.mu.Lock()
	s.requests[f.RequestID] = pendingRequest{toolName: req.ToolName, input: req.Input, suggestions: req.Suggestions, prompt: prompt}
	s.mu.Unlock()
	s.emit(adapter.Event{Kind: kind, ID: f.RequestID, Prompt: &prompt})
}

// questions maps AskUserQuestion input. The CLI looks answers up by question
// text, so the question text doubles as the ID.
func questions(input json.RawMessage) []adapter.Question {
	var in struct {
		Questions []struct {
			Header   string           `json:"header"`
			Question string           `json:"question"`
			Options  []adapter.Option `json:"options"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(input, &in)
	var out []adapter.Question
	for i, q := range in.Questions {
		id := q.Question
		if id == "" {
			id = fmt.Sprintf("q-%d", i)
		}
		header := q.Header
		if header == "" {
			header = fmt.Sprintf("Question %d", i+1)
		}
		out = append(out, adapter.Question{ID: id, Header: header, Question: q.Question, Options: q.Options})
	}
	return out
}

// summarize renders a tool call as one readable line: the command for shells,
// the path for file tools, otherwise compact JSON.
func summarize(name string, input json.RawMessage) string {
	var in map[string]json.RawMessage
	_ = json.Unmarshal(input, &in)
	for _, key := range []string{"command", "cmd"} {
		if v := looseText(in[key]); v != "" {
			return name + ": " + truncate(strings.TrimSpace(v), 400)
		}
	}
	for _, key := range []string{"file_path", "path", "pattern", "url", "description", "prompt"} {
		if v := looseText(in[key]); v != "" {
			return name + ": " + truncate(strings.TrimSpace(v), 400)
		}
	}
	if len(in) == 0 {
		return name
	}
	var raw any
	if json.Unmarshal(input, &raw) == nil {
		b, _ := json.MarshalIndent(raw, "", "  ")
		return name + "\n" + truncate(string(b), 2000)
	}
	return name
}
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// contentText flattens a tool_result content value: a string or a list of blocks.
func contentText(raw json.RawMessage) string {
	if v := looseText(raw); !strings.HasPrefix(v, "[") {
		return v
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return strings.TrimSpace(ansi.ReplaceAllString(s, "")) }

// looseText reads a field whose shape may vary: a string, a string array, an
// object with a message, or nothing.
func looseText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, " ")
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	return string(raw)
}
