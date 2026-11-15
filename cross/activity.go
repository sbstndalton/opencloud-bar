package main

import (
	"bytes"
	"fmt"
	"io"
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
	if s.w != nil {
		_, _ = s.w.Write(b)
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
