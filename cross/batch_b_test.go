package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func folder(path, state, detail string) FolderStatus {
	return FolderStatus{Path: path, Name: filepath.Base(path), State: state, Detail: detail}
}

func titles(ns []note) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Title)
	}
	return out
}

func TestNotificationsOncePerTransition(t *testing.T) {
	var ns notifyState
	st := Status{Folders: []FolderStatus{folder("A", StNeedsRepair, "sync history is out of date"), folder("B", StOK, "")}}
	// Start-up: problems already there show in the menu, not as a burst of notifications.
	if got := planNotifications(&ns, st, nil); len(got) != 0 {
		t.Fatalf("seed notified: %v", titles(got))
	}
	// B fails: one notification. It keeps failing at every sync (syncing in between): no repeats.
	st.Folders[1] = folder("B", StFailed, "rclone failed; will retry (see the sync log)")
	got := planNotifications(&ns, st, nil)
	if len(got) != 1 || got[0].Title != "Sync failed for “B”" || !strings.HasPrefix(got[0].Body, "Rclone failed") {
		t.Fatalf("failure: %+v", got)
	}
	for i := 0; i < 3; i++ {
		st.Folders[1].State = StSyncing
		if got := planNotifications(&ns, st, nil); len(got) != 0 {
			t.Fatalf("syncing notified: %v", titles(got))
		}
		st.Folders[1] = folder("B", StFailed, "rclone failed; will retry (see the sync log)")
		if got := planNotifications(&ns, st, nil); len(got) != 0 {
			t.Fatalf("repeated failure notified: %v", titles(got))
		}
	}
	// It recovers, then fails again: a new transition, a new notification.
	st.Folders[1] = folder("B", StOK, "")
	planNotifications(&ns, st, nil)
	st.Folders[1] = folder("B", StFailed, "timed out; will retry")
	if got := planNotifications(&ns, st, nil); len(got) != 1 {
		t.Fatalf("second failure: %v", titles(got))
	}
	// A failed folder that now needs repair is a different problem.
	st.Folders[1] = folder("B", StNeedsRepair, "x")
	if got := planNotifications(&ns, st, nil); len(got) != 1 || got[0].Title != "“B” needs repair" {
		t.Fatalf("failed -> repair: %v", titles(got))
	}
}

func TestNotificationsCoalesceFolders(t *testing.T) {
	var ns notifyState
	st := Status{Folders: []FolderStatus{folder("A", StOK, ""), folder("B", StOK, ""), folder("C", StOK, ""), folder("D", StOK, "")}}
	planNotifications(&ns, st, nil)
	st.Folders = []FolderStatus{folder("A", StNeedsRepair, "x"), folder("B", StNeedsRepair, "y"),
		folder("C", StFailed, missingRemoteText), folder("D", StBigDeletion, "")}
	got := planNotifications(&ns, st, []string{"E", "F"})
	want := []string{"“C” is missing or empty in OpenCloud", "2 folders need repair", "Large deletion blocked in “D”", "2 folders were merged automatically"}
	if strings.Join(titles(got), "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", titles(got), want)
	}
	if !strings.Contains(got[1].Body, "“A”, “B”") {
		t.Errorf("coalesced body should list the folders: %q", got[1].Body)
	}
	for _, n := range got {
		if n.Section != "folders" {
			t.Errorf("%q opens %q", n.Title, n.Section)
		}
	}
}

func TestNotificationSignedOut(t *testing.T) {
	var ns notifyState
	st := Status{Folders: []FolderStatus{folder("A", StOK, "")}}
	planNotifications(&ns, st, nil)
	st.SignedOut = true
	st.Folders[0] = folder("A", StFailed, "signed out")
	got := planNotifications(&ns, st, nil)
	if len(got) != 1 || got[0].Title != "Signed out of OpenCloud" || got[0].Section != "account" {
		t.Fatalf("sign-out: %+v", got)
	}
	if again := planNotifications(&ns, st, nil); len(again) != 0 {
		t.Fatal("sign-out notified twice")
	}
	// Signed out at start-up: the menu says so; no notification.
	var fresh notifyState
	if got := planNotifications(&fresh, st, nil); len(got) != 0 {
		t.Fatal("start-up sign-out notified")
	}
}

func TestUpdateNotificationOncePerVersion(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notified-update")
	u := UpdateState{Running: "0.4.0", Latest: "0.5.0", Available: true}
	if n := updateNote(u, file); n == nil || n.Title != "OpenCloud Bar 0.5.0 is available" {
		t.Fatalf("first: %+v", n)
	}
	if n := updateNote(u, file); n != nil {
		t.Fatal("same version notified again (also after a restart: it is on disk)")
	}
	u.Latest = "0.5.1"
	if n := updateNote(u, file); n == nil {
		t.Fatal("a newer version wasn't announced")
	}
	if n := updateNote(UpdateState{Running: "0.5.1", Latest: "0.5.1"}, file); n != nil {
		t.Fatal("announced without an update")
	}
}

func TestEngineNotificationsHonourSetting(t *testing.T) {
	e := pausedRig(t, `,"kept":[{"path":"Docs","stateName":"docs-1"}]`)
	var mu sync.Mutex
	var shown []string
	old := showNotification
	showNotification = func(title, body string) error { mu.Lock(); shown = append(shown, title); mu.Unlock(); return nil }
	defer func() { showNotification = old }()

	e.checkNotifications() // seed
	e.setState(KeptFolder{Path: "Docs"}, StBigDeletion, "")
	e.checkNotifications()
	e.checkNotifications()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(shown) == 1 })

	off := false
	e.cfg.Notifications = &off
	e.setState(KeptFolder{Path: "Docs"}, StOK, "")
	e.checkNotifications()
	e.setState(KeptFolder{Path: "Docs"}, StNeedsRepair, "x")
	e.checkNotifications()
	mu.Lock()
	defer mu.Unlock()
	if len(shown) != 1 {
		t.Errorf("notifications shown while turned off: %v", shown)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestStatsFilter(t *testing.T) {
	// Real rclone v1.76 output with --stats --stats-log-level NOTICE (one block, then a normal line).
	out := "2026/11/15 06:35:10 NOTICE: \n" +
		"Transferred:   \t   74.673 MiB / 85.831 MiB, 87%, 0 B/s, ETA -\n" +
		"Checks:               900 / 900, 100%, Listed 1200\n" +
		"Transferred:          260 / 300, 87%\n" +
		"Elapsed time:         0.2s\n" +
		"Transferring:\n" +
		" *                                      f561.bin:100% / 292.969 KiB, 0 B/s, -\n" +
		"\n" +
		"2026/11/15 06:35:11 NOTICE: f.txt: Can't follow symlink without -L/--copy-links\r\n" +
		"2026/11/15 06:35:12 NOTICE: \n" +
		"Transferred:          300 / 300, 100%\n" +
		"\n" +
		"2026/11/15 06:35:13 ERROR : Bisync critical error: path1 and path2 are out of sync"
	var log bytes.Buffer
	var counts []int
	sf := &statsFilter{w: &log, onFiles: func(n int) { counts = append(counts, n) }}
	for i := 0; i < len(out); i += 7 { // arrives in pieces, split anywhere
		end := min(i+7, len(out))
		_, _ = sf.Write([]byte(out[i:end]))
	}
	sf.Flush()
	if len(counts) != 2 || counts[0] != 260 || counts[1] != 300 {
		t.Errorf("counts %v", counts)
	}
	want := "2026/11/15 06:35:11 NOTICE: f.txt: Can't follow symlink without -L/--copy-links\r\n" +
		"2026/11/15 06:35:13 ERROR : Bisync critical error: path1 and path2 are out of sync"
	if log.String() != want {
		t.Errorf("log kept:\n%q\nwant\n%q", log.String(), want)
	}
}

func TestActivityText(t *testing.T) {
	cases := map[string]string{
		activityText("", 0, 0, 0):        "Syncing…",
		activityText("School", 1, 1, 0):  "Syncing “School”…",
		activityText("School", 2, 3, 0):  "Syncing “School” (2 of 3)…",
		activityText("School", 2, 3, 1):  "Syncing “School” (2 of 3) · 1 file…",
		activityText("agents", 1, 1, 12): "Syncing “agents” · 12 files…",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if s := driveLetterInUse("o:"); s != "Drive letter O: is already in use — choose another in Settings ▸ Drive" {
		t.Errorf("drive letter text: %q", s)
	}
	if !isDriveLetter("O:") || isDriveLetter("~/OpenCloud-Drive") || isDriveLetter("OO:") {
		t.Error("isDriveLetter")
	}
}

func TestUISetupAddressAndAbout(t *testing.T) {
	r := newUIRig(t)
	r.login()
	if resp, body := r.post("/ui/api/setup", `{"serverURL":"http://evil.example"}`); resp.StatusCode != 400 {
		t.Errorf("plain-http address accepted: %d %s", resp.StatusCode, body)
	}
	resp, body := r.do("GET", "/ui/api/about", "", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"version"`) || !strings.Contains(body, `"rclone"`) {
		t.Errorf("about: %d %s", resp.StatusCode, body)
	}
	resp, body = r.do("GET", "/ui/icon.png", "", "", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(body, "\x89PNG") {
		t.Errorf("icon: %d", resp.StatusCode)
	}
	if b, _ := os.ReadFile("assets/icon-256.png"); len(b) == 0 {
		t.Error("embedded icon missing")
	}
}

func TestOpenWindowSwitchesSection(t *testing.T) {
	r := newUIRig(t)
	r.login()
	r.ui.requestSection("about")
	r.ui.requestSection("not-a-section") // ignored
	_, body := r.do("GET", "/ui/api/goto", "", "", nil)
	if !strings.Contains(body, `"about"`) {
		t.Fatalf("goto: %s", body)
	}
	if _, body = r.do("GET", "/ui/api/goto", "", "", nil); strings.Contains(body, "about") {
		t.Error("a request is used once")
	}
}
