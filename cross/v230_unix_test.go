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
	if err := e.SyncFolder("A"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := bisyncs(fr); len(got) != 0 {
		t.Errorf("synced while paused: %v", got)
	}
}

// Through the settings page's API: a known path syncs only it, an unknown one is a 400.
func TestUISyncOnePath(t *testing.T) {
	e, fr := parRig(t, 4)
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
	if err := e.ConflictAction("Docs", "b.txt.conflict1", "discard"); err != nil {
		t.Fatalf("not queued: %v", err)
	}
	e.mu.Lock()
	queued := len(e.exclQ)
	e.mu.Unlock()
	if queued != 1 || !exists(filepath.Join(local, "b.txt.conflict1")) {
		t.Fatalf("ran during the sync: queued=%d", queued)
	}
	// A second action meanwhile is refused (one waits at a time).
	if err := e.ConflictAction("Docs", "a.txt.conflict1", "keep"); err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Errorf("second action: %v", err)
	}
	endLane()
	waitFor(t, func() bool { return !exists(filepath.Join(local, "b.txt.conflict1")) })
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })

	// The copy was synced away while it waited: the action fails, nothing else changes.
	startLane()
	if err := e.ConflictAction("Docs", "a.txt.conflict1", "keep"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(local, "a.txt.conflict1"))
	endLane()
	waitFor(t, func() bool { return idle(e) && !e.Status().Syncing })
	if b, _ := os.ReadFile(filepath.Join(local, "a.txt")); string(b) != "winner" {
		t.Errorf("a.txt is %q", b)
	}
}
