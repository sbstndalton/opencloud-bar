package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Recent Changes: what each successful sync did, read from rclone bisync's INFO output as it
// arrives (changeCollector; nothing is buffered beyond what is recorded), kept in activity.json
// in the sync state folder (the last maxChanges). The macOS app does the same (Changes.swift).
// The lines bisync v1.76 writes (checked with a local bisync):
//
//	INFO  : Synching Path1 "/home/sam/OpenCloud/Docs/" with Path2 ":webdav{1sYgV}:Docs/"
//	INFO  : - Path1    Queue copy to Path2       - :webdav{1sYgV}:Docs/new file.txt
//	INFO  : - Path2    Queue copy to Path1       - /home/sam/OpenCloud/Docs/fromb.txt
//	INFO  : - Path2    Queue delete              - :webdav{1sYgV}:Docs/old.txt
//	INFO  : - Path1    Queue delete              - /home/sam/OpenCloud/Docs/del.txt
//
// A copy names its destination; a delete names the side it removes from. Path1 is this
// computer, Path2 OpenCloud. bisync doesn't report renames or moves, and guessing them hides
// real deletions, so a rename shows as what bisync did: a new file and a deleted one. A merge
// (first sync, Repair, automatic merge) copies in bulk: it is one summary line instead, and so
// is a sync that changed more than bulkChanges files.

const (
	maxChanges  = 500
	bulkChanges = 200
)

// Change is one line of Recent Changes.
type Change struct {
	Time   time.Time `json:"time"`
	Folder string    `json:"folder"` // KeptFolder.Key()
	Name   string    `json:"name"`   // the folder's display name
	Path   string    `json:"path"`   // relative to the kept folder ("" for a summary)
	// Action: uploaded, downloaded, deleted here (so removed from OpenCloud), deleted in
	// OpenCloud (so removed here), or summary.
	Action string `json:"action"`
	Text   string `json:"text,omitempty"` // a summary's words
	// Trigger: what started the sync, "timer" or "local change" (live sync); "" for merges.
	Trigger string `json:"trigger,omitempty"`
}

const (
	actUploaded       = "uploaded"
	actDownloaded     = "downloaded"
	actDeletedHere    = "deleted here"
	actDeletedRemote  = "deleted in OpenCloud"
	actSummary        = "summary"
	changesFileName   = "activity.json"
	conflictCopyRegex = `\.conflict[0-9]+(\.[^/]*)?$`
)

var (
	reSynching   = regexp.MustCompile(`Synching Path1 "(.*)" with Path2 "(.*)"\s*$`)
	reQueueCopy  = regexp.MustCompile(`- Path[12]\s+Queue copy to (Path[12])\s+- (.*?)\s*$`)
	reQueueDel   = regexp.MustCompile(`- (Path[12])\s+Queue delete\s+- (.*?)\s*$`)
	reCopied     = regexp.MustCompile(`INFO  : (.+): Copied \(`)
	reChecks     = regexp.MustCompile(`Checks:\s+([0-9]+) / ([0-9]+)`)
	reConflict   = regexp.MustCompile(conflictCopyRegex)
	reANSI       = regexp.MustCompile("\x1b\\[[0-9;]*m")
	changesMu    sync.Mutex
	changesCache []Change
	changesKey   string
)

// skipChange: conflict copies and the app's own backup folder are noise here.
func skipChange(p string) bool {
	return p == "" || reConflict.MatchString(p) || p == backupFolderName || strings.HasPrefix(p, backupFolderName+"/")
}

// changeCollector reads bisync's output line by line (statsFilter.onLine) and keeps only what
// Recent Changes records: at most bulkChanges events, and counts.
type changeCollector struct {
	mu           sync.Mutex
	root1, root2 string
	events       []Change
	counts       map[string]int
	copied       int
	compared     int
	wrote        []string // paths written here (downloads, deletions from OpenCloud): live sync
	wroteOver    bool
}

func newChangeCollector() *changeCollector {
	return &changeCollector{counts: map[string]int{}, compared: -1}
}

// slash: rclone writes this computer's paths with \ on Windows; Recent Changes uses /.
func slash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

func (c *changeCollector) line(l string) {
	l = strings.TrimRight(reANSI.ReplaceAllString(l, ""), "\r\n")
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := reSynching.FindStringSubmatch(l); m != nil {
		c.root1, c.root2 = slash(m[1]), m[2]
		return
	}
	if m := reChecks.FindStringSubmatch(l); m != nil {
		c.compared, _ = strconv.Atoi(m[2])
		return
	}
	if m := reCopied.FindStringSubmatch(l); m != nil {
		if !skipChange(slash(m[1])) {
			c.copied++
		}
		return
	}
	add := func(side, p, act string) {
		root := c.root1
		if side == "Path2" {
			root = c.root2
		} else {
			p = slash(p)
		}
		if root == "" || !strings.HasPrefix(p, root) {
			return
		}
		p = strings.TrimPrefix(p, root)
		if skipChange(p) {
			return
		}
		c.counts[act]++
		if act == actDownloaded || act == actDeletedRemote {
			if len(c.wrote) < liveWroteCap {
				c.wrote = append(c.wrote, p)
			} else {
				c.wroteOver = true
			}
		}
		if len(c.events) < bulkChanges+1 {
			c.events = append(c.events, Change{Path: p, Action: act})
		}
	}
	if m := reQueueCopy.FindStringSubmatch(l); m != nil {
		act := actUploaded
		if m[1] == "Path1" {
			act = actDownloaded
		}
		add(m[1], m[2], act)
		return
	}
	if m := reQueueDel.FindStringSubmatch(l); m != nil {
		act := actDeletedHere // removed from OpenCloud: it was deleted here
		if m[1] == "Path1" {
			act = actDeletedRemote
		}
		add(m[1], m[2], act)
	}
}

// localWrites: what the run wrote on this computer, as far as its output says.
func (c *changeCollector) localWrites() ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.wrote...), c.wroteOver
}

// result is what the run changed: one line per file, or one summary line for a merge or a
// sync that changed more than bulkChanges files.
func (c *changeCollector) result(kind string, f KeptFolder, now time.Time) []Change {
	c.mu.Lock()
	defer c.mu.Unlock()
	if kind != "sync" {
		what := "Merge"
		switch {
		case strings.Contains(kind, "repair"):
			what = "Repair"
		case strings.Contains(kind, "first sync"):
			what = "First sync"
		case strings.Contains(kind, "excludes"):
			what = "Sync after the excludes changed"
		}
		text := fmt.Sprintf("%s of %s: %d updated", what, f.DisplayName(), c.copied)
		if c.compared >= 0 {
			text = fmt.Sprintf("%s of %s: %d files compared, %d updated", what, f.DisplayName(), c.compared, c.copied)
		}
		return []Change{{Time: now, Folder: f.Key(), Name: f.DisplayName(), Action: actSummary, Text: text}}
	}
	total := 0
	for _, n := range c.counts {
		total += n
	}
	if total > bulkChanges {
		var parts []string
		for _, a := range []string{actUploaded, actDownloaded, actDeletedHere, actDeletedRemote} {
			if n := c.counts[a]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, actionText(a)))
			}
		}
		return []Change{{Time: now, Folder: f.Key(), Name: f.DisplayName(), Action: actSummary,
			Text: fmt.Sprintf("Sync of %s: %s", f.DisplayName(), strings.Join(parts, ", "))}}
	}
	out := make([]Change, 0, len(c.events))
	for _, e := range c.events {
		e.Time, e.Folder, e.Name = now, f.Key(), f.DisplayName()
		out = append(out, e)
	}
	return out
}

// parseChanges reads a whole run's output (tests; the engine streams it).
func parseChanges(out, kind string, f KeptFolder, now time.Time) []Change {
	c := newChangeCollector()
	for _, l := range strings.Split(out, "\n") {
		c.line(l)
	}
	return c.result(kind, f, now)
}

// actionText is an action in words, for people.
func actionText(a string) string {
	switch a {
	case actDeletedHere:
		return "deleted on this computer (also removed from OpenCloud)"
	case actDeletedRemote:
		return "deleted in OpenCloud (also removed here)"
	}
	return a
}

func changesFile(cfg Config) string { return filepath.Join(expand(cfg.StateDir), changesFileName) }

// loadChanges reads the stored changes, newest last.
func loadChanges(cfg Config) []Change {
	changesMu.Lock()
	defer changesMu.Unlock()
	return append([]Change(nil), loadChangesLocked(cfg)...)
}

func loadChangesLocked(cfg Config) []Change {
	file := changesFile(cfg)
	if changesKey == file && changesCache != nil {
		return changesCache
	}
	var list []Change
	if b, err := os.ReadFile(file); err == nil {
		if json.Unmarshal(b, &list) != nil {
			// Damaged (or written by something else): keep it aside, start a new list.
			_ = os.Rename(file, file+".bad")
			list = nil
		}
	}
	if len(list) > maxChanges {
		list = list[len(list)-maxChanges:]
	}
	changesCache, changesKey = list, file
	if changesCache == nil {
		changesCache = []Change{}
	}
	return changesCache
}

// addChanges stores new changes, keeping the last maxChanges.
func addChanges(cfg Config, add []Change) {
	if len(add) == 0 {
		return
	}
	changesMu.Lock()
	defer changesMu.Unlock()
	list := append(append([]Change(nil), loadChangesLocked(cfg)...), add...)
	if len(list) > maxChanges {
		list = list[len(list)-maxChanges:]
	}
	changesCache = list
	writeChangesLocked(cfg)
}

// forgetChanges drops a folder's changes (Stop Keeping).
func forgetChanges(cfg Config, key string) {
	changesMu.Lock()
	defer changesMu.Unlock()
	keep := []Change{}
	for _, c := range loadChangesLocked(cfg) {
		if c.Folder != key {
			keep = append(keep, c)
		}
	}
	changesCache = keep
	writeChangesLocked(cfg)
}

// writeChangesLocked writes the list atomically (a temporary file, then a rename).
func writeChangesLocked(cfg Config) {
	b, err := json.Marshal(changesCache)
	if err != nil {
		return
	}
	_ = os.MkdirAll(expand(cfg.StateDir), 0o700)
	tmp := changesFile(cfg) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		if os.Rename(tmp, changesFile(cfg)) != nil {
			_ = os.Remove(tmp)
		}
	}
}

// RecentChanges is the newest n changes first (all for n <= 0).
func (e *Engine) RecentChanges(n int) []Change {
	list := loadChanges(e.config())
	out := make([]Change, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i])
		if n > 0 && len(out) == n {
			break
		}
	}
	return out
}

// ChangeLabel is a change in a few words, for the tray and the page.
func ChangeLabel(c Change) string {
	if c.Action == actSummary {
		return c.Text
	}
	s := fmt.Sprintf("%s: %s %s", c.Name, c.Path, actionText(c.Action))
	if c.Trigger == "local change" {
		s += " (after a local change)"
	}
	return s
}

// pageChange is a change for the settings page: with its words and whether it is still there.
type pageChange struct {
	Change
	Label  string `json:"label"`
	Exists bool   `json:"exists"`
}

func (e *Engine) changesForPage() []pageChange {
	out := []pageChange{}
	for _, c := range e.RecentChanges(0) {
		pc := pageChange{Change: c, Label: ChangeLabel(c)}
		if c.Path != "" && c.Action != actDeletedHere && c.Action != actDeletedRemote {
			if p, err := e.itemLocal(c.Folder, c.Path); err == nil && exists(p) {
				pc.Exists = true
			}
		}
		out = append(out, pc)
	}
	return out
}

// itemLocal is where rel of a kept folder is on this computer (never outside the folder).
func (e *Engine) itemLocal(key, rel string) (string, error) {
	f, ok := e.find(key)
	if !ok {
		return "", fmt.Errorf("%q isn't a kept folder", key)
	}
	p := e.config().Local(f)
	for _, part := range strings.Split(rel, "/") {
		if part == "" {
			continue
		}
		if part == "." || part == ".." || hasControl(part) || strings.ContainsAny(part, `\:`) {
			return "", fmt.Errorf("not a path in the folder")
		}
		p = filepath.Join(p, part)
	}
	return p, nil
}
