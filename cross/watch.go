package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
//   - Events while the app itself syncs the folder (and liveTail after) are its own writes.
//   - Paused: changes mark the folder; it syncs on resume. Never two runs at once: the folder
//     goes to the same queue as a folder kept while a sync runs.
//   - Watches are per directory. Past maxWatchDirs, or when the system refuses more (inotify's
//     limit), the folder goes back to the timer, with a one-time notice.
//
// The macOS app does the same with FSEvents (LiveSync.swift).

var (
	liveQuiet    = 5 * time.Second
	liveMaxWait  = 60 * time.Second
	liveMinGap   = 30 * time.Second
	liveTail     = 2 * time.Second
	liveTickTime = 500 * time.Millisecond
	maxWatchDirs = 50_000
)

const pendingText = "Change detected, syncing soon"

type liveFolder struct {
	pending     bool
	first, last time.Time
	ownUntil    time.Time // ignore events until then (the app's own sync)
	own         bool      // the app is syncing it now
	lastLive    time.Time // when watching last started a run
	lastDur     time.Duration
	fallback    bool // watching gave up: timer only
	watch       *folderWatch
}

type liveState struct {
	folders   map[string]*liveFolder
	triggered map[string]bool // queued by watching: their next run's changes say "local change"
	running   bool
	stop      chan struct{}
	noticed   bool // the one-time watch-limit notice was given
	watchDirs int  // directories watched, all folders
	// Hooks for tests.
	now      func() time.Time
	addWatch func(w *fsnotify.Watcher, dir string) error
}

func (e *Engine) live() *liveState {
	if e.liveS == nil {
		e.liveS = &liveState{folders: map[string]*liveFolder{}, triggered: map[string]bool{},
			now: time.Now, addWatch: func(w *fsnotify.Watcher, dir string) error { return w.Add(dir) }}
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

// ignoredLocal: names watching never reacts to (rclone's temporary files, the backups).
func ignoredLocal(rel string) bool {
	rel = slash(rel)
	base := rel[strings.LastIndex(rel, "/")+1:]
	return rel == backupFolderName || strings.HasPrefix(rel, backupFolderName+"/") ||
		strings.HasSuffix(base, ".partial") || strings.Contains(base, ".~tmp~") || reConflict.MatchString(base)
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

// folderWatch is one kept folder's fsnotify watcher.
type folderWatch struct {
	w     *fsnotify.Watcher
	root  string
	key   string
	dirs  int
	any   map[string]bool
	top   map[string]bool
	close sync.Once
}

var errWatchLimit = errors.New("too many folders to watch")

func (fw *folderWatch) skipDir(rel string) bool {
	if rel == "." || rel == "" {
		return false
	}
	rel = slash(rel)
	base := rel[strings.LastIndex(rel, "/")+1:]
	return ignoredLocal(rel) || fw.any[base] || fw.top[rel] || (strings.Contains(rel, "/") && fw.top[rel[:strings.LastIndex(rel, "/")]])
}

// addTree watches dir and the folders in it (the caller holds e.mu).
func (e *Engine) addTreeLocked(fw *folderWatch, dir string) error {
	l := e.live()
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // a folder that went away or can't be read: skip it
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(fw.root, p)
		if fw.skipDir(rel) {
			return filepath.SkipDir
		}
		if l.watchDirs >= maxWatchDirs {
			return errWatchLimit
		}
		if err := l.addWatch(fw.w, p); err != nil {
			if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EMFILE) {
				return errWatchLimit
			}
			return nil
		}
		fw.dirs++
		l.watchDirs++
		return nil
	})
}

// updateWatchesLocked starts and stops folder watches to match the settings: on with
// liveSync, for every kept folder whose local copy exists. The caller holds e.mu.
func (e *Engine) updateWatchesLocked() {
	l := e.live()
	want := map[string]KeptFolder{}
	if e.cfg.LiveSync && e.cfgErr == nil && !e.quitting {
		for _, f := range e.cfg.Kept {
			want[f.Key()] = f
		}
	}
	for key, lf := range l.folders {
		f, ok := want[key]
		if lf.watch != nil && (!ok || lf.watch.root != e.cfg.Local(f)) {
			e.stopWatchLocked(lf)
		}
		if !ok {
			delete(l.folders, key)
		}
	}
	for key, f := range want {
		lf := l.folder(key)
		if lf.watch != nil || lf.fallback {
			continue
		}
		root := e.cfg.Local(f)
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			continue // not there (yet): the next update tries again
		}
		w, err := fsnotify.NewWatcher()
		if err != nil {
			e.watchFallbackLocked(key, f, err)
			continue
		}
		any, top := excludedDirs(append(append([]string{}, f.ExtraFilters...), e.cfg.CommonFilters...))
		fw := &folderWatch{w: w, root: root, key: key, any: any, top: top}
		if err := e.addTreeLocked(fw, root); err != nil {
			e.watchFallbackLocked(key, f, err)
			l.watchDirs -= fw.dirs
			_ = w.Close()
			continue
		}
		lf.watch = fw
		go e.watchLoop(fw)
	}
	if len(want) > 0 && !l.running {
		l.running = true
		l.stop = make(chan struct{})
		go e.liveLoop(l.stop)
	} else if len(want) == 0 && l.running {
		l.running = false
		close(l.stop)
	}
}

func (e *Engine) stopWatchLocked(lf *liveFolder) {
	if lf.watch == nil {
		return
	}
	fw := lf.watch
	lf.watch = nil
	e.live().watchDirs -= fw.dirs
	fw.close.Do(func() { _ = fw.w.Close() })
}

// watchFallbackLocked: this folder can't be watched; it syncs on the timer. One notice.
func (e *Engine) watchFallbackLocked(key string, f KeptFolder, err error) {
	l := e.live()
	l.folder(key).fallback = true
	e.logLine("live sync: can't watch %s (%v); it syncs on the timer", f.Path, err)
	if !l.noticed {
		l.noticed = true
		e.message = "Live sync can't watch “" + f.DisplayName() + "” (too many folders in it); it syncs every interval instead."
	}
}

// watchLoop turns one folder's events into localEvent calls, and watches new folders.
func (e *Engine) watchLoop(fw *folderWatch) {
	for {
		select {
		case ev, ok := <-fw.w.Events:
			if !ok {
				return
			}
			rel, err := filepath.Rel(fw.root, ev.Name)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() && !fw.skipDir(rel) {
					e.mu.Lock()
					if e.live().folders[fw.key] != nil && e.live().folders[fw.key].watch == fw {
						if err := e.addTreeLocked(fw, ev.Name); errors.Is(err, errWatchLimit) {
							var f KeptFolder
							for _, k := range e.cfg.Kept {
								if k.Key() == fw.key {
									f = k
								}
							}
							lf := e.live().folders[fw.key]
							e.stopWatchLocked(lf)
							e.watchFallbackLocked(fw.key, f, err)
						}
					}
					e.mu.Unlock()
				}
			}
			if ev.Op == fsnotify.Chmod {
				continue // metadata only (Spotlight, backups touching files)
			}
			if fw.skipDir(filepath.Dir(rel)) || ignoredLocal(rel) {
				continue
			}
			e.localEvent(fw.key, rel)
		case err, ok := <-fw.w.Errors:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				e.localEvent(fw.key, ".") // events were lost: sync to be sure
			}
		}
	}
}

// localEvent notes a change in a kept folder's local copy.
func (e *Engine) localEvent(key, rel string) {
	if ignoredLocal(rel) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.live()
	lf := l.folders[key]
	if lf == nil {
		return
	}
	now := l.now()
	if lf.own || now.Before(lf.ownUntil) {
		return // the app's own writes
	}
	if !lf.pending {
		lf.pending, lf.first = true, now
	}
	lf.last = now
	if s := e.status[key]; s != nil {
		s.Pending = true
	}
}

// liveTick queues folders whose changes have settled. Runs every liveTickTime.
func (e *Engine) liveTick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.live()
	now := l.now()
	if e.cfg.Paused || e.cfgErr != nil || e.signedOut || e.quitting {
		return // still pending: syncs after resume
	}
	started := false
	for key, lf := range l.folders {
		if !lf.pending || lf.own {
			continue
		}
		settled := now.Sub(lf.last) >= liveQuiet || now.Sub(lf.first) >= liveMaxWait
		gap := max(liveMinGap, lf.lastDur)
		if !settled || (!lf.lastLive.IsZero() && now.Sub(lf.lastLive) < gap) || e.queued[key] {
			continue
		}
		lf.pending = false
		l.triggered[key] = true
		e.queueLocked(key)
		started = true
	}
	if started {
		e.startQueuedLocked()
	}
}

func (e *Engine) liveLoop(stop chan struct{}) {
	t := time.NewTicker(liveTickTime)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			e.liveTick()
			if n++; n%60 == 0 { // now and then: folders that appeared (or came back)
				e.mu.Lock()
				e.updateWatchesLocked()
				e.mu.Unlock()
			}
		}
	}
}

// ownSyncLocked marks the app's own sync of a folder: events are ignored during it and for
// liveTail after. It returns the run's trigger for Recent Changes. The caller holds e.mu.
func (e *Engine) ownSyncLocked(key string, start bool, began time.Time) string {
	l := e.live()
	lf := l.folders[key]
	if start {
		trigger := "timer"
		if l.triggered[key] {
			trigger = "local change"
			delete(l.triggered, key)
			if lf != nil {
				lf.lastLive = l.now()
			}
		}
		if lf != nil {
			lf.own = true
			lf.pending = false // this run picks up what changed so far
		}
		if s := e.status[key]; s != nil {
			s.Pending = false
		}
		return trigger
	}
	if lf != nil {
		lf.own = false
		lf.ownUntil = l.now().Add(liveTail)
		lf.lastDur = l.now().Sub(began)
	}
	return ""
}

// stopWatches closes every watcher (quit).
func (e *Engine) stopWatchesLocked() {
	l := e.live()
	for _, lf := range l.folders {
		e.stopWatchLocked(lf)
	}
	if l.running {
		l.running = false
		close(l.stop)
	}
}
