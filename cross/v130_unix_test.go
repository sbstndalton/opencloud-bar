//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder kept while a sync runs is saved at once and syncs right after that run, once, on
// its own (the folders already synced aren't synced again). One folder at a time here; with
// a free lane it starts at once (TestParallelKeepStartsInFreeLane).
func TestKeepWhileSyncingQueues(t *testing.T) {
	e, fr, _ := syncRig(t, false)
	e.cfg.Paused = false
	e.cfg.SyncParallel = 1
	hold := filepath.Join(fr.dir, "hold")
	_ = os.WriteFile(hold, nil, 0o600)
	e.mu.Lock()
	e.startSyncAllLocked(false)
	e.mu.Unlock()
	waitFor(t, func() bool { return strings.Contains(fr.calls(), "bisync") })
	if err := e.Keep("New", ""); err != nil {
		t.Fatalf("keep during a sync: %v", err)
	}
	if c := e.config(); len(c.Kept) != 2 {
		t.Fatalf("not saved: %+v", c.Kept)
	}
	e.mu.Lock()
	queued, detail := e.waitingLocked("New"), e.status["New"].Detail
	e.mu.Unlock()
	if !queued || detail == "" {
		t.Errorf("not queued: %v %q", queued, detail)
	}
	_ = os.Remove(hold)
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.syncing && !e.waitingLocked("New") && strings.Contains(fr.calls(), "New")
	})
	var docs, runsNew int
	for _, l := range strings.Split(fr.calls(), "\n") {
		if strings.HasPrefix(l, "bisync") && strings.Contains(l, "Docs") {
			docs++
		}
		if strings.HasPrefix(l, "bisync") && strings.Contains(l, "/New") {
			runsNew++
		}
	}
	if docs != 1 || runsNew == 0 {
		t.Errorf("Docs synced %d times, New %d:\n%s", docs, runsNew, fr.calls())
	}
	// Nothing left queued: the next run is the regular one.
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.queued != nil && len(e.queued) > 0 {
		t.Errorf("still queued: %v", e.queued)
	}
}

// Council on 1.3.0: a change to a folder's excludes (Choose Subfolders, Change Excludes, the
// common excludes) is followed by one automatic backed-up merge of that folder, right away,
// instead of a Repair. Other folders aren't touched.
func TestExcludesChangeMergesAutomatically(t *testing.T) {
	e, fr, f := syncRig(t, false)
	e.cfg.Paused = false
	_ = os.WriteFile(filepath.Join(e.cfg.Local(f), "a.txt"), []byte("x"), 0o600)
	fr.lsf("a.txt\n")
	_ = os.WriteFile(e.cfg.filtersPath(f), []byte(filtersText(e.cfg, f)), 0o600) // synced before
	if _, err := applyConfigPatch(configPatch{KeptSubfolders: map[string]map[string]bool{"Docs": {"Big": false}}}); err != nil {
		t.Fatal(err)
	}
	fr.bisync("ERROR : Bisync critical error: filters file has changed (must run --resync): /x\n", 1)
	fr.reset()
	if err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(fr.calls(), "--resync") && !e.Status().Syncing })
	c := fr.calls()
	if !strings.Contains(c, "--backup-dir1") || !strings.Contains(c, "--resync-mode newer") {
		t.Errorf("not a backed-up merge: %s", c)
	}
	cfg := e.config()
	if exists(cfg.statePath(f, ".auto-merge")) {
		t.Error("counted as the daily automatic merge")
	}
	if b, _ := os.ReadFile(cfg.filtersPath(f)); !strings.HasPrefix(string(b), "- /Big/**\n") {
		t.Errorf("filters: %s", b)
	}
	// A reload with nothing changed merges nothing.
	fr.reset()
	if err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	if exists(cfg.statePath(f, ".filters-upgraded")) {
		t.Error("marker written without a change")
	}
}

// A setting saved while a sync runs is used by the run that follows it.
func TestQueuedRunUsesSettingsSavedDuringSync(t *testing.T) {
	e, fr, _ := syncRig(t, false)
	e.cfg.Paused = false
	e.cfg.SyncParallel = 1
	hold := filepath.Join(fr.dir, "hold")
	_ = os.WriteFile(hold, nil, 0o600)
	e.mu.Lock()
	e.startSyncAllLocked(false)
	e.mu.Unlock()
	waitFor(t, func() bool { return strings.Contains(fr.calls(), "bisync") })
	if err := e.Keep("New", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfigPatch(configPatch{KeptFilters: map[string][]string{"New": {"- *.psd"}}}); err != nil {
		t.Fatal(err)
	}
	if e.Reload() == nil {
		t.Fatal("reload during a sync")
	}
	e.reloadWhenIdle()
	_ = os.Remove(hold)
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.syncing && !e.waitingLocked("New") && strings.Contains(fr.calls(), "/New")
	})
	var nf KeptFolder
	for _, k := range e.config().Kept {
		if k.Path == "New" {
			nf = k
		}
	}
	if b, _ := os.ReadFile(e.config().filtersPath(nf)); !strings.HasPrefix(string(b), "- *.psd\n") {
		t.Errorf("queued run used old settings: %q", b)
	}
}
