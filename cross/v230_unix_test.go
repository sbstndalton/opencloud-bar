//go:build !windows

package main

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SyncFolder queues one folder only; the others stay out of the run.
func TestSyncFolderQueuesOnlyThatKey(t *testing.T) {
	e, fr := parRig(t, 4)
	release := hold(fr)
	if err := e.SyncFolder("Nope"); err == nil {
		t.Error("unknown folder accepted")
	}
	if err := e.SyncFolder("B"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(bisyncs(fr)) == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := bisyncs(fr); len(got) != 1 || got[0] != "B" {
		t.Fatalf("synced %v, want only B", got)
	}
	release()
	waitFor(t, func() bool { return idle(e) })
	if got := bisyncs(fr); len(got) != 1 {
		t.Errorf("after the run: %v", got)
	}
	// A folder that waits shows as queued in the page's state.
	e.mu.Lock()
	e.queueLocked("A")
	e.mu.Unlock()
	for _, f := range e.uiState()["folders"].([]uiFolder) {
		if want := f.Path == "A"; f.Queued != want {
			t.Errorf("%s: queued=%v", f.Path, f.Queued)
		}
	}
	e.mu.Lock()
	e.queued = nil
	e.mu.Unlock()
	// Not while paused.
	if err := e.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	fr.reset()
	if err := e.SyncFolder("A"); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("paused: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := bisyncs(fr); len(got) != 0 {
		t.Errorf("synced while paused: %v", got)
	}
}

// uiRigOn serves the settings page for an existing engine.
func uiRigOn(t *testing.T, e *Engine) *uiRig {
	t.Helper()
	ui := newWebUI()
	srv := httptest.NewServer(apiHandler(e, func() {}, "tok", ui))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	var port int
	fmt.Sscanf(u.Port(), "%d", &port)
	ui.setPort(port)
	jar, _ := cookiejar.New(nil)
	r := &uiRig{t: t, e: e, ui: ui, srv: srv, port: port, clock: time.Now(),
		client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	ui.now = func() time.Time { return r.clock }
	r.login()
	return r
}

// Through the settings page's API: a known path syncs only it, an unknown one is a 400.
func TestUISyncOnePath(t *testing.T) {
	e, fr := parRig(t, 4)
	r := uiRigOn(t, e)
	if resp, body := r.post("/ui/api/sync", `{"path":"Nope"}`); resp.StatusCode != 400 {
		t.Fatalf("unknown: %d %s", resp.StatusCode, body)
	}
	if resp, body := r.post("/ui/api/sync", `{"path":"C"}`); resp.StatusCode != 200 {
		t.Fatalf("C: %d %s", resp.StatusCode, body)
	}
	waitFor(t, func() bool { return len(bisyncs(fr)) >= 1 && idle(e) })
	if got := bisyncs(fr); len(got) != 1 || got[0] != "C" {
		t.Errorf("synced %v, want only C", got)
	}
}

// A conflict action chosen while a sync runs waits for it, then re-checks the copy.
func TestConflictActionWaitsForRunningSync(t *testing.T) {
	e, _, f := syncRig(t, false)
	cfg := e.config()
	local := cfg.Local(f)
	now := time.Now()
	writeFile(t, filepath.Join(local, "a.txt"), "winner", now)
	writeFile(t, filepath.Join(local, "a.txt.conflict1"), "loser", now.Add(-time.Minute))
	writeFile(t, filepath.Join(local, "b.txt.conflict1"), "discard me", now)
	e.updateConflicts(cfg, f)
	_ = os.MkdirAll(filepath.Join(home(), ".Trash"), 0o700) // macOS test runs; Linux uses XDG
	endLane := func() {                                     // the fake running lane ends
		e.mu.Lock()
		e.lanes = 0
		e.busyLocked()
		e.mu.Unlock()
		e.runsIdle()
	}
	startLane := func() {
		e.mu.Lock()
		e.lanes = 1
		e.busyLocked()
		e.mu.Unlock()
	}

	startLane()
	if q, err := e.ConflictAction("Docs", "b.txt.conflict1", "discard"); err != nil || !q {
		t.Fatalf("not queued: queued=%v %v", q, err)
	}
	e.mu.Lock()
	queued := len(e.exclQ)
	e.mu.Unlock()
	if queued != 1 || !exists(filepath.Join(local, "b.txt.conflict1")) {
		t.Fatalf("ran during the sync: queued=%d", queued)
	}
	// A second action meanwhile is refused (one waits at a time).
	if _, err := e.ConflictAction("Docs", "a.txt.conflict1", "keep"); err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Errorf("second action: %v", err)
	}
	endLane()
	waitFor(t, func() bool { return !exists(filepath.Join(local, "b.txt.conflict1")) })
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })

	// The copy was synced away while it waited: the action fails, nothing else changes.
	startLane()
	e.setMessage("")
	if q, err := e.ConflictAction("Docs", "a.txt.conflict1", "keep"); err != nil || !q {
		t.Fatalf("queued=%v %v", q, err)
	}
	_ = os.Remove(filepath.Join(local, "a.txt.conflict1"))
	endLane()
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })
	if b, _ := os.ReadFile(filepath.Join(local, "a.txt")); string(b) != "winner" {
		t.Errorf("a.txt is %q", b)
	}
	// It failed, and the failure was reported (not silent).
	if msg := e.Status().Message; !strings.Contains(msg, "a.txt.conflict1") || !strings.Contains(msg, "Couldn't keep") {
		t.Errorf("failure not reported: message %q", msg)
	}
}

// laneHooks fakes a running sync: start holds the engine busy, end releases it.
func laneHooks(e *Engine) (start, end func()) {
	start = func() {
		e.mu.Lock()
		e.lanes = 1
		e.busyLocked()
		e.mu.Unlock()
	}
	end = func() {
		e.mu.Lock()
		e.lanes = 0
		e.busyLocked()
		e.mu.Unlock()
		e.runsIdle()
	}
	return
}

// needRepair marks folders as needing a repair (the state file the app writes).
func needRepair(t *testing.T, e *Engine, keys ...string) {
	t.Helper()
	for _, k := range keys {
		f, ok := e.find(k)
		if !ok {
			t.Fatalf("no folder %q", k)
		}
		if err := os.WriteFile(e.stateFile(f, ".needs-repair"), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// SyncFolder during the timer's batch really starts that folder, ahead of the batch's waiting ones.
func TestSyncFolderDuringTimerBatch(t *testing.T) {
	e, fr := parRig(t, 1)
	release := hold(fr)
	e.mu.Lock()
	e.startSyncAllLocked(false) // the timer: A runs, B and C wait
	e.mu.Unlock()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 1 })
	e.mu.Lock()
	on := e.timerOn
	e.mu.Unlock()
	if !on {
		t.Fatal("the timer batch isn't running")
	}
	if err := e.SyncFolder("C"); err != nil {
		t.Fatalf("refused during the timer batch: %v", err)
	}
	release()
	waitFor(t, func() bool { return len(bisyncs(fr)) == 3 && idle(e) })
	if got := strings.Join(bisyncs(fr), ","); got != "A,C,B" {
		t.Errorf("order %s, want A,C,B (C ahead of the batch's B)", got)
	}
}

// SyncFolder says why it can't start: paused (also through the control API), or an action waits.
func TestSyncFolderRefusals(t *testing.T) {
	e, fr := parRig(t, 2)
	if err := e.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if err := e.SyncFolder("A"); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Errorf("paused: %v", err)
	}
	srv := httptest.NewServer(apiHandler(e, func() {}, "tok", newWebUI()))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("POST", srv.URL+"/sync?path=A", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 { // the CLI turns this into a non-zero exit
		t.Errorf("control API while paused: %d", resp.StatusCode)
	}
	if err := e.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	start, end := laneHooks(e)
	start()
	needRepair(t, e, "B")
	if q, err := e.Repair("B"); err != nil || !q {
		t.Fatalf("queue the repair: %v %v", q, err)
	}
	if err := e.SyncFolder("A"); err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Errorf("action waiting: %v", err)
	}
	end()
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })
	_ = fr
}

// A queued Allow Large Deletion whose folder changed meanwhile aborts and never forces.
func TestQueuedAllowBigDeletionChanged(t *testing.T) {
	e, fr, f := syncRig(t, false)
	_ = os.WriteFile(filepath.Join(e.cfg.Local(f), "keep.txt"), []byte("x"), 0o600)
	setBig := func(side string) {
		e.mu.Lock()
		e.status[f.Key()].State, e.status[f.Key()].Detail = StBigDeletion, side
		e.status[f.Key()].BlockGen++
		e.mu.Unlock()
	}
	gen := func() int { _, g, _ := e.bigDeletionDetail(f.Key()); return g }
	setBig("on this computer")
	if _, err := e.AllowBigDeletion("Docs", "in OpenCloud", gen()); err == nil {
		t.Error("a different approval was accepted")
	}
	start, end := laneHooks(e)
	start()
	if q, err := e.AllowBigDeletion("Docs", "on this computer", gen()); err != nil || !q {
		t.Fatalf("queued=%v %v", q, err)
	}
	setBig("in OpenCloud") // the deletion is now on the other side
	fr.reset()
	end()
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })
	if strings.Contains(fr.calls(), "--force") || strings.Contains(fr.calls(), "bisync") {
		t.Errorf("ran after the folder changed: %s", fr.calls())
	}
	if msg := e.Status().Message; msg != changedWhileWaiting {
		t.Errorf("message %q", msg)
	}
	// No longer blocked at all: the same.
	setBig("on this computer")
	start()
	if _, err := e.AllowBigDeletion("Docs", "on this computer", gen()); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.status[f.Key()].State, e.status[f.Key()].Detail = StOK, ""
	e.mu.Unlock()
	e.setMessage("")
	end()
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })
	if strings.Contains(fr.calls(), "bisync") || e.Status().Message != changedWhileWaiting {
		t.Errorf("ran when no longer blocked: %s / %q", fr.calls(), e.Status().Message)
	}
}

// A queued Repair of a folder that is healthy by the time it runs is aborted.
func TestQueuedRepairNowHealthy(t *testing.T) {
	e, fr, f := syncRig(t, false)
	needRepair(t, e, "Docs")
	start, end := laneHooks(e)
	start()
	if q, err := e.Repair("Docs"); err != nil || !q {
		t.Fatalf("queued=%v %v", q, err)
	}
	_ = os.Remove(e.stateFile(f, ".needs-repair")) // repaired meanwhile
	fr.reset()
	end()
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })
	if strings.Contains(fr.calls(), "bisync") {
		t.Errorf("repair ran on a healthy folder: %s", fr.calls())
	}
	if msg := e.Status().Message; !strings.Contains(msg, "no longer needs a repair") {
		t.Errorf("message %q", msg)
	}
}

// The settings page's timed pause has the CLI's limits: 1 minute to 30 days.
func TestUIPauseLimits(t *testing.T) {
	r := newUIRig(t)
	r.login()
	for name, d := range map[string]time.Duration{"31 days": 31 * 24 * time.Hour, "30 seconds": 30 * time.Second} {
		until := time.Now().Add(d).UTC().Format(time.RFC3339)
		if resp, body := r.post("/ui/api/pause", `{"paused":true,"until":"`+until+`"}`); resp.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if r.e.Status().Paused {
		t.Error("paused after a refused request")
	}
}

// Actions that may wait answer {"queued": true|false}.
func TestUIActionsReportQueued(t *testing.T) {
	e, _ := parRig(t, 2)
	r := uiRigOn(t, e)
	start, end := laneHooks(e)
	needRepair(t, e, "A", "B")
	e.mu.Lock()
	e.status["C"].State, e.status["C"].Detail, e.status["C"].BlockGen = StBigDeletion, "in OpenCloud", 3
	e.mu.Unlock()

	if resp, body := r.post("/ui/api/allow-delete", `{"path":"C"}`); resp.StatusCode != 400 {
		t.Errorf("allow-delete without the approved side: %d %s", resp.StatusCode, body)
	}
	// Nothing runs: not queued.
	resp, body := r.post("/ui/api/repair", `{"path":"A"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"queued":false`) {
		t.Errorf("idle repair: %d %s", resp.StatusCode, body)
	}
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })

	start()
	resp, body = r.post("/ui/api/repair", `{"path":"B"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"queued":true`) {
		t.Errorf("queued repair: %d %s", resp.StatusCode, body)
	}
	end()
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })

	start()
	if resp, body := r.post("/ui/api/allow-delete", `{"path":"C","side":"in OpenCloud"}`); resp.StatusCode != 400 {
		t.Errorf("allow-delete without gen: %d %s", resp.StatusCode, body)
	}
	resp, body = r.post("/ui/api/allow-delete", `{"path":"C","side":"in OpenCloud","gen":3}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"queued":true`) {
		t.Errorf("queued allow-delete: %d %s", resp.StatusCode, body)
	}
	end()
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })
}

// A block of the same side that was entered again (a different deletion set) while the Allow
// waited is not what the user approved: the Allow aborts and never forces.
func TestQueuedAllowBigDeletionSameSideReblocked(t *testing.T) {
	e, fr, f := syncRig(t, false)
	_ = os.WriteFile(filepath.Join(e.cfg.Local(f), "keep.txt"), []byte("x"), 0o600)
	e.setState(f, StBigDeletion, "on this computer")
	_, gen, _ := e.bigDeletionDetail(f.Key())
	start, end := laneHooks(e)
	start()
	if q, err := e.AllowBigDeletion("Docs", "on this computer", gen); err != nil || !q {
		t.Fatalf("queued=%v %v", q, err)
	}
	e.setState(f, StBigDeletion, "on this computer") // blocked again, same side
	if _, g2, _ := e.bigDeletionDetail(f.Key()); g2 == gen {
		t.Fatal("generation didn't change")
	}
	fr.reset()
	end()
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })
	if strings.Contains(fr.calls(), "--force") || strings.Contains(fr.calls(), "bisync") {
		t.Errorf("ran after the folder was blocked again: %s", fr.calls())
	}
	if msg := e.Status().Message; msg != changedWhileWaiting {
		t.Errorf("message %q", msg)
	}
}
