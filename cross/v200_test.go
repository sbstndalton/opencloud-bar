package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveSyncOnByDefault(t *testing.T) {
	testEnv(t)
	_ = os.MkdirAll(appDir(), 0o700)
	write := func(extra string) Config {
		t.Helper()
		_ = os.WriteFile(configFile(), []byte(`{"davPath":"/remote.php/dav/files/sam","serverURL":"https://cloud.example.com"`+extra+`}`), 0o600)
		c, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := write(""); !c.LiveOn() || c.LiveSync != nil {
		t.Errorf("absent: on %v", c.LiveOn())
	}
	if c := write(`,"liveSync":false`); c.LiveOn() {
		t.Error("explicit false ignored")
	}
	if c := write(`,"liveSync":true`); !c.LiveOn() {
		t.Error("explicit true")
	}
	// Turning it off writes false; on removes the key; other saves keep the choice.
	raw := func() map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		b, _ := os.ReadFile(configFile())
		_ = json.Unmarshal(b, &m)
		return m
	}
	write("")
	off, on := false, true
	if _, err := applyConfigPatch(configPatch{LiveSync: &off}); err != nil {
		t.Fatal(err)
	}
	if string(raw()["liveSync"]) != "false" {
		t.Errorf("off: %s", raw()["liveSync"])
	}
	c, _ := loadConfig()
	if err := saveConfig(c); err != nil {
		t.Fatal(err)
	}
	if string(raw()["liveSync"]) != "false" {
		t.Error("a save dropped the off choice")
	}
	if _, err := applyConfigPatch(configPatch{LiveSync: &on}); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw()["liveSync"]; ok {
		t.Error("on (the default) left a key")
	}
}

func TestSafetyNetAndLargeFolderMath(t *testing.T) {
	iv := 5 * time.Minute
	for _, c := range []struct {
		dur  time.Duration
		live bool
		want time.Duration
	}{
		{30 * time.Second, false, iv},
		{30 * time.Second, true, 15 * time.Minute},
		{4 * time.Minute, false, 8 * time.Minute},
		{4 * time.Minute, true, 15 * time.Minute},
		{20 * time.Minute, true, 15 * time.Minute},
	} {
		if got := timerInterval(iv, c.dur, c.live); got != c.want {
			t.Errorf("timerInterval(%v, %v, %v) = %v, want %v", iv, c.dur, c.live, got, c.want)
		}
	}
	if got := timerInterval(time.Hour, time.Minute, true); got != time.Hour {
		t.Errorf("a longer interval stays: %v", got)
	}
	for _, c := range []struct{ dur, want time.Duration }{
		{10 * time.Second, liveMinGap},
		{50 * time.Second, 50 * time.Second},
		{2 * time.Minute, 2 * time.Minute},
		{3 * time.Minute, 10 * time.Minute},
		{4 * time.Minute, 10 * time.Minute},
		{8 * time.Minute, 16 * time.Minute},
	} {
		if got := liveGap(c.dur); got != c.want {
			t.Errorf("liveGap(%v) = %v, want %v", c.dur, got, c.want)
		}
	}
}

// A covered folder is synced by the timer every 15 minutes; one that fell back keeps the interval.
func TestSafetyNetTimer(t *testing.T) {
	e, _, f := liveRig(t)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.LiveSync = nil
	e.cfg.Auth = "oidc"
	e.cfg.SyncIntervalSeconds = 300
	lf := e.live().folder("Docs")
	lf.watch = &folderWatch{}
	e.runTimes["Docs"] = runTimes{start: time.Now().Add(-6 * time.Minute), dur: 30 * time.Second}
	if !e.pacedOutLocked(f, time.Now()) {
		t.Error("covered folder synced by the timer after 6 min")
	}
	e.runTimes["Docs"] = runTimes{start: time.Now().Add(-15 * time.Minute), dur: 30 * time.Second}
	if e.pacedOutLocked(f, time.Now()) {
		t.Error("covered folder not synced after 15 min")
	}
	if n := e.scheduleNoteLocked(f, 30*time.Second); n != "Checked every 15 min; changes sync right away" {
		t.Errorf("note %q", n)
	}
	e.runTimes["Docs"] = runTimes{start: time.Now().Add(-6 * time.Minute), dur: 30 * time.Second}
	lf.fallback = "watch limit"
	if e.pacedOutLocked(f, time.Now()) || e.scheduleNoteLocked(f, 30*time.Second) != "" {
		t.Error("fallen-back folder: not on the interval")
	}
	lf.fallback = ""
	e.cfg.Auth = "rclone" // no server checks
	if e.pacedOutLocked(f, time.Now()) {
		t.Error("rclone sign-in: not on the interval")
	}
	e.cfg.Auth = "oidc"
	lf.watch = nil // local folder missing: not watched
	if e.pacedOutLocked(f, time.Now()) {
		t.Error("unwatched folder: not on the interval")
	}
	if n := e.scheduleNoteLocked(f, 4*time.Minute); n != "large folder: syncs at most every 10 min" {
		t.Errorf("large note %q", n)
	}
}

// A large folder's live-triggered runs are at least max(10 min, 2 × its last run) apart;
// changes meanwhile keep it pending.
func TestLargeFolderLiveGap(t *testing.T) {
	e, clk, f := liveRig(t)
	e.mu.Lock()
	start := clk.now()
	e.ownSyncLocked(f.Key(), true, start)
	e.live().folders["Docs"].lastLive = start
	e.mu.Unlock()
	clk.add(4 * time.Minute)
	e.mu.Lock()
	e.ownSyncLocked(f.Key(), false, start)
	e.mu.Unlock()
	clk.add(3 * time.Second)
	e.localEvent("Docs", "agent.log.json")
	clk.add(5 * time.Minute) // 9 min after the last start
	e.liveTick()
	if queued(e, "Docs") || !e.Status().Folders[0].Pending {
		t.Fatal("large folder synced again within 10 minutes, or the change was dropped")
	}
	clk.add(time.Minute)
	e.liveTick()
	if !queued(e, "Docs") {
		t.Fatal("not synced 10 minutes after the last start")
	}
}

func TestLiveSyncNoticeOnce(t *testing.T) {
	testEnv(t)
	dir := t.TempDir()
	cfg := Config{ServerURL: "https://cloud.example.com", DavPath: "/remote.php/dav/files/sam", StateDir: dir,
		Kept: []KeptFolder{{Path: "Docs", StateName: "d"}}}
	if !cfg.Configured() {
		t.Skip("rig config not configured")
	}
	marker := filepath.Join(dir, "notified-live-sync")
	n := liveSyncNotice(cfg, marker, time.Now())
	if n == nil || !strings.Contains(n.Body, "Settings ▸ Sync") {
		t.Fatalf("no notice: %+v", n)
	}
	if liveSyncNotice(cfg, marker, time.Now()) != nil {
		t.Error("notice repeated")
	}
	if ch := loadChanges(cfg); len(ch) != 1 || ch[0].Text != liveNoticeText {
		t.Errorf("activity %+v", ch)
	}
	off := false
	cfg.LiveSync = &off
	if liveSyncNotice(cfg, filepath.Join(dir, "m2"), time.Now()) != nil {
		t.Error("notice for someone who chose")
	}
	cfg.LiveSync = nil
	cfg.Kept = nil
	if liveSyncNotice(cfg, filepath.Join(dir, "m3"), time.Now()) != nil {
		t.Error("notice with nothing kept")
	}
}
