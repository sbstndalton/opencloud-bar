package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	e.holdRuns = true // nothing actually runs: queued folders wait in e.queued
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

func TestLiveOwnSyncWrites(t *testing.T) {
	e, clk, f := liveRig(t)
	run := func(events, wrote []string, unknown bool) time.Time {
		e.mu.Lock()
		began := clk.now()
		e.ownSyncLocked(f.Key(), true, began)
		e.mu.Unlock()
		for _, p := range events {
			e.localEvent("Docs", p)
		}
		clk.add(10 * time.Second)
		e.mu.Lock()
		e.ownWritesLocked(f.Key(), wrote, unknown)
		e.ownSyncLocked(f.Key(), false, began)
		e.mu.Unlock()
		return began
	}
	// Only what the run wrote (a file, its new folder; a late event within the tail): nothing.
	run([]string{"new/sub", "new/sub/a.txt", "gone.txt"}, []string{"new/sub/a.txt", "gone.txt"}, false)
	clk.add(time.Second)
	e.localEvent("Docs", "new")
	if e.Status().Folders[0].Pending {
		t.Fatal("own writes counted during the tail")
	}
	clk.add(2 * time.Second)
	e.liveTick()
	if e.Status().Folders[0].Pending || queued(e, "Docs") {
		t.Fatal("own writes counted after the run")
	}
	// A real change during the run: one follow-up, after the minimum gap from that run's start.
	run([]string{"a.txt", "user.txt"}, []string{"a.txt"}, false)
	clk.add(3 * time.Second)
	e.liveTick()
	if !e.Status().Folders[0].Pending || queued(e, "Docs") {
		t.Fatal("a change during the run: not pending, or synced before the gap")
	}
	clk.add(20 * time.Second) // 33 s after the run started
	e.liveTick()
	if !queued(e, "Docs") {
		t.Fatal("no follow-up sync")
	}
	clearQueue(e)
	// A merge (writes unknown) with events: a follow-up. Too many events: a follow-up.
	clk.add(time.Minute)
	run([]string{"x"}, nil, true)
	clk.add(3 * time.Second)
	e.liveTick()
	if !e.Status().Folders[0].Pending {
		t.Fatal("merge with events: no follow-up")
	}
	e.mu.Lock()
	e.live().folders["Docs"].pending = false
	e.status["Docs"].Pending = false
	e.mu.Unlock()
	var many []string
	for i := 0; i <= liveOwnCap; i++ {
		many = append(many, "f"+strconv.Itoa(i))
	}
	run(many, many, false)
	clk.add(3 * time.Second)
	e.liveTick()
	if !e.Status().Folders[0].Pending {
		t.Fatal("overflowed during the run: no follow-up")
	}
	// After a real change outside a run: counted at once.
	e.mu.Lock()
	e.live().folders["Docs"].pending = false
	e.status["Docs"].Pending = false
	e.mu.Unlock()
	e.localEvent("Docs", "user2.txt")
	if !e.Status().Folders[0].Pending {
		t.Fatal("a real change after the tail ignored")
	}
}

func TestLiveIgnoredNames(t *testing.T) {
	e, _, _ := liveRig(t)
	for _, p := range []string{"~$Report.docx", "a/.~lock.Sheet.ods#", "x/.DS_Store", "a.tmp", "Thumbs.db", "desktop.ini", "._x", "b~"} {
		e.localEvent("Docs", p)
	}
	if e.Status().Folders[0].Pending {
		t.Error("noise counted as a change")
	}
	names := ignoredNames([]string{"- *.bak", "- **/*.log", "- /Old/**", "- a/b.txt", "+ *.keep"})
	if !nameMatches(names, "x.bak") || !nameMatches(names, "y.log") || nameMatches(names, "b.txt") || len(names) != 2 {
		t.Errorf("names %v", names)
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

// resetLive stops the watches the engine started, so a test starts over.
func resetLive(e *Engine) {
	e.mu.Lock()
	l := e.live()
	var stops []*folderWatch
	for _, lf := range l.folders {
		if lf.watch != nil {
			stops = append(stops, lf.watch)
		}
	}
	l.folders = map[string]*liveFolder{}
	l.noticed = false
	e.message = ""
	e.mu.Unlock()
	for _, fw := range stops {
		fw.stop()
	}
}

func watchOf(e *Engine, key string) *folderWatch {
	e.mu.Lock()
	defer e.mu.Unlock()
	if lf := e.live().folders[key]; lf != nil {
		return lf.watch
	}
	return nil
}

// fakeWatcher behaves like fsnotify on Windows: Add and Remove are served by the goroutine
// that sends the events, so while nobody takes events, Add waits.
type fakeWatcher struct {
	events chan fsnotify.Event
	errs   chan error
	input  chan chan error
	done   chan struct{}
	once   sync.Once
	addErr error
	flood  func(i int) (fsnotify.Event, bool)
	inject chan fsnotify.Event
	hang   atomic.Bool  // requests are taken and never answered (fsnotify on Windows, closed meanwhile)
	adds   atomic.Int64 // requests taken
}

func newFakeWatcher(addErr error, flood func(i int) (fsnotify.Event, bool)) *fakeWatcher {
	w := &fakeWatcher{events: make(chan fsnotify.Event), errs: make(chan error), input: make(chan chan error),
		done: make(chan struct{}), addErr: addErr, flood: flood, inject: make(chan fsnotify.Event)}
	go w.reader()
	return w
}

func (w *fakeWatcher) reader() {
	for i := 0; ; i++ {
		ev, more := fsnotify.Event{}, false
		if w.flood != nil {
			ev, more = w.flood(i)
		}
		if !more {
			select {
			case r := <-w.input:
				w.answer(r)
			case ev := <-w.inject:
				select {
				case w.events <- ev:
				case <-w.done:
					return
				}
			case <-w.done:
				return
			}
			continue
		}
		select {
		case r := <-w.input:
			w.answer(r)
		default:
		}
		select {
		case w.events <- ev:
		case <-w.done:
			return
		}
	}
}

func (w *fakeWatcher) answer(r chan error) {
	w.adds.Add(1)
	if !w.hang.Load() {
		r <- w.addErr
	}
}

func (w *fakeWatcher) call() error {
	r := make(chan error)
	select {
	case w.input <- r:
		return <-r // like fsnotify on Windows: no way out if the answer never comes
	case <-w.done:
		return fsnotify.ErrClosed
	}
}
func (w *fakeWatcher) Add(string) error              { return w.call() }
func (w *fakeWatcher) Remove(string) error           { return w.call() }
func (w *fakeWatcher) Close() error                  { w.once.Do(func() { close(w.done) }); return nil }
func (w *fakeWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *fakeWatcher) Errors() <-chan error          { return w.errs }

func TestLiveWatchLimitFallsBack(t *testing.T) {
	e := pausedRig(t, `,"liveSync":true,"kept":[{"path":"Big","stateName":"big"},{"path":"Small","stateName":"small"}]`)
	for _, d := range []string{"Big/a", "Big/b", "Big/c/d", "Small"} {
		_ = os.MkdirAll(e.cfg.Local(KeptFolder{Path: d}), 0o700)
	}
	l := e.live()
	old := l.maxDirs.Load()
	l.maxDirs.Store(3)
	resetLive(e)
	e.reconcileWatches()
	e.mu.Lock()
	big, small := l.folders["Big"], l.folders["Small"]
	msg := e.message
	e.mu.Unlock()
	if big == nil || big.fallback == "" || big.watch != nil {
		t.Fatalf("Big: %+v", big)
	}
	if small == nil || small.watch == nil || small.fallback != "" {
		t.Fatalf("Small: %+v", small)
	}
	if !strings.Contains(msg, "can't watch “Big”: the system's watch limit was reached, or the folder has too many subfolders") {
		t.Errorf("notice %q", msg)
	}
	if n := l.watchDirs.Load(); n != 1 {
		t.Errorf("watched dirs %d, want 1 (Small)", n)
	}
	// The system refusing more watches (inotify's limit) does the same, with no second notice.
	l.maxDirs.Store(old)
	e.mu.Lock()
	l.newWatcher = func() (fsWatcher, error) {
		return newFakeWatcher(&os.SyscallError{Syscall: "inotify_add_watch", Err: syscall.ENOSPC}, nil), nil
	}
	l.noticed = true
	e.mu.Unlock()
	resetLive(e)
	e.mu.Lock()
	l.noticed = true
	e.mu.Unlock()
	e.reconcileWatches()
	e.mu.Lock()
	fb := l.folders["Small"].fallback
	again := e.message
	e.mu.Unlock()
	if fb == "" || again != "" {
		t.Errorf("ENOSPC: fallback %q, notice %q", fb, again)
	}
	// Changing the folder's excludes tries again.
	e.mu.Lock()
	l.newWatcher = newRealWatcher
	e.cfg.Kept[1].ExtraFilters = []string{"- *.bak"}
	e.mu.Unlock()
	e.reconcileWatches()
	if watchOf(e, "Small") == nil {
		t.Error("not watched again after the excludes changed")
	}
}

// Events pour in while folders are added: fsnotify (Windows) can't take an Add while its
// events wait, so this hangs unless events are taken without e.mu and Add runs without it.
func TestLiveNoDeadlockUnderFlood(t *testing.T) {
	e := pausedRig(t, `,"liveSync":true,"kept":[{"path":"Docs","stateName":"docs-1"}]`)
	local := e.cfg.Local(e.cfg.Kept[0])
	for i := 0; i < 30; i++ {
		_ = os.MkdirAll(filepath.Join(local, "d"+strconv.Itoa(i)), 0o700)
	}
	real, _ := resolveRoot(local)
	var stopFlood atomic.Bool
	e.mu.Lock()
	e.live().newWatcher = func() (fsWatcher, error) {
		return newFakeWatcher(nil, func(i int) (fsnotify.Event, bool) {
			if stopFlood.Load() {
				return fsnotify.Event{}, false
			}
			if i%10 == 0 {
				return fsnotify.Event{Name: filepath.Join(real, "late"+strconv.Itoa(i/10%5)), Op: fsnotify.Create}, true
			}
			return fsnotify.Event{Name: filepath.Join(real, "f.txt"), Op: fsnotify.Write}, true
		}), nil
	}
	e.mu.Unlock()
	resetLive(e) // after the hook: the engine's own liveLoop may reconcile at any moment
	defer stopFlood.Store(true)
	done := make(chan struct{})
	go func() { e.reconcileWatches(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: watching the folder never finished")
	}
	for i := 0; i < 5; i++ { // new folders while the events keep coming: Add from the event loop
		_ = os.MkdirAll(filepath.Join(local, "late"+strconv.Itoa(i)), 0o700)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		fw := watchOf(e, "Docs")
		if fw != nil && fw.count() == 36 && e.Status().Folders[0].Pending {
			break
		}
		if time.Now().After(deadline) {
			n := -1
			if fw != nil {
				n = fw.count()
			}
			t.Fatalf("deadlock or lost folders: watched %d", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopFlood.Store(true)
}

// The real watcher: a file written in a kept folder makes it pending; in an excluded folder
// not. Watched folders are counted once and forgotten when removed.
func TestLiveWatcherSeesChanges(t *testing.T) {
	e := pausedRig(t, `,"liveSync":true,"kept":[{"path":"Docs","stateName":"docs-1","extraFilters":["- /Skip/**"]}]`)
	local := e.cfg.Local(e.cfg.Kept[0])
	_ = os.MkdirAll(filepath.Join(local, "Skip"), 0o700)
	e.reconcileWatches() // the folder exists now
	fw := watchOf(e, "Docs")
	if fw == nil || fw.count() != 1 {
		t.Fatalf("watch %+v", fw)
	}
	_ = os.WriteFile(filepath.Join(local, "Skip", "x.txt"), []byte("x"), 0o600)
	time.Sleep(300 * time.Millisecond)
	if e.Status().Folders[0].Pending {
		t.Fatal("excluded folder watched")
	}
	_ = os.MkdirAll(filepath.Join(local, "new", "sub"), 0o700)
	waitFor(t, func() bool { return fw.count() == 3 })
	_ = os.WriteFile(filepath.Join(local, "new", "a.txt"), []byte("x"), 0o600)
	waitFor(t, func() bool { return e.Status().Folders[0].Pending })
	_ = os.RemoveAll(filepath.Join(local, "new"))
	waitFor(t, func() bool { return fw.count() == 1 })
	if n := e.live().watchDirs.Load(); n != 1 {
		t.Errorf("watched dirs %d after removing a folder", n)
	}
	e.mu.Lock()
	e.cfg.LiveSync = false
	e.mu.Unlock()
	e.reconcileWatches()
	e.mu.Lock()
	n := len(e.live().folders)
	e.mu.Unlock()
	if n != 0 || e.live().watchDirs.Load() != 0 {
		t.Errorf("watches left after turning it off: %d", n)
	}
}

// The watched root renamed away: the watch starts over (when the folder is back) and the
// folder syncs. A symlinked root is watched where it leads.
func TestLiveRootReplacedAndSymlinked(t *testing.T) {
	e := pausedRig(t, `,"liveSync":true,"kept":[{"path":"Docs","stateName":"docs-1"}]`)
	local := e.cfg.Local(e.cfg.Kept[0])
	target := filepath.Join(filepath.Dir(local), "elsewhere")
	_ = os.MkdirAll(target, 0o700)
	_ = os.RemoveAll(local)
	if err := os.Symlink(target, local); err != nil {
		t.Skip("no symlinks:", err)
	}
	resetLive(e)
	e.reconcileWatches()
	fw := watchOf(e, "Docs")
	real, _ := resolveRoot(target)
	if fw == nil || fw.root != real {
		t.Fatalf("symlinked root: %+v", fw)
	}
	_ = os.WriteFile(filepath.Join(local, "a.txt"), []byte("x"), 0o600)
	waitFor(t, func() bool { return e.Status().Folders[0].Pending })
	e.mu.Lock()
	e.live().folders["Docs"].pending = false
	e.status["Docs"].Pending = false
	e.mu.Unlock()
	// Point the link at another folder: the next reconcile notices the root changed.
	other := filepath.Join(filepath.Dir(local), "other")
	_ = os.MkdirAll(other, 0o700)
	_ = os.Remove(local)
	_ = os.Symlink(other, local)
	e.reconcileWatches()
	fw2 := watchOf(e, "Docs")
	real2, _ := resolveRoot(other)
	if fw2 == nil || fw2 == fw || fw2.root != real2 {
		t.Fatalf("root changed, watch %+v", fw2)
	}
	// The root itself removed: restart (nothing to watch now) and a sync.
	_ = os.RemoveAll(other)
	waitFor(t, func() bool { return e.Status().Folders[0].Pending && watchOf(e, "Docs") != fw2 })
}

// fakeRig: one kept folder watched through a fakeWatcher.
func fakeRig(t *testing.T) (*Engine, *fakeWatcher, string) {
	t.Helper()
	e := pausedRig(t, `,"liveSync":true,"kept":[{"path":"Docs","stateName":"docs-1"}]`)
	local := e.cfg.Local(e.cfg.Kept[0])
	_ = os.MkdirAll(local, 0o700)
	// The hook first: the engine's own liveLoop may reconcile at any moment.
	var made sync.Map // *fakeWatcher -> true
	e.mu.Lock()
	e.live().newWatcher = func() (fsWatcher, error) { w := newFakeWatcher(nil, nil); made.Store(w, true); return w, nil }
	e.mu.Unlock()
	var fake *fakeWatcher
	for try := 0; try < 20 && fake == nil; try++ { // the engine's own first reconcile may still attach a real one
		resetLive(e)
		e.reconcileWatches()
		if fw := watchOf(e, "Docs"); fw != nil {
			if w, ok := fw.w.(*fakeWatcher); ok {
				if _, mine := made.Load(w); mine {
					fake = w
				}
			}
		}
	}
	if fake == nil {
		t.Fatal("not watched through the fake")
	}
	real, _ := resolveRoot(local)
	return e, fake, real
}

// An Add that never comes back (fsnotify on Windows, watcher closed meanwhile): the watch is
// dead, starts over, and the folder syncs. After stop, no Add or Remove is tried at all, and
// nothing is counted for a stopped watch.
func TestLiveHungAddRestarts(t *testing.T) {
	e, fake, root := fakeRig(t)
	l := e.live()
	l.callTimeout.Store(int64(100 * time.Millisecond))
	fw := watchOf(e, "Docs")
	fake.hang.Store(true)
	_ = os.MkdirAll(filepath.Join(root, "new"), 0o700)
	fake.inject <- fsnotify.Event{Name: filepath.Join(root, "new"), Op: fsnotify.Create}
	waitFor(t, func() bool { return fw.dead.Load() && watchOf(e, "Docs") != fw })
	waitFor(t, func() bool { return e.Status().Folders[0].Pending })
	select {
	case <-fw.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the dead watch wasn't stopped")
	}
	// The restart (reconcile after the kick) watches it again with a new watcher.
	e.mu.Lock()
	e.live().newWatcher = func() (fsWatcher, error) { return newFakeWatcher(nil, nil), nil }
	e.mu.Unlock()
	e.reconcileWatches()
	fw2 := watchOf(e, "Docs")
	if fw2 == nil || fw2 == fw {
		t.Fatal("not watched again")
	}
	// Stopped: Add isn't called, nothing is counted.
	before, total := fake.adds.Load(), l.watchDirs.Load()
	fw.stop()
	if err := fw.addTree(root, nil, nil); err != nil {
		t.Fatal(err)
	}
	fw.forget(filepath.Join(root, "new"))
	if fake.adds.Load() != before || l.watchDirs.Load() != total || fw.count() != 0 {
		t.Errorf("after stop: adds %d→%d, watched %d→%d, count %d", before, fake.adds.Load(), total, l.watchDirs.Load(), fw.count())
	}
	// A dead watch that nobody noticed yet: the next reconcile restarts it and syncs.
	e.mu.Lock()
	e.live().folders["Docs"].pending = false
	e.status["Docs"].Pending = false
	e.mu.Unlock()
	fw2.dead.Store(true)
	e.reconcileWatches()
	if w := watchOf(e, "Docs"); w == nil || w == fw2 || !e.Status().Folders[0].Pending {
		t.Fatalf("dead watch kept: %v", w == fw2)
	}
}

// Files made in a new folder before its watch was added count as changes (also during the
// app's own sync, so they aren't taken for its writes unless it wrote them).
func TestLiveNewFolderFiles(t *testing.T) {
	e, fake, root := fakeRig(t)
	e.mu.Lock()
	e.ownSyncLocked("Docs", true, time.Now())
	e.mu.Unlock()
	_ = os.MkdirAll(filepath.Join(root, "new", "deep"), 0o700)
	_ = os.WriteFile(filepath.Join(root, "new", "a.txt"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "new", "deep", "b.txt"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "new", ".DS_Store"), []byte("x"), 0o600)
	fake.inject <- fsnotify.Event{Name: filepath.Join(root, "new"), Op: fsnotify.Create}
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		p := e.live().folders["Docs"].ownPaths
		return p["new/a.txt"] && p["new/deep/b.txt"]
	})
	e.mu.Lock()
	ds := e.live().folders["Docs"].ownPaths["new/.DS_Store"]
	e.mu.Unlock()
	if ds {
		t.Error("noise listed")
	}
	if n := watchOf(e, "Docs").count(); n != 3 {
		t.Errorf("watched %d folders, want 3", n)
	}
}
