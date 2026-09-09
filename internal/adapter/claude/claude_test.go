package claude

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/testclaude"
)

func TestProtocolProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-- hypercode-fixture") {
		return
	}
	if err := testclaude.Run(os.Stdin, os.Stdout, testclaude.SessionFromArgs(os.Args), testclaude.ModelFromArgs(os.Args)); err != nil {
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

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestLifecycle(t *testing.T) {
	a := testAdapter(t)
	s, err := a.Open(t.Context(), t.TempDir(), adapter.WorkspaceWrite, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ref := s.Ref()
	if !uuidPattern.MatchString(ref) {
		t.Fatalf("ref is not a UUID: %s", ref)
	}
	if s.Model() != "fixture-model" {
		t.Fatalf("effective model not pinned: %q", s.Model())
	}
	if err = s.Stop(t.Context()); err == nil {
		t.Fatal("Stop before any turn should fail")
	}
	if err = s.Send(t.Context(), adapter.Input{Text: "[approval]"}); err != nil {
		t.Fatal(err)
	}
	nextKind(t, s, "tool_started")
	approval := nextKind(t, s, "approval_requested")
	if approval.Prompt.Title != "Allow Bash?" || !strings.Contains(approval.Prompt.Detail, "printf 'fixture command'") {
		t.Fatalf("prompt=%+v", approval.Prompt)
	}
	if strings.Join(approval.Prompt.Decisions, ",") != "accept,acceptForSession,decline,cancel" {
		t.Fatalf("decisions=%v", approval.Prompt.Decisions)
	}
	if err = s.Respond(t.Context(), approval.ID, adapter.Answer{Decision: "maybe"}); err == nil {
		t.Fatal("invalid decision accepted")
	}
	if err = s.Respond(t.Context(), approval.ID, adapter.Answer{Decision: "decline"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Respond(t.Context(), approval.ID, adapter.Answer{Decision: "decline"}); err == nil {
		t.Fatal("duplicate reply accepted")
	}
	done := nextKind(t, s, "tool_done")
	if done.Status != "failed" || !strings.Contains(done.Text, "declined") {
		t.Fatalf("tool_done=%+v", done)
	}
	if e := nextKind(t, s, "turn_done"); e.Status != "completed" {
		t.Fatalf("turn=%+v", e)
	}
	if err = s.Send(t.Context(), adapter.Input{Text: "[question]"}); err != nil {
		t.Fatal(err)
	}
	q := nextKind(t, s, "question_asked")
	if len(q.Prompt.Questions) != 2 || q.Prompt.Questions[0].ID != "Which approach should I use?" || len(q.Prompt.Questions[0].Options) != 2 {
		t.Fatalf("questions=%+v", q.Prompt.Questions)
	}
	if err = s.Respond(t.Context(), q.ID, adapter.Answer{Answers: map[string][]string{q.Prompt.Questions[0].ID: {"Refactor"}}}); err == nil {
		t.Fatal("missing answer accepted")
	}
	if err = s.Respond(t.Context(), q.ID, adapter.Answer{Answers: map[string][]string{q.Prompt.Questions[0].ID: {"Refactor"}, q.Prompt.Questions[1].ID: {"Alpha"}}}); err != nil {
		t.Fatal(err)
	}
	text := nextKind(t, s, "text_done")
	if !strings.Contains(text.Text, `"Which approach should I use?":"Refactor"`) || !strings.Contains(text.Text, `"What should the feature be called?":"Alpha"`) {
		t.Fatalf("answers not keyed by question text: %s", text.Text)
	}
	nextKind(t, s, "turn_done")
	if err = s.Send(t.Context(), adapter.Input{Text: "[wait]"}); err != nil {
		t.Fatal(err)
	}
	nextKind(t, s, "text_delta")
	if err = s.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e := nextKind(t, s, "turn_done"); e.Status != "interrupted" {
		t.Fatalf("turn=%+v", e)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := a.Resume(t.Context(), t.TempDir(), ref, adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumed.Close() }()
	if resumed.Ref() != ref {
		t.Fatalf("resume changed ref: %s", resumed.Ref())
	}
	if err = resumed.Send(t.Context(), adapter.Input{Text: "hello again"}); err != nil {
		t.Fatal(err)
	}
	if e := nextKind(t, resumed, "text_done"); !strings.Contains(e.Text, "Ready to build") {
		t.Fatalf("text=%q", e.Text)
	}
	nextKind(t, resumed, "turn_done")
}
func TestProcessLoss(t *testing.T) {
	s, err := testAdapter(t).Open(t.Context(), t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_ = s.Send(t.Context(), adapter.Input{Text: "[crash]"})
	e := nextKind(t, s, "error")
	if !strings.Contains(e.Text, "Resume the chat") {
		t.Fatalf("error=%q", e.Text)
	}
	if _, ok := <-s.Events(); ok {
		t.Fatal("event stream stayed open after process loss")
	}
}
func TestMissingExecutable(t *testing.T) {
	_, err := (Adapter{Executable: "/nonexistent/claude"}).Open(context.Background(), t.TempDir(), adapter.ReadOnly, "")
	if err == nil || !strings.Contains(err.Error(), "Install Claude Code") {
		t.Fatalf("error=%v", err)
	}
}
func TestCancelledRequestExpires(t *testing.T) {
	s, err := testAdapter(t).Open(t.Context(), t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.Send(t.Context(), adapter.Input{Text: "[cancel]"}); err != nil {
		t.Fatal(err)
	}
	approval := nextKind(t, s, "approval_requested")
	resolved := nextKind(t, s, "status")
	if resolved.Status != "request_resolved" || resolved.ID != approval.ID {
		t.Fatalf("status=%+v", resolved)
	}
	if err = s.Respond(t.Context(), approval.ID, adapter.Answer{Decision: "accept"}); err == nil {
		t.Fatal("withdrawn request still accepts replies")
	}
	if e := nextKind(t, s, "turn_done"); e.Status != "completed" {
		t.Fatalf("turn=%+v", e)
	}
}
func TestPlainTextStdoutIsNotFatal(t *testing.T) {
	s, err := testAdapter(t).Open(t.Context(), t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.Send(t.Context(), adapter.Input{Text: "[noise]"}); err != nil {
		t.Fatal(err)
	}
	noticed := false
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatal("event stream closed")
			}
			switch e.Kind {
			case "error":
				t.Fatalf("plain text on stdout was treated as fatal: %q", e.Text)
			case "status":
				if !strings.Contains(e.Text, "npm WARN fixture") {
					t.Fatalf("status text=%q", e.Text)
				}
				noticed = true
			case "turn_done":
				if !noticed {
					t.Fatal("no notice about ignored output")
				}
				return
			}
		case <-t.Context().Done():
			t.Fatal("test canceled")
		}
	}
}
func replay(t *testing.T, path string) []adapter.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(nil)
	s.ref = "recorded-session"
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		s.handle([]byte(line))
	}
	close(s.events)
	var got []adapter.Event
	for e := range s.events {
		got = append(got, e)
	}
	return got
}
func TestSyntheticFixture(t *testing.T) {
	got := replay(t, "testdata/notifications.jsonl")
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.Kind)
	}
	want := "text_delta,text_delta,text_done,tool_started,approval_requested,tool_done,question_asked,status,status,turn_done"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("got %v", kinds)
	}
	if got[2].Text != "Hello world" || got[2].ID != "message-1-0" {
		t.Fatalf("text_done=%+v", got[2])
	}
	if got[3].ID != "tool-1" || got[3].Text != "Bash: pwd" || got[5].Text != "Bash: pwd\n/tmp/project" || got[5].Status != "completed" {
		t.Fatalf("tool events=%+v %+v", got[3], got[5])
	}
	if got[7].Status != "request_resolved" || got[7].ID != "request-2" {
		t.Fatalf("cancel=%+v", got[7])
	}
	if !strings.Contains(got[8].Text, "future_thing") {
		t.Fatalf("unsupported request notice=%+v", got[8])
	}
}
func TestRecordedProtocol(t *testing.T) {
	for _, name := range []string{"live", "approval", "question", "interrupt", "image"} {
		t.Run(name, func(t *testing.T) {
			var body, status string
			var prompt *adapter.Prompt
			toolStatus := ""
			for _, e := range replay(t, "testdata/recorded-"+name+".jsonl") {
				switch e.Kind {
				case "text_done":
					body = e.Text
				case "turn_done":
					status = e.Status
				case "approval_requested", "question_asked":
					prompt = e.Prompt
				case "tool_done":
					toolStatus = e.Status
				}
			}
			switch name {
			case "live":
				if body != "Hypercode connected." {
					t.Fatalf("body=%q", body)
				}
			case "approval":
				if prompt == nil || prompt.Title != "Allow Bash?" || !strings.Contains(prompt.Detail, "touch approval-test.txt") || toolStatus != "failed" {
					t.Fatalf("prompt=%+v tool=%s", prompt, toolStatus)
				}
			case "question":
				if prompt == nil || len(prompt.Questions) != 1 || prompt.Questions[0].ID != "Which color?" || prompt.Questions[0].Options[1].Label != "Blue" {
					t.Fatalf("prompt=%+v", prompt)
				}
			}
			if name == "image" && !strings.Contains(strings.ToLower(body), "red") {
				t.Fatalf("image response lost: %q", body)
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
func TestResultFailure(t *testing.T) {
	s := newSession(nil)
	s.handle([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["[ede_diagnostic] x","Not logged in"],"session_id":"s"}`))
	s.handle([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s"}`))
	close(s.events)
	var got []adapter.Event
	for e := range s.events {
		got = append(got, e)
	}
	if len(got) != 2 || got[0].Status != "failed" || got[0].Text != "Not logged in" || got[1].Status != "failed" || !strings.Contains(got[1].Text, "error_max_turns") {
		t.Fatalf("events=%+v", got)
	}
}
func TestModels(t *testing.T) {
	models, err := testAdapter(t).Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "" || !models[0].Default || models[1].ID != "fixture-fast" || models[1].Name != "Fixture Fast" {
		t.Fatalf("models=%+v", models)
	}
}
func TestModelFlag(t *testing.T) {
	s, err := testAdapter(t).Open(t.Context(), t.TempDir(), adapter.ReadOnly, "fixture-fast")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Model() != "fixture-fast" {
		t.Fatalf("model=%q", s.Model())
	}
	if err = s.Send(t.Context(), adapter.Input{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if e := nextKind(t, s, "text_done"); !strings.Contains(e.Text, "model: fixture-fast") {
		t.Fatalf("model flag not passed: %q", e.Text)
	}
}

func TestAttachmentInputs(t *testing.T) {
	a := testAdapter(t)
	s, err := a.Open(t.Context(), t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, []byte("image fixture bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	img := adapter.Attachment{Name: "image.png", Path: path, MediaType: "image/png"}
	for _, text := range []string{"", "Review these"} {
		in := adapter.Input{Text: text, Attachments: []adapter.Attachment{img, img}}
		if text != "" {
			in.Attachments = append(in.Attachments, adapter.Attachment{Name: "notes.txt", Path: "/tmp/notes.txt", MediaType: "text/plain"})
		}
		if err := s.Send(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		got := nextKind(t, s, "text_done").Text
		if !strings.Contains(got, "Received 2 images (38 bytes)") {
			t.Fatalf("image payload lost: %s", got)
		}
		if text != "" && (!strings.Contains(got, text) || !strings.Contains(got, "/tmp/notes.txt")) {
			t.Fatalf("document reference lost: %s", got)
		}
		nextKind(t, s, "turn_done")
	}
}

func TestLargeImageFrame(t *testing.T) {
	a := testAdapter(t)
	s, err := a.Open(t.Context(), t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(t.TempDir(), "large.png")
	// Three allowed-size images produce a base64 input larger than 16 MiB.
	if err := os.WriteFile(path, make([]byte, 4_500_000), 0600); err != nil {
		t.Fatal(err)
	}
	img := adapter.Attachment{Name: "large.png", Path: path, MediaType: "image/png"}
	if err := s.Send(t.Context(), adapter.Input{Attachments: []adapter.Attachment{img, img, img}}); err != nil {
		t.Fatal(err)
	}
	got := nextKind(t, s, "text_done").Text
	if !strings.Contains(got, "Received 3 images (13500000 bytes)") {
		t.Fatal(got)
	}
	nextKind(t, s, "turn_done")
}
