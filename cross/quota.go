package main

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Storage quota warnings: the personal space's quota, as GET /graph/v1.0/me/drives gives it
// (libre-graph Drive.quota: used, total, remaining, state; total missing or 0 means no limit),
// checked at most every 30 minutes from the engine's tick (a failed check is tried again after
// 2 minutes). A warning is given once when the
// storage reaches 90% and once when it is full (100%); it is given again only after the use
// went below 90%. The last level warned about is kept in the sync state folder
// (<stateDir>/quota-warned), so a restart doesn't repeat it. Signing out, signing in as
// someone else or changing the server forgets the shown quota and that file, and the next
// check runs at once. The macOS app does the same (Quota.swift).

const (
	quotaEvery = 30 * time.Minute
	quotaRetry = 2 * time.Minute // after a failed check
)

// quotaLevel is the warning level the quota is at: 0, 90 or 100. No limit: 0.
func quotaLevel(q *quotaInfo) int {
	if q == nil || q.Total <= 0 || q.Used < 0 {
		return 0
	}
	switch {
	case q.Used >= q.Total:
		return 100
	case float64(q.Used) >= 0.9*float64(q.Total):
		return 90
	}
	return 0
}

// nextQuotaWarning: given the level last warned about, which warning to give now (0: none)
// and the level to remember. A reading below 90% (or without a limit) re-arms both warnings;
// no reading at all (q == nil) keeps the remembered level.
func nextQuotaWarning(last int, q *quotaInfo) (warn, remember int) {
	if q == nil {
		return 0, last
	}
	lvl := quotaLevel(q)
	switch {
	case lvl == 0:
		return 0, 0
	case lvl > last:
		return lvl, lvl
	}
	return 0, last
}

// quotaText is "1.2 GB of 10 GB used".
func quotaText(q quotaInfo) string {
	if q.Total <= 0 {
		return humanSize(q.Used) + " used (no limit)"
	}
	return humanSize(q.Used) + " of " + humanSize(q.Total) + " used"
}

// humanSize is a size like the settings page writes it (1024-based).
func humanSize(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatInt(n, 10) + " B"
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " " + units[i]
}

// quotaNote is the warning for level.
func quotaNote(level int, q quotaInfo) note {
	if level >= 100 {
		return note{"Your OpenCloud storage is full", quotaText(q) + ". New files and changes can't be uploaded until you free up space (empty the trash in OpenCloud too) or get more storage.", "account"}
	}
	return note{"Your OpenCloud storage is almost full", quotaText(q) + ". When it is full, new files and changes can't be uploaded.", "account"}
}

func quotaWarnedFile(cfg Config) string { return filepath.Join(expand(cfg.StateDir), "quota-warned") }

func readQuotaWarned(file string) int {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || (n != 90 && n != 100) {
		return 0
	}
	return n
}

func writeQuotaWarned(file string, level int) error {
	if level == 0 {
		err := os.Remove(file)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(file, []byte(strconv.Itoa(level)+"\n"), 0o600)
}

// fetchQuota reads the personal space's quota (nil: the server gave none).
func fetchQuota(ctx context.Context, base, tok string) (*quotaInfo, error) {
	var drives struct {
		Value []struct {
			DriveType string     `json:"driveType"`
			Quota     *quotaInfo `json:"quota"`
		} `json:"value"`
	}
	if err := ocGet(ctx, base, "/graph/v1.0/me/drives", url.Values{"$filter": {"driveType eq 'personal'"}}, tok, &drives); err != nil {
		return nil, err
	}
	for _, d := range drives.Value {
		if d.DriveType == "personal" || d.DriveType == "" {
			return d.Quota, nil
		}
	}
	return nil, nil
}

// quotaDueLocked starts a quota check when one is due (browser sign-in, signed in).
func (e *Engine) quotaDueLocked() {
	if e.quotaBusy || !e.cfg.OIDC() || e.signedOut || time.Since(e.quotaAt) < quotaEvery {
		return
	}
	e.quotaBusy, e.quotaAt = true, time.Now()
	gen := e.quotaGen
	go func() {
		defer func() {
			e.mu.Lock()
			e.quotaBusy = false
			e.mu.Unlock()
		}()
		e.checkQuota(gen)
	}()
}

// quotaIdentity says whose quota it is: the server and the signed-in account.
func quotaIdentity(cfg Config) string {
	base, _ := normalizeServerURL(cfg.ServerURL)
	st, err := readAuthState()
	if err != nil {
		return base
	}
	return base + "|" + st.account()
}

// resetQuotaLocked forgets the shown quota and what was warned about, so the next check runs
// at once (sign-out, sign-in, another server or account). A check still running for the old
// state is dropped when it ends.
func (e *Engine) resetQuotaLocked(cfgs ...Config) {
	e.quotaShown, e.quotaAt = nil, time.Time{}
	e.quotaGen++
	for _, c := range cfgs {
		if c.StateDir == "" {
			continue // never loaded: quotaWarnedFile would be a relative "quota-warned"
		}
		_ = os.Remove(quotaWarnedFile(c))
	}
	e.resetSrvNotesLocked(cfgs...) // the notifications seen belong to the same sign-in
}

// quotaFailed schedules the retry after a failed check (unless the state was reset meanwhile).
func (e *Engine) quotaFailed(gen uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if gen == e.quotaGen {
		e.quotaAt = time.Now().Add(quotaRetry - quotaEvery)
	}
}

// Test hooks (nil otherwise): after checkQuota captured its config, and after applyQuotaFor
// read the warned file.
var afterQuotaConfig, afterQuotaRead func()

// checkQuota reads the quota and warns when it crossed a level.
func (e *Engine) checkQuota(gen uint64) {
	cfg := e.config()
	if afterQuotaConfig != nil {
		afterQuotaConfig()
	}
	// Another account or server than the quota on hand is from: forget that one first. A
	// reset since this check began (gen moved) means cfg may be the old server's: give up,
	// the next check starts afresh.
	e.mu.Lock()
	if gen != e.quotaGen {
		e.mu.Unlock()
		return
	}
	if id := quotaIdentity(cfg); id != "" && e.quotaID != id {
		e.quotaID = id
		e.resetQuotaLocked(cfg)
		e.quotaAt = time.Now() // this check is the one that was due
		gen = e.quotaGen
	}
	e.mu.Unlock()
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		e.quotaFailed(gen)
		return
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		e.quotaFailed(gen)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	q, err := fetchQuota(ctx, base, tok)
	if err != nil {
		appLog("storage check: %v", err)
		e.quotaFailed(gen)
		return
	}
	e.applyQuotaFor(gen, cfg, q, time.Now())
}

// applyQuota applies a reading to the current state.
func (e *Engine) applyQuota(cfg Config, q *quotaInfo, now time.Time) {
	e.mu.Lock()
	gen := e.quotaGen
	e.mu.Unlock()
	e.applyQuotaFor(gen, cfg, q, now)
}

// applyQuotaFor remembers the quota for the status and gives a warning when one is due. A
// reading from before a reset (gen no longer current) is dropped.
func (e *Engine) applyQuotaFor(gen uint64, cfg Config, q *quotaInfo, now time.Time) {
	e.mu.Lock()
	if gen != e.quotaGen {
		e.mu.Unlock()
		return
	}
	e.quotaShown = q
	on := e.cfg.NotificationsOn() && e.cfgErr == nil
	e.mu.Unlock()
	file := quotaWarnedFile(cfg)
	last := readQuotaWarned(file)
	warn, remember := nextQuotaWarning(last, q)
	if afterQuotaRead != nil {
		afterQuotaRead()
	}
	// Write under the lock, and only if no reset came in since the generation check above.
	e.mu.Lock()
	if gen != e.quotaGen {
		e.mu.Unlock()
		return
	}
	if remember != last {
		if err := writeQuotaWarned(file, remember); err != nil && warn > 0 {
			e.mu.Unlock()
			appLog("storage warning not given (it couldn't be remembered): %v", err)
			e.notify()
			return // don't risk repeating it every half hour
		}
	}
	e.mu.Unlock()
	if warn > 0 {
		n := quotaNote(warn, *q)
		e.logLine("storage warning: %s", quotaText(*q))
		addChanges(cfg, []Change{{Time: now, Name: "OpenCloud Bar", Action: actSummary, Text: n.Title + ": " + n.Body}})
		if on {
			go func() {
				if err := showNotification(n.Title, n.Body); err != nil {
					appLog("notification not shown: %v", err)
				}
			}()
		}
	}
	e.notify()
}
