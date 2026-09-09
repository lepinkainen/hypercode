// Package web serves embedded HTML and incremental SSE updates.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"hypercode/internal/adapter"
	"hypercode/internal/session"
	"hypercode/internal/store"
)

//go:embed templates/*.html assets/*
var files embed.FS

type Server struct {
	manager   *session.Manager
	templates *template.Template
	handler   http.Handler
}
type page struct {
	Chats     []store.Chat
	Chat      *store.Chat
	Directory string
}
type fragment struct {
	ID   string  `json:"id"`
	HTML string  `json:"html"`
	Text *string `json:"text,omitempty"`
}
type stream struct {
	Snapshot     bool       `json:"snapshot"`
	HTML         string     `json:"html"`
	Conversation string     `json:"conversation,omitempty"`
	Items        []fragment `json:"items"`
}

func New(m *session.Manager, dir string) (*Server, error) {
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	t, err := template.New("").Funcs(template.FuncMap{
		"markdown": func(s string) template.HTML {
			var b bytes.Buffer
			if md.Convert([]byte(s), &b) != nil {
				return template.HTML(template.HTMLEscapeString(s))
			}
			return template.HTML(b.String())
		}, // Goldmark disables raw HTML and unsafe links.
		"timeLabel": func(t time.Time) string { return t.Local().Format("15:04") },
		"decision": func(s string) string {
			switch s {
			case "accept":
				return "Allow once"
			case "acceptForSession":
				return "Allow for session"
			case "decline":
				return "Deny"
			case "cancel":
				return "Cancel turn"
			}
			return s
		},
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{manager: m, templates: t}
	mux := http.NewServeMux()
	assets, _ := fs.Sub(files, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.page(w, r, "", dir) })
	mux.HandleFunc("GET /chats/{id}", func(w http.ResponseWriter, r *http.Request) { s.page(w, r, r.PathValue("id"), dir) })
	mux.HandleFunc("POST /chats", s.create)
	mux.HandleFunc("POST /chats/{id}/send", s.send)
	mux.HandleFunc("POST /chats/{id}/stop", s.stop)
	mux.HandleFunc("POST /chats/{id}/resume", s.resume)
	mux.HandleFunc("POST /chats/{id}/answer/{item}", s.answer)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	s.handler = s.boundary(mux)
	return s, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }
func (s *Server) boundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if r.Method == http.MethodPost {
			site := r.Header.Get("Sec-Fetch-Site")
			if site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "Cross-origin actions are not allowed", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				if err != nil || u.Host != r.Host || u.Scheme != scheme || u.Path != "" {
					http.Error(w, "Cross-origin actions are not allowed", http.StatusForbidden)
					return
				}
			}
			r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "Invalid or oversized form", http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) render(name string, data any) string {
	var b bytes.Buffer
	if err := s.templates.ExecuteTemplate(&b, name, data); err != nil {
		slog.Error("render template", "name", name, "error", err)
		return ""
	}
	return b.String()
}

func (s *Server) live(data page) string {
	markup := strings.Replace(s.render("chat-list", data), `id="chat-list"`, `id="chat-list" hx-swap-oob="true"`, 1)
	if data.Chat != nil {
		title := template.HTMLEscapeString(data.Chat.Title)
		markup += `<span id="breadcrumb-title" class="chat-title" hx-swap-oob="true">` + title + `</span>`
		markup += `<h1 id="chat-name" hx-swap-oob="true">` + title + `</h1>`
		markup += strings.Replace(s.render("status", data.Chat), `id="chat-status"`, `id="chat-status" hx-swap-oob="true"`, 1)
		markup += strings.Replace(s.render("controls", data.Chat), `id="turn-controls"`, `id="turn-controls" hx-swap-oob="true"`, 1)
	}
	return markup
}
func (s *Server) page(w http.ResponseWriter, r *http.Request, id, dir string) {
	chats, chat := s.manager.View(id)
	if id != "" && chat == nil {
		http.NotFound(w, r)
		return
	}
	name := "page"
	if r.Header.Get("HX-Request") == "true" {
		name = "workspace"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s.render(name, page{chats, chat, dir})))
}
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	id, err := s.manager.Create(r.Context(), "codex", r.FormValue("directory"), adapter.PermissionMode(r.FormValue("mode")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("HX-Redirect", "/chats/"+id)
	w.Header().Set("Location", "/chats/"+id)
	w.WriteHeader(http.StatusCreated)
}
func (s *Server) action(w http.ResponseWriter, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	s.action(w, s.manager.Send(r.Context(), r.PathValue("id"), r.FormValue("message")))
}
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	s.action(w, s.manager.Stop(r.Context(), r.PathValue("id")))
}
func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	s.action(w, s.manager.Resume(r.Context(), r.PathValue("id")))
}
func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	a := adapter.Answer{Decision: r.FormValue("decision"), Answers: map[string][]string{}}
	for key, v := range r.PostForm {
		if strings.HasPrefix(key, "question-") {
			for _, answer := range v {
				if strings.TrimSpace(answer) != "" {
					name := strings.TrimPrefix(key, "question-")
					a.Answers[name] = append(a.Answers[name], answer)
				}
			}
		}
	}
	s.action(w, s.manager.Respond(r.Context(), r.PathValue("id"), r.PathValue("item"), a))
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unavailable", 500)
		return
	}
	id := r.URL.Query().Get("chat")
	_, chat := s.manager.View(id)
	if id != "" && chat == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	wake, unsubscribe := s.manager.Subscribe()
	defer unsubscribe()
	generation := ""
	var seq uint64
	if parts := strings.SplitN(r.Header.Get("Last-Event-ID"), ":", 2); len(parts) == 2 {
		generation = parts[0]
		seq, _ = strconv.ParseUint(parts[1], 10, 64)
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		gen, next, snapshot, updates, chats, chat := s.manager.Read(id, generation, seq)
		if snapshot || next != seq {
			data := page{Chats: chats, Chat: chat}
			payload := stream{Snapshot: snapshot, HTML: s.live(data)}
			if snapshot {
				payload.Conversation = s.render("conversation", data)
			} else {
				// Collapse repeated updates to each item at the same sequence boundary.
				order := []string{}
				latest := map[string]store.Item{}
				for _, u := range updates {
					if u.ChatID == id && u.Item != nil {
						if _, ok := latest[u.Item.ID]; !ok {
							order = append(order, u.Item.ID)
						}
						latest[u.Item.ID] = *u.Item
					}
				}
				for _, key := range order {
					i := latest[key]
					f := fragment{ID: i.ID, HTML: s.render("item", i)}
					if i.Kind == "assistant" && i.Status == "streaming" {
						body := i.Body
						f.Text = &body
					}
					payload.Items = append(payload.Items, f)
				}
			}
			b, err := json.Marshal(payload)
			if err != nil {
				return
			}
			if err = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				slog.Debug("SSE write deadline", "error", err)
			}
			if _, err = fmt.Fprintf(w, "id: %s:%d\ndata: %s\n\n", gen, next, b); err != nil {
				return
			}
			flusher.Flush()
			generation = gen
			seq = next
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-ticker.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
