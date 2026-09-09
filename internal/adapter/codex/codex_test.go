package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/testcodex"
)

func TestProtocolProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-- hypercode-fixture") {
		return
	}
	if err := testcodex.Run(os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
func testAdapter(t *testing.T) Adapter {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Adapter{Executable: exe, Args: []string{"-test.run=^TestProtocolProcess$", "--", "hypercode-fixture"}}
}
func nextKind(t *testing.T, s adapter.Session, kind string) adapter.Event {
	t.Helper()
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatal("event stream closed")
			}
			if e.Kind == kind {
				return e
			}
		case <-t.Context().Done():
			t.Fatal("test canceled")
		}
	}
}
func TestLifecycle(t *testing.T) {
	a := testAdapter(t)
	s, err := a.Open(t.Context(), t.TempDir(), adapter.WorkspaceWrite, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Ref() != "fixture-thread" {
		t.Fatalf("ref=%s", s.Ref())
	}
	if s.Model() != "fixture-large" {
		t.Fatalf("effective model not reported: %q", s.Model())
	}
	if err = s.Send(t.Context(), "[approval]"); err != nil {
		t.Fatal(err)
	}
	prompt := nextKind(t, s, "approval_requested")
	if prompt.Prompt.RequestID != `"approval-1"` {
		t.Fatalf("string request id lost: %+v", prompt)
	}
	if err = s.Respond(t.Context(), prompt.ID, adapter.Answer{Decision: "invalid"}); err == nil {
		t.Fatal("accepted invalid decision")
	}
	if err = s.Respond(t.Context(), prompt.ID, adapter.Answer{Decision: "acceptForSession"}); err != nil {
		t.Fatal(err)
	}
	if nextKind(t, s, "turn_done").Status != "completed" {
		t.Fatal("turn did not complete")
	}
	if err = s.Respond(t.Context(), prompt.ID, adapter.Answer{Decision: "accept"}); err == nil {
		t.Fatal("accepted duplicate approval")
	}
	if err = s.Send(t.Context(), "[question]"); err != nil {
		t.Fatal(err)
	}
	q := nextKind(t, s, "question_asked")
	if q.Prompt.RequestID != "99" {
		t.Fatal("numeric id not preserved")
	}
	if err = s.Respond(t.Context(), q.ID, adapter.Answer{Answers: map[string][]string{"approach": {"Small change"}, "name": {"Hypercode"}}}); err != nil {
		t.Fatal(err)
	}
	nextKind(t, s, "turn_done")
	if err = s.Send(t.Context(), "[wait]"); err != nil {
		t.Fatal(err)
	}
	nextKind(t, s, "text_delta")
	if err = s.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if nextKind(t, s, "turn_done").Status != "interrupted" {
		t.Fatal("interrupt failed")
	}
	ref := s.Ref()
	_ = s.Close()
	resumed, err := a.Resume(t.Context(), t.TempDir(), ref, adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumed.Close() }()
	if resumed.Ref() != ref {
		t.Fatal("resume changed reference")
	}
	if err = resumed.Send(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	nextKind(t, resumed, "turn_done")
}
func TestProcessLoss(t *testing.T) {
	s, err := testAdapter(t).Open(t.Context(), t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_ = s.Send(t.Context(), "[crash]")
	nextKind(t, s, "error")
}
func TestNotificationFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/notifications.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	s := &session{ref: "recorded-thread", events: make(chan adapter.Event, 32), closing: make(chan struct{}), requests: map[string]pendingRequest{}}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var p packet
		if err = json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatal(err)
		}
		s.notification(p)
	}
	close(s.events)
	var kinds []string
	for e := range s.events {
		kinds = append(kinds, e.Kind)
	}
	want := "text_delta,text_done,tool_started,tool_updated,tool_done,turn_done"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("got %v", kinds)
	}
}
func TestMissingExecutable(t *testing.T) {
	_, err := (Adapter{Executable: "/nonexistent/codex"}).Open(context.Background(), t.TempDir(), adapter.ReadOnly, "")
	if err == nil || !strings.Contains(err.Error(), "Install Codex") {
		t.Fatalf("error=%v", err)
	}
}

func TestRecordedProtocol(t *testing.T) {
	for _, name := range []string{"live", "approval", "interrupt"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/recorded-" + name + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			s := &session{ref: "recorded-thread", events: make(chan adapter.Event, 256), closing: make(chan struct{}), requests: map[string]pendingRequest{}}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var p packet
				if err = json.Unmarshal([]byte(line), &p); err != nil {
					t.Fatal(err)
				}
				if len(p.ID) > 0 {
					s.request(p)
				} else {
					s.notification(p)
				}
			}
			close(s.events)
			var body, status string
			approval := false
			for e := range s.events {
				switch e.Kind {
				case "text_done":
					body = e.Text
				case "turn_done":
					status = e.Status
				case "approval_requested":
					approval = true
					if strings.Join(e.Prompt.Decisions, ",") != "accept,cancel" {
						t.Fatalf("did not honor available decisions: %v", e.Prompt.Decisions)
					}
				}
			}
			if name == "live" && body != "Hypercode connected." {
				t.Fatalf("body=%q", body)
			}
			if name == "approval" && !approval {
				t.Fatal("recorded approval not emitted")
			}
			expected := "completed"
			if name == "interrupt" {
				expected = "interrupted"
			}
			if status != expected {
				t.Fatalf("status=%s", status)
			}
		})
	}
}

func TestResolvedServerRequestExpires(t *testing.T) {
	s := &session{ref: "thread", events: make(chan adapter.Event, 4), closing: make(chan struct{}), requests: map[string]pendingRequest{"7": {}}}
	s.notification(packet{Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":7}`)})
	e := <-s.events
	if e.Kind != "status" || e.Status != "request_resolved" || e.ID != "7" {
		t.Fatalf("event=%+v", e)
	}
	if _, exists := s.requests["7"]; exists {
		t.Fatal("resolved request still accepts replies")
	}
}

func TestExplicitRPCRejection(t *testing.T) {
	s, err := testAdapter(t).Open(t.Context(), t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Send(t.Context(), "[reject]")
	var rejected *adapter.RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("RPC rejection was not classified: %v", err)
	}
	if !strings.Contains(err.Error(), "-32602") {
		t.Fatalf("RPC code lost: %v", err)
	}
	if err = s.Send(t.Context(), "a valid retry"); err != nil {
		t.Fatal(err)
	}
	nextKind(t, s, "turn_done")
}

// The descendant stays alive until the test releases its connection. This makes
// pipe inheritance deterministic without relying on a particular MCP server.
func TestInheritedPipeProcess(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "pipe-parent" && arg != "pipe-child" {
			continue
		}
		if arg == "pipe-child" {
			conn, err := net.Dial("tcp", os.Args[i+1])
			if err != nil {
				os.Exit(1)
			}
			_, _ = io.Copy(io.Discard, conn)
			os.Exit(0)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestInheritedPipeProcess$", "--", "pipe-child", os.Args[i+1])
		if os.Args[i+2] == "stdout" {
			child.Stdout = os.Stdout
		} else {
			child.Stderr = os.Stderr
		}
		if child.Start() != nil {
			os.Exit(1)
		}
		if testcodex.Run(os.Stdin, os.Stdout) != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestCloseWithInheritedPipe(t *testing.T) {
	for _, pipe := range []string{"stdout", "stderr"} {
		t.Run(pipe, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			a := testAdapter(t)
			a.Args = []string{"-test.run=^TestInheritedPipeProcess$", "--", "pipe-parent", listener.Addr().String(), pipe}
			s, err := a.Open(t.Context(), t.TempDir(), adapter.ReadOnly, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
			child, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close()
			done := make(chan struct{})
			go func() { _ = s.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Error("Close waited for an unrelated descendant to release " + pipe)
				_ = child.Close()
				<-done
			}
		})
	}
}

func TestPlainTextStdoutIsNotFatal(t *testing.T) {
	s, err := testAdapter(t).Open(t.Context(), t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.Send(t.Context(), "[noise]"); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatal("event stream closed")
			}
			kinds = append(kinds, e.Kind)
			if e.Kind == "error" {
				t.Fatalf("plain text on stdout was treated as fatal: %q", e.Text)
			}
			if e.Kind == "status" && !strings.Contains(e.Text, "npm WARN fixture") {
				t.Fatalf("status text=%q", e.Text)
			}
			if e.Kind == "turn_done" {
				if !strings.Contains(strings.Join(kinds, ","), "status") {
					t.Fatalf("no notice about ignored output: %v", kinds)
				}
				return
			}
		case <-t.Context().Done():
			t.Fatal("test canceled")
		}
	}
}
func TestNotificationToleratesShapeChanges(t *testing.T) {
	s := &session{ref: "recorded-thread", events: make(chan adapter.Event, 32), closing: make(chan struct{}), requests: map[string]pendingRequest{}}
	lines := []string{
		// item.command as argv array instead of a string.
		`{"method":"item/completed","params":{"threadId":"recorded-thread","item":{"id":"c1","type":"commandExecution","command":["git","status"],"aggregatedOutput":"clean","status":"completed"}}}`,
		// turn.error as a bare string instead of an object.
		`{"method":"turn/completed","params":{"threadId":"recorded-thread","turn":{"id":"1","status":"failed","error":"quota exceeded"}}}`,
	}
	for _, line := range lines {
		var p packet
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatal(err)
		}
		s.notification(p)
	}
	close(s.events)
	var got []adapter.Event
	for e := range s.events {
		got = append(got, e)
	}
	if len(got) != 2 {
		t.Fatalf("events=%+v", got)
	}
	if got[0].Kind != "tool_done" || !strings.HasPrefix(got[0].Text, "git status") {
		t.Fatalf("tool event=%+v", got[0])
	}
	if got[1].Kind != "turn_done" || got[1].Status != "failed" || got[1].Text != "quota exceeded" {
		t.Fatalf("turn event=%+v", got[1])
	}
}
func TestModels(t *testing.T) {
	models, err := testAdapter(t).Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "fixture-large" || !models[0].Default || models[1].ID != "fixture-small" {
		t.Fatalf("hidden models not filtered or order lost: %+v", models)
	}
}
func TestModelParam(t *testing.T) {
	s, err := testAdapter(t).Open(t.Context(), t.TempDir(), adapter.ReadOnly, "fixture-small")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.Send(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if e := nextKind(t, s, "text_done"); !strings.Contains(e.Text, "model: fixture-small") {
		t.Fatalf("model not passed to thread/start: %q", e.Text)
	}
}
