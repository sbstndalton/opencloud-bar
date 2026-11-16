package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Live sync (preview, setting liveSync, off by default): kept folders' local copies are
// watched (fsnotify: inotify on Linux, ReadDirectoryChangesW on Windows), and a change syncs
// that folder soon instead of at the next interval.
//
//   - Debounce: a folder syncs once it has been quiet for liveQuiet, or liveMaxWait after the
//     first change while changes keep coming.
//   - Runs started by watching are at least max(liveMinGap, the folder's last run) apart.
//   - Events while the app itself syncs the folder (and liveTail after) are kept aside. When
//     the run is over, the paths the run wrote here (from its parsed output) are taken out;
//     anything left (or too much to tell) is a change: one follow-up sync after the gap.
//   - Paused: changes mark the folder; it syncs on resume. Never two runs at once: the folder
//     goes to the same queue as a folder kept while a sync runs.
//   - Watches are per directory (fsnotify v1.9 has recursive watches only in its own tests).
//     Past the cap (Windows 2,000 directories: each costs a 64 KiB buffer; Linux 20,000), or
//     when the system refuses more (inotify's limit), the folder goes back to the timer, with
//     a one-time notice.
//   - Locking: watcher.Add/Remove and directory walks never run under e.mu. On Windows,
//     fsnotify's Add goes through the goroutine that delivers events, so each watcher has a
//     goroutine that only moves events into a bounded queue (drain), and a second one that
//     handles them (watchLoop). Starting and stopping watches happens in reconcileWatches,
//     which takes what it needs under e.mu and does the I/O without it.
//   - Any watcher error, or the watched root removed or renamed, restarts that folder's watch
//     and syncs it; lost events (overflow) sync it. Every reconcile checks that the root is
//     still the same folder (symlinks resolved).
//
// The macOS app does the same with FSEvents (LiveSync.swift).

var (
	liveQuiet    = 5 * time.Second
	liveMaxWait  = 60 * time.Second
	liveMinGap   = 30 * time.Second
	liveTail     = 2 * time.Second
	liveTickTime = 500 * time.Millisecond
	liveOwnCap   = 1000    // events kept during the app's own sync
	liveWroteCap = 10_000  // paths (and their folders) a run wrote here
	liveQueueCap = 100_000 // events waiting to be handled per folder; past it: lost, sync
)

// defaultWatchCap: directories watched, all folders together.
func defaultWatchCap() int64 {
	if runtime.GOOS == "windows" {
		return 2_000
	}
	return 20_000
}

const (
	pendingText = "Change detected, syncing soon"
	limitText   = "the system's watch limit was reached, or the folder has too many subfolders"
)

// liveIgnoredNames: files that never count as a change (the default excludes' simple names,
// Office and LibreOffice lock files). A folder's own simple excludes are added to these.
var liveIgnoredNames = []string{".DS_Store", "Thumbs.db", "desktop.ini", "._*", "*~", "*.tmp",
	".sync_*.db", ".sync_*.db-*", "~$*", ".~lock.*#"}

// fsWatcher is the part of fsnotify.Watcher used here (a fake in tests).
type fsWatcher interface {
	Add(string) error
	Remove(string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

type realWatcher struct{ w *fsnotify.Watcher }

func (r realWatcher) Add(p string) error            { return r.w.Add(p) }
func (r realWatcher) Remove(p string) error         { return r.w.Remove(p) }
func (r realWatcher) Close() error                  { return r.w.Close() }
func (r realWatcher) Events() <-chan fsnotify.Event { return r.w.Events }
func (r realWatcher) Errors() <-chan error          { return r.w.Errors }

func newRealWatcher() (fsWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return realWatcher{w}, nil
}

type liveFolder struct {
	pending     bool
	first, last time.Time
	own         bool      // the app is syncing it now
	ownBegan    time.Time // when that run started
	ownUntil    time.Time // events until then are kept aside (the app's own writes)
	ownCheck    bool      // once ownUntil passes: compare ownPaths with ownWrote
	ownPaths    map[string]bool
	ownOver     bool // more than liveOwnCap events, or events lost
	ownWrote    map[string]bool
	ownWroteAll bool      // the run's writes aren't known (a merge, too many)
	local       bool      // pending for a change here
	server      bool      // pending for a change in OpenCloud (remote.go)
	lastLive    time.Time // when live sync last started a run
	lastDur     time.Duration
	fallback    string // the root and filters watching gave up on: timer only (until they change)
	watch       *folderWatch
}

type liveState struct {
	folders        map[string]*liveFolder
	triggered      map[string]string        // queued by live sync: the next run's trigger ("local change", "server change")
	remote         map[string]*remoteFolder // server change checks (remote.go)
	remoteGone     map[string]bool          // stopped being kept: no checks, no .server-etag writes
	remoteResumeAt time.Time                // after a sleep: no checks before this (wall clock)
	fileMu         sync.Mutex               // .server-etag writes and removals
	remoteRun      int                      // checks running now
	running        bool
	stop           chan struct{}
	kick           chan struct{} // reconcile soon
	noticed        bool          // the one-time notice was given
	watchDirs      atomic.Int64  // directories watched, all folders
	maxDirs        atomic.Int64
	// callTimeout: the longest a watcher Add/Remove may take (nanoseconds); past it the
	// watch is dead (fsnotify on Windows can wait forever on a watcher closed meanwhile).
	callTimeout atomic.Int64
	rmu         sync.Mutex // one reconcile at a time
	// Hooks for tests (read under e.mu).
	now         func() time.Time
	newWatcher  func() (fsWatcher, error)
	remoteToken func(serverURL string) (string, error)
	remoteETag  func(ctx context.Context, davURL, tok string) (string, int, error)
}

func (e *Engine) live() *liveState {
	if e.liveS == nil {
		e.liveS = &liveState{folders: map[string]*liveFolder{}, triggered: map[string]string{}, remote: map[string]*remoteFolder{}, remoteGone: map[string]bool{},
			kick: make(chan struct{}, 1), now: time.Now, newWatcher: newRealWatcher,
			remoteToken: accessToken, remoteETag: davETag}
		e.liveS.maxDirs.Store(defaultWatchCap())
		e.liveS.callTimeout.Store(int64(5 * time.Second))
	}
	return e.liveS
}

func (l *liveState) folder(key string) *liveFolder {
	lf := l.folders[key]
	if lf == nil {
		lf = &liveFolder{}
		l.folders[key] = lf
	}
	return lf
}

// ignoredLocal: paths watching never reacts to (rclone's temporary files, the backups,
// conflict copies, the default ignored names).
func ignoredLocal(rel string) bool {
	rel = slash(rel)
	base := rel[strings.LastIndex(rel, "/")+1:]
	return rel == backupFolderName || strings.HasPrefix(rel, backupFolderName+"/") ||
		strings.HasSuffix(base, ".partial") || strings.Contains(base, ".~tmp~") || reConflict.MatchString(base) ||
		nameMatches(liveIgnoredNames, base)
}

func nameMatches(patterns []string, base string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, base); ok {
			return true
		}
	}
	return false
}

// ignoredNames: the folder's simple file excludes ("- *.bak", "- **/*.log"): a name pattern
// without folders.
func ignoredNames(filters []string) []string {
	var out []string
	for _, l := range filters {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "- ") {
			continue
		}
		p := strings.TrimPrefix(strings.TrimPrefix(l, "- "), "**/")
		if p == "" || strings.ContainsAny(p, "/{}\\") || strings.Contains(p, "**") {
			continue
		}
		if _, err := path.Match(p, ""); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// excludedDirs reads the cheap part of a folder's filters: whole-folder excludes by name at
// any depth ("- **/node_modules/**", "- node_modules/**") and from the top ("- /Old/**").
// Those folders aren't watched.
func excludedDirs(filters []string) (anyDepth, top map[string]bool) {
	anyDepth, top = map[string]bool{}, map[string]bool{}
	for _, l := range filters {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "- ") || !strings.HasSuffix(l, "/**") {
			continue
		}
		p := strings.TrimSuffix(strings.TrimPrefix(l, "- "), "/**")
		switch {
		case strings.HasPrefix(p, "**/") && !strings.ContainsAny(p[3:], "/*?[]{}\\"):
			anyDepth[p[3:]] = true
		case strings.HasPrefix(p, "/"):
			if s, ok := parseSubfolderRule(l); ok {
				top[s] = true
			}
		case !strings.ContainsAny(p, "/*?[]{}\\"):
			anyDepth[p] = true
		}
	}
	return anyDepth, top
}

// watchWant is one folder reconcile wants watched.
type watchWant struct {
	f       KeptFolder
	root    string // as configured
	sig     string // root and filters: the watch starts again when they change
	filters []string
}

// folderWatch is one kept folder's watcher.
type folderWatch struct {
	w        fsWatcher
	l        *liveState
	key      string
	cfgRoot  string // as configured
	root     string // symlinks resolved: what is watched
	rootInfo os.FileInfo
	sig      string
	any, top map[string]bool
	names    []string

	mu     sync.Mutex
	dirs   map[string]bool // watched directories (nil once stopped)
	closed bool            // stop was asked: no more Add/Remove

	dead atomic.Bool  // an Add/Remove never came back: start over
	busy atomic.Int64 // when watchLoop started on the current batch (unix ns), 0 idle

	qmu   sync.Mutex
	queue []fsnotify.Event
	errs  []error
	lost  bool
	wake  chan struct{}
	done  chan struct{}
	once  sync.Once
}

var (
	errWatchLimit  = errors.New(limitText)
	errNoRoot      = errors.New("local folder missing")
	errWatchClosed = errors.New("watch stopped")
	errWatchDead   = errors.New("the watcher stopped answering")
)

// liveStall: a batch taking longer than this means watchLoop is stuck; the watch starts over.
const liveStall = time.Minute

// call runs a watcher Add or Remove, never after stop, and gives up on one that doesn't come
// back (the watch is dead then; reconcile starts it over and the folder syncs).
func (fw *folderWatch) call(f func() error) error {
	fw.mu.Lock()
	closed := fw.closed
	fw.mu.Unlock()
	if closed {
		return errWatchClosed
	}
	res := make(chan error, 1)
	go func() { res <- f() }()
	t := time.NewTimer(time.Duration(fw.l.callTimeout.Load()))
	defer t.Stop()
	select {
	case err := <-res:
		return err
	case <-fw.done:
		return errWatchClosed
	case <-t.C:
		fw.dead.Store(true)
		return errWatchDead
	}
}

// excluded: rel (slash-separated, relative to the root) is in or is a folder not watched.
func (fw *folderWatch) excluded(rel string) bool {
	rel = slash(rel)
	if rel == "." || rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if fw.any[p] {
			return true
		}
	}
	if fw.top[parts[0]] || (len(parts) > 1 && fw.top[parts[0]+"/"+parts[1]]) {
		return true
	}
	return rel == backupFolderName || strings.HasPrefix(rel, backupFolderName+"/")
}

func (fw *folderWatch) ignored(rel string) bool {
	rel = slash(rel)
	return ignoredLocal(rel) || fw.excluded(rel) || nameMatches(fw.names, rel[strings.LastIndex(rel, "/")+1:])
}

func (fw *folderWatch) count() int {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return len(fw.dirs)
}

func (fw *folderWatch) signal() {
	select {
	case fw.wake <- struct{}{}:
	default:
	}
}

// drain only moves events into the queue, so fsnotify's own goroutine never waits on us.
func (fw *folderWatch) drain() {
	ev, er := fw.w.Events(), fw.w.Errors()
	for ev != nil || er != nil {
		select {
		case <-fw.done:
			return
		case x, ok := <-ev:
			if !ok {
				ev = nil
				continue
			}
			fw.qmu.Lock()
			if len(fw.queue) < liveQueueCap {
				fw.queue = append(fw.queue, x)
			} else {
				fw.lost = true
			}
			fw.qmu.Unlock()
			fw.signal()
		case err, ok := <-er:
			if !ok {
				er = nil
				continue
			}
			fw.qmu.Lock()
			if len(fw.errs) < 16 {
				fw.errs = append(fw.errs, err)
			} else {
				fw.lost = true
			}
			fw.qmu.Unlock()
			fw.signal()
		}
	}
}

// stop closes the watcher (never under e.mu: on Windows Close talks to fsnotify's goroutine).
func (fw *folderWatch) stop() {
	fw.once.Do(func() {
		fw.mu.Lock()
		fw.closed = true
		fw.l.watchDirs.Add(-int64(len(fw.dirs)))
		fw.dirs = nil
		fw.mu.Unlock()
		close(fw.done)
		go func() { _ = fw.w.Close() }() // may wait on fsnotify's goroutine: not here
	})
}

// valid: the watch works (not dead, not stuck) and the configured root still leads to the
// folder being watched.
func (fw *folderWatch) valid() bool {
	if fw.dead.Load() {
		return false
	}
	if b := fw.busy.Load(); b != 0 && time.Since(time.Unix(0, b)) > liveStall {
		return false
	}
	real, err := resolveRoot(fw.cfgRoot)
	if err != nil || real != fw.root {
		return false
	}
	st, err := os.Stat(real)
	return err == nil && os.SameFile(st, fw.rootInfo)
}

func resolveRoot(root string) (string, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	return filepath.Abs(real)
}

// addTree watches dir and the folders in it (without e.mu). files, if given, gets the files
// found in it (relative, at most liveOwnCap; more: *over), which no watch saw being made.
func (fw *folderWatch) addTree(dir string, files *[]string, over *bool) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		select {
		case <-fw.done:
			return filepath.SkipAll
		default:
		}
		if err != nil {
			if p == dir {
				return err
			}
			return nil // a folder that went away or can't be read: skip it
		}
		rel, _ := filepath.Rel(fw.root, p)
		if !d.IsDir() {
			if files != nil && !fw.ignored(rel) {
				if len(*files) < liveOwnCap {
					*files = append(*files, slash(rel))
				} else {
					*over = true
				}
			}
			return nil
		}
		if fw.excluded(rel) {
			return filepath.SkipDir
		}
		fw.mu.Lock()
		have := fw.dirs[p]
		fw.mu.Unlock()
		if have {
			return nil
		}
		if fw.l.watchDirs.Load() >= fw.l.maxDirs.Load() {
			return errWatchLimit
		}
		if err := fw.call(func() error { return fw.w.Add(p) }); err != nil {
			switch {
			case errors.Is(err, errWatchClosed):
				return filepath.SkipAll
			case errors.Is(err, errWatchDead):
				return err
			case errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EMFILE):
				return errWatchLimit
			}
			return nil
		}
		fw.mu.Lock()
		if !fw.closed && !fw.dirs[p] {
			fw.dirs[p] = true
			fw.l.watchDirs.Add(1)
		}
		fw.mu.Unlock()
		return nil
	})
}

// forget: a watched directory was removed or renamed; it and the folders in it aren't
// watched any more (the new name comes as a Create).
func (fw *folderWatch) forget(name string) {
	fw.mu.Lock()
	if !fw.dirs[name] {
		fw.mu.Unlock()
		return
	}
	var gone []string
	for d := range fw.dirs {
		if d == name || strings.HasPrefix(d, name+string(filepath.Separator)) {
			gone = append(gone, d)
			delete(fw.dirs, d)
		}
	}
	fw.l.watchDirs.Add(-int64(len(gone)))
	fw.mu.Unlock()
	for _, d := range gone {
		if err := fw.call(func() error { return fw.w.Remove(d) }); errors.Is(err, errWatchDead) || errors.Is(err, errWatchClosed) {
			return // inotify drops a deleted folder's watch itself; others: the watch starts over
		}
	}
}

// updateWatchesLocked asks for the watches to match the settings (keep, stop keeping, the
// setting, a changed local folder or excludes). The caller holds e.mu; the work is done by
// liveLoop (reconcileWatches) without it.
func (e *Engine) updateWatchesLocked() {
	l := e.live()
	if e.cfg.LiveOn() && e.cfgErr == nil && !e.quitting && !l.running {
		l.running = true
		l.stop = make(chan struct{})
		go e.liveLoop(l.stop)
	}
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// reconcileWatches starts and stops folder watches: on with liveSync, for every kept folder
// whose local copy exists, unless watching it gave up. Not under e.mu.
func (e *Engine) reconcileWatches() {
	l := e.live()
	l.rmu.Lock()
	defer l.rmu.Unlock()

	e.mu.Lock()
	wants := map[string]watchWant{}
	if e.cfg.LiveOn() && e.cfgErr == nil && !e.quitting {
		for _, f := range e.cfg.Kept {
			filters := append(append([]string{}, f.ExtraFilters...), e.cfg.CommonFilters...)
			root := e.cfg.Local(f)
			wants[f.Key()] = watchWant{f: f, root: root, sig: root + "\x00" + strings.Join(filters, "\x00"), filters: filters}
		}
	}
	var stops []*folderWatch
	cur := map[string]*folderWatch{}
	for key, lf := range l.folders {
		w, ok := wants[key]
		if !ok {
			if lf.watch != nil {
				stops = append(stops, lf.watch)
			}
			delete(l.folders, key)
			delete(l.triggered, key)
			continue
		}
		if lf.fallback != "" && lf.fallback != w.sig {
			lf.fallback = "" // the folder or its excludes changed: try again
		}
		if lf.watch != nil {
			if lf.watch.sig != w.sig {
				stops = append(stops, lf.watch)
				lf.watch = nil
			} else {
				cur[key] = lf.watch
			}
		}
	}
	newWatcher := l.newWatcher
	e.mu.Unlock()

	for key, fw := range cur { // dead or stuck, or the root moved, went away or is another folder now
		if fw.valid() {
			continue
		}
		broken := fw.dead.Load() || fw.busy.Load() != 0
		e.mu.Lock()
		if lf := l.folders[key]; lf != nil && lf.watch == fw {
			lf.watch = nil
			if broken {
				e.noteLocked(key, lf, nil, true) // changes may have been missed: sync
			}
		}
		e.mu.Unlock()
		stops = append(stops, fw)
	}
	for _, fw := range stops {
		fw.stop()
	}

	for key, w := range wants {
		e.mu.Lock()
		lf := l.folders[key]
		need := lf == nil || (lf.watch == nil && lf.fallback == "")
		e.mu.Unlock()
		if !need {
			continue
		}
		fw, err := e.startWatch(l, key, w, newWatcher)
		if errors.Is(err, errNoRoot) || errors.Is(err, errWatchDead) {
			e.mu.Lock()
			l.folder(key) // not there (yet): the next reconcile tries again
			e.mu.Unlock()
			continue
		}
		e.mu.Lock()
		lf = l.folder(key)
		logText := ""
		switch {
		case !e.cfg.LiveOn() || e.quitting || lf.watch != nil:
		case err != nil:
			logText = e.watchFallbackLocked(key, w, err)
		default:
			lf.watch, fw = fw, nil
			go e.watchLoop(lf.watch)
		}
		e.mu.Unlock()
		if logText != "" {
			e.logLine("%s", logText)
		}
		if fw != nil {
			fw.stop()
		}
	}
}

// startWatch makes a folder's watcher and watches its tree (I/O; not under e.mu).
func (e *Engine) startWatch(l *liveState, key string, w watchWant, newWatcher func() (fsWatcher, error)) (*folderWatch, error) {
	if st, err := os.Stat(w.root); err != nil || !st.IsDir() {
		return nil, errNoRoot
	}
	real, err := resolveRoot(w.root)
	if err != nil {
		return nil, errNoRoot
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, errNoRoot
	}
	fsw, err := newWatcher()
	if err != nil {
		return nil, err
	}
	any, top := excludedDirs(w.filters)
	fw := &folderWatch{w: fsw, l: l, key: key, cfgRoot: w.root, root: real, rootInfo: info, sig: w.sig,
		any: any, top: top, names: ignoredNames(w.filters), dirs: map[string]bool{},
		wake: make(chan struct{}, 1), done: make(chan struct{})}
	go fw.drain() // before the first Add (Windows)
	if err := fw.addTree(real, nil, nil); err != nil {
		fw.stop()
		return nil, err
	}
	if fw.count() == 0 {
		fw.stop()
		return nil, errors.New("no folder could be watched")
	}
	return fw, nil
}

// watchFallbackLocked: this folder can't be watched; it syncs on the timer. One notice. It
// returns the line for app.log, written by the caller after unlocking (no file I/O under e.mu).
func (e *Engine) watchFallbackLocked(key string, w watchWant, err error) string {
	l := e.live()
	l.folder(key).fallback = w.sig
	if !l.noticed {
		l.noticed = true
		e.message = "Live sync can't watch “" + w.f.DisplayName() + "”: " + limitText + ". It syncs every interval instead."
	}
	return fmt.Sprintf("live sync: can't watch %s (%v); it syncs on the timer", w.f.Path, err)
}

// watchLoop handles one folder's events: new folders are watched, changes noted.
func (e *Engine) watchLoop(fw *folderWatch) {
	for {
		select {
		case <-fw.done:
			return
		case <-fw.wake:
		}
		fw.busy.Store(time.Now().UnixNano())
		fw.qmu.Lock()
		batch, errs, lost := fw.queue, fw.errs, fw.lost
		fw.queue, fw.errs, fw.lost = nil, nil, false
		fw.qmu.Unlock()
		full, restart := lost, false
		var limit error
		for _, err := range errs {
			full = true // events may be lost: sync to be sure
			if !errors.Is(err, fsnotify.ErrEventOverflow) {
				restart = true
			}
		}
		var rels []string
		for _, ev := range batch {
			name := filepath.Clean(ev.Name)
			if name == fw.root {
				if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					restart, full = true, true
				}
				continue
			}
			rel, err := filepath.Rel(fw.root, name)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				fw.forget(name)
			}
			if ev.Op&fsnotify.Create != 0 && limit == nil && !fw.excluded(rel) {
				if st, err := os.Lstat(name); err == nil && st.IsDir() {
					// Files made in it before its watch was added: no event saw them.
					var files []string
					over := false
					switch err := fw.addTree(name, &files, &over); {
					case errors.Is(err, errWatchLimit):
						limit = err
					case errors.Is(err, errWatchDead):
						restart, full = true, true
					}
					rels = append(rels, files...)
					full = full || over
				}
			}
			if ev.Op == fsnotify.Chmod || fw.ignored(rel) {
				continue // metadata only (Spotlight, backups touching files), or noise
			}
			rels = append(rels, slash(rel))
		}
		if fw.dead.Load() {
			restart, full = true, true
		}
		ok := e.liveEvents(fw, rels, full, restart, limit)
		fw.busy.Store(0)
		if !ok {
			return
		}
	}
}

// liveEvents notes a batch of changes; false when fw isn't the folder's watch any more.
func (e *Engine) liveEvents(fw *folderWatch, rels []string, full, restart bool, limit error) bool {
	e.mu.Lock()
	l := e.live()
	lf := l.folders[fw.key]
	if lf == nil || lf.watch != fw {
		e.mu.Unlock()
		fw.stop()
		return false
	}
	logText := ""
	if limit != nil {
		lf.watch = nil
		for _, f := range e.cfg.Kept {
			if f.Key() == fw.key {
				logText = e.watchFallbackLocked(fw.key, watchWant{f: f, sig: fw.sig}, limit)
			}
		}
		full = true // what happened in the folders not watched isn't known
	} else if restart {
		lf.watch = nil
	}
	e.noteLocked(fw.key, lf, rels, full)
	detached := lf.watch == nil
	if detached {
		select {
		case l.kick <- struct{}{}:
		default:
		}
	}
	e.mu.Unlock()
	if logText != "" {
		e.logLine("%s", logText)
	}
	if detached {
		fw.stop()
		return false
	}
	return true
}

// noteLocked: changes in a folder (full: some may be lost, sync it). The caller holds e.mu.
func (e *Engine) noteLocked(key string, lf *liveFolder, rels []string, full bool) {
	if len(rels) == 0 && !full {
		return
	}
	l := e.live()
	now := l.now()
	if lf.own || now.Before(lf.ownUntil) { // maybe the app's own writes: decided after the run
		if lf.ownPaths == nil {
			lf.ownPaths = map[string]bool{}
		}
		lf.ownOver = lf.ownOver || full
		for _, r := range rels {
			if len(lf.ownPaths) >= liveOwnCap {
				lf.ownOver = true
				break
			}
			lf.ownPaths[r] = true
		}
		return
	}
	if !lf.pending {
		lf.pending, lf.first = true, now
	}
	lf.local = true
	lf.last = now
	if full {
		lf.last = now.Add(-liveQuiet) // settled: sync as soon as the gap allows
	}
	if s := e.status[key]; s != nil {
		s.Pending = true
	}
}

// localEvent notes one change in a kept folder's local copy (tests, and the shape of it).
func (e *Engine) localEvent(key, rel string) {
	if ignoredLocal(rel) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if lf := e.live().folders[key]; lf != nil {
		e.noteLocked(key, lf, []string{slash(rel)}, false)
	}
}

// liveTick queues folders whose changes have settled. Runs every liveTickTime.
func (e *Engine) liveTick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.live()
	now := l.now()
	for key, lf := range l.folders { // runs that ended: what changed here that they didn't write?
		if !lf.ownCheck || lf.own || now.Before(lf.ownUntil) {
			continue
		}
		lf.ownCheck = false
		left := lf.ownOver || (lf.ownWroteAll && len(lf.ownPaths) > 0)
		for p := range lf.ownPaths {
			if !lf.ownWrote[p] {
				left = true
				break
			}
		}
		lf.ownPaths, lf.ownOver, lf.ownWrote, lf.ownWroteAll = nil, false, nil, false
		if left {
			if !lf.pending {
				lf.pending, lf.first = true, now
			}
			lf.local = true
			lf.last = now.Add(-liveQuiet)
			if lf.lastLive.Before(lf.ownBegan) {
				lf.lastLive = lf.ownBegan // one follow-up, after the gap
			}
			if s := e.status[key]; s != nil {
				s.Pending = true
			}
		}
	}
	if e.cfg.Paused || e.cfgErr != nil || e.signedOut || e.quitting {
		return // still pending: syncs after resume
	}
	started := false
	for key, lf := range l.folders {
		if !lf.pending || lf.own {
			continue
		}
		settled := now.Sub(lf.last) >= liveQuiet || now.Sub(lf.first) >= liveMaxWait
		gap := liveGap(lf.lastDur)
		if !settled || (!lf.lastLive.IsZero() && now.Sub(lf.lastLive) < gap) || e.waitingLocked(key) {
			continue
		}
		lf.pending = false
		trig := "local change"
		if lf.server && !lf.local {
			trig = "server change"
		}
		lf.local, lf.server = false, false
		l.triggered[key] = trig
		e.queueLocked(key)
		started = true
	}
	if started {
		e.startQueuedLocked()
	}
}

func (e *Engine) liveLoop(stop chan struct{}) {
	l := e.live()
	t := time.NewTicker(liveTickTime)
	defer t.Stop()
	n := 0
	var lastWall time.Time
	reconcile := func() bool {
		e.reconcileWatches()
		e.mu.Lock()
		defer e.mu.Unlock()
		if l.stop != stop {
			return false
		}
		if !e.cfg.LiveOn() || e.cfgErr != nil || e.quitting {
			l.running = false
			return false
		}
		return true
	}
	if !reconcile() {
		return
	}
	for {
		select {
		case <-stop:
			return
		case <-l.kick:
			if !reconcile() {
				return
			}
		case <-t.C:
			// The wall clock jumped (the computer slept): give the network a moment before
			// asking the server again (the monotonic clock may not count the sleep).
			wall := time.Now().Round(0)
			if !lastWall.IsZero() && wall.Sub(lastWall) > remoteWakeGap {
				e.mu.Lock()
				l.remoteResumeAt = wall.Add(remoteWakeWait)
				e.mu.Unlock()
			}
			lastWall = wall
			e.liveTick()
			e.remoteTick()
			if n++; n%60 == 0 && !reconcile() { // now and then: folders that appeared, roots that changed
				return
			}
		}
	}
}

// ownSyncLocked marks the app's own sync of a folder: events during it and for liveTail
// after are kept aside. It returns the run's trigger for Recent Changes. The caller holds e.mu.
func (e *Engine) ownSyncLocked(key string, start bool, began time.Time) string {
	l := e.live()
	lf := l.folders[key]
	if start {
		trigger := "timer"
		if t, ok := l.triggered[key]; ok {
			trigger = t
			delete(l.triggered, key)
		} else if lf != nil && lf.pending && (lf.local || lf.server) {
			// A change waiting for its own sync: this run (the timer's, say) takes it.
			trigger = "server change"
			if lf.local {
				trigger = "local change"
			}
		}
		if trigger != "timer" {
			if lf != nil {
				lf.lastLive = l.now()
			}
		}
		if lf != nil {
			lf.local, lf.server = false, false
			lf.own, lf.ownBegan = true, began
			lf.pending = false // this run picks up what changed so far
			if !lf.ownCheck {  // a check still waiting: this run takes its events too
				lf.ownPaths, lf.ownOver, lf.ownWrote, lf.ownWroteAll = nil, false, nil, false
			}
			lf.ownCheck = false
		}
		if s := e.status[key]; s != nil {
			s.Pending = false
		}
		e.unqueueLocked(key) // this run takes what was queued for it
		return trigger
	}
	if lf != nil {
		lf.own = false
		lf.ownUntil = l.now().Add(liveTail)
		lf.lastDur = l.now().Sub(began)
		lf.ownCheck = true
	}
	return ""
}

// ownWritesLocked records what a run wrote here (downloads, deletions from OpenCloud, from
// its parsed output) and the folders they are in; unknown: it can't be told (a merge, a
// failed run's partial output is still recorded). The caller holds e.mu.
func (e *Engine) ownWritesLocked(key string, paths []string, unknown bool) {
	lf := e.live().folders[key]
	if lf == nil {
		return
	}
	if lf.ownWrote == nil {
		lf.ownWrote = map[string]bool{}
	}
	lf.ownWroteAll = lf.ownWroteAll || unknown
	for _, p := range paths {
		for p = slash(p); p != "" && p != "." && !lf.ownWrote[p]; {
			if len(lf.ownWrote) >= liveWroteCap {
				lf.ownWroteAll = true
				return
			}
			lf.ownWrote[p] = true
			i := strings.LastIndex(p, "/")
			if i < 0 {
				break
			}
			p = p[:i]
		}
	}
}

// stopWatchesLocked stops watching (quit). The watchers close in the background.
func (e *Engine) stopWatchesLocked() {
	l := e.live()
	for _, lf := range l.folders {
		if fw := lf.watch; fw != nil {
			lf.watch = nil
			go fw.stop()
		}
	}
	if l.running {
		l.running = false
		close(l.stop)
		l.stop = nil
	}
}
