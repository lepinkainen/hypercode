package web

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/attachments"
	"github.com/lepinkainen/hypercode/internal/limits"
)

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, chat := s.manager.View(id)
	if chat == nil {
		http.NotFound(w, r)
		return
	}
	in := adapter.Input{}
	// Ownership transfers only when the manager retains a history reference,
	// including explicit rejection and uncertain agent acceptance.
	defer func() {
		_, current := s.manager.View(id)
		var unused []adapter.Attachment
		for _, a := range in.Attachments {
			retained := false
			if current != nil {
				for _, item := range current.Items {
					for _, saved := range item.Attachments {
						if a.ID == saved.ID {
							retained = true
						}
					}
				}
			}
			if !retained {
				unused = append(unused, a)
			}
		}
		s.uploads.Remove(id, unused)
	}()
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media == "multipart/form-data" {
		reader, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "Invalid multipart upload", http.StatusBadRequest)
			return
		}
		var total int64
		seenMessage := false
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				uploadError(w, err)
				return
			}
			switch part.FormName() {
			case "message":
				if seenMessage || part.FileName() != "" {
					http.Error(w, "Invalid message field", http.StatusBadRequest)
					return
				}
				seenMessage = true
				body, err := io.ReadAll(io.LimitReader(part, 3*limits.MaxMessageUnits+1))
				if err != nil {
					uploadError(w, err)
					return
				}
				if len(body) > 3*limits.MaxMessageUnits {
					http.Error(w, "Message is too long", http.StatusRequestEntityTooLarge)
					return
				}
				in.Text = string(body)
			case "files":
				if len(in.Attachments) >= limits.MaxAttachments {
					http.Error(w, "Too many attachments", http.StatusBadRequest)
					return
				}
				a, err := s.uploads.Add(id, part.FileName(), part.Header.Get("Content-Type"), part)
				if err != nil {
					uploadError(w, err)
					return
				}
				in.Attachments = append(in.Attachments, a)
				total += a.Size
				if total > limits.MaxAttachmentBytes {
					http.Error(w, "Attachments exceed the total size limit", http.StatusRequestEntityTooLarge)
					return
				}
				if a.MediaType == "application/pdf" && chat.Harness == "codex" {
					if _, err := exec.LookPath("pdftotext"); err != nil {
						http.Error(w, "PDF attachments for Codex require pdftotext on the Hypercode host", http.StatusUnprocessableEntity)
						return
					}
				}
			default:
				http.Error(w, "Unexpected upload field", http.StatusBadRequest)
				return
			}
			if err := part.Close(); err != nil {
				uploadError(w, err)
				return
			}
		}
	} else {
		in.Text = r.FormValue("message")
	}
	s.action(w, s.manager.Send(r.Context(), id, in))
}

func uploadError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var large *http.MaxBytesError
	if errors.As(err, &large) {
		status = http.StatusRequestEntityTooLarge
	}
	http.Error(w, fmt.Sprintf("Could not attach file: %s", err), status)
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	id, fileID := r.PathValue("id"), r.PathValue("attachment")
	if !attachments.ValidID(id) || !attachments.ValidID(fileID) {
		http.NotFound(w, r)
		return
	}
	_, chat := s.manager.View(id)
	if chat == nil {
		http.NotFound(w, r)
		return
	}
	for _, item := range chat.Items {
		for _, a := range item.Attachments {
			if a.ID != fileID {
				continue
			}
			path, err := s.uploads.Path(id, a)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			f, err := os.Open(path)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil || !info.Mode().IsRegular() {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", a.MediaType)
			w.Header().Set("Cache-Control", "private, no-store")
			disposition := "attachment"
			if a.Image() {
				disposition = "inline"
			}
			w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.Name}))
			http.ServeContent(w, r, a.Name, info.ModTime(), f)
			return
		}
	}
	http.NotFound(w, r)
}
