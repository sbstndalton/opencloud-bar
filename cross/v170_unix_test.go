//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parRig: three initialised kept folders A, B, C and a fake rclone.
func parRig(t *testing.T, parallel int) (*Engine, *fakeRclone) {
	t.Helper()
	fr := newFakeRclone(t)
	e := pausedRig(t, `,"rclonePath":"`+fr.path()+`","kept":[{"path":"A","stateName":"a"},{"path":"B","stateName":"b"},{"path":"C","stateName":"c"}]`)
	_ = os.MkdirAll(expand(e.cfg.StateDir), 0o700)
	for _, f := range e.cfg.Kept {
		_ = os.WriteFile(e.cfg.statePath(f, ".initialised"), nil, 0o600)
		_ = os.MkdirAll(e.cfg.Local(f), 0o700)
	}
	e.mu.Lock()
	e.cfg.Paused = false
	e.cfg.SyncParallel = parallel
	e.mu.Unlock()
	return e, fr
}

func hold(fr *fakeRclone) func() {
	p := filepath.Join(fr.dir, "hold")
	_ = os.WriteFile(p, nil, 0o600)
	return func() { _ = os.Remove(p) }
}

// bisyncs lists the folders of the bisync calls so far, in order.
func bisyncs(fr *fakeRclone) []string {
	var out []string
	for _, l := range strings.Split(fr.calls(), "\n") {
		if !strings.HasPrefix(l, "bisync ") {
			continue
		}
		for _, k := range []string{"A", "B", "C"} {
			if strings.Contains(l, "/"+k+" ") {
				s := k
				if strings.Contains(l, "--resync") {
					s += "*"
				}
				out = append(out, s)
			}
		}
	}
	return out
}

func idle(e *Engine) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.syncing && len(e.work) == 0 && len(e.queued) == 0 && len(e.exclQ) == 0
}

func TestParallelCap(t *testing.T) {
	e, fr := parRig(t, 2)
	release := hold(fr)
	e.mu.Lock()
	e.startSyncAllLocked(false)
	e.mu.Unlock()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 2 })
	time.Sleep(300 * time.Millisecond)
	if n := len(bisyncs(fr)); n != 2 {
		t.Fatalf("%d folders at once, cap 2", n)
	}
	if s := e.Status().Summary; s != "Syncing 2 folders…" {
		t.Errorf("summary %q", s)
	}
	release()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 3 && idle(e) })
	// One at a time with the setting at 1.
	e2, fr2 := parRig(t, 1)
	release2 := hold(fr2)
	e2.mu.Lock()
	e2.startSyncAllLocked(false)
	e2.mu.Unlock()
	waitFor(t, func() bool { return len(bisyncs(fr2)) == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(bisyncs(fr2)); n != 1 {
		t.Fatalf("%d folders at once, cap 1", n)
	}
	if s := e2.Status().Summary; !strings.HasPrefix(s, "Syncing “") || !strings.Contains(s, "(1 of 3)") {
		t.Errorf("summary %q", s)
	}
	release2()
	waitFor(t, func() bool { return len(bisyncs(fr2)) == 3 && idle(e2) })
}

// The same folder never syncs twice at once: a change queued while it syncs runs after.
func TestParallelSameFolderNeverTwice(t *testing.T) {
	e, fr := parRig(t, 4)
	release := hold(fr)
	e.mu.Lock()
	e.startRunLocked(false, map[string]bool{"A": true})
	e.mu.Unlock()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 1 })
	e.mu.Lock()
	e.queueLocked("A")
	e.startQueuedLocked()
	e.mu.Unlock()
	time.Sleep(400 * time.Millisecond)
	if got := bisyncs(fr); len(got) != 1 {
		t.Fatalf("A twice at once: %v", got)
	}
	release()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 2 && idle(e) })
	// The timer doesn't add a folder that syncs now.
	release = hold(fr)
	fr.reset()
	e.mu.Lock()
	e.startRunLocked(false, map[string]bool{"A": true})
	e.mu.Unlock()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 1 })
	e.mu.Lock()
	e.timerOn = false
	e.startSyncAllLocked(false)
	e.mu.Unlock()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 3 }) // B and C
	release()
	waitFor(t, func() bool { return idle(e) })
	if got := strings.Join(bisyncs(fr), ","); strings.Count(got, "A") != 1 {
		t.Errorf("A synced again by the timer: %s", got)
	}
}

// A folder with a change goes before the timer's folders.
func TestParallelLiveFirst(t *testing.T) {
	e, fr := parRig(t, 1)
	release := hold(fr)
	e.mu.Lock()
	e.startSyncAllLocked(false) // A runs; B, C wait
	e.mu.Unlock()
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(bisyncs(fr)) == 1 && len(e.work) == 2
	})
	e.mu.Lock()
	e.queueLocked("C") // live sync: C changed
	e.startQueuedLocked()
	e.mu.Unlock()
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, it := range e.work {
			if it.key == "C" && it.prio {
				return true
			}
		}
		return false
	})
	release()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 3 && idle(e) })
	if got := strings.Join(bisyncs(fr), ","); got != "A,C,B" {
		t.Errorf("order %s, want A,C,B", got)
	}
}

// Repair waits for every running sync, and nothing new starts meanwhile.
func TestParallelExclusiveWaits(t *testing.T) {
	e, fr := parRig(t, 2)
	release := hold(fr)
	e.mu.Lock()
	e.startSyncAllLocked(false) // A, B run; C waits
	e.mu.Unlock()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 2 })
	if err := e.Repair("C"); err != nil {
		t.Fatalf("repair while syncing: %v", err)
	}
	if err := e.Repair("A"); err == nil {
		t.Error("a second waiting action accepted")
	}
	time.Sleep(300 * time.Millisecond)
	if got := bisyncs(fr); len(got) != 2 {
		t.Fatalf("started while an action waits: %v", got)
	}
	release()
	waitFor(t, func() bool { return idle(e) && len(bisyncs(fr)) >= 3 })
	got := bisyncs(fr)
	if len(got) < 3 || got[2] != "C*" {
		t.Fatalf("calls %v: the repair (C*) must come right after A and B", got)
	}
}

func TestPacing(t *testing.T) {
	iv := 5 * time.Minute
	for _, c := range []struct{ dur, want time.Duration }{
		{time.Minute, iv},
		{150 * time.Second, iv}, // exactly half: not paced
		{200 * time.Second, 400 * time.Second},
		{4 * time.Minute, 8 * time.Minute},
		{20 * time.Minute, 15 * time.Minute}, // 3 × the interval
	} {
		if got := pacedInterval(c.dur, iv); got != c.want {
			t.Errorf("pacedInterval(%v, %v) = %v, want %v", c.dur, iv, got, c.want)
		}
	}
	if got := pacedInterval(25*time.Minute, time.Hour); got != time.Hour {
		t.Errorf("long interval: %v", got)
	}
	if got := pacedInterval(50*time.Minute, time.Hour); got != time.Hour { // min(100, 30, 180) < interval
		t.Errorf("never under the interval: %v", got)
	}
	if got := pacedInterval(20*time.Minute, 15*time.Minute); got != 30*time.Minute {
		t.Errorf("30 min cap: %v", got)
	}
	// A big folder is skipped by the timer until its paced time; Sync Now still syncs it.
	e, fr := parRig(t, 2)
	e.mu.Lock()
	e.cfg.SyncIntervalSeconds = 300
	e.runTimes["A"] = runTimes{start: time.Now(), dur: 4 * time.Minute}
	e.startSyncAllLocked(false)
	e.mu.Unlock()
	waitFor(t, func() bool { return idle(e) && len(bisyncs(fr)) == 2 })
	if got := strings.Join(bisyncs(fr), ","); strings.Contains(got, "A") {
		t.Errorf("paced folder synced by the timer: %s", got)
	}
	e.SyncNow()
	waitFor(t, func() bool { return idle(e) && strings.Contains(strings.Join(bisyncs(fr), ","), "A") })
	// The detail says so after a long run.
	e.mu.Lock()
	e.status["B"].State = StOK
	e.noteRunLocked(e.cfg.Kept[1], time.Now().Add(-4*time.Minute), true)
	d := e.status["B"].Detail
	e.mu.Unlock()
	if d != pacedText {
		t.Errorf("detail %q", d)
	}
}

// A run that takes a waiting change says so, whatever started it.
func TestTriggerCoalescing(t *testing.T) {
	e, clk, f := liveRig(t)
	e.localEvent("Docs", "deleted.txt") // not settled yet: the timer's run takes it
	e.mu.Lock()
	tr := e.ownSyncLocked(f.Key(), true, clk.now())
	e.ownSyncLocked(f.Key(), false, clk.now())
	e.mu.Unlock()
	if tr != "local change" {
		t.Errorf("local: %q", tr)
	}
	e.mu.Lock()
	e.serverChangeLocked("Docs")
	tr = e.ownSyncLocked(f.Key(), true, clk.now())
	e.ownSyncLocked(f.Key(), false, clk.now())
	e.mu.Unlock()
	if tr != "server change" {
		t.Errorf("server: %q", tr)
	}
	clk.add(3 * time.Second) // past the last run's tail
	e.localEvent("Docs", "x.txt")
	e.mu.Lock()
	e.serverChangeLocked("Docs")
	tr = e.ownSyncLocked(f.Key(), true, clk.now())
	e.ownSyncLocked(f.Key(), false, clk.now())
	tr2 := e.ownSyncLocked(f.Key(), true, clk.now())
	e.mu.Unlock()
	if tr != "local change" || tr2 != "timer" {
		t.Errorf("both: %q, then nothing waiting: %q", tr, tr2)
	}
}
