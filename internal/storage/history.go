package storage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is one line of history.jsonl: an image this app saved.
type Record struct {
	ID string `json:"id"`
	// Path is the file the gallery shows and deletes: the copy in the gallery
	// directory, or the only file when the image was saved there.
	Path string `json:"path"`
	// OutputPath is the file saved where the caller asked (an MCP output_dir
	// other than the gallery directory), kept apart from the gallery copy and
	// never deleted by the gallery. Empty when there is no separate copy.
	OutputPath    string    `json:"output_path,omitempty"`
	Kind          string    `json:"kind"`
	Source        string    `json:"source"`
	Prompt        string    `json:"prompt"`
	RevisedPrompt string    `json:"revised_prompt,omitempty"`
	Model         string    `json:"model"`
	Provider      string    `json:"provider"`
	Size          string    `json:"size,omitempty"`
	Width         int       `json:"width,omitempty"`
	Height        int       `json:"height,omitempty"`
	Bytes         int       `json:"bytes,omitempty"`
	MIME          string    `json:"mime,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// ErrNotFound reports a history record that does not exist (any more).
var ErrNotFound = errors.New("图库中没有这张图片，可能已被删除")

// Delete removes the image with id from the gallery: it rewrites the history
// without that record (via a temp file and rename, keeping every other line
// byte for byte, malformed ones included) and then deletes the record's Path,
// the gallery's file. The copy at OutputPath, saved where an MCP caller asked,
// is never touched. A gallery file that is already gone is not an error.
//
// id is Record.ID. It returns the deleted record, ErrNotFound when no record
// has that id, or an error when the history cannot be rewritten (then nothing
// was deleted) or the file cannot be removed (then the record is already gone).
func (s *Store) Delete(id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.historyPath)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	var kept []byte
	var removed *Record
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Record
		if removed == nil && json.Unmarshal([]byte(line), &r) == nil && r.ID == id && id != "" {
			removed = &r
			continue
		}
		if !strings.HasSuffix(line, "\n") {
			line += "\n"
		}
		kept = append(kept, line...)
	}
	if removed == nil {
		return Record{}, ErrNotFound
	}
	if err := writeFileAtomic(s.historyPath, kept, 0o600); err != nil {
		return Record{}, fmt.Errorf("更新历史记录失败: %w", err)
	}
	if err := os.Remove(removed.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return *removed, fmt.Errorf("已从图库移除，但删除文件失败: %w", err)
	}
	return *removed, nil
}

// Query filters the history listing. Zero values mean "no filter".
type Query struct {
	Limit   int       // page size, default 20, capped at 200
	Offset  int       // records to skip, newest first
	Model   string    // exact model ID
	Keyword string    // case-insensitive match in prompt, model, provider or path
	Since   time.Time // only records created at or after this time
}

// append writes one record as a JSON line, creating the file (0600) and its
// directory when needed.
func (s *Store) append(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.historyPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.historyPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// List returns history records matching q, newest first, and the number of
// matching records before paging. Malformed lines are skipped. A missing
// history file yields an empty result.
func (s *Store) List(q Query) (int, []Record, error) {
	s.mu.Lock()
	f, err := os.Open(s.historyPath)
	if err != nil {
		s.mu.Unlock()
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil, nil
		}
		return 0, nil, err
	}
	var all []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Path != "" {
			all = append(all, r)
		}
	}
	scanErr := sc.Err()
	f.Close()
	s.mu.Unlock()
	if scanErr != nil {
		return 0, nil, scanErr
	}

	kw := strings.ToLower(strings.TrimSpace(q.Keyword))
	var matched []Record
	for i := len(all) - 1; i >= 0; i-- {
		r := all[i]
		if q.Model != "" && r.Model != q.Model {
			continue
		}
		if !q.Since.IsZero() && r.CreatedAt.Before(q.Since) {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(r.Prompt+"\n"+r.Model+"\n"+r.Provider+"\n"+r.Path+"\n"+r.OutputPath), kw) {
			continue
		}
		matched = append(matched, r)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	offset := max(q.Offset, 0)
	if offset >= len(matched) {
		return len(matched), nil, nil
	}
	end := min(offset+limit, len(matched))
	return len(matched), matched[offset:end], nil
}
