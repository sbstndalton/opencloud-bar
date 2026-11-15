//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder kept while a sync runs is saved at once and syncs right after that run, once, on
// its own (the folders already synced aren't synced again).
func TestKeepWhileSyncingQueues(t *testing.T) {
	e, fr, _ := syncRig(t, false)
	e.cfg.Paused = false
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
	queued, detail := e.queued["New"], e.status["New"].Detail
	e.mu.Unlock()
	if !queued || detail == "" {
		t.Errorf("not queued: %v %q", queued, detail)
	}
	_ = os.Remove(hold)
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.syncing && len(e.queued) == 0 && strings.Contains(fr.calls(), "New")
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
