//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRclone is a stand-in rclone: it records every call, answers lsf with the file "lsf"
// and bisync with the file "bisync.out" and exit code "bisync.code" (all in dir).
type fakeRclone struct{ dir string }

func newFakeRclone(t *testing.T) *fakeRclone {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
d="` + dir + `"
echo "$*" >> "$d/calls"
case "$1" in
lsf) [ -f "$d/lsf" ] && cat "$d/lsf"; exit 0 ;;
bisync) [ -f "$d/bisync.out" ] && cat "$d/bisync.out"; [ -f "$d/bisync.code" ] && exit "$(cat "$d/bisync.code")"; exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &fakeRclone{dir}
}

func (r *fakeRclone) path() string { return filepath.Join(r.dir, "rclone") }

func (r *fakeRclone) bisync(out string, code int) {
	_ = os.WriteFile(filepath.Join(r.dir, "bisync.out"), []byte(out), 0o600)
	_ = os.WriteFile(filepath.Join(r.dir, "bisync.code"), []byte(itoa(code)), 0o600)
}

func (r *fakeRclone) lsf(out string) {
	_ = os.WriteFile(filepath.Join(r.dir, "lsf"), []byte(out), 0o600)
}

func (r *fakeRclone) calls() string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "calls"))
	return string(b)
}

func (r *fakeRclone) reset() { _ = os.Remove(filepath.Join(r.dir, "calls")) }

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// syncRig is an engine with one kept folder "Docs" that has synced before (unless fresh).
func syncRig(t *testing.T, fresh bool) (*Engine, *fakeRclone, KeptFolder) {
	t.Helper()
	fr := newFakeRclone(t)
	e := pausedRig(t, `,"rclonePath":"`+fr.path()+`","kept":[{"path":"Docs","stateName":"docs-1","futureFolderKey":"x"}]`)
	f := e.cfg.Kept[0]
	_ = os.MkdirAll(expand(e.cfg.StateDir), 0o700)
	if !fresh {
		_ = os.WriteFile(e.cfg.statePath(f, ".initialised"), nil, 0o600)
	}
	_ = os.MkdirAll(e.cfg.Local(f), 0o700)
	return e, fr, f
}

func state(e *Engine, f KeptFolder) FolderStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return *e.status[f.Path]
}

// N1: an emptied copy on this computer is never "applied" to OpenCloud with --force.
func TestLocalEmptyBigChangeNeverForces(t *testing.T) {
	e, fr, f := syncRig(t, false)
	fr.bisync(tooManyOut, 1) // "... on Path1"
	_ = os.WriteFile(filepath.Join(e.cfg.Local(f), ".DS_Store"), []byte("x"), 0o600)
	e.syncFolder(f, modeNormal)
	if s := state(e, f); s.State != StFailed || s.Detail != missingLocalText("Docs") {
		t.Fatalf("local empty: %+v", s)
	}
	if err := e.AllowBigDeletion("Docs"); err == nil {
		t.Fatal("large deletion offered for an empty local copy")
	}

	// With files left it is a real deletion, named by side; allowing it re-checks both sides.
	_ = os.WriteFile(filepath.Join(e.cfg.Local(f), "keep.txt"), []byte("x"), 0o600)
	e.syncFolder(f, modeNormal)
	if s := state(e, f); s.State != StBigDeletion || s.Detail != "on this computer" {
		t.Fatalf("real deletion: %+v", s)
	}
	if !strings.Contains(attentionReason(state(e, f)), "on this computer") {
		t.Errorf("reason doesn't name the side: %q", attentionReason(state(e, f)))
	}
	_ = os.Remove(filepath.Join(e.cfg.Local(f), "keep.txt")) // the disk went away meanwhile
	fr.reset()
	if err := e.AllowBigDeletion("Docs"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !e.Status().Syncing })
	if strings.Contains(fr.calls(), "--force") {
		t.Fatalf("--force with an empty local copy: %s", fr.calls())
	}
	if s := state(e, f); s.State != StFailed || s.Detail != missingLocalText("Docs") {
		t.Errorf("after allow: %+v", s)
	}

	// The server side the same way (Path2, empty listing).
	_ = os.WriteFile(filepath.Join(e.cfg.Local(f), "keep.txt"), []byte("x"), 0o600)
	fr.bisync(allChangedOut, 1)
	fr.lsf("")
	e.syncFolder(f, modeNormal)
	if s := state(e, f); s.State != StFailed || s.Detail != missingRemoteText {
		t.Errorf("remote empty: %+v", s)
	}
}

// Merges keep the copies they overwrite, on both sides, and prune old local backups.
func TestMergesKeepBackups(t *testing.T) {
	e, fr, f := syncRig(t, true)
	root := filepath.Join(expand(e.cfg.StateDir), "backups", f.StateName)
	old := filepath.Join(root, backupStamp(time.Now().Add(-40*24*time.Hour)))
	recent := filepath.Join(root, backupStamp(time.Now().Add(-2*24*time.Hour)))
	other := filepath.Join(root, "notes")
	for _, d := range []string{old, recent, other} {
		_ = os.MkdirAll(d, 0o700)
	}
	if !e.syncFolder(f, modeNormal) {
		t.Fatalf("first sync failed: %+v", state(e, f))
	}
	calls := fr.calls()
	if !strings.Contains(calls, "--resync") || !strings.Contains(calls, "--backup-dir1 "+root+string(filepath.Separator)) ||
		!strings.Contains(calls, "--backup-dir2 "+e.cfg.RemoteSpec()+backupFolderName+"/"+f.StateName+"/") {
		t.Errorf("merge without backups: %s", calls)
	}
	if exists(old) || !exists(recent) || !exists(other) {
		t.Errorf("pruning: old %v recent %v other %v", exists(old), exists(recent), exists(other))
	}
	b, _ := os.ReadFile(e.cfg.filtersPath(f))
	if !strings.Contains(string(b), backupFilter+"\n") {
		t.Errorf("filters don't exclude the backups: %q", b)
	}
	// A normal sync doesn't move anything aside.
	fr.reset()
	e.syncFolder(f, modeNormal)
	if c := fr.calls(); strings.Contains(c, "--backup-dir") || strings.Contains(c, "--resync") {
		t.Errorf("plain sync: %s", c)
	}
	if p := pathProblem(".opencloud-bar-backups/x"); p == "" {
		t.Error("the backup folder can be kept")
	}
}

// N3: no automatic merge unless the once-a-day record could be written.
func TestAutoMergeNeedsItsRecord(t *testing.T) {
	e, fr, f := syncRig(t, false)
	fr.bisync(driftOut, 1)
	_ = os.MkdirAll(e.cfg.statePath(f, ".auto-merge"), 0o700) // a directory: writing the record fails
	e.syncFolder(f, modeNormal)
	if strings.Contains(fr.calls(), "--resync") {
		t.Fatalf("merged without a record: %s", fr.calls())
	}
	if s := state(e, f); s.State != StNeedsRepair {
		t.Errorf("state: %+v", s)
	}
}

// N5: a folder parked after drift tries a plain sync each interval and recovers by itself.
func TestDriftParkedRetriesPlainSync(t *testing.T) {
	e, fr, f := syncRig(t, false)
	_ = os.WriteFile(e.cfg.statePath(f, ".auto-merge"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
	fr.bisync(driftOut, 1)
	e.syncFolder(f, modeNormal) // merged today already: parked
	if s := state(e, f); s.State != StNeedsRepair || !exists(e.cfg.statePath(f, ".drift-parked")) {
		t.Fatalf("not parked: %+v", s)
	}
	runAll := func() {
		e.mu.Lock()
		e.startSyncAllLocked(false)
		e.mu.Unlock()
		waitFor(t, func() bool { return !e.Status().Syncing })
	}
	// Still drifting: stays parked, never merges.
	fr.reset()
	runAll()
	if c := fr.calls(); !strings.Contains(c, "bisync") || strings.Contains(c, "--resync") {
		t.Errorf("parked retry: %s", c)
	}
	if s := state(e, f); s.State != StNeedsRepair {
		t.Errorf("parked retry state: %+v", s)
	}
	// Settled: fine again, markers gone.
	fr.bisync("", 0)
	runAll()
	if s := state(e, f); s.State != StOK || exists(e.cfg.statePath(f, ".needs-repair")) || exists(e.cfg.statePath(f, ".drift-parked")) {
		t.Errorf("not cleared: %+v", s)
	}
}

// Pause, Keep and the settings page change only their own keys, and keep each other's changes.
func TestConfigEditsDontLoseUpdates(t *testing.T) {
	e, _, _ := syncRig(t, false)
	// Someone else (the settings page, a hand edit) changes the file after the engine loaded it.
	if _, err := applyConfigPatch(configPatch{SyncIntervalSeconds: ptr(900)}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if err := e.Keep("Photos", ""); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		SyncIntervalSeconds int              `json:"syncIntervalSeconds"`
		Paused              bool             `json:"paused"`
		Kept                []map[string]any `json:"kept"`
		FutureKey           int              `json:"futureKey"`
	}
	data, _ := os.ReadFile(configFile())
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.SyncIntervalSeconds != 900 || !raw.Paused || raw.FutureKey != 7 || len(raw.Kept) != 2 ||
		raw.Kept[0]["futureFolderKey"] != "x" || raw.Kept[1]["path"] != "Photos" {
		t.Fatalf("lost an update: %s", data)
	}
	if len(e.config().Kept) != 2 || !e.config().Paused {
		t.Errorf("in memory: %+v", e.config())
	}
}

// N9: a failed update check isn't the daily check, and is retried after a growing wait.
func TestUpdateCheckFailureBacksOff(t *testing.T) {
	testEnv(t)
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "nope", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v9.1.0","html_url":"https://github.com/sbstndalton/opencloud-bar/releases/tag/v9.1.0"}`))
	}))
	defer srv.Close()
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	u := newUpdater()
	u.api, u.now = srv.URL, func() time.Time { return clock }
	if st := u.Check(context.Background()); st.Error == "" || !st.CheckedAt.IsZero() {
		t.Fatalf("failed check: %+v", st)
	}
	if u.due() {
		t.Error("retried right away")
	}
	clock = clock.Add(16 * time.Minute)
	if !u.due() {
		t.Error("not retried after 15 min")
	}
	u.Check(context.Background())
	clock = clock.Add(16 * time.Minute)
	if u.due() {
		t.Error("second wait didn't grow")
	}
	clock = clock.Add(15 * time.Minute)
	fail = false
	if st := u.Check(context.Background()); st.Error != "" || st.CheckedAt.IsZero() || u.due() {
		t.Errorf("after success: %+v", st)
	}
	if updateRetry(20) != 6*time.Hour {
		t.Error("no cap")
	}
}

func TestLocalMissingOrEmpty(t *testing.T) {
	d := t.TempDir()
	if !localMissingOrEmpty(filepath.Join(d, "gone")) || !localMissingOrEmpty(d) {
		t.Error("missing/empty")
	}
	_ = os.MkdirAll(filepath.Join(d, "a", "b"), 0o700)
	_ = os.WriteFile(filepath.Join(d, "a", ".DS_Store"), nil, 0o600)
	if !localMissingOrEmpty(d) {
		t.Error("folders and .DS_Store count as files")
	}
	_ = os.WriteFile(filepath.Join(d, "a", "b", "f"), nil, 0o600)
	if localMissingOrEmpty(d) {
		t.Error("a file was missed")
	}
}

// A sync uses a copy of the settings: reloading or pausing meanwhile is no data race (go test -race).
func TestSyncUsesSettingsSnapshot(t *testing.T) {
	e, _, f := syncRig(t, false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			e.syncFolder(f, modeNormal)
			_ = e.serverTooBusy()
			_ = e.UnsyncedCount("Docs")
		}
	}()
	for i := 0; i < 20; i++ {
		_ = e.Reload()
		_ = e.SetPaused(i%2 == 0)
		_ = e.Status()
	}
	<-done
}

// Round 3: an upgrade that only adds the app's own filter line merges once by itself (backed
// up, not counted as the daily automatic merge) instead of asking for Repair.
func TestUpgradeFilterChangeMergesOnce(t *testing.T) {
	e, fr, f := syncRig(t, false)
	old := strings.TrimSuffix(filtersText(e.cfg, f), backupFilter+"\n") // what 0.5 wrote
	_ = os.WriteFile(e.cfg.filtersPath(f), []byte(old), 0o600)
	_ = os.WriteFile(filepath.Join(e.cfg.Local(f), "a.txt"), []byte("x"), 0o600)
	fr.lsf("a.txt\n")
	fr.bisync("ERROR : Bisync critical error: filters file has changed (must run --resync): /x/filters.txt\n"+
		"ERROR : Bisync aborted. Error is retryable without --resync due to --resilient mode.", 1)
	// The fake answers every bisync the same way, so the merge "fails" too; what matters is
	// that a backed-up merge is tried, once, and the daily automatic merge is untouched.
	e.syncFolder(f, modeNormal)
	c := fr.calls()
	if strings.Count(c, "--resync") < 1 || !strings.Contains(c, "--backup-dir1") || !strings.Contains(c, "--backup-dir2") {
		t.Fatalf("no backed-up merge: %s", c)
	}
	if exists(e.cfg.statePath(f, ".auto-merge")) {
		t.Error("the upgrade merge used the daily automatic merge")
	}
	if exists(e.cfg.statePath(f, ".filters-upgraded")) {
		t.Error("the upgrade merge would be tried again")
	}
	// Next time (still failing) it's an ordinary Repair, not another merge.
	fr.reset()
	e.syncFolder(f, modeNormal)
	if strings.Contains(fr.calls(), "--resync") {
		t.Errorf("merged twice: %s", fr.calls())
	}
	if s := state(e, f); s.State != StNeedsRepair {
		t.Errorf("state: %+v", s)
	}

	// Success path: marker cleared, folder OK.
	_ = os.Remove(e.cfg.statePath(f, ".needs-repair"))
	_ = os.WriteFile(e.cfg.filtersPath(f), []byte(old), 0o600)
	fr.bisync("", 0)
	if !e.syncFolder(f, modeNormal) || state(e, f).State != StOK {
		t.Errorf("after upgrade: %+v", state(e, f))
	}

	// The user's own excludes changed too: that's a Repair, never automatic.
	_ = os.WriteFile(e.cfg.filtersPath(f), []byte("- *.tmp\n"), 0o600)
	fr.bisync("ERROR : Bisync critical error: filters file has changed (must run --resync): /x\n", 1)
	fr.reset()
	e.syncFolder(f, modeNormal)
	if strings.Contains(fr.calls(), "--resync") || state(e, f).State != StNeedsRepair {
		t.Errorf("user filter change merged automatically: %s %+v", fr.calls(), state(e, f))
	}
	// An emptied side is never merged into.
	_ = os.Remove(e.cfg.statePath(f, ".needs-repair"))
	_ = os.WriteFile(e.cfg.filtersPath(f), []byte(old), 0o600)
	_ = os.Remove(filepath.Join(e.cfg.Local(f), "a.txt"))
	fr.reset()
	e.syncFolder(f, modeNormal)
	if strings.Contains(fr.calls(), "--resync") || state(e, f).Detail != missingLocalText("Docs") {
		t.Errorf("merged into an empty local copy: %s %+v", fr.calls(), state(e, f))
	}
}

// Round 3: Stop Keeping a folder whose local copy is gone doesn't sync or trash anything.
func TestUnkeepMissingLocalCopy(t *testing.T) {
	e, fr, f := syncRig(t, false)
	_ = os.RemoveAll(e.cfg.Local(f))
	done := make(chan error, 1)
	if err := e.Unkeep("Docs", func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fr.calls(), "bisync") {
		t.Errorf("synced: %s", fr.calls())
	}
	if len(e.config().Kept) != 0 || exists(e.cfg.statePath(f, ".initialised")) {
		t.Errorf("still kept: %+v", e.config().Kept)
	}
}

func TestClassifyFiltersChanged(t *testing.T) {
	out := "ERROR : Bisync critical error: filters file has changed (must run --resync): /x\n" +
		"ERROR : Bisync aborted. Error is retryable without --resync due to --resilient mode."
	if r := classifyBisync(out, false); r.outcome != outcomeFiltersChanged {
		t.Errorf("got %v", r.outcome)
	}
	if !onlyAppFilterChanged("- a\n", "- a\n"+backupFilter+"\n") || onlyAppFilterChanged("- a\n", "- b\n"+backupFilter+"\n") ||
		onlyAppFilterChanged("- a\n", "- a\n") {
		t.Error("onlyAppFilterChanged")
	}
}
