package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sync activity: bisync runs with `--stats 2s --stats-log-level NOTICE`, so rclone prints a
// progress block every 2 seconds. statsFilter reads the transferred-file count from it and
// keeps the blocks out of the sync log (they would fill it). Everything else passes through.

var (
	statsStartRe = regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d NOTICE:\s*$`)
	statsFilesRe = regexp.MustCompile(`^Transferred:\s+(\d+) / (\d+), `)
)

type statsFilter struct {
	w       io.Writer // may be nil
	onFiles func(done int)
	onLine  func(line string) // every line but the progress blocks (Recent Changes); may be nil
	partial []byte
	inStats bool
}

func (s *statsFilter) Write(p []byte) (int, error) {
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		s.line(s.partial[:i+1])
		s.partial = s.partial[i+1:]
	}
	return len(p), nil
}

// Flush writes a last line that had no newline.
func (s *statsFilter) Flush() {
	if len(s.partial) > 0 {
		s.line(s.partial)
		s.partial = nil
	}
}

func (s *statsFilter) line(b []byte) {
	text := strings.TrimRight(string(b), "\r\n")
	if s.inStats {
		if strings.TrimSpace(text) == "" {
			s.inStats = false
		} else if m := statsFilesRe.FindStringSubmatch(text); m != nil && s.onFiles != nil {
			n, _ := strconv.Atoi(m[1])
			s.onFiles(n)
		}
		return
	}
	if statsStartRe.MatchString(text) {
		s.inStats = true
		return
	}
	if s.onLine != nil {
		s.onLine(text)
	}
	if s.w != nil {
		_, _ = s.w.Write(b)
	}
}

// headTail keeps a run's output for classifying how it ended without holding all of it: the
// first headMax bytes and the last tailMax. A first sync of a large folder at INFO level
// writes a line per file.
type headTail struct {
	mu      sync.Mutex
	head    []byte
	tail    []byte
	dropped bool
}

const (
	headMax = 64 << 10
	tailMax = 512 << 10
)

func (h *headTail) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(p)
	if room := headMax - len(h.head); room > 0 {
		k := min(room, len(p))
		h.head = append(h.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		h.tail = append(h.tail, p...)
		if over := len(h.tail) - tailMax; over > 0 {
			h.tail = append(h.tail[:0:0], h.tail[over:]...)
			h.dropped = true
		}
	}
	return n, nil
}

func (h *headTail) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dropped {
		return string(h.head) + "\n[…]\n" + string(h.tail)
	}
	return string(h.head) + string(h.tail)
}

// rotatingLog appends to a log file and starts a new one (the old becomes .1) when it passes
// logMax, also in the middle of a long sync.
type rotatingLog struct {
	name string
	f    *os.File
	n    int64
}

const logMax = 5_000_000

func newRotatingLog(name string) *rotatingLog {
	f := openLogFile(name) // rotates a full file first
	if f == nil {
		return nil
	}
	l := &rotatingLog{name: name, f: f}
	if st, err := f.Stat(); err == nil {
		l.n = st.Size()
	}
	return l
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	if l.f == nil {
		return len(p), nil
	}
	if l.n+int64(len(p)) > logMax {
		l.f.Close()
		path := filepath.Join(logDir(), l.name)
		_ = os.Rename(path, path+".1")
		l.f, _ = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		l.n = 0
		if l.f == nil {
			return len(p), nil
		}
	}
	n, err := l.f.Write(p)
	l.n += int64(n)
	return n, err
}

func (l *rotatingLog) Close() {
	if l.f != nil {
		l.f.Close()
	}
}

// activityText is the menu's first line while syncing.
func activityText(name string, index, n, files int) string {
	if name == "" {
		return "Syncing…"
	}
	s := "Syncing “" + name + "”"
	if n > 1 {
		s += fmt.Sprintf(" (%d of %d)", index, n)
	}
	switch {
	case files == 1:
		s += " · 1 file"
	case files > 1:
		s += fmt.Sprintf(" · %d files", files)
	}
	return s + "…"
}

const projectURL = "https://github.com/sbstndalton/opencloud-bar"

var (
	rcloneVersionMu   sync.Mutex
	rcloneVersionText string
)

// about is the settings page's About section. rclone's version is asked once and remembered.
func (e *Engine) about() map[string]any {
	rcloneVersionMu.Lock()
	v := rcloneVersionText
	rcloneVersionMu.Unlock()
	if v == "" {
		out, err := e.run(15*time.Second, nil, e.config().Rclone(), "version")
		if line, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); err == nil && strings.HasPrefix(line, "rclone ") {
			v = strings.TrimPrefix(line, "rclone ")
			rcloneVersionMu.Lock()
			rcloneVersionText = v
			rcloneVersionMu.Unlock()
		} else {
			v = "not found"
		}
	}
	return map[string]any{"version": version, "devBuild": isDevBuild(version), "rclone": v,
		"rclonePath": e.config().Rclone(), "project": projectURL, "license": "MIT"}
}
