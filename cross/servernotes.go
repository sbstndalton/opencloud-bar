package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// OpenCloud notifications as desktop notifications (2.7): what the web interface's bell shows
// (shares, space membership, expiring shares, virus finds, mentions…).
//
// The route, from the OpenCloud source:
//
//	GET <server>/ocs/v2.php/apps/notifications/api/v1/notifications?format=json
//
//   - Served by the userlog service: opencloud-eu/opencloud services/userlog/pkg/server/http/
//     server.go (mux.Route "/ocs/v2.php/apps/notifications/api/v1/notifications": Get "/" is
//     HandleGetEvents, Delete "/" HandleDeleteEvents), routed to it by the proxy
//     (services/proxy/pkg/config/defaults/defaultconfig.go, "reroute oc10 notifications
//     endpoint to userlog service").
//   - The answer (services/userlog/pkg/service/http/service.go HandleGetEvents, response.go,
//     and conversion.go OC10Notification): {"ocs":{"meta":{…},"data":[{"notification_id",
//     "app","user","datetime","object_id","object_type","subject","message", …}]}}; "data"
//     may be null, and an empty body is possible. The ids are strings.
//   - The web client (opencloud-eu/web packages/web-runtime/src/components/Topbar/
//     Notifications.vue) reads the same route and deletes ("marks as read") with DELETE on it.
//     This app never deletes: it only reads.
//
// Rules: polled every 3 minutes, only while signed in with the browser, not paused and with the
// setting on (Settings ▸ General ▸ Show OpenCloud notifications, default on). The first
// successful fetch after install, sign-in or a server change only records the ids it sees. The
// seen ids (at most srvNotesMaxSeen, more only when the server itself lists more) are kept in
// <stateDir>/server-notifications.json; signing out, signing in or changing the server or
// account forgets them (resetQuotaLocked, with the quota state). Errors back off, doubling up
// to 30 minutes. A 404 means the service isn't deployed: no more polling this session
// (until the next sign-in or server change). The macOS app does the same (ServerNotes.swift).

const (
	srvNotesEvery      = 3 * time.Minute
	srvNotesMaxBackoff = 30 * time.Minute
	srvNotesMaxSeen    = 500
	srvNotesMaxToasts  = 5 // more new ones at once: the rest become one summary
	srvNotesPath       = "/ocs/v2.php/apps/notifications/api/v1/notifications"
)

var errSrvNotesGone = errors.New("the server has no notifications service")

// srvNote is one notification from the server.
type srvNote struct{ ID, Subject, Message string }

// flexID reads an id that is a string (or a number).
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexID(n.String())
	return nil
}

// parseSrvNotes reads the server's answer. An empty body is no notifications; entries without
// an id are skipped (they can't be told apart), and an id repeated is read once.
func parseSrvNotes(r io.Reader) ([]srvNote, error) {
	b, err := io.ReadAll(io.LimitReader(r, 2<<20))
	if err != nil {
		return nil, errors.New("the server's answer couldn't be read")
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var doc struct {
		OCS struct {
			Data []struct {
				ID      flexID `json:"notification_id"`
				Subject string `json:"subject"`
				Message string `json:"message"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, errors.New("the server's answer wasn't what was expected")
	}
	var out []srvNote
	seen := map[string]bool{}
	for _, d := range doc.OCS.Data {
		id := strings.TrimSpace(string(d.ID))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, srvNote{ID: id, Subject: d.Subject, Message: d.Message})
	}
	return out, nil
}

// fetchSrvNotes reads the notifications (GET only; accountClient follows no redirects, so the
// token never leaves the configured server). A 404 is errSrvNotesGone.
func fetchSrvNotes(ctx context.Context, base, tok string) ([]srvNote, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+srvNotesPath+"?format=json", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := accountClient.Do(req)
	if err != nil {
		return nil, errors.New("the server didn't answer")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		return parseSrvNotes(resp.Body)
	case 404:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, errSrvNotesGone
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
}

// srvSeen is what has been seen: Seeded is false until the first successful fetch.
type srvSeen struct {
	Seeded bool
	IDs    []string // oldest first
}

// planSrvNotes compares a fetch with what was seen: the new notifications to show (none for
// the first fetch, which only seeds), and the state to keep. Ids the server no longer lists
// stay remembered until the cap pushes them out, oldest first; ids it does list are never
// pushed out.
func planSrvNotes(s srvSeen, got []srvNote) (show []srvNote, next srvSeen) {
	have := make(map[string]bool, len(s.IDs))
	for _, id := range s.IDs {
		have[id] = true
	}
	cur := make([]string, 0, len(got))
	inCur := make(map[string]bool, len(got))
	for _, n := range got {
		cur = append(cur, n.ID)
		inCur[n.ID] = true
		if s.Seeded && !have[n.ID] {
			show = append(show, n)
		}
	}
	ids := make([]string, 0, len(s.IDs)+len(cur))
	for _, id := range s.IDs {
		if !inCur[id] {
			ids = append(ids, id)
		}
	}
	ids = append(ids, cur...)
	if limit := max(srvNotesMaxSeen, len(cur)); len(ids) > limit {
		ids = ids[len(ids)-limit:]
	}
	return show, srvSeen{Seeded: true, IDs: ids}
}

// srvNotesBackoff is the wait after fails errors in a row: 6 minutes, doubling, up to 30.
func srvNotesBackoff(fails int) time.Duration {
	d := srvNotesEvery
	for i := 0; i < fails && d < srvNotesMaxBackoff; i++ {
		d *= 2
	}
	return min(d, srvNotesMaxBackoff)
}

// cleanNoteText makes server text safe to show: no control characters, one line of spaces,
// at most max characters.
func cleanNoteText(s string, max int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// srvToasts turns new notifications into what to show: each one, or for more than
// srvNotesMaxToasts the first ones and a summary of the rest.
func srvToasts(news []srvNote) []note {
	one := func(n srvNote) note {
		title, body := cleanNoteText(n.Subject, 100), cleanNoteText(n.Message, 240)
		if title == "" {
			title = "OpenCloud"
		}
		if body == "" && title == "OpenCloud" {
			body = "You have a new notification."
		}
		return note{Title: title, Body: body, Section: "general"}
	}
	shown := news
	if len(news) > srvNotesMaxToasts {
		shown = news[:srvNotesMaxToasts-1]
	}
	var out []note
	for _, n := range shown {
		out = append(out, one(n))
	}
	if rest := len(news) - len(shown); rest > 0 {
		out = append(out, note{Title: "OpenCloud", Body: strconv.Itoa(rest) + " more notifications. Open OpenCloud to read them.", Section: "general"})
	}
	return out
}

func srvNotesFile(cfg Config) string {
	return filepath.Join(expand(cfg.StateDir), "server-notifications.json")
}

// readSrvSeen reads the remembered ids. A missing or damaged file is "never seeded".
func readSrvSeen(file string) srvSeen {
	b, err := os.ReadFile(file)
	if err != nil {
		return srvSeen{}
	}
	var d struct {
		IDs []string `json:"ids"`
	}
	if json.Unmarshal(b, &d) != nil {
		return srvSeen{}
	}
	return srvSeen{Seeded: true, IDs: d.IDs}
}

func writeSrvSeen(file string, ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	b, _ := json.Marshal(map[string]any{"ids": ids})
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// resetSrvNotesLocked forgets what was seen (sign-out, sign-in, another server or account),
// the file in each state folder given included, and lets polling start again. A poll still
// running for the old state is dropped when it ends. Called with the quota state's reset.
func (e *Engine) resetSrvNotesLocked(cfgs ...Config) {
	e.snGen++
	e.snAt, e.snFails, e.snOff = time.Time{}, 0, false
	e.snSeen, e.snLoaded = srvSeen{}, false
	for _, c := range cfgs {
		if c.StateDir == "" {
			continue // never loaded: the file would be a relative one
		}
		_ = os.Remove(srvNotesFile(c))
	}
}

// srvNotesDueLocked starts a poll when one is due (browser sign-in, signed in, not paused, the
// setting on, not given up for this session).
func (e *Engine) srvNotesDueLocked() {
	if e.snBusy || e.snOff || e.quitting || e.cfgErr != nil || e.signedOut || e.cfg.Paused ||
		!e.cfg.OIDC() || !e.cfg.ServerNotesOn() || time.Now().Before(e.snAt) {
		return
	}
	e.snBusy = true
	gen := e.snGen
	go func() {
		defer func() {
			e.mu.Lock()
			e.snBusy = false
			e.mu.Unlock()
		}()
		e.pollServerNotes(gen)
	}()
}

// srvNotesFailed schedules the next poll after an error (unless the state was reset meanwhile).
func (e *Engine) srvNotesFailed(gen uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if gen != e.snGen {
		return
	}
	e.snFails++
	e.snAt = time.Now().Add(srvNotesBackoff(e.snFails))
}

// pollServerNotes fetches the notifications once and shows the new ones.
func (e *Engine) pollServerNotes(gen uint64) {
	cfg := e.config()
	e.mu.Lock()
	if gen != e.snGen {
		e.mu.Unlock()
		return
	}
	// Another account or server than the seen ids are from: forget those first.
	if id := quotaIdentity(cfg); id != "" && e.snID != id {
		e.snID = id
		e.resetSrvNotesLocked(cfg)
		gen = e.snGen
	}
	e.mu.Unlock()
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		e.srvNotesFailed(gen)
		return
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		e.srvNotesFailed(gen)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := fetchSrvNotes(ctx, base, tok)
	if errors.Is(err, errSrvNotesGone) {
		e.mu.Lock()
		if gen == e.snGen {
			e.snOff = true // quietly: this server has no such service
		}
		e.mu.Unlock()
		return
	}
	if err != nil {
		appLog("OpenCloud notifications: %v", err)
		e.srvNotesFailed(gen)
		return
	}
	e.applySrvNotes(gen, cfg, got, time.Now())
}

// applySrvNotes records a fetch and shows what is new. A fetch from before a reset (gen no
// longer current) is dropped.
func (e *Engine) applySrvNotes(gen uint64, cfg Config, got []srvNote, now time.Time) {
	e.mu.Lock()
	if gen != e.snGen {
		e.mu.Unlock()
		return
	}
	file := srvNotesFile(cfg)
	if !e.snLoaded {
		e.snSeen, e.snLoaded = readSrvSeen(file), true
	}
	show, next := planSrvNotes(e.snSeen, got)
	// Written under the lock, so no file comes back after a reset removed it. When it can't be
	// remembered nothing is shown: it would be shown again at every poll.
	if err := writeSrvSeen(file, next.IDs); err != nil {
		e.mu.Unlock()
		appLog("OpenCloud notifications not shown (the seen ones couldn't be remembered): %v", err)
		e.srvNotesFailed(gen)
		return
	}
	e.snSeen, e.snFails, e.snAt = next, 0, now.Add(srvNotesEvery)
	on := e.cfg.NotificationsOn() && e.cfgErr == nil
	e.mu.Unlock()
	if len(show) == 0 || !on {
		return // with notifications off they are recorded: turning them on shows no flood
	}
	e.logLine("OpenCloud notifications: %d new", len(show))
	for _, n := range srvToasts(show) { // this runs in the poll's own goroutine: no tick waits for it
		if err := showNotification(n.Title, n.Body); err != nil {
			appLog("notification not shown: %v", err)
		}
	}
}
