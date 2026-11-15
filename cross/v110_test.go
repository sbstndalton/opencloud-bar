//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConflictNames(t *testing.T) {
	for name, want := range map[string]string{
		"report.pdf.conflict1": "report.pdf",
		"noext.conflict12":     "noext",
		"x.tar.gz.conflict3":   "x.tar.gz",
		".hidden.conflict1":    ".hidden",
		"conflict.txt":         "",
		"my conflict notes.md": "",
		"a.conflict":           "",
		"a.conflict0":          "",
		"a.conflictA":          "",
		"a.conflict1.txt":      "", // not rclone's form: the suffix goes after the extension
		"report.conflict1.pdf": "",
		".conflict1":           "",
		"a.Conflict1":          "",
		"name.conflict1 ":      "",
	} {
		if got := conflictOriginal(name); got != want {
			t.Errorf("conflictOriginal(%q) = %q, want %q", name, got, want)
		}
	}
}

func writeFile(t *testing.T, p, body string, mod time.Time) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, mod, mod)
}

func TestScanConflictsSkipsExcludedFolders(t *testing.T) {
	d := t.TempDir()
	now := time.Now()
	writeFile(t, filepath.Join(d, "a.txt"), "win", now)
	writeFile(t, filepath.Join(d, "a.txt.conflict1"), "lose", now.Add(-time.Hour))
	writeFile(t, filepath.Join(d, "sub", "b.md.conflict2"), "lose", now)
	writeFile(t, filepath.Join(d, "node_modules", "x.js.conflict1"), "skip", now)
	writeFile(t, filepath.Join(d, "conflict notes.txt"), "not one", now)
	got := scanConflicts(d, []string{"- **/node_modules/**", "- *.tmp"})
	if len(got) != 2 || got[0].File != "sub/b.md.conflict2" || got[0].Original != "sub/b.md" ||
		got[1].File != "a.txt.conflict1" || got[1].Original != "a.txt" {
		t.Fatalf("scan: %+v", got)
	}
}

func TestConflictKeepAndDiscard(t *testing.T) {
	e, _, f := syncRig(t, false)
	cfg := e.config()
	local := cfg.Local(f)
	now := time.Now()
	writeFile(t, filepath.Join(local, "n", "todo.txt"), "winner", now)
	writeFile(t, filepath.Join(local, "n", "todo.txt.conflict1"), "loser", now.Add(-time.Minute))
	writeFile(t, filepath.Join(local, "other.txt.conflict1"), "discard me", now)
	if fresh := e.updateConflicts(cfg, f); len(fresh) != 2 {
		t.Fatalf("fresh = %+v", fresh)
	}
	if st := e.Status(); st.Conflicts != 2 || !strings.Contains(st.Summary, "2 conflicts") {
		t.Errorf("status: %d %q", st.Conflicts, st.Summary)
	}
	if n := conflictNote(e.newConflicts); n == nil || !strings.HasPrefix(n.Title, "2 conflicts") {
		t.Errorf("note: %+v", n)
	}
	// Only listed copies, inside the folder.
	for _, bad := range []string{"../x.conflict1", "n/todo.txt", "nope.txt.conflict1", "/etc/passwd.conflict1"} {
		if err := e.ConflictAction("Docs", bad, "keep"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// Refused while a sync runs.
	e.mu.Lock()
	e.syncing = true
	e.mu.Unlock()
	if err := e.ConflictAction("Docs", "n/todo.txt.conflict1", "keep"); err == nil {
		t.Error("acted during a sync")
	}
	e.mu.Lock()
	e.syncing = false
	e.mu.Unlock()

	if err := e.ConflictAction("Docs", "n/todo.txt.conflict1", "keep"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(local, "n", "todo.txt")); string(b) != "loser" {
		t.Errorf("kept version: %q", b)
	}
	if exists(filepath.Join(local, "n", "todo.txt.conflict1")) {
		t.Error("conflict copy left behind")
	}
	backups, _ := filepath.Glob(filepath.Join(expand(cfg.StateDir), "backups", f.StateName, "*", "n", "todo.txt"))
	if len(backups) != 1 {
		t.Fatalf("replaced version not in the backups: %v", backups)
	} else if b, _ := os.ReadFile(backups[0]); string(b) != "winner" {
		t.Errorf("backup holds %q", b)
	}

	_ = os.MkdirAll(filepath.Join(home(), ".Trash"), 0o700) // macOS test runs; Linux uses XDG
	if err := e.ConflictAction("Docs", "other.txt.conflict1", "discard"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(local, "other.txt.conflict1")) {
		t.Error("discarded copy still there")
	}
	if st := e.Status(); st.Conflicts != 0 || strings.Contains(st.Summary, "conflict") {
		t.Errorf("after actions: %d %q", st.Conflicts, st.Summary)
	}
	// A restart remembers nothing stale: the list file is gone with the last conflict.
	if exists(cfg.conflictsPath(f)) {
		t.Error("empty list still saved")
	}
}

func TestBwLimit(t *testing.T) {
	for _, c := range []struct {
		up, down string
		want     []string
	}{
		{"", "", nil},
		{"512K", "", []string{"--bwlimit", "512K:off"}},
		{"", "10M", []string{"--bwlimit", "off:10M"}},
		{"1.5M", "100", []string{"--bwlimit", "1.5M:100"}},
	} {
		if got := bwLimitArgs(c.up, c.down); !reflect.DeepEqual(got, c.want) {
			t.Errorf("bwLimitArgs(%q, %q) = %v", c.up, c.down, got)
		}
	}
	for _, ok := range []string{"", "1", "512K", "2M", "1.5G", "0.5M"} {
		if err := bwLimitProblem("x", ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"0", "0K", "-1M", "1MB", "fast", "1 M", "off", "1M:2M", "1T", "1,5M"} {
		if bwLimitProblem("x", bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}

	// The settings page saves them (upper-cased), validates, and an empty value removes the key.
	r := newUIRig(t)
	r.login()
	if resp, b := r.post("/ui/api/config", `{"bwLimitUp":"512k","bwLimitDown":"10M"}`); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if c := r.e.config(); c.BwLimitUp != "512K" || c.BwLimitDown != "10M" {
		t.Errorf("saved %+v", c)
	}
	if resp, _ := r.post("/ui/api/config", `{"bwLimitUp":"lots"}`); resp.StatusCode != 400 {
		t.Errorf("bad limit: %d", resp.StatusCode)
	}
	if resp, _ := r.post("/ui/api/config", `{"bwLimitUp":""}`); resp.StatusCode != 200 {
		t.Errorf("clear: %d", resp.StatusCode)
	}
	data, _ := os.ReadFile(configFile())
	if strings.Contains(string(data), "bwLimitUp") || !strings.Contains(string(data), `"bwLimitDown": "10M"`) {
		t.Errorf("file: %s", data)
	}
}

// Syncs pass the limit to rclone.
func TestBisyncUsesBwLimit(t *testing.T) {
	e, fr, f := syncRig(t, false)
	e.mu.Lock()
	e.cfg.BwLimitUp = "1M"
	e.mu.Unlock()
	e.syncFolder(f, modeNormal)
	if !strings.Contains(fr.calls(), "--bwlimit 1M:off") {
		t.Errorf("calls: %s", fr.calls())
	}
}

// Two Keeps of the same file within one second share the backup stamp folder: the second
// backup must not replace the first (the original would be lost).
func TestKeepTwiceSameSecondKeepsBothBackups(t *testing.T) {
	local, state := t.TempDir(), t.TempDir()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(local, "a.txt"), "original", now)
	writeFile(t, filepath.Join(local, "a.txt.conflict1"), "c1", now)
	writeFile(t, filepath.Join(local, "a.txt.conflict2"), "c2", now)
	list := scanConflicts(local, nil)
	for _, c := range []string{"a.txt.conflict1", "a.txt.conflict2"} {
		if err := keepConflictVersion(local, state, "st", c, list, now); err != nil {
			t.Fatal(err)
		}
	}
	dir := localBackupDir(state, "st", backupStamp(now))
	for file, want := range map[string]string{"a.txt": "original", "a (2).txt": "c1"} {
		if b, err := os.ReadFile(filepath.Join(dir, file)); err != nil || string(b) != want {
			t.Errorf("backup %s = %q, %v", file, b, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(local, "a.txt")); string(b) != "c2" {
		t.Errorf("file = %q", b)
	}
}

// A merge's second pass uses its own backup folder, and pruning still reads its date.
func TestMergeSecondPassOwnBackupDir(t *testing.T) {
	e, fr, f := syncRig(t, true)
	fr.bisync(driftOut, 1) // both passes drift
	e.syncFolder(f, modeNormal)
	c := fr.calls()
	if !strings.Contains(c, "Z-2 --backup-dir2") && !strings.Contains(c, "Z-2 ") {
		t.Errorf("second pass shares the first pass's backup folder: %s", c)
	}
	root := filepath.Join(expand(e.cfg.StateDir), "backups", f.StateName)
	old := filepath.Join(root, backupStamp(time.Now().Add(-40*24*time.Hour))+"-2")
	_ = os.MkdirAll(old, 0o700)
	pruneBackups(e.cfg.StateDir, f.StateName, time.Now())
	if exists(old) {
		t.Error("old second-pass backup not pruned")
	}
}

func TestBwLimitLowercase(t *testing.T) {
	if bwLimitProblem("x", "512k") != nil || normBwLimit(" 2m ") != "2M" {
		t.Error("lower-case units")
	}
	e := pausedRig(t, `,"bwLimitUp":"512k"`)
	if e.cfg.BwLimitUp != "512K" || e.cfgErr != nil {
		t.Errorf("loaded %q %v", e.cfg.BwLimitUp, e.cfgErr)
	}
}
