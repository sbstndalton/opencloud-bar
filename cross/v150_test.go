package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fakeClock drives live sync's timing in tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func liveRig(t *testing.T) (*Engine, *fakeClock, KeptFolder) {
	t.Helper()
	e := pausedRig(t, `,"kept":[{"path":"Docs","stateName":"docs-1"}]`)
	clk := &fakeClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	e.mu.Lock()
	e.cfg.Paused = false
	e.syncing = true // nothing actually runs: queued folders wait in e.queued
	l := e.live()
	l.now = clk.now
	l.folder("Docs")
	e.mu.Unlock()
	return e, clk, e.cfg.Kept[0]
}

func queued(e *Engine, key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.queued[key]
}

func clearQueue(e *Engine) {
	e.mu.Lock()
	e.queued = nil
	e.mu.Unlock()
}

func TestLiveDebounce(t *testing.T) {
	e, clk, _ := liveRig(t)
	e.localEvent("Docs", "a.txt")
	if s := e.Status().Folders[0]; !s.Pending {
		t.Errorf("not pending: %+v", s)
	}
	clk.add(4 * time.Second)
	e.liveTick()
	if queued(e, "Docs") {
		t.Fatal("synced before 5 s of quiet")
	}
	clk.add(time.Second)
	e.liveTick()
	if !queued(e, "Docs") {
		t.Fatal("not synced after 5 s of quiet")
	}
	// Changes that never stop: synced after 60 s anyway.
	clearQueue(e)
	e.mu.Lock()
	e.live().folders["Docs"].lastLive = time.Time{}
	e.mu.Unlock()
	for i := 0; i < 59; i++ {
		e.localEvent("Docs", "b.txt")
		clk.add(time.Second)
		e.liveTick()
		if queued(e, "Docs") {
			t.Fatalf("synced after %d s of continuous changes", i+1)
		}
	}
	e.localEvent("Docs", "b.txt")
	clk.add(time.Second)
	e.liveTick()
	if !queued(e, "Docs") {
		t.Fatal("not synced after 60 s of continuous changes")
	}
	// rclone's temporary files and the backups don't count.
	clearQueue(e)
	e.mu.Lock()
	e.live().folders["Docs"].pending = false
	e.status["Docs"].Pending = false
	e.mu.Unlock()
	for _, p := range []string{"x.pdf.partial", "a/.~tmp~b", ".opencloud-bar-backups/x", "r.conflict1.txt"} {
		e.localEvent("Docs", p)
	}
	if e.Status().Folders[0].Pending {
		t.Error("noise counted as a change")
	}
}

func TestLiveIgnoresOwnSync(t *testing.T) {
	e, clk, f := liveRig(t)
	e.mu.Lock()
	trigger := e.ownSyncLocked(f.Key(), true, clk.now())
	e.mu.Unlock()
	if trigger != "timer" {
		t.Errorf("trigger %q", trigger)
	}
	e.localEvent("Docs", "written-by-sync.txt")
	clk.add(10 * time.Second)
	e.mu.Lock()
	e.ownSyncLocked(f.Key(), false, clk.now().Add(-10*time.Second))
	e.mu.Unlock()
	clk.add(time.Second)
	e.localEvent("Docs", "late-event-of-own-write.txt") // within the 2 s tail
	if e.Status().Folders[0].Pending {
		t.Fatal("own writes counted")
	}
	clk.add(2 * time.Second)
	e.localEvent("Docs", "user.txt")
	if !e.Status().Folders[0].Pending {
		t.Fatal("a real change after the tail ignored")
	}
}

func TestLiveMinimumGap(t *testing.T) {
	e, clk, f := liveRig(t)
	e.localEvent("Docs", "a")
	clk.add(5 * time.Second)
	e.liveTick()
	clearQueue(e)
	// That run starts (triggered by watching) and takes 50 s.
	e.mu.Lock()
	if tr := e.ownSyncLocked(f.Key(), true, clk.now()); tr != "local change" {
		t.Errorf("trigger %q", tr)
	}
	start := clk.now()
	e.mu.Unlock()
	clk.add(50 * time.Second)
	e.mu.Lock()
	e.ownSyncLocked(f.Key(), false, start)
	e.mu.Unlock()
	clk.add(3 * time.Second)
	e.localEvent("Docs", "b")
	clk.add(5 * time.Second) // 58 s after the last start: under max(30 s, 50 s)? no, over 50 s
	e.liveTick()
	if !queued(e, "Docs") {
		t.Fatal("not synced 58 s after a 50 s run")
	}
	clearQueue(e)
	// A short run: the 30 s minimum holds.
	e.mu.Lock()
	e.ownSyncLocked(f.Key(), true, clk.now())
	start = clk.now()
	e.mu.Unlock()
	clk.add(2 * time.Second)
	e.mu.Lock()
	e.ownSyncLocked(f.Key(), false, start)
	e.mu.Unlock()
	clk.add(3 * time.Second)
	e.localEvent("Docs", "c")
	clk.add(6 * time.Second) // 11 s after the start
	e.liveTick()
	if queued(e, "Docs") {
		t.Fatal("synced 11 s after the last live run")
	}
	clk.add(20 * time.Second) // 31 s
	e.liveTick()
	if !queued(e, "Docs") {
		t.Fatal("not synced after the 30 s gap")
	}
}

func TestLivePausedMarksDirty(t *testing.T) {
	e, clk, _ := liveRig(t)
	e.mu.Lock()
	e.cfg.Paused = true
	e.mu.Unlock()
	e.localEvent("Docs", "a")
	clk.add(time.Minute)
	e.liveTick()
	if queued(e, "Docs") || !e.Status().Folders[0].Pending {
		t.Fatal("paused: synced, or the change was forgotten")
	}
	e.mu.Lock()
	e.cfg.Paused = false
	e.mu.Unlock()
	e.liveTick()
	if !queued(e, "Docs") {
		t.Fatal("not synced after resume")
	}
}

func TestLiveWatchLimitFallsBack(t *testing.T) {
	e := pausedRig(t, `,"liveSync":true,"kept":[{"path":"Big","stateName":"big"},{"path":"Small","stateName":"small"}]`)
	for _, d := range []string{"Big/a", "Big/b", "Big/c/d", "Small"} {
		_ = os.MkdirAll(filepath.Join(e.cfg.Local(KeptFolder{Path: d}), ""), 0o700)
	}
	e.mu.Lock()
	l := e.live()
	for _, lf := range l.folders { // started by NewEngine: start over with the limit
		e.stopWatchLocked(lf)
	}
	l.folders = map[string]*liveFolder{}
	old := maxWatchDirs
	maxWatchDirs = 3
	defer func() { maxWatchDirs = old }()
	e.message = ""
	e.updateWatchesLocked()
	big, small := l.folders["Big"], l.folders["Small"]
	msg := e.message
	e.mu.Unlock()
	if big == nil || !big.fallback || big.watch != nil {
		t.Fatalf("Big: %+v", big)
	}
	if small == nil || small.watch == nil || small.fallback {
		t.Fatalf("Small: %+v", small)
	}
	if !strings.Contains(msg, "can't watch “Big”") {
		t.Errorf("notice %q", msg)
	}
	// The system refusing more watches (inotify's limit) does the same, with no second notice.
	e.mu.Lock()
	e.stopWatchesLocked()
	l.folders = map[string]*liveFolder{}
	maxWatchDirs = old
	l.addWatch = func(w *fsnotify.Watcher, dir string) error {
		return &os.SyscallError{Syscall: "inotify_add_watch", Err: syscall.ENOSPC}
	}
	e.message = ""
	e.updateWatchesLocked()
	fb := l.folders["Small"].fallback
	again := e.message
	e.mu.Unlock()
	if !fb || again != "" {
		t.Errorf("ENOSPC: fallback %v, notice %q", fb, again)
	}
	if !errors.Is(&os.SyscallError{Err: syscall.ENOSPC}, syscall.ENOSPC) {
		t.Fatal("errors.Is")
	}
}

// The real watcher: a file written in a kept folder makes it pending; in an excluded folder not.
func TestLiveWatcherSeesChanges(t *testing.T) {
	e := pausedRig(t, `,"liveSync":true,"kept":[{"path":"Docs","stateName":"docs-1","extraFilters":["- /Skip/**"]}]`)
	local := e.cfg.Local(e.cfg.Kept[0])
	_ = os.MkdirAll(filepath.Join(local, "Skip"), 0o700)
	e.mu.Lock()
	e.updateWatchesLocked() // the folder exists now
	e.mu.Unlock()
	_ = os.WriteFile(filepath.Join(local, "Skip", "x.txt"), []byte("x"), 0o600)
	time.Sleep(300 * time.Millisecond)
	if e.Status().Folders[0].Pending {
		t.Fatal("excluded folder watched")
	}
	_ = os.MkdirAll(filepath.Join(local, "new"), 0o700)
	time.Sleep(200 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(local, "new", "a.txt"), []byte("x"), 0o600)
	waitFor(t, func() bool { return e.Status().Folders[0].Pending })
	e.mu.Lock()
	e.cfg.LiveSync = false
	e.updateWatchesLocked()
	n := len(e.live().folders)
	e.mu.Unlock()
	if n != 0 {
		t.Errorf("watches left after turning it off: %d", n)
	}
}
