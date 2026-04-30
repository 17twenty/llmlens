package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Session owns the on-disk artifact tree for one run.
//
// Layout:
//
//	<root>/<run-id>/
//	  <host>/
//	    <url-path>.md
//	    <url-path>.html
//	    <url-path>.png
//	  events.log
type Session struct {
	RunID string
	Root  string // <root>/<run-id>

	logMu sync.Mutex
	logFD *os.File
}

// Event is one line in events.log. Errors are flattened into a typed Error
// so post-hoc tooling can filter by category without parsing strings.
type Event struct {
	Timestamp  time.Time   `json:"ts"`
	Tool       string      `json:"tool"`
	Params     any         `json:"params,omitempty"`
	DurationMS int64       `json:"duration_ms"`
	OK         bool        `json:"ok"`
	Error      *EventError `json:"error,omitempty"`
}

type EventError struct {
	Category string `json:"category"`
	Message  string `json:"message"`
}

// AppendEvent writes one JSONL record to <root>/events.log. Best-effort —
// errors are returned but logging never blocks tool execution.
func (s *Session) AppendEvent(ev Event) error {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFD == nil {
		f, err := os.OpenFile(filepath.Join(s.Root, "events.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		s.logFD = f
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = s.logFD.Write(data)
	return err
}

func (s *Session) Close() error {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFD != nil {
		err := s.logFD.Close()
		s.logFD = nil
		return err
	}
	return nil
}

func New(rootDir string) (*Session, error) {
	if rootDir == "" {
		rootDir = "runs"
	}
	id := newRunID()
	full := filepath.Join(rootDir, id)
	if err := os.MkdirAll(full, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir session: %w", err)
	}
	return &Session{RunID: id, Root: full}, nil
}

func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// SavePage writes html, md, and png artifacts for a URL into the host/path
// folder layout. Any of the byte slices may be nil to skip that artifact.
func (s *Session) SavePage(rawURL string, html, markdown []byte, screenshot []byte) error {
	host, slug := splitURL(rawURL)
	dir := filepath.Join(s.Root, host)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := filepath.Join(dir, slug)
	for _, w := range []struct {
		ext  string
		data []byte
	}{
		{".html", html},
		{".md", markdown},
		{".png", screenshot},
	} {
		if w.data == nil {
			continue
		}
		if err := os.WriteFile(base+w.ext, w.data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// splitURL turns a URL into a (host, path-slug) pair safe for filesystem use.
// Root path becomes "_index"; trailing slashes collapse; query+fragment fold in.
func splitURL(raw string) (string, string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "_unknown", sanitize(raw)
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		path = "_index"
	}
	if u.RawQuery != "" {
		path += "_q_" + u.RawQuery
	}
	return sanitize(u.Host), sanitize(path)
}

func sanitize(s string) string {
	if s == "" {
		return "_"
	}
	r := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"?", "_",
		"#", "_",
		"&", "_",
		"=", "-",
		" ", "-",
	)
	out := r.Replace(s)
	if len(out) > 180 {
		out = out[:180]
	}
	return out
}
