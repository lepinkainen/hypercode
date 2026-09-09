package codex

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"hypercode/internal/adapter"
	"hypercode/internal/testcodex"
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
	s, err := a.Open(t.Context(), t.TempDir(), adapter.WorkspaceWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Ref() != "fixture-thread" {
		t.Fatalf("ref=%s", s.Ref())
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
	resumed, err := a.Resume(t.Context(), t.TempDir(), ref, adapter.ReadOnly)
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
	s, err := testAdapter(t).Open(t.Context(), t.TempDir(), adapter.ReadOnly)
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
	_, err := (Adapter{Executable: "/nonexistent/codex"}).Open(context.Background(), t.TempDir(), adapter.ReadOnly)
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
