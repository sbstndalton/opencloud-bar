package main

import (
	"fmt"
	"time"
)

// Parallel sync (v1.7): kept folders sync in up to Config.Parallel() lanes at once (setting
// syncParallel, 1–4, default 2: the server stays light). Folders wait in e.work; a lane takes
// the next one that isn't running, live-sync and newly kept folders first, so a small folder
// with a change doesn't wait behind a long timer run of a big one.
//
//   - Never the same folder twice at once (e.running); kept folders never overlap on disk (the
//     settings refuse that).
//   - Exclusive actions (Repair, allow a large deletion, Stop Keeping) wait: no lane takes a
//     new folder while one waits, and it starts when the last run ends. Conflict actions and
//     Reload still refuse while anything runs.
//   - The server memory check gates every batch of starts (timer, Sync Now, queued folders).
//   - Pacing: a folder whose last run took more than half the interval syncs on the timer
//     only every pacedInterval (live sync and Sync Now still sync it).
//   - e.syncing is "something runs": lanes, a batch waiting on the memory check, or an
//     exclusive action.

type workItem struct {
	key    string
	manual bool // Sync Now: a blocked large deletion is tried again; pacing doesn't apply
	prio   bool // live sync, a newly kept folder: before the timer's folders
	timer  bool // part of a timer (or Sync Now) batch
}

type activeRun struct {
	name  string
	files int
}

type runTimes struct {
	start time.Time
	dur   time.Duration
}

const (
	defaultParallel = 2
	maxParallel     = 4
	paceMax         = 30 * time.Minute
)

// Parallel is how many folders sync at once.
func (c Config) Parallel() int {
	if c.SyncParallel <= 0 {
		return defaultParallel
	}
	return min(c.SyncParallel, maxParallel)
}

func (c Config) interval() time.Duration {
	return time.Duration(max(60, c.SyncIntervalSeconds)) * time.Second
}

// pacedInterval: a folder whose last run took more than half the interval syncs on the timer
// at most every min(2 × that run, 30 minutes, 3 × the interval), never more often than the
// interval.
func pacedInterval(lastDur, interval time.Duration) time.Duration {
	if lastDur <= interval/2 {
		return interval
	}
	return max(interval, min(2*lastDur, paceMax, 3*interval))
}

const pacedText = "syncs less often: large folder"

// Live sync makes the timer a safety net (2.0): a folder that live sync covers (watched here,
// checked in OpenCloud) is synced by the timer at most every safetyInterval.
const (
	safetyInterval = 15 * time.Minute
	largeRun       = 2 * time.Minute  // a run longer than this: a large folder for live sync
	largeLiveGap   = 10 * time.Minute // its live-triggered runs at least this far apart
)

// timerInterval: how often the timer syncs a folder. live: live sync covers it.
func timerInterval(interval, lastDur time.Duration, live bool) time.Duration {
	base := interval
	if live {
		base = max(interval, safetyInterval)
	}
	return max(base, pacedInterval(lastDur, interval))
}

// liveGap: the least time between live-triggered runs of a folder whose last run took lastDur.
// A large folder (over largeRun) gets max(largeLiveGap, 2 × that run), so constant changes
// don't keep the server busy.
func liveGap(lastDur time.Duration) time.Duration {
	if lastDur > largeRun {
		return max(largeLiveGap, 2*lastDur)
	}
	return max(liveMinGap, lastDur)
}

// liveCoversLocked: live sync covers f: it is on, the folder is watched here, and OpenCloud
// is checked for changes (the browser sign-in). Folders that fell back keep the interval.
func (e *Engine) liveCoversLocked(f KeptFolder) bool {
	if !e.cfg.LiveOn() || !e.cfg.OIDC() || e.liveS == nil {
		return false
	}
	lf := e.liveS.folders[f.Key()]
	return lf != nil && lf.watch != nil && lf.fallback == ""
}

// pacedOutLocked: the timer skips f this time (pacing, live sync's safety net). The caller
// holds e.mu.
func (e *Engine) pacedOutLocked(f KeptFolder, now time.Time) bool {
	rt, ok := e.runTimes[f.Key()]
	if !ok || rt.start.IsZero() {
		return false
	}
	iv := e.cfg.interval()
	p := timerInterval(iv, rt.dur, e.liveCoversLocked(f))
	slack := min(time.Minute, iv/10) // the timer ticks every interval: a run started just after a tick
	return p > iv && now.Add(slack).Before(rt.start.Add(p))
}

// scheduleNoteLocked is a synced folder's detail about how often it syncs ("" for none).
func (e *Engine) scheduleNoteLocked(f KeptFolder, dur time.Duration) string {
	iv := e.cfg.interval()
	live := e.liveCoversLocked(f)
	switch {
	case e.cfg.LiveOn() && dur > largeRun:
		return fmt.Sprintf("large folder: syncs at most every %d min", minutes(liveGap(dur)))
	case pacedInterval(dur, iv) > iv:
		return pacedText
	case live:
		return fmt.Sprintf("Checked every %d min; changes sync right away", minutes(timerInterval(iv, dur, true)))
	}
	return ""
}

func minutes(d time.Duration) int { return int((d + time.Minute - 1) / time.Minute) }

func (e *Engine) busyLocked() { e.syncing = e.lanes > 0 || e.exclOn || e.starting > 0 }

func (e *Engine) idleLocked() bool { return e.lanes == 0 && e.starting == 0 && !e.exclOn }

// timerCheckLocked: the timer batch is over when none of its folders waits or runs.
func (e *Engine) timerCheckLocked() {
	if e.timerStarting {
		return
	}
	for _, it := range e.work {
		if it.timer {
			return
		}
	}
	for _, it := range e.running {
		if it.timer {
			return
		}
	}
	e.timerOn = false
}

// startRunLocked syncs every kept folder (the timer, Sync Now: only nil), or the folders in
// only (queued: kept meanwhile, live sync) first. The caller holds e.mu.
func (e *Engine) startRunLocked(manual bool, only map[string]bool) {
	now := time.Now()
	var folders []KeptFolder
	for _, f := range e.cfg.Kept {
		if only != nil && !only[f.Key()] {
			continue
		}
		if only == nil && !manual && e.pacedOutLocked(f, now) {
			continue
		}
		folders = append(folders, f)
	}
	timer := only == nil
	if timer {
		e.nextSyncAt = now.Add(e.cfg.interval())
	}
	if len(folders) == 0 {
		return
	}
	if timer {
		e.timerOn, e.timerStarting = true, true
	}
	e.starting++
	if e.startingKeys == nil {
		e.startingKeys = map[string]int{}
	}
	for _, f := range folders {
		e.startingKeys[f.Key()]++
	}
	e.busyLocked()
	go func() {
		reason := e.serverTooBusy()
		e.mu.Lock()
		e.starting--
		for _, f := range folders {
			if e.startingKeys[f.Key()]--; e.startingKeys[f.Key()] <= 0 {
				delete(e.startingKeys, f.Key())
			}
		}
		if timer {
			e.timerStarting = false
		}
		if reason == "" && !e.quitting {
			for _, f := range folders {
				e.enqueueLocked(workItem{key: f.Key(), manual: manual, prio: !timer, timer: timer})
			}
		}
		e.timerCheckLocked()
		e.dispatchLocked()
		e.busyLocked()
		idle := e.idleLocked()
		e.mu.Unlock()
		if reason != "" {
			for _, f := range folders {
				e.setState(f, StSkipped, reason)
			}
		}
		if idle {
			e.runsIdle()
		}
	}()
}

// enqueueLocked adds a folder to the waiting list, or merges it with its waiting entry.
func (e *Engine) enqueueLocked(it workItem) {
	if _, ok := e.running[it.key]; ok && !it.prio {
		return // the timer: the run going on now covers it
	}
	for i := range e.work {
		if e.work[i].key == it.key {
			e.work[i].prio = e.work[i].prio || it.prio
			e.work[i].manual = e.work[i].manual || it.manual
			e.work[i].timer = e.work[i].timer || it.timer
			return
		}
	}
	e.work = append(e.work, it)
	e.batchN++
}

// waitingLocked: the folder waits to sync (queued, or in the waiting list).
func (e *Engine) waitingLocked(key string) bool {
	if e.queued[key] || e.startingKeys[key] > 0 {
		return true
	}
	for _, it := range e.work {
		if it.key == key {
			return true
		}
	}
	return false
}

// unqueueLocked: a sync of the folder starts, so what waited for it is done by it.
func (e *Engine) unqueueLocked(key string) {
	delete(e.queued, key)
	for i := 0; i < len(e.work); i++ {
		if e.work[i].key == key {
			e.work = append(e.work[:i], e.work[i+1:]...)
			e.batchDone++ // counted as done with this run
			i--
		}
	}
}

func (e *Engine) canStartLocked() bool {
	// Settings saved during a run wait for all runs to end (Reload), then apply to what follows.
	return !e.holdRuns && !e.quitting && !e.exclOn && len(e.exclQ) == 0 && !e.cfg.Paused &&
		e.cfgErr == nil && !e.signedOut && !reloadRetrying.Load()
}

// pickLocked takes the next folder to sync: live/new ones first, then the timer's, skipping
// folders that sync now.
func (e *Engine) pickLocked() (workItem, bool) {
	if !e.canStartLocked() {
		if e.cfg.Paused || e.quitting || e.signedOut {
			e.work = nil // they sync again after resuming (the timer, live sync's pending marks)
		}
		return workItem{}, false
	}
	for _, wantPrio := range []bool{true, false} {
		for i, it := range e.work {
			if it.prio == wantPrio && !e.isRunningLocked(it.key) {
				e.work = append(e.work[:i], e.work[i+1:]...)
				return it, true
			}
		}
	}
	return workItem{}, false
}

func (e *Engine) isRunningLocked(key string) bool {
	_, ok := e.running[key]
	return ok
}

// dispatchLocked starts lanes for the folders that can start now.
func (e *Engine) dispatchLocked() {
	if !e.canStartLocked() {
		return
	}
	avail, seen := 0, map[string]bool{}
	for _, it := range e.work {
		if !e.isRunningLocked(it.key) && !seen[it.key] {
			seen[it.key] = true
			avail++
		}
	}
	for e.lanes < e.cfg.Parallel() && avail > 0 {
		e.lanes++
		avail--
		go e.lane()
	}
	e.busyLocked()
}

// lane syncs waiting folders one after another until none is left for it.
func (e *Engine) lane() {
	for {
		e.mu.Lock()
		it, ok := e.pickLocked()
		if !ok {
			e.lanes--
			e.timerCheckLocked()
			e.busyLocked()
			idle := e.idleLocked()
			e.mu.Unlock()
			if idle {
				e.runsIdle()
			}
			e.notify()
			return
		}
		var f KeptFolder
		found := false
		for _, k := range e.cfg.Kept {
			if k.Key() == it.key {
				f, found = k, true
			}
		}
		state := ""
		if s := e.status[it.key]; s != nil {
			state = s.State
		}
		e.running[it.key] = it
		e.mu.Unlock()
		if found {
			e.runItem(f, it, state)
		}
		e.mu.Lock()
		delete(e.running, it.key)
		e.batchDone++
		e.timerCheckLocked()
		e.mu.Unlock()
		e.notify()
	}
}

// runItem syncs one folder of a batch (what the sequential run did for each folder).
func (e *Engine) runItem(f KeptFolder, it workItem, state string) {
	if r := e.needsRepairReason(f); r != "" {
		if exists(e.stateFile(f, ".drift-parked")) {
			e.syncFolder(f, modeDriftRetry)
		} else {
			e.setState(f, StNeedsRepair, r)
		}
		return
	}
	if state == StBigDeletion && !it.manual {
		return
	}
	e.syncFolder(f, modeNormal)
}

// runsIdle: the last run ended. Settings saved meanwhile are applied, then a waiting
// exclusive action runs, else the folders queued meanwhile start.
func (e *Engine) runsIdle() {
	if reloadRetrying.Load() {
		if err := e.Reload(); err == nil {
			reloadRetrying.Store(false) // applied: the waiting folders may start
			e.logLine("settings applied after the sync finished")
		}
	}
	e.mu.Lock()
	if !e.idleLocked() {
		e.mu.Unlock()
		return
	}
	e.batchN, e.batchDone = 0, 0
	if len(e.exclQ) > 0 && !e.quitting {
		body := e.exclQ[0]
		e.exclQ = e.exclQ[1:]
		e.exclOn = true
		e.busyLocked()
		e.mu.Unlock()
		e.notify()
		go func() {
			defer e.exclEnd()
			body()
		}()
		return
	}
	e.startQueuedLocked()
	e.dispatchLocked() // folders that waited for the reload
	e.mu.Unlock()
	e.notify()
}

// exclusive runs body as the only sync activity: now, or when the running syncs end (no
// new folder starts meanwhile).
func (e *Engine) exclusive(body func()) error {
	e.mu.Lock()
	if e.exclOn || len(e.exclQ) > 0 {
		e.mu.Unlock()
		return fmt.Errorf("another action is waiting for the running sync; try again when it finishes")
	}
	if !e.idleLocked() {
		e.exclQ = append(e.exclQ, body)
		e.mu.Unlock()
		e.notify()
		return nil
	}
	e.exclOn = true
	e.busyLocked()
	e.mu.Unlock()
	go func() {
		defer e.exclEnd()
		body()
	}()
	return nil
}

func (e *Engine) exclEnd() {
	e.mu.Lock()
	e.exclOn = false
	e.busyLocked()
	e.mu.Unlock()
	e.runsIdle()
}

// syncFinished: kept for callers that ended an activity of their own.
func (e *Engine) syncFinished() { e.runsIdle() }

// startQueuedLocked starts the queued folders (kept meanwhile, live sync), ahead of the
// timer's, unless syncing is off or an exclusive action waits.
func (e *Engine) startQueuedLocked() {
	if len(e.queued) > 0 && e.canStartLocked() && e.cfg.Configured() {
		only := e.queued
		e.queued = nil
		e.startRunLocked(false, only)
	}
}

// queueLocked adds folders to the next queued run.
func (e *Engine) queueLocked(keys ...string) {
	if e.queued == nil {
		e.queued = map[string]bool{}
	}
	for _, k := range keys {
		e.queued[k] = true
	}
}

// activitySummaryLocked is the menu's first line while syncing.
func (e *Engine) activitySummaryLocked() string {
	switch len(e.active) {
	case 0:
		return "Syncing…"
	case 1:
		for _, a := range e.active {
			idx, n := e.batchDone+1, e.batchN
			if n <= 1 || idx > n {
				idx, n = 1, 1
			}
			return activityText(a.name, idx, n, a.files)
		}
	}
	files := 0
	for _, a := range e.active {
		files += a.files
	}
	s := fmt.Sprintf("Syncing %d folders", len(e.active))
	switch {
	case files == 1:
		s += " · 1 file"
	case files > 1:
		s += fmt.Sprintf(" · %d files", files)
	}
	return s + "…"
}

// noteRunLocked records a run's times (pacing) and says when a folder syncs less often.
func (e *Engine) noteRunLocked(f KeptFolder, start time.Time, ok bool) {
	dur := time.Since(start)
	e.runTimes[f.Key()] = runTimes{start: start, dur: dur}
	if s := e.status[f.Key()]; s != nil && ok && s.State == StOK {
		s.Detail = e.scheduleNoteLocked(f, dur)
	}
}
