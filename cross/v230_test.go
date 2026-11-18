package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 2.3: pause for a set time, Sync Now for one folder.

func TestPauseForParsing(t *testing.T) {
	for in, want := range map[string]time.Duration{"1h": time.Hour, "90m": 90 * time.Minute, " 2h30m ": 150 * time.Minute, "720h": 720 * time.Hour} {
		if d, err := pauseFor(in); err != nil || d != want {
			t.Errorf("%q: %v %v", in, d, err)
		}
	}
	for _, bad := range []string{"", "soon", "1", "-1h", "0", "30s", "721h", "1d"} {
		if d, err := pauseFor(bad); err == nil {
			t.Errorf("%q accepted: %v", bad, d)
		}
	}
	for args, want := range map[string]time.Duration{"": 0, "--for 1h": time.Hour, "--for=45m": 45 * time.Minute} {
		d, err := parsePauseArgs(strings.Fields(args))
		if err != nil || d != want {
			t.Errorf("args %q: %v %v", args, d, err)
		}
	}
	for _, bad := range []string{"--for", "--for 1h extra", "1h", "--until 1h", "--for=soon"} {
		if _, err := parsePauseArgs(strings.Fields(bad)); err == nil {
			t.Errorf("args %q accepted", bad)
		}
	}
}

func TestNextMorning(t *testing.T) {
	loc := time.FixedZone("x", 2*3600)
	at := func(y, m, d, h, mi int) time.Time { return time.Date(y, time.Month(m), d, h, mi, 0, 0, loc) }
	for _, c := range []struct{ now, want time.Time }{
		{at(2026, 11, 17, 22, 19), at(2026, 11, 18, 8, 0)},
		{at(2026, 11, 17, 3, 0), at(2026, 11, 17, 8, 0)},
		{at(2026, 11, 17, 8, 0), at(2026, 11, 18, 8, 0)},
		{at(2026, 12, 31, 9, 0), at(2027, 1, 1, 8, 0)},
	} {
		if got := nextMorning(c.now); !got.Equal(c.want) {
			t.Errorf("%v: %v, want %v", c.now, got, c.want)
		}
	}
}

// pausedUntil is saved next to paused, survives a restart and a Reload, and Resume removes both.
func TestPausedUntilRoundTrip(t *testing.T) {
	e := pausedRig(t, `,"kept":[{"path":"Docs","stateName":"docs-1"}]`)
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := e.PauseUntil(until); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(configFile())
	if !strings.Contains(string(data), `"paused": true`) || !strings.Contains(string(data), `"pausedUntil": "`+until.Format(time.RFC3339)+`"`) ||
		!strings.Contains(string(data), `"futureKey": 7`) {
		t.Fatalf("not saved (or other keys lost): %s", data)
	}
	st := e.Status()
	if !st.Paused || st.PausedUntil != until.Format(time.RFC3339) {
		t.Errorf("status: %+v", st)
	}
	if again := NewEngine(); !again.cfg.Paused || !again.cfg.pauseEnd().Equal(until) {
		t.Errorf("restart lost the pause: %+v", again.cfg)
	}
	if err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	if c := e.config(); !c.Paused || !c.pauseEnd().Equal(until) {
		t.Errorf("Reload lost the pause: %q", c.PausedUntil)
	}
	// Pausing without a time drops the time; Resume drops both.
	if err := e.SetPaused(true); err != nil || e.config().PausedUntil != "" {
		t.Errorf("until-resumed pause: %v %q", err, e.config().PausedUntil)
	}
	if err := e.PauseUntil(until); err != nil {
		t.Fatal(err)
	}
	if err := e.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(configFile())
	if strings.Contains(string(data), `"paused`) || e.config().Paused || e.config().PausedUntil != "" {
		t.Errorf("resume left the pause behind: %s", data)
	}
}

// tick resumes when the time has come, and removes the key; before that it stays paused.
func TestTickResumesTimedPause(t *testing.T) {
	e := pausedRig(t, ``) // nothing kept: the tick after the resume starts no sync
	if err := e.PauseUntil(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if !e.config().Paused {
		t.Fatal("resumed early")
	}
	e.mu.Lock()
	e.nextSyncAt = time.Now().Add(time.Hour) // keep the tick from starting a sync
	e.cfg.PausedUntil = time.Now().Add(-time.Second).Format(time.RFC3339)
	e.mu.Unlock()
	e.tick()
	if c := e.config(); c.Paused || c.PausedUntil != "" {
		t.Fatalf("still paused: %+v", c)
	}
	if data, _ := os.ReadFile(configFile()); strings.Contains(string(data), `"paused`) {
		t.Errorf("keys left in the file: %s", data)
	}
	// A pause without a time never ends by itself; a garbage time neither.
	if err := e.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.cfg.PausedUntil = "garbage"
	e.mu.Unlock()
	e.tick()
	if !e.config().Paused {
		t.Error("an until-resumed pause ended by itself")
	}
}

func TestPauseClock(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)
	if got := pauseClock(time.Date(2026, 11, 17, 14, 30, 0, 0, time.Local), now); got != "14:30" {
		t.Errorf("today: %q", got)
	}
	if got := pauseClock(time.Date(2026, 11, 18, 8, 0, 0, 0, time.Local), now); got != "Wed 08:00" {
		t.Errorf("tomorrow: %q", got)
	}
}

// POST /ui/api/sync with a path that isn't kept is a 400; without a path it is Sync Now.
func TestUISyncUnknownPath(t *testing.T) {
	r := newUIRig(t)
	r.login()
	resp, body := r.post("/ui/api/sync", `{"path":"Nope"}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "isn't a kept folder") {
		t.Errorf("unknown path: %d %s", resp.StatusCode, body)
	}
	if resp, body := r.post("/ui/api/sync", `{}`); resp.StatusCode != 200 {
		t.Errorf("sync all: %d %s", resp.StatusCode, body)
	}
}
