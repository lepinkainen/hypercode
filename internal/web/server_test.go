package web

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"hypercode/internal/adapter"
	"hypercode/internal/session"
	"hypercode/internal/store"
)

func testServer(t *testing.T) (*Server, *session.Manager) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := session.New(db, map[string]adapter.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(m, "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(); _ = db.Close() })
	return s, m
}
func TestHomeAndEmbeddedAssets(t *testing.T) {
	s, _ := testServer(t)
	for _, path := range []string{"/", "/assets/htmx.min.js", "/assets/app.js", "/assets/app.css", "/healthz"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if path == "/" && !strings.Contains(w.Body.String(), "Create chat") {
			t.Fatal("missing form")
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("HX-Request", "true")
	s.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "<!doctype") {
		t.Fatal("partial included full document")
	}
}
func TestMutationOriginBoundary(t *testing.T) {
	s, _ := testServer(t)
	for _, tt := range []struct {
		origin, site string
		blocked      bool
	}{{"http://evil.example", "cross-site", true}, {"http://example.com", "same-origin", false}, {"https://example.com", "same-origin", true}, {"null", "", true}, {"", "same-site", true}, {"", "", false}} {
		r := httptest.NewRequest("POST", "http://example.com/chats", strings.NewReader(url.Values{"directory": {"/tmp"}, "mode": {"read-only"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", tt.origin)
		r.Header.Set("Sec-Fetch-Site", tt.site)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if (w.Code == 403) != tt.blocked {
			t.Errorf("%+v: %d", tt, w.Code)
		}
	}
}
func TestMarkdownEscapesHTMLAndUnsafeLinks(t *testing.T) {
	s, _ := testServer(t)
	html := s.render("item", store.Item{ID: "safe", Kind: "assistant", Status: "done", Body: "<script>alert(1)</script>\n\n[bad](javascript:alert(1))\n\n**safe**"})
	if strings.Contains(html, "<script>") || strings.Contains(html, `href="javascript:`) {
		t.Fatal("unsafe markdown")
	}
	if !strings.Contains(html, "<strong>safe</strong>") {
		t.Fatal("markdown not rendered")
	}
}
func TestSSEInitialSnapshot(t *testing.T) {
	s, _ := testServer(t)
	server := httptest.NewServer(s)
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("not SSE")
	}
	scanner := bufio.NewScanner(res.Body)
	var id string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			id = strings.TrimPrefix(line, "id: ")
		}
		if strings.HasPrefix(line, "data: ") {
			var payload stream
			if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatal(err)
			}
			if !payload.Snapshot || id == "" || !strings.Contains(payload.HTML, "chat-list") {
				t.Fatal("incomplete snapshot")
			}
			return
		}
	}
	if err = scanner.Err(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	t.Fatal("no snapshot")
}
