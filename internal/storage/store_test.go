package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/972579/open-image-mcp/internal/testutil"
)

// TestDeleteKeepsOtherLines checks Delete removes exactly one record and its
// gallery file, leaves every other history line as it was (malformed ones
// included), and never touches the output copy.
func TestDeleteKeepsOtherLines(t *testing.T) {
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	s := New(hist)
	png := testutil.PNG(8, 8)
	out, gallery := filepath.Join(dir, "out"), filepath.Join(dir, "gallery")

	first, err := s.Save([]Blob{{Data: png}}, out, gallery, "a", Meta{Kind: "generate", Source: "mcp", Prompt: "一", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save([]Blob{{Data: png}}, gallery, gallery, "b", Meta{Kind: "generate", Source: "debug", Prompt: "二", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	// A line another version wrote that this one cannot parse must survive.
	f, err := os.OpenFile(hist, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not json\n")
	f.Close()

	total, recs, err := s.List(Query{Keyword: "一"})
	if err != nil || total != 1 {
		t.Fatalf("list: %d %v", total, err)
	}
	rec, err := s.Delete(recs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Path != first[0].GalleryPath || rec.OutputPath != first[0].Path {
		t.Fatalf("deleted record = %+v", rec)
	}
	if _, err := os.Stat(first[0].GalleryPath); !os.IsNotExist(err) {
		t.Errorf("gallery file kept: %v", err)
	}
	if _, err := os.Stat(first[0].Path); err != nil {
		t.Errorf("output file removed: %v", err)
	}
	data, _ := os.ReadFile(hist)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"prompt":"二"`) || lines[1] != "not json" {
		t.Fatalf("history after delete:\n%s", data)
	}
	if _, err := s.Delete(rec.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
}
