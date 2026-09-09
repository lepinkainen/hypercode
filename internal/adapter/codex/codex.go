// Package codex implements the Codex app-server stdio protocol.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
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

// Models asks a short-lived app-server for the account's model catalog.
func (a Adapter) Models(ctx context.Context) ([]adapter.Model, error) {
	s, err := a.start(ctx, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	var result struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			Hidden      bool   `json:"hidden"`
			IsDefault   bool   `json:"isDefault"`
		} `json:"data"`
	}
	if err = s.call(ctx, "model/list", map[string]any{"limit": 100}, &result); err != nil {
		return nil, err
	}
	var models []adapter.Model
	for _, m := range result.Data {
		if m.Hidden || m.ID == "" {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		models = append(models, adapter.Model{ID: m.ID, Name: name, Description: m.Description, Default: m.IsDefault})
	}
	return models, nil
}

type packet struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

type pendingRequest struct {
	id     json.RawMessage
	method string
	prompt adapter.Prompt
}
type session struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	writeMu   sync.Mutex
	mu        sync.Mutex
	calls     map[string]chan packet
	requests  map[string]pendingRequest
	ref       string
	model     string
	turn      string
	next      atomic.Uint64
	events    chan adapter.Event
	done      chan struct{}
	closing   chan struct{}
	closeOnce sync.Once
	stderr    *tailBuffer
	trace     io.Writer
	traceMu   sync.Mutex
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

// start spawns the app-server and completes the initialize handshake.
func (a Adapter) start(ctx context.Context, dir string) (*session, error) {
	exe := a.Executable
	if exe == "" {
		exe = "codex"
	}
	args := append(append([]string{}, a.Args...), "app-server", "--listen", "stdio://")
	cmd := exec.Command(exe, args...) // Lifetime belongs to the session, never an HTTP request.
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &session{cmd: cmd, stdin: stdin, stdout: stdout, calls: map[string]chan packet{}, requests: map[string]pendingRequest{}, events: make(chan adapter.Event, 256), done: make(chan struct{}), closing: make(chan struct{}), stderr: &tailBuffer{}, trace: a.Trace}
	cmd.Stderr = s.stderr
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Codex: %w. Install Codex and run codex login on this host", err)
	}
	go s.read(stdout)
	if err = s.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "hypercode", "title": "Hypercode", "version": "0.1.0"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err = s.write(map[string]any{"method": "initialized"}); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}
func (a Adapter) connect(ctx context.Context, dir, ref string, mode adapter.PermissionMode, model string) (adapter.Session, error) {
	if !mode.Valid() {
		return nil, errors.New("invalid permission mode")
	}
	s, err := a.start(ctx, dir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (adapter.Session, error) { _ = s.Close(); return nil, err }
	sandbox := string(mode)
	policy := "on-request"
	if mode == adapter.FullAccess {
		sandbox = "danger-full-access"
		policy = "never"
	}
	params := map[string]any{"cwd": dir, "sandbox": sandbox, "approvalPolicy": policy, "approvalsReviewer": "user"}
	if model != "" {
		params["model"] = model
	}
	method := "thread/start"
	if ref != "" {
		method = "thread/resume"
		params["threadId"] = ref
		params["excludeTurns"] = true
	}
	var result struct {
		Thread struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"thread"`
		Model string `json:"model"`
	}
	if err = s.call(ctx, method, params, &result); err != nil {
		return fail(err)
	}
	if result.Thread.ID == "" {
		return fail(errors.New("Codex returned no thread ID"))
	}
	s.mu.Lock()
	s.ref = result.Thread.ID
	s.model = result.Model
	if s.model == "" {
		s.model = result.Thread.Model
	}
	if s.model == "" {
		s.model = model
	}
	s.mu.Unlock()
	return s, nil
}
func (s *session) Ref() string                  { s.mu.Lock(); defer s.mu.Unlock(); return s.ref }
func (s *session) Model() string                { s.mu.Lock(); defer s.mu.Unlock(); return s.model }
func (s *session) Events() <-chan adapter.Event { return s.events }
func (s *session) write(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.record("sent", v)
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
func (s *session) call(ctx context.Context, method string, params any, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	id := fmt.Sprint(s.next.Add(1))
	ch := make(chan packet, 1)
	s.mu.Lock()
	s.calls[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.calls, id); s.mu.Unlock() }()
	if err := s.write(map[string]any{"id": json.RawMessage(id), "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case p := <-ch:
		if p.Error != nil {
			return &adapter.RejectedError{Err: fmt.Errorf("Codex %s: %w", method, p.Error)}
		}
		if result != nil {
			return json.Unmarshal(p.Result, result)
		}
		return nil
	case <-s.done:
		return fmt.Errorf("Codex disconnected. %s", s.stderr.String())
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *session) Send(ctx context.Context, in adapter.Input) error {
	input := []map[string]any{}
	if text := in.Prompt(); text != "" {
		input = append(input, map[string]any{"type": "text", "text": text, "text_elements": []any{}})
	}
	for _, a := range in.Attachments {
		if a.Image() {
			input = append(input, map[string]any{"type": "localImage", "path": a.Path})
		}
	}

	// turn/started is authoritative; setting the ID from a late RPC response can
	// resurrect an already completed turn.
	return s.call(ctx, "turn/start", map[string]any{"threadId": s.Ref(), "input": input}, nil)
}
func (s *session) Stop(ctx context.Context) error {
	s.mu.Lock()
	turn := s.turn
	s.mu.Unlock()
	if turn == "" {
		return errors.New("Codex has not started a turn yet; try Stop again")
	}
	return s.call(ctx, "turn/interrupt", map[string]string{"threadId": s.Ref(), "turnId": turn}, nil)
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
	var result any
	if req.method == "item/tool/requestUserInput" {
		answers := map[string]any{}
		for _, q := range req.prompt.Questions {
			v := answer.Answers[q.ID]
			if len(v) == 0 || strings.TrimSpace(v[0]) == "" {
				return fmt.Errorf("answer required for %s", q.Header)
			}
			answers[q.ID] = map[string]any{"answers": v}
		}
		result = map[string]any{"answers": answers}
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
		result = map[string]string{"decision": answer.Decision}
	}
	if err := s.write(map[string]any{"id": req.id, "result": result}); err != nil {
		return err
	}
	delete(s.requests, id)
	return nil
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
		var p packet
		if err := json.Unmarshal(line, &p); err != nil {
			// Wrapper scripts and shims print plain text on stdout. The process
			// is still healthy, so surface the line without ending the turn.
			text := string(line)
			if len(text) > 200 {
				text = text[:200] + "…"
			}
			slog.Warn("ignoring non-protocol Codex output", "line", text, "error", err)
			s.emit(adapter.Event{Kind: "status", Text: "Ignored non-protocol output from Codex: " + text})
			continue
		}
		if p.Method == "" {
			s.mu.Lock()
			ch := s.calls[string(p.ID)]
			s.mu.Unlock()
			if ch != nil {
				ch <- p
			}
			continue
		}
		if len(p.ID) > 0 {
			s.request(p)
			continue
		}
		s.notification(p)
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
	detail := "Codex process exited. Resume the chat to reconnect."
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
func (s *session) request(p packet) {
	var params struct {
		Reason             string             `json:"reason"`
		Command            string             `json:"command"`
		Cwd                string             `json:"cwd"`
		Questions          []adapter.Question `json:"questions"`
		AvailableDecisions []json.RawMessage  `json:"availableDecisions"`
	}
	if err := json.Unmarshal(p.Params, &params); err != nil {
		_ = s.write(map[string]any{"id": p.ID, "error": rpcError{-32602, "Invalid request parameters"}})
		return
	}
	prompt := adapter.Prompt{RequestID: string(p.ID), Detail: strings.TrimSpace(params.Command + "\n" + params.Cwd + "\n" + params.Reason)}
	kind := "approval_requested"
	switch p.Method {
	case "item/commandExecution/requestApproval":
		prompt.Title = "Allow this command?"
		prompt.Decisions = []string{"accept", "acceptForSession", "decline", "cancel"}
	case "item/fileChange/requestApproval":
		prompt.Title = "Allow these file changes?"
		prompt.Decisions = []string{"accept", "acceptForSession", "decline", "cancel"}
	case "item/tool/requestUserInput":
		kind = "question_asked"
		prompt.Title = "Codex needs your input"
		prompt.Questions = params.Questions
	default:
		// Fail closed for capabilities this client does not implement.
		_ = s.write(map[string]any{"id": p.ID, "error": rpcError{-32601, "Unsupported client request: " + p.Method}})
		s.emit(adapter.Event{Kind: "status", Text: "Unsupported Codex request: " + p.Method})
		return
	}
	if kind == "approval_requested" && len(params.AvailableDecisions) > 0 {
		prompt.Decisions = nil
		for _, raw := range params.AvailableDecisions {
			var decision string
			if json.Unmarshal(raw, &decision) == nil {
				switch decision {
				case "accept", "acceptForSession", "decline", "cancel":
					prompt.Decisions = append(prompt.Decisions, decision)
				}
			}
		}
		if len(prompt.Decisions) == 0 {
			_ = s.write(map[string]any{"id": p.ID, "error": rpcError{-32601, "No supported approval decisions"}})
			s.emit(adapter.Event{Kind: "status", Text: "Codex requested an unsupported approval policy."})
			return
		}
	}
	s.mu.Lock()
	s.requests[prompt.RequestID] = pendingRequest{p.ID, p.Method, prompt}
	s.mu.Unlock()
	s.emit(adapter.Event{Kind: kind, ID: prompt.RequestID, Prompt: &prompt})
}
func (s *session) notification(p packet) {
	var v struct {
		ThreadID  string          `json:"threadId"`
		ItemID    string          `json:"itemId"`
		RequestID json.RawMessage `json:"requestId"`
		Delta     string          `json:"delta"`
		Item      struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Command json.RawMessage `json:"command"`
			Output  string          `json:"aggregatedOutput"`
			Status  string          `json:"status"`
		} `json:"item"`
		Turn struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"turn"`
		Error     json.RawMessage `json:"error"`
		WillRetry bool            `json:"willRetry"`
	}
	if err := json.Unmarshal(p.Params, &v); err != nil {
		// A type mismatch still fills every other field. Dropping the whole
		// notification would lose turn/completed and leave the chat running.
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			slog.Warn("ignoring undecodable Codex notification", "method", p.Method, "error", err)
			return
		}
		slog.Warn("Codex notification field has unexpected shape", "method", p.Method, "field", mismatch.Field, "error", err)
	}
	ref := s.Ref()
	if v.ThreadID != "" && ref != "" && v.ThreadID != ref {
		return
	}
	switch p.Method {
	case "serverRequest/resolved":
		id := string(v.RequestID)
		s.mu.Lock()
		delete(s.requests, id)
		s.mu.Unlock()
		s.emit(adapter.Event{Kind: "status", ID: id, Status: "request_resolved"})
	case "turn/started":
		s.mu.Lock()
		s.turn = v.Turn.ID
		s.mu.Unlock()
	case "item/agentMessage/delta":
		s.emit(adapter.Event{Kind: "text_delta", ID: v.ItemID, Text: v.Delta})
	case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		s.emit(adapter.Event{Kind: "tool_updated", ID: v.ItemID, Text: v.Delta})
	case "item/started", "item/completed":
		completed := p.Method == "item/completed"
		if v.Item.Type == "userMessage" || v.Item.Type == "reasoning" {
			return
		}
		if v.Item.Type == "agentMessage" {
			if completed {
				s.emit(adapter.Event{Kind: "text_done", ID: v.Item.ID, Text: v.Item.Text})
			}
			return
		}
		text := looseText(v.Item.Command)
		if text == "" {
			text = v.Item.Type
		}
		if v.Item.Output != "" {
			text += "\n" + v.Item.Output
		} else if v.Item.Type != "commandExecution" {
			var pretty strings.Builder
			var raw any
			if json.Unmarshal(p.Params, &raw) == nil {
				b, _ := json.MarshalIndent(raw, "", "  ")
				pretty.Write(b)
			}
			text += "\n" + pretty.String()
		}
		kind := "tool_started"
		if completed {
			kind = "tool_done"
		}
		s.emit(adapter.Event{Kind: kind, ID: v.Item.ID, Text: text, Status: v.Item.Status})
	case "turn/completed":
		s.mu.Lock()
		s.turn = ""
		clear(s.requests)
		s.mu.Unlock()
		s.emit(adapter.Event{Kind: "turn_done", Status: v.Turn.Status, Text: looseText(v.Turn.Error)})
	case "error":
		kind := "error"
		if v.WillRetry {
			kind = "status"
		}
		s.emit(adapter.Event{Kind: kind, Text: looseText(v.Error)})
	}
}

// looseText reads a field whose shape varies across Codex versions: a string,
// an argv array, an object with a message, or nothing.
func looseText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var argv []string
	if json.Unmarshal(raw, &argv) == nil {
		return strings.Join(argv, " ")
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	return string(raw)
}
