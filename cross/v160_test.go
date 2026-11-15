package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDav answers PROPFIND on the kept folder's root with an ETag; it records what it got.
type fakeDav struct {
	srv    *httptest.Server
	etag   atomic.Value
	status atomic.Int64
	delay  atomic.Int64
	calls  atomic.Int64
	mu     sync.Mutex
	auth   []string
	paths  []string
}

func newFakeDav(t *testing.T) *fakeDav {
	d := &fakeDav{}
	d.etag.Store(`"e1"`)
	d.status.Store(207)
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.calls.Add(1)
		d.mu.Lock()
		d.auth = append(d.auth, r.Header.Get("Authorization"))
		d.paths = append(d.paths, r.Method+" "+r.URL.Path+" depth="+r.Header.Get("Depth"))
		d.mu.Unlock()
		if ms := d.delay.Load(); ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		if st := int(d.status.Load()); st != 207 {
			w.WriteHeader(st)
			return
		}
		w.WriteHeader(207)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>/x/</d:href>` +
			`<d:propstat><d:prop><d:getetag>` + d.etag.Load().(string) + `</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func remoteRig(t *testing.T) (*Engine, *fakeClock, *fakeDav) {
	t.Helper()
	e, clk, _ := liveRig(t)
	d := newFakeDav(t)
	e.mu.Lock()
	e.cfg.LiveSync = true // no liveLoop runs: the test drives remoteTick
	e.cfg.Auth = "oidc"
	e.cfg.ServerURL = d.srv.URL
	e.signedOut = false
	e.live().remoteToken = func(string) (string, error) { return "tok-123", nil }
	e.mu.Unlock()
	_ = os.MkdirAll(expand(e.cfg.StateDir), 0o700)
	return e, clk, d
}

// check runs the due checks and waits for them.
func check(t *testing.T, e *Engine) {
	t.Helper()
	e.remoteTick()
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.live().remoteRun == 0
	})
}

func remoteOf(e *Engine, key string) remoteFolder {
	e.mu.Lock()
	defer e.mu.Unlock()
	if rf := e.live().remote[key]; rf != nil {
		return *rf
	}
	return remoteFolder{}
}

func TestRemoteChangeQueuesOneSync(t *testing.T) {
	e, clk, d := remoteRig(t)
	check(t, e) // the first check after launch only records
	if queued(e, "Docs") || e.Status().Folders[0].Pending || remoteOf(e, "Docs").etag != `"e1"` {
		t.Fatalf("first check: %+v", remoteOf(e, "Docs"))
	}
	d.mu.Lock()
	if d.auth[0] != "Bearer tok-123" || d.paths[0] != "PROPFIND /remote.php/dav/files/sam/Docs depth=0" {
		t.Errorf("request %q %q", d.auth[0], d.paths[0])
	}
	d.mu.Unlock()
	clk.add(10 * time.Second)
	check(t, e) // not due yet
	if d.calls.Load() != 1 {
		t.Fatalf("checked again after 10 s: %d", d.calls.Load())
	}
	clk.add(30 * time.Second)
	check(t, e) // same ETag
	if d.calls.Load() != 2 || e.Status().Folders[0].Pending {
		t.Fatal("unchanged ETag: a sync")
	}
	d.etag.Store(`"e2"`)
	clk.add(40 * time.Second)
	check(t, e)
	if !e.Status().Folders[0].Pending {
		t.Fatal("changed ETag: not pending")
	}
	e.liveTick()
	if !queued(e, "Docs") {
		t.Fatal("changed ETag: no sync queued")
	}
	e.mu.Lock()
	trig := e.ownSyncLocked("Docs", true, clk.now())
	e.ownSyncLocked("Docs", false, clk.now())
	e.mu.Unlock()
	if trig != "server change" {
		t.Errorf("trigger %q", trig)
	}
	if b, _ := os.ReadFile(e.cfg.statePath(e.cfg.Kept[0], ".server-etag")); strings.TrimSpace(string(b)) != `"e2"` {
		t.Errorf("stored %q", b)
	}
	clearQueue(e)
	clk.add(40 * time.Second)
	check(t, e)
	e.liveTick()
	if queued(e, "Docs") {
		t.Fatal("synced twice for one change")
	}
	if l := ChangeLabel(Change{Name: "Docs", Path: "a.txt", Action: actDownloaded, Trigger: "server change"}); !strings.HasSuffix(l, "(after a change in OpenCloud)") {
		t.Errorf("label %q", l)
	}
}

// The app's own uploads change the ETag: the ETag read after the sync is the baseline.
func TestRemoteOwnRunBaseline(t *testing.T) {
	e, clk, d := remoteRig(t)
	check(t, e)
	f := e.cfg.Kept[0]
	e.mu.Lock()
	e.ownSyncLocked("Docs", true, clk.now())
	e.remoteSyncStartedLocked("Docs")
	e.mu.Unlock()
	clk.add(40 * time.Second)
	n := d.calls.Load()
	check(t, e) // during the sync: no check
	if d.calls.Load() != n {
		t.Fatal("checked during the app's own sync")
	}
	d.etag.Store(`"after-upload"`)
	e.mu.Lock()
	e.ownSyncLocked("Docs", false, clk.now())
	job := e.remoteSyncEndedLocked(f, true)
	e.mu.Unlock()
	if job == nil {
		t.Fatal("no baseline read")
	}
	e.remoteBaseline(*job)
	if rf := remoteOf(e, "Docs"); rf.etag != `"after-upload"` || rf.busy || rf.pending {
		t.Fatalf("baseline %+v", rf)
	}
	clk.add(40 * time.Second)
	check(t, e)
	if e.Status().Folders[0].Pending {
		t.Fatal("own upload started another sync")
	}
	// A check that was running when a sync started is ignored.
	e.mu.Lock()
	rf := e.live().remote["Docs"]
	j := remoteJob{key: "Docs", gen: rf.gen}
	rf.busy = true
	e.live().remoteRun++
	e.remoteSyncStartedLocked("Docs")
	e.mu.Unlock()
	e.remoteResult(j, "", `"stale"`, 207, nil)
	if e.Status().Folders[0].Pending || remoteOf(e, "Docs").etag != `"after-upload"` {
		t.Fatal("a stale check counted")
	}
}

// After a restart the stored ETag is the baseline; the first check doesn't sync.
func TestRemoteRestartNoSync(t *testing.T) {
	e, _, d := remoteRig(t)
	state := e.cfg.statePath(e.cfg.Kept[0], ".server-etag")
	_ = os.WriteFile(state, []byte(`"e1"`+"\n"), 0o600)
	st, _ := os.Stat(state)
	check(t, e)
	if e.Status().Folders[0].Pending || d.calls.Load() != 1 {
		t.Fatal("restart: a sync")
	}
	if st2, _ := os.Stat(state); !st2.ModTime().Equal(st.ModTime()) {
		t.Error("unchanged ETag written again")
	}
	if rf := remoteOf(e, "Docs"); !rf.known || rf.etag != `"e1"` {
		t.Errorf("baseline %+v", rf)
	}
}

func TestRemoteBackoff(t *testing.T) {
	e, clk, d := remoteRig(t)
	d.status.Store(500)
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		check(t, e)
		rf := remoteOf(e, "Docs")
		if got := rf.nextAt.Sub(clk.now()); rf.fails != i+1 || got != w {
			t.Fatalf("failure %d: next in %v, want %v (%+v)", i+1, got, w, rf)
		}
		n := d.calls.Load()
		clk.add(w - time.Second)
		check(t, e)
		if d.calls.Load() != n {
			t.Fatalf("checked during the backoff after failure %d", i+1)
		}
		clk.add(time.Second)
	}
	if e.Status().Folders[0].Pending || e.message != "" {
		t.Errorf("a failure notified or synced: %q", e.message)
	}
	// A timeout counts the same; an answer again resets it.
	old := remoteTimeout
	remoteTimeout = 50 * time.Millisecond
	defer func() { remoteTimeout = old }()
	d.status.Store(207)
	d.delay.Store(300)
	check(t, e)
	if rf := remoteOf(e, "Docs"); rf.fails != len(want)+1 {
		t.Fatalf("timeout: %+v", rf)
	}
	d.delay.Store(0)
	clk.add(5 * time.Minute)
	check(t, e)
	if rf := remoteOf(e, "Docs"); rf.fails != 0 || !rf.known {
		t.Fatalf("after recovering: %+v", rf)
	}
}

func TestRemoteNoChecksWhenOff(t *testing.T) {
	e, clk, d := remoteRig(t)
	for _, set := range []func(on bool){
		func(on bool) { e.cfg.Paused = on },
		func(on bool) { e.signedOut = on },
		func(on bool) { e.cfg.LiveSync = !on },
		func(on bool) {
			if on {
				e.cfg.Auth = "rclone"
			} else {
				e.cfg.Auth = "oidc"
			}
		},
	} {
		e.mu.Lock()
		set(true)
		e.mu.Unlock()
		clk.add(time.Hour)
		check(t, e)
		e.mu.Lock()
		set(false)
		e.mu.Unlock()
	}
	if n := d.calls.Load(); n != 0 {
		t.Fatalf("%d checks while paused, signed out, off or without the browser sign-in", n)
	}
	check(t, e)
	if d.calls.Load() != 1 {
		t.Fatal("no check when on")
	}
}

// A redirect isn't followed: the token never goes elsewhere.
func TestRemoteNoTokenOnRedirect(t *testing.T) {
	e, _, d := remoteRig(t)
	var got atomic.Value
	got.Store("")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store("request, Authorization=" + r.Header.Get("Authorization"))
		w.WriteHeader(207)
	}))
	defer other.Close()
	d.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	})
	check(t, e)
	if g := got.Load().(string); g != "" {
		t.Fatalf("redirect followed: %s", g)
	}
	if rf := remoteOf(e, "Docs"); rf.fails != 1 || rf.known {
		t.Errorf("redirect taken as an answer: %+v", rf)
	}
}
