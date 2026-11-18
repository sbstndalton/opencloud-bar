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
//
// The block's "Transferring:" section lists the files in flight, one per line (rclone's
// accounting package: stats.go writes the section, accounting.go's Account.String the line).
// With `--stats-file-name-length 0` the name is whole and unpadded, as rclone v1.76 prints it:
//
//	Transferring:
//	 * sub dir/big, file: v1 (45%).bin: 34% / 2.861 MiB, 1019.990 KiB/s, 1s
//	 * b.bin:100% / 1.907 MiB, 976.550 KiB/s, -
//	 * name: transferring               (size not known yet)
//
// rclone doesn't say which way a file is going, so a Transfer carries no direction.

var (
	statsStartRe = regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d NOTICE:\s*$`)
	statsFilesRe = regexp.MustCompile(`^Transferred:\s+(\d+) / (\d+), `)
	// The name may hold ": ", ", " and "45%": the tail (percent, size, speed, ETA) is what is matched,
	// from the last colon that is followed by one.
	transferRe     = regexp.MustCompile(`^\s*\*\s+(.*):\s*(\d{1,3})%\s*/\s*([^,]+),\s*([^,]+),\s*(\S.*?)\s*$`)
	transferBareRe = regexp.MustCompile(`^\s*\*\s+(.*):\s*transferring\s*$`)
)

// Transfer is one file rclone is moving right now. Percent is -1 when rclone doesn't know the
// size yet; Size, Speed and ETA are rclone's own text ("2.861 MiB", "1.9 MiB/s", "1s"), empty when unknown.
type Transfer struct {
	Folder  string `json:"folder"` // the kept folder's key
	Name    string `json:"name"`
	Percent int    `json:"percent"`
	Size    string `json:"size"`
	Speed   string `json:"speed"`
	ETA     string `json:"eta"`
}

// parseTransfer reads one line of the "Transferring:" section.
func parseTransfer(text string) (Transfer, bool) {
	if m := transferRe.FindStringSubmatch(text); m != nil {
		pct, _ := strconv.Atoi(m[2])
		eta := strings.TrimSpace(m[5])
		if eta == "-" {
			eta = ""
		}
		return Transfer{Name: m[1], Percent: min(pct, 100), Size: strings.TrimSpace(m[3]),
			Speed: strings.TrimSpace(m[4]), ETA: eta}, m[1] != ""
	}
	if m := transferBareRe.FindStringSubmatch(text); m != nil {
		return Transfer{Name: m[1], Percent: -1}, m[1] != ""
	}
	return Transfer{}, false
}

type statsFilter struct {
	w       io.Writer // may be nil
	onFiles func(done int)
	// onTransfers gets the files in flight once per progress block (empty when none); may be nil.
	onTransfers func([]Transfer)
	folder      string            // the kept folder's key, put on each Transfer
	onLine      func(line string) // every line but the progress blocks (Recent Changes); may be nil
	partial     []byte
	inStats     bool
	inXfers     bool // inside the block's "Transferring:" section
	xfers       []Transfer
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
			s.inStats, s.inXfers = false, false
			if s.onTransfers != nil {
				s.onTransfers(s.xfers)
			}
			s.xfers = nil
		} else if strings.TrimSpace(text) == "Transferring:" {
			s.inXfers = true
		} else if s.inXfers {
			if t, ok := parseTransfer(text); ok {
				t.Folder = s.folder
				s.xfers = append(s.xfers, t)
			}
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
