// Package attachments stores validated uploads outside project directories.
package attachments

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/limits"
	"github.com/lepinkainen/hypercode/internal/store"
	_ "golang.org/x/image/webp"
)

type Store struct{ root string }

func ValidID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == strings.ToLower(id)
}

func New(root string) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	// Only unfinished uploads are swept. Referenced files live with history.
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() || !ValidID(d.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".tmp") && ValidID(strings.TrimSuffix(f.Name(), ".tmp")) {
				if err := os.Remove(filepath.Join(root, d.Name(), f.Name())); err != nil {
					return nil, err
				}
			}
		}
	}
	return &Store{root: root}, nil
}

func extension(media string) string {
	return map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "application/pdf": ".pdf", "text/plain": ".txt", "text/markdown": ".md"}[media]
}

func (s *Store) Path(chat string, a adapter.Attachment) (string, error) {
	ext := extension(a.MediaType)
	if !ValidID(chat) || !ValidID(a.ID) || ext == "" {
		return "", errors.New("invalid attachment")
	}
	return filepath.Join(s.root, chat, a.ID+ext), nil
}

func (s *Store) Remove(chat string, files []adapter.Attachment) {
	for _, a := range files {
		if path, err := s.Path(chat, a); err == nil {
			_ = os.Remove(path)
		}
	}
}

// Add reads at most one file limit plus one byte. The caller enforces the
// request-wide limit. No filename supplied by the browser enters a disk path.
func (s *Store) Add(chat, name, declared string, r io.Reader) (a adapter.Attachment, err error) {
	if !ValidID(chat) {
		return a, errors.New("invalid chat")
	}
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return a, errors.New("invalid attachment filename")
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return a, err
	}
	head = head[:n]
	if n == 0 {
		return a, errors.New("empty attachment")
	}
	media := strings.Split(http.DetectContentType(head), ";")[0]
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".pdf":
		want := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".pdf": "application/pdf"}[ext]
		if media != want {
			return a, errors.New("file contents do not match its extension")
		}
	case ".txt", ".md", ".markdown":
		// Text may start with HTML/XML/JSON. Validate all bytes below and always
		// serve these files as downloads, regardless of the sniffer's guess.
		if bytes.IndexByte(head, 0) >= 0 {
			return a, errors.New("document must contain UTF-8 text")
		}
		media = "text/plain"
		if ext != ".txt" {
			media = "text/markdown"
		}
	default:
		return a, errors.New("supported files: PNG, JPEG, WebP, TXT, Markdown, and PDF")
	}
	if supplied, _, e := mime.ParseMediaType(declared); e == nil && supplied != "application/octet-stream" && supplied != media {
		if !(strings.HasPrefix(media, "text/") && strings.HasPrefix(supplied, "text/")) {
			return a, errors.New("file contents do not match its media type")
		}
	}
	a = adapter.Attachment{ID: store.ID(), Name: name, MediaType: media}
	path, err := s.Path(chat, a)
	if err != nil {
		return a, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return a, err
	}
	tmp := filepath.Join(filepath.Dir(path), a.ID+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return a, err
	}
	defer func() { _ = f.Close(); _ = os.Remove(tmp) }()
	limit := limits.MaxDocumentBytes
	if a.Image() {
		limit = limits.MaxImageBytes
	}
	a.Size, err = io.Copy(f, io.LimitReader(io.MultiReader(bytes.NewReader(head), r), limit+1))
	if err != nil {
		return a, err
	}
	if a.Size > limit {
		return a, fmt.Errorf("%s exceeds the %d byte file limit", name, limit)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return a, err
	}
	if a.Image() {
		cfg, _, e := image.DecodeConfig(f)
		if e != nil {
			return a, errors.New("invalid image")
		}
		if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > limits.MaxImageDimension || cfg.Height > limits.MaxImageDimension || int64(cfg.Width)*int64(cfg.Height) > limits.MaxImagePixels {
			return a, errors.New("image exceeds 8000 pixels per side or 16 megapixels")
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return a, err
		}
		if _, _, err = image.Decode(f); err != nil {
			return a, errors.New("invalid or truncated image")
		}
	} else if strings.HasPrefix(media, "text/") {
		data, e := io.ReadAll(f)
		if e != nil {
			return a, e
		}
		if !utf8.Valid(data) || bytes.ContainsFunc(data, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) {
			return a, errors.New("document must contain UTF-8 text")
		}
	} else {
		tail := make([]byte, min(a.Size, 1024))
		if _, err = f.ReadAt(tail, a.Size-int64(len(tail))); err != nil {
			return a, err
		}
		if !bytes.Contains(tail, []byte("%%EOF")) {
			return a, errors.New("PDF is incomplete")
		}
	}
	if err = f.Sync(); err != nil {
		return a, err
	}
	if err = f.Close(); err != nil {
		return a, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return a, err
	}
	a.Path = path
	return a, nil
}
