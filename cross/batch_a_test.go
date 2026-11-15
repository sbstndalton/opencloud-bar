package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real rclone v1.76 output, shortened (see the A1 notes in the report).
const (
	driftOut = `2026/11/15 05:39:03 ERROR : -          Size not equal in listing. Path1: 12549, Path2: 12874 - ms01-sync/handoff/flips/LIVE-SWITCHES.md
2026/11/15 05:39:03 ERROR : -          Modtime not equal in listing. Path1: 2026-11-15 10:18:32 +0000 UTC, Path2: 2026-11-15 10:27:49 +0000 UTC - ms01-sync/handoff/flips/LIVE-SWITCHES.md
2026/11/15 05:39:03 ERROR : Bisync critical error: path1 and path2 are out of sync, run --resync to recover
2026/11/15 05:39:03 ERROR : Bisync aborted. Must run --resync to recover.
2026/11/15 05:39:03 NOTICE: Failed to bisync: bisync aborted`
	// The next run with --recover falls back to the old listings and fails the same way, but
	// says "retryable": without drift detection it would fail forever.
	driftRetryOut = `NOTICE: Listings not found. Reverting to prior backup as --recover is set.
ERROR : -          Size not equal in listing. Path1: 30, Path2: 33 - sub/f.md
ERROR : Bisync critical error: path1 and path2 are out of sync, run --resync to recover
ERROR : Bisync aborted. Error is retryable without --resync due to --resilient mode.`
	allChangedOut = `ERROR : Safety abort: all files were changed on Path2 "opencloud:Docs/". Run with --force if desired.
NOTICE: Bisync aborted. Please try again.
NOTICE: Failed to bisync: all files were changed`
	tooManyOut   = `ERROR : Safety abort: too many deletes (>50%, 8 of 10) on Path1 "/home/sam/Docs". Run with --force if desired.`
	retryableOut = `ERROR : Bisync aborted. Error is retryable without --resync due to --resilient mode.`
	resyncOut    = `ERROR : cannot find prior Path1 or Path2 listings, likely due to critical error on prior run
ERROR : Bisync aborted. Must run --resync to recover.`
	interruptOut = `NOTICE: Signal received: interrupt
NOTICE: Attempting to gracefully shutdown.
ERROR : Bisync interrupted. Must run --resync to recover.`
)

func TestClassifyBisync(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		timedOut bool
		want     syncOutcome
		path     string
		side     string
	}{
		{"drift", driftOut, false, outcomeDrift, "ms01-sync/handoff/flips/LIVE-SWITCHES.md", ""},
		{"drift after --recover says retryable", driftRetryOut, false, outcomeDrift, "sub/f.md", ""},
		{"all files changed on the server", allChangedOut, false, outcomeBigChange, "", "Path2"},
		{"too many deletes here", tooManyOut, false, outcomeBigChange, "", "Path1"},
		{"retryable", retryableOut, false, outcomeFailed, "", ""},
		{"needs resync", resyncOut, false, outcomeNeedsResync, "", ""},
		{"only the generic phrase", "ERROR : something --resync to recover", false, outcomeNeedsResync, "", ""},
		{"interrupted, not a repair", interruptOut, false, outcomeInterrupted, "", ""},
		{"timed out", "anything", true, outcomeInterrupted, "", ""}, // timeout flag checked below
		{"signed out", "token command: " + errSignedOut.Error(), false, outcomeSignedOut, "", ""},
		{"a file named interrupt.txt is not an interruption", "ERROR : interrupt.txt: failed to copy: 500", false, outcomeFailed, "", ""},
		{"other", "ERROR : couldn't connect", false, outcomeFailed, "", ""},
		{"path with a dash", "ERROR : -  Modtime not equal in listing. Path1: 2026-11-15 10:18:32 +0000 UTC, Path2: 2026-11-15 10:27:49 +0000 UTC - AI thing - copy/notes.md\nERROR : out of sync", false, outcomeDrift, "AI thing - copy/notes.md", ""},
	}
	if r := classifyBisync(interruptOut, true); !r.timeout {
		t.Error("a timed-out run must say so")
	}
	if r := classifyBisync(interruptOut, false); r.timeout {
		t.Error("a quit is not a timeout")
	}
	for _, c := range cases {
		r := classifyBisync(c.out, c.timedOut)
		r.timeout = false
		if r.outcome != c.want || r.path != c.path || r.side != c.side {
			t.Errorf("%s: got %+v, want outcome %d path %q side %q", c.name, r, c.want, c.path, c.side)
		}
	}
}

func TestAutoMergeCapIsDailyAndPersisted(t *testing.T) {
	f := filepath.Join(t.TempDir(), "docs.auto-merge")
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	if !canAutoMerge(f, now) {
		t.Fatal("first auto-merge must be allowed")
	}
	if err := recordAutoMerge(f, now); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{0, time.Minute, 23*time.Hour + 59*time.Minute} {
		if canAutoMerge(f, now.Add(d)) {
			t.Errorf("auto-merge allowed again after %s", d)
		}
	}
	if !canAutoMerge(f, now.Add(24*time.Hour)) {
		t.Error("auto-merge not allowed after 24 h")
	}
	// The clock went back a long way: don't stay blocked forever, but not right away either.
	if canAutoMerge(f, now.Add(-time.Hour)) {
		t.Error("a slightly earlier clock must still count as within 24 h")
	}
	if !canAutoMerge(f, now.Add(-25*time.Hour)) {
		t.Error("a far-future record must not block forever")
	}
	_ = os.WriteFile(f, []byte("garbage"), 0o600)
	if !canAutoMerge(f, now) {
		t.Error("an unreadable record must not block merging")
	}
	if got := driftReason("a/b.md", "choose Repair"); got != "“b.md” keeps changing on both sides; choose Repair" {
		t.Error("drift reason should name the file")
	}
}

// pausedRig is an engine with a complete, legacy-free config that never touches the network:
// no drive, no update checks, nothing kept.
func pausedRig(t *testing.T, extra string) *Engine {
	t.Helper()
	testEnv(t)
	_ = os.MkdirAll(appDir(), 0o700)
	cfg := `{"davPath":"/remote.php/dav/files/sam","serverURL":"https://cloud.example.com","auth":"rclone",
	  "useTunnel":false,"mountEnabled":false,"checkForUpdates":false,"futureKey":7` + extra + `}`
	if err := os.WriteFile(configFile(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewEngine()
	if e.cfgErr != nil || !e.cfg.Configured() {
		t.Fatalf("rig config: %v %+v", e.cfgErr, e.cfg)
	}
	return e
}

func TestPauseResumePersistsAndStopsSyncing(t *testing.T) {
	e := pausedRig(t, `,"kept":[{"path":"Docs","stateName":"docs-1"}]`)
	if err := e.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(configFile())
	if !strings.Contains(string(data), `"paused": true`) || !strings.Contains(string(data), `"futureKey": 7`) {
		t.Fatalf("pause not saved (or other keys lost): %s", data)
	}
	st := e.Status()
	if !st.Paused || !strings.HasPrefix(st.Summary, "Paused") {
		t.Errorf("status while paused: %+v", st)
	}
	e.nextSyncAt = time.Time{}
	e.tick()
	e.SyncNow()
	if e.Status().Syncing {
		t.Fatal("a sync started while paused")
	}
	// A restart keeps it paused.
	if again := NewEngine(); !again.cfg.Paused {
		t.Error("paused not loaded from the settings")
	}
	// Resume: saved, and the next tick may sync right away.
	e.nextSyncAt = time.Now().Add(time.Hour)
	if err := e.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if e.cfg.Paused || !e.nextSyncAt.IsZero() {
		t.Errorf("resume: paused=%v next=%v", e.cfg.Paused, e.nextSyncAt)
	}
	data, _ = os.ReadFile(configFile())
	if strings.Contains(string(data), `"paused"`) {
		t.Errorf("resume left paused in the file: %s", data)
	}
	// With a broken settings file nothing is written.
	e.cfgErr = os.ErrInvalid
	if e.SetPaused(true) == nil {
		t.Error("pause accepted while the settings file has a problem")
	}
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		latest, running string
		want            bool
	}{
		{"v0.4.0", "0.3.0", true},
		{"v0.4.1", "0.4.0", true},
		{"v1.0.0", "0.9.9", true},
		{"v0.10.0", "0.9.0", true}, // numbers, not text
		{"v0.4.0", "0.4.0", false},
		{"v0.3.9", "0.4.0", false},
		{"v0.4.0", "0.4.0-rc.1", true}, // a release beats its pre-release
		{"v0.4.0-rc.1", "0.4.0", false},
		{"v0.4.0-rc.2", "0.4.0-rc.1", true},
		{"v9.9.9", "0.0.0-dev", false}, // development builds are never offered updates
		{"v9.9.9", "0.0.0", false},
		{"v9.9.9", "1.2.3-dev", false},
		{"nonsense", "0.4.0", false},
		{"v0.5", "0.4.0", true},
		{"v0.4.0+build.7", "0.4.0", false},
	}
	for _, c := range cases {
		if got := updateAvailable(c.latest, c.running); got != c.want {
			t.Errorf("updateAvailable(%q, %q) = %v, want %v", c.latest, c.running, got, c.want)
		}
	}
	for _, bad := range []string{"", "v", "1", "1.2.3.4", "1.x.3", "-1.2.3"} {
		if _, ok := parseSemver(bad); ok {
			t.Errorf("parseSemver(%q) accepted", bad)
		}
	}
}

func TestUpdateCheck(t *testing.T) {
	testEnv(t)
	hits := 0
	body := `{"tag_name":"v9.1.0","html_url":"https://github.com/sbstndalton/opencloud-bar/releases/tag/v9.1.0","name":"x"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") != "" {
			t.Error("the update check must not send credentials")
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	old := version
	version = "0.4.0"
	defer func() { version = old }()
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	u := newUpdater()
	u.api, u.now = srv.URL, func() time.Time { return clock }
	if !u.due() {
		t.Fatal("first check should be due")
	}
	st := u.Check(context.Background())
	if !st.Available || st.Latest != "9.1.0" || !strings.HasSuffix(st.URL, "/v9.1.0") || st.Error != "" {
		t.Fatalf("check: %+v", st)
	}
	clock = clock.Add(23 * time.Hour)
	if u.due() {
		t.Error("checked again within 24 h")
	}
	// Remembered across restarts.
	u2 := newUpdater()
	u2.now = func() time.Time { return clock }
	if u2.due() || !u2.State().Available || u2.State().Latest != "9.1.0" {
		t.Errorf("not remembered: %+v", u2.State())
	}
	clock = clock.Add(2 * time.Hour)
	if !u2.due() {
		t.Error("not due after 24 h")
	}

	// A release page outside this project is never opened.
	body = `{"tag_name":"v9.2.0","html_url":"https://evil.example/download"}`
	if st := u.Check(context.Background()); st.URL != releasesPagePrefix+"latest" {
		t.Errorf("foreign URL kept: %q", st.URL)
	}
	body = `{"tag_name":"latest-and-greatest"}`
	if st := u.Check(context.Background()); st.Error == "" {
		t.Error("a non-version tag was accepted")
	}
	if hits != 3 {
		t.Errorf("hits = %d", hits)
	}
}

func TestSettingsSafety(t *testing.T) {
	e := pausedRig(t, `,"kept":[{"path":"Docs","stateName":"docs-1"}]`)

	// Keep and Unkeep refuse while the settings file has a problem.
	e.cfgErr = os.ErrInvalid
	if err := e.Keep("Photos", ""); err == nil {
		t.Error("Keep accepted with a broken settings file")
	}
	if err := e.Unkeep("Docs", func(error) {}); err == nil {
		t.Error("Unkeep accepted with a broken settings file")
	}
	e.cfgErr = nil

	// saveConfig never overwrites a file it can't parse.
	broken := []byte(`{"davPath": "/remote.php/dav/files/sam", oops`)
	_ = os.WriteFile(configFile(), broken, 0o600)
	if err := saveConfig(e.cfg); err == nil {
		t.Error("saveConfig overwrote an unparseable file")
	}
	if b, _ := os.ReadFile(configFile()); string(b) != string(broken) {
		t.Error("the unparseable file was changed")
	}
	if err := e.Keep("Photos", ""); err == nil {
		t.Error("Keep saved over an unparseable file")
	}

	// localPath must not be (or contain) home, a root, localRoot or another kept folder.
	h := home()
	root := e.cfg.LocalRoot
	other := filepath.Join(h, "Elsewhere", "Docs")
	base := Config{LocalRoot: root, Kept: []KeptFolder{{Path: "Docs", StateName: "d", LocalPath: other}}}
	bad := map[string]string{
		"home":                 h,
		"filesystem root":      string(filepath.Separator),
		"localRoot":            expand(root),
		"contains localRoot":   filepath.Dir(expand(root)),
		"other folder":         other,
		"contains other":       filepath.Dir(other),
		"inside other":         filepath.Join(other, "sub"),
		"relative":             "relative/dir",
		"other's default path": filepath.Join(expand(root), "Music"),
	}
	for name, p := range bad {
		c := base
		c.Kept = append(append([]KeptFolder{}, base.Kept...), KeptFolder{Path: "Photos", StateName: "p", LocalPath: p})
		if name == "other's default path" {
			c.Kept = append(c.Kept, KeptFolder{Path: "Music", StateName: "m"})
		}
		if err := c.Validate(); err == nil {
			t.Errorf("localPath %s (%s) accepted", name, p)
		}
	}
	ok := base
	ok.Kept = append(append([]KeptFolder{}, base.Kept...),
		KeptFolder{Path: "Photos", StateName: "p", LocalPath: filepath.Join(h, "Pictures", "OpenCloud Photos")},
		KeptFolder{Path: "Music", StateName: "m"})
	if err := ok.Validate(); err != nil {
		t.Errorf("a good localPath was refused: %v", err)
	}
	// Legacy configs (no localPath) are unaffected.
	legacy := Config{LocalRoot: root, Kept: []KeptFolder{{Path: "A", StateName: "a"}, {Path: "B", StateName: "b"}}}
	if err := legacy.Validate(); err != nil {
		t.Errorf("legacy config refused: %v", err)
	}
}

func TestAllowBigDeletionOnlyWhenBlocked(t *testing.T) {
	e := pausedRig(t, `,"kept":[{"path":"Docs","stateName":"docs-1"}]`)
	if err := e.AllowBigDeletion("Docs"); err == nil {
		t.Error("deletion override accepted for a folder that isn't blocked")
	}
	e.status["Docs"].State, e.status["Docs"].Detail = StFailed, missingRemoteText
	if err := e.AllowBigDeletion("Docs"); err == nil {
		t.Error("deletion override accepted when the server copy is missing")
	}
}

func TestMenuTexts(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 5 * time.Minute: "5 min ago", 3 * time.Hour: "3 h ago", 50 * time.Hour: "2 days ago",
	} {
		if got := agoAt(now.Add(-d), now); got != want {
			t.Errorf("agoAt(-%s) = %q, want %q", d, got, want)
		}
	}
	ok := FolderStatus{Name: "School", State: StOK, LastSuccess: now.Add(-5 * time.Minute)}
	if got := folderLabelAt(ok, now); got != "School — synced 5 min ago" {
		t.Errorf("ok label: %q", got)
	}
	bad := FolderStatus{Name: "agents", State: StNeedsRepair, Detail: "sync history is out of date"}
	if got := folderLabelAt(bad, now); got != "⚠ agents — needs repair" {
		t.Errorf("repair label: %q", got)
	}
	if got := attentionReason(bad); got != "needs repair: sync history is out of date" {
		t.Errorf("repair reason: %q", got)
	}
	gone := FolderStatus{Name: "Docs", State: StFailed, Detail: missingRemoteText}
	sum := attentionSummary([]FolderStatus{gone, bad})
	if !strings.Contains(sum, "“Docs”") || !strings.Contains(sum, missingRemoteText) || !strings.Contains(sum, "1 more") {
		t.Errorf("summary: %q", sum)
	}
}

func TestRcCallsAreAuthenticated(t *testing.T) {
	e := pausedRig(t, "")
	var gotUser, gotPass string
	var authed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, authed = r.BasicAuth()
		_ = json.NewEncoder(w).Encode(map[string]any{"diskCache": map[string]int{"uploadsInProgress": 1, "uploadsQueued": 2}})
	}))
	defer srv.Close()
	var port int
	_, p, _ := strings.Cut(srv.URL, "127.0.0.1:")
	for _, c := range p {
		port = port*10 + int(c-'0')
	}
	e.cfg.LocalPort = port - 100 // RcPort is LocalPort + 100
	out, err := e.rc("vfs/stats", 5*time.Second)
	if err != nil || !strings.Contains(string(out), "uploadsQueued") {
		t.Fatalf("rc: %v %s", err, out)
	}
	if !authed || gotUser != e.rcUser || gotPass != e.rcPass || len(e.rcPass) < 32 {
		t.Errorf("rc login not sent: %q %q", gotUser, gotPass)
	}
	if other := NewEngine(); other.rcPass == e.rcPass {
		t.Error("the rc password must be random per start")
	}
}

func TestSignOutFailsClosedOnUnknownUploads(t *testing.T) {
	e := pausedRig(t, "")
	e.mount = &exec.Cmd{} // a drive "running" whose rc doesn't answer
	e.cfg.LocalPort = 1   // nothing listens on port 101
	if err := e.SignOut(false); err != errUploadsUnknown {
		t.Fatalf("SignOut with an unknown upload count: %v", err)
	}
	if e.signedOut {
		t.Error("signed out anyway")
	}
}
