package storage

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is one line of history.jsonl: an image this app saved.
type Record struct {
	ID            string    `json:"id"`
	Path          string    `json:"path"`
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
		if kw != "" && !strings.Contains(strings.ToLower(r.Prompt+"\n"+r.Model+"\n"+r.Provider+"\n"+r.Path), kw) {
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
