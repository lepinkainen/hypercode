package attachments

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/limits"
	"github.com/lepinkainen/hypercode/internal/store"
)

func TestUploadValidationAndCleanup(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	chat := store.ID()
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, media string
		data        []byte
		valid       bool
	}{
		{"picture.png", "image/png", pngData.Bytes(), true},
		{"notes.md", "text/markdown", []byte("# hello\n世界"), true},
		{"notes.txt", "text/plain", []byte("<html>download only</html>"), true},
		{"notes.pdf", "application/pdf", []byte("%PDF-1.4\nfixture\n%%EOF\n"), true},
		{"notes.docx", "application/octet-stream", []byte("PK"), false},
		{"bad.png", "image/png", []byte("not an image"), false},
		{"bad.png", "application/pdf", pngData.Bytes(), false},
		{"broken.png", "image/png", pngData.Bytes()[:40], false},
		{"empty.txt", "text/plain", nil, false},
		{"bad.txt", "text/plain", []byte{0xff, 0xfe}, false},
		{"bad.txt", "text/plain", []byte("binary\x00"), false},
		{"bad.pdf", "application/pdf", []byte("%PDF-1.4\ntruncated"), false},
	} {
		t.Run(tt.name+tt.media, func(t *testing.T) {
			a, err := s.Add(chat, tt.name, tt.media, bytes.NewReader(tt.data))
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v err=%v", tt.valid, err)
			}
			if !tt.valid {
				return
			}
			if !ValidID(a.ID) || a.Size != int64(len(tt.data)) {
				t.Fatalf("metadata: %+v", a)
			}
			got, err := os.ReadFile(a.Path)
			if err != nil || !bytes.Equal(got, tt.data) {
				t.Fatalf("stored bytes: %v", err)
			}
			s.Remove(chat, []adapter.Attachment{a})
		})
	}
	entries, _ := os.ReadDir(filepath.Join(root, chat))
	if len(entries) != 0 {
		t.Fatalf("uploads leaked: %v", entries)
	}
	if _, err := s.Add("../outside", "file.txt", "text/plain", strings.NewReader("x")); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := s.Path(chat, adapter.Attachment{ID: "../outside", MediaType: "text/plain"}); err == nil {
		t.Fatal("invalid ID accepted")
	}
}

type repeatedText struct{}

func (repeatedText) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestLimitsAndStartupSweep(t *testing.T) {
	root, chat := t.TempDir(), store.ID()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(chat, "large.txt", "text/plain", io.LimitReader(repeatedText{}, limits.MaxDocumentBytes+1)); err == nil {
		t.Fatal("oversize accepted")
	}
	var wide bytes.Buffer
	if err := png.Encode(&wide, image.NewRGBA(image.Rect(0, 0, limits.MaxImageDimension+1, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(chat, "wide.png", "image/png", &wide); err == nil {
		t.Fatal("oversize dimensions accepted")
	}
	a, err := s.Add(chat, "keep.txt", "text/plain", strings.NewReader("keep"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, chat, store.ID()+".tmp")
	if err := os.WriteFile(tmp, []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("temporary upload survived startup")
	}
	if _, err := os.Stat(a.Path); err != nil {
		t.Fatal("completed upload removed", err)
	}
}
