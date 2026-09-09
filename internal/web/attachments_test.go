package web

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/session"
	"github.com/lepinkainen/hypercode/internal/store"
)

type uploadAgent struct {
	events chan adapter.Event
	input  adapter.Input
	err    error
}

func (a *uploadAgent) Open(context.Context, string, adapter.PermissionMode, string) (adapter.Session, error) {
	return a, nil
}
func (a *uploadAgent) Resume(context.Context, string, string, adapter.PermissionMode, string) (adapter.Session, error) {
	return a, nil
}
func (a *uploadAgent) Models(context.Context) ([]adapter.Model, error)       { return nil, nil }
func (a *uploadAgent) Ref() string                                           { return "upload-test" }
func (a *uploadAgent) Model() string                                         { return "" }
func (a *uploadAgent) Send(_ context.Context, in adapter.Input) error        { a.input = in; return a.err }
func (a *uploadAgent) Stop(context.Context) error                            { return nil }
func (a *uploadAgent) Respond(context.Context, string, adapter.Answer) error { return nil }
func (a *uploadAgent) Events() <-chan adapter.Event                          { return a.events }
func (a *uploadAgent) Close() error                                          { close(a.events); return nil }

func uploadServer(t *testing.T) (*Server, *uploadAgent, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := &uploadAgent{events: make(chan adapter.Event)}
	m, err := session.New(db, map[string]adapter.Adapter{"codex": a})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(m, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.Create(t.Context(), "codex", t.TempDir(), adapter.ReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(); _ = db.Close() })
	return s, a, id
}

func uploadRequest(t *testing.T, s *Server, id, text string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("message", text); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		part, err := w.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/chats/"+id+"/send", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	return out
}

func TestMultipartSendAndDownload(t *testing.T) {
	s, a, id := uploadServer(t)
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	res := uploadRequest(t, s, id, "", map[string][]byte{"shot.png": img.Bytes(), "notes.md": []byte("# notes")})
	if res.Code != 204 {
		t.Fatal(res.Code, res.Body.String())
	}
	if len(a.input.Attachments) != 2 || a.input.Text != "" {
		t.Fatalf("input: %+v", a.input)
	}
	_, chat := s.manager.View(id)
	html := s.render("item", itemView{chat.Items[0], "codex"})
	if !strings.Contains(html, "shot.png") || !strings.Contains(html, "notes.md") || strings.Contains(html, a.input.Attachments[0].Path) {
		t.Fatal("incorrect attachment rendering")
	}
	for _, f := range a.input.Attachments {
		if _, err := os.Stat(f.Path); err != nil {
			t.Fatal("file missing at dispatch", err)
		}
		get := httptest.NewRecorder()
		s.ServeHTTP(get, httptest.NewRequest("GET", "/chats/"+id+"/attachments/"+f.ID, nil))
		if get.Code != 200 || get.Header().Get("Content-Type") != f.MediaType || get.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(get.Code, get.Header())
		}
		if !f.Image() && !strings.HasPrefix(get.Header().Get("Content-Disposition"), "attachment;") {
			t.Fatal("document served inline")
		}
		get = httptest.NewRecorder()
		s.ServeHTTP(get, httptest.NewRequest("GET", "/chats/"+store.ID()+"/attachments/"+f.ID, nil))
		if get.Code != 404 {
			t.Fatal("attachment accessible from another chat")
		}
	}
}

func TestUploadFailureOwnership(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "busy", true: "agent rejection"}[rejected], func(t *testing.T) {
			s, a, id := uploadServer(t)
			if rejected {
				a.err = &adapter.RejectedError{Err: errors.New("rejected")}
			} else {
				if err := s.manager.Send(t.Context(), id, adapter.Input{Text: "busy"}); err != nil {
					t.Fatal(err)
				}
			}
			res := uploadRequest(t, s, id, "retry", map[string][]byte{"notes.txt": []byte("note")})
			if res.Code != 409 {
				t.Fatal(res.Code, res.Body.String())
			}
			_, chat := s.manager.View(id)
			if rejected {
				f := chat.Items[0].Attachments[0]
				path, _ := s.uploads.Path(id, f)
				if _, err := os.Stat(path); err != nil {
					t.Fatal("rejected message lost file", err)
				}
			} else {
				path, _ := s.uploads.Path(id, adapter.Attachment{ID: store.ID(), MediaType: "text/plain"})
				entries, _ := os.ReadDir(filepath.Dir(path))
				if len(entries) != 0 {
					t.Fatal("unaccepted upload leaked", entries)
				}
			}
		})
	}
}

func TestInvalidMultipartCleanup(t *testing.T) {
	s, a, id := uploadServer(t)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile("files", "good.txt")
	_, _ = part.Write([]byte("good"))
	part, _ = w.CreateFormFile("files", "bad.png")
	_, _ = part.Write([]byte("bad"))
	_ = w.Close()
	for _, route := range []string{"/chats/" + id + "/send", "/chats"} {
		r := httptest.NewRequest("POST", route, bytes.NewReader(body.Bytes()))
		r.Header.Set("Content-Type", w.FormDataContentType())
		out := httptest.NewRecorder()
		s.ServeHTTP(out, r)
		if out.Code != 400 {
			t.Fatal(out.Code, out.Body.String())
		}
	}
	if len(a.input.Attachments) != 0 {
		t.Fatal("invalid upload reached agent")
	}
	path, _ := s.uploads.Path(id, adapter.Attachment{ID: store.ID(), MediaType: "text/plain"})
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 0 {
		t.Fatal("partial request leaked files", entries)
	}
	r := httptest.NewRequest("POST", "/chats/"+id+"/send", bytes.NewReader(body.Bytes()))
	r.Header.Set("Content-Type", w.FormDataContentType())
	r.Header.Set("Origin", "http://evil.example")
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 403 {
		t.Fatal("cross-origin upload accepted")
	}
}

func TestAttachmentCountLimit(t *testing.T) {
	s, _, id := uploadServer(t)
	res := uploadRequest(t, s, id, "", map[string][]byte{"1.txt": []byte("1"), "2.txt": []byte("2"), "3.txt": []byte("3"), "4.txt": []byte("4"), "5.txt": []byte("5"), "6.txt": []byte("6")})
	if res.Code != 400 || !strings.Contains(res.Body.String(), "Too many") {
		t.Fatal(res.Code, res.Body.String())
	}
	path, _ := s.uploads.Path(id, adapter.Attachment{ID: store.ID(), MediaType: "text/plain"})
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 0 {
		t.Fatal("count rejection leaked uploads")
	}
}
