package main

import (
	"errors"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// How a bisync run ended, read from rclone's output. The macOS app classifies the same way
// (BisyncOutcome in AppModel.swift).
type syncOutcome int

const (
	outcomeOK             syncOutcome = iota
	outcomeInterrupted                // quit, sleep or a timeout: --recover picks up on the next run
	outcomeDrift                      // listings say the two sides differ ("out of sync"): a merge fixes it
	outcomeNeedsResync                // sync history unusable for another reason (listings lost)
	outcomeFiltersChanged             // the filters file changed since the last sync: rclone wants a resync
	outcomeBigChange                  // safety abort: too many deletes, or all files changed on one side
	outcomeSignedOut                  // the token command said the sign-in is gone
	outcomeFailed                     // anything else: retried on the next run
)

// The path follows the first " - " after Path2's value (paths may contain " - " themselves).
var driftPathRe = regexp.MustCompile(`not equal in listing\..*?Path2: .*? - (.+?)\s*$`)

// bisyncResult is what classifyBisync found.
type bisyncResult struct {
	outcome syncOutcome
	path    string // drift: the first file whose listings differ
	side    string // big change: "Path1" (this computer) or "Path2" (OpenCloud), if rclone said
	timeout bool   // interrupted because it ran too long (not quit or sleep)
}

// classifyBisync sorts a failed run. Order matters: a run that drifted keeps printing
// "retryable without --resync" (--recover falls back to the old listings, which differ the
// same way), so drift is checked before the retryable case.
func classifyBisync(out string, timedOut bool) bisyncResult {
	switch {
	case strings.Contains(out, errSignedOut.Error()):
		return bisyncResult{outcome: outcomeSignedOut}
	// rclone's own words when it gets SIGINT/SIGTERM (file names are on other parts of the
	// line, so only whole phrases are matched).
	case timedOut, strings.Contains(out, "Signal received: "), strings.Contains(out, "Attempting to gracefully shutdown"),
		strings.Contains(out, "Bisync interrupted"), strings.Contains(out, "context canceled"):
		return bisyncResult{outcome: outcomeInterrupted, timeout: timedOut}
	case strings.Contains(out, "out of sync") || strings.Contains(out, "not equal in listing"):
		r := bisyncResult{outcome: outcomeDrift}
		for _, line := range strings.Split(out, "\n") {
			if m := driftPathRe.FindStringSubmatch(line); m != nil {
				r.path = m[1]
				break
			}
		}
		return r
	case strings.Contains(out, "Safety abort") || strings.Contains(out, "too many deletes") ||
		strings.Contains(out, "all files were changed"):
		r := bisyncResult{outcome: outcomeBigChange}
		if i := strings.Index(out, "Safety abort"); i >= 0 {
			line, _, _ := strings.Cut(out[i:], "\n")
			if strings.Contains(line, "Path2") {
				r.side = "Path2"
			} else if strings.Contains(line, "Path1") {
				r.side = "Path1"
			}
		}
		return r
	// rclone v1.76 says "filters file has changed (must run --resync)" and, with --resilient,
	// "retryable without --resync" too: retrying would fail forever, so check this first.
	case strings.Contains(out, "filters file has changed"):
		return bisyncResult{outcome: outcomeFiltersChanged}
	case strings.Contains(out, "retryable without --resync"):
		return bisyncResult{outcome: outcomeFailed}
	case strings.Contains(out, "Must run --resync") || strings.Contains(out, "--resync to recover"):
		return bisyncResult{outcome: outcomeNeedsResync}
	}
	return bisyncResult{outcome: outcomeFailed}
}

// autoMergeWindow is how often a folder may be merged automatically after listing drift.
const autoMergeWindow = 24 * time.Hour

// canAutoMerge reports whether the folder whose marker file is path had no automatic merge in
// the last 24 hours. The time is kept on disk so a restart doesn't reset the limit.
func canAutoMerge(path string, now time.Time) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return true
	}
	// A time in the future (clock changed) counts as "just now" until it is 24 h old.
	return now.Sub(t) >= autoMergeWindow || t.Sub(now) > autoMergeWindow
}

func recordAutoMerge(path string, now time.Time) error {
	return os.WriteFile(path, []byte(now.UTC().Format(time.RFC3339)), 0o600)
}

// driftReason is the needs-repair text when the sides stay out of sync; note says what to do.
func driftReason(path, note string) string {
	what := "files keep changing on both sides"
	if path != "" {
		what = "“" + pathpkg.Base(path) + "” keeps changing on both sides"
	}
	return what + "; " + note
}

const missingRemoteText = "Folder is missing or empty in OpenCloud"

// missingLocalText is shown when this computer's copy vanished (an unplugged disk, a moved
// folder). Never offer the large-deletion override then: it would empty the server copy.
func missingLocalText(name string) string {
	return "This computer's copy of “" + name + "” is empty or missing. If it is on a disk, reconnect it; " +
		"to stop syncing it here instead, choose Stop Keeping (the folder in OpenCloud stays as it is)"
}

// localMissingOrEmpty is true when dir doesn't exist or holds no files (empty folders and
// .DS_Store don't count). It stops at the first real file.
func localMissingOrEmpty(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return true
	}
	found := errors.New("found")
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable parts: keep looking elsewhere
		}
		if !d.IsDir() && d.Name() != ".DS_Store" {
			return found
		}
		return nil
	})
	return err != found
}

// Merges (first sync, Repair, automatic merge) keep every copy they overwrite: the older copy
// goes to a dated backup folder, on this computer under the state folder and in OpenCloud
// under backupFolderName at the top of the drive. Local backups are pruned after 30 days.
const (
	backupFolderName = ".opencloud-bar-backups"
	backupKeep       = 30 * 24 * time.Hour
	backupStampFmt   = "20060102T150405Z"
)

// backupFilter keeps the backup folder out of every sync.
const backupFilter = "- " + backupFolderName + "/**"

// onlyAppFilterChanged is true when two filters files differ only in the line the app adds
// itself (the backup folder, new in 1.0): the user's excludes are the same. An upgrade then
// merges once on its own instead of asking for Repair.
func onlyAppFilterChanged(old, new string) bool {
	strip := func(s string) string {
		var keep []string
		for _, l := range strings.Split(s, "\n") {
			if strings.TrimSpace(l) != backupFilter {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	return old != new && strip(old) == strip(new)
}

func backupStamp(now time.Time) string { return now.UTC().Format(backupStampFmt) }

// localBackupDir is <stateDir>/backups/<stateName>/<stamp>.
func localBackupDir(stateDir, stateName, stamp string) string {
	return filepath.Join(expand(stateDir), "backups", stateName, stamp)
}

// remoteBackupDir is <remote root>/.opencloud-bar-backups/<stateName>/<stamp> (the same remote
// as the folder, outside it, as rclone requires).
func remoteBackupDir(remoteSpec, stateName, stamp string) string {
	return remoteSpec + backupFolderName + "/" + stateName + "/" + stamp
}

// pruneBackups removes this computer's backups of a folder older than 30 days (by the stamp
// in their name; folders with other names are left alone).
func pruneBackups(stateDir, stateName string, now time.Time) {
	dir := filepath.Join(expand(stateDir), "backups", stateName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, en := range entries {
		t, err := time.Parse(backupStampFmt, en.Name())
		if err == nil && en.IsDir() && now.Sub(t) > backupKeep {
			_ = os.RemoveAll(filepath.Join(dir, en.Name()))
		}
	}
}
