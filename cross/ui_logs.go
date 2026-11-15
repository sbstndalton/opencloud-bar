package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	logTailBytes    = 2 << 20 // never read more than this from the end of a file
	logDefaultLines = 3000
	logMaxLines     = 5000
)

// logNameRe is the shape of a log file name: one path element, no separators, no leading dot.
var logNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}$`)

// resolveLog maps a log name from the page to a file inside the logs folder. It refuses
// anything that isn't a plain file name, a symlink, or a file that resolves outside the folder.
func resolveLog(name string) (string, error) {
	if !logNameRe.MatchString(name) || strings.Contains(name, "..") || !strings.Contains(name, ".log") {
		return "", errors.New("not a log file")
	}
	dir := logDir()
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", errors.New("no logs yet")
	}
	p := filepath.Join(root, name)
	st, err := os.Lstat(p) // Lstat: a symlink is refused, not followed
	if err != nil {
		return "", errors.New("no such log")
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("not a log file")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", errors.New("no such log")
	}
	if rel, err := filepath.Rel(root, real); err != nil || rel != name {
		return "", errors.New("not a log file")
	}
	return real, nil
}

// readTail returns the last n lines of a file, reading at most logTailBytes from its end.
func readTail(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if st.Size() > logTailBytes {
		start = st.Size() - logTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(f, logTailBytes))
	if err != nil {
		return "", err
	}
	text := string(b)
	if start > 0 { // began mid-line
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

var secretRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)((?:access|refresh|id)[_-]?token["'=:\s]+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`()eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`),
}

// redact hides anything in a log that looks like a token before it goes to the page.
func redact(s string) string {
	for _, re := range secretRes {
		s = re.ReplaceAllString(s, "${1}[hidden]")
	}
	return s
}

type logSource struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

func (e *Engine) logSources() []logSource {
	out := []logSource{{"app.log", "App log"}, {"drive.log", "Drive log"}}
	if e.config().Tunnel() {
		out = append(out, logSource{"tunnel.log", "SSH tunnel log"})
	}
	for _, f := range e.config().Kept {
		out = append(out, logSource{"sync-" + f.StateName + ".log", "Sync: " + f.DisplayName()})
	}
	return out
}

func (e *Engine) serveLog(w http.ResponseWriter, r *http.Request) {
	p, err := resolveLog(r.URL.Query().Get("name"))
	if err != nil {
		// A source that has no file yet (nothing logged) is an empty log, not an error.
		if err.Error() == "no such log" || err.Error() == "no logs yet" {
			writeJSON(w, 200, map[string]any{"text": ""})
			return
		}
		jsonErr(w, http.StatusBadRequest, err)
		return
	}
	n := logDefaultLines
	if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && v > 0 {
		n = min(v, logMaxLines)
	}
	text, err := readTail(p, n)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, fmt.Errorf("couldn't read the log"))
		return
	}
	writeJSON(w, 200, map[string]any{"text": redact(text)})
}

func (e *Engine) clearLog(w http.ResponseWriter, r *http.Request) {
	p, err := resolveLog(r.URL.Query().Get("name"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err)
		return
	}
	jsonOK(w, os.Truncate(p, 0))
}
