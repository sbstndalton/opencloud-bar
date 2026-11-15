package main

import (
	"os"
	pathpkg "path"
	"regexp"
	"strings"
	"time"
)

// How a bisync run ended, read from rclone's output. The macOS app classifies the same way
// (BisyncOutcome in AppModel.swift).
type syncOutcome int

const (
	outcomeOK          syncOutcome = iota
	outcomeInterrupted             // quit, sleep or a timeout: --recover picks up on the next run
	outcomeDrift                   // listings say the two sides differ ("out of sync"): a merge fixes it
	outcomeNeedsResync             // sync history unusable for another reason (filters changed, listings lost)
	outcomeBigChange               // safety abort: too many deletes, or all files changed on one side
	outcomeSignedOut               // the token command said the sign-in is gone
	outcomeFailed                  // anything else: retried on the next run
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
