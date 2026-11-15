package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Recent Changes: what each successful sync did, read from rclone bisync's INFO output, kept
// in activity.json in the sync state folder (the last maxChanges). The macOS app does the same
// (Changes.swift). The lines bisync v1.76 writes (checked with a local bisync):
//
//	INFO  : Synching Path1 "/home/sam/OpenCloud/Docs/" with Path2 ":webdav{1sYgV}:Docs/"
//	INFO  : - Path1    Queue copy to Path2       - :webdav{1sYgV}:Docs/new file.txt
//	INFO  : - Path2    Queue copy to Path1       - /home/sam/OpenCloud/Docs/fromb.txt
//	INFO  : - Path2    Queue delete              - :webdav{1sYgV}:Docs/old.txt
//	INFO  : - Path1    Queue delete              - /home/sam/OpenCloud/Docs/del.txt
//
// A copy names its destination; a delete names the side it removes from. Path1 is this
// computer, Path2 OpenCloud. bisync doesn't report renames or moves: a delete and a copy in the
// same direction of a file with the same name in another folder (and no other such pair) are
// shown as one move. A file renamed in place shows as a new file and a deleted one: bisync
// gives nothing to tell that apart from two separate changes. A merge (first sync, Repair, automatic merge) copies in bulk: it is
// one summary line instead.

const maxChanges = 500

// Change is one line of Recent Changes.
type Change struct {
	Time   time.Time `json:"time"`
	Folder string    `json:"folder"` // KeptFolder.Key()
	Name   string    `json:"name"`   // the folder's display name
	Path   string    `json:"path"`   // relative to the kept folder ("" for a summary)
	From   string    `json:"from,omitempty"`
	// Action: uploaded, downloaded, deleted here (so removed from OpenCloud), deleted in
	// OpenCloud (so removed here), renamed here, renamed in OpenCloud, or summary.
	Action string `json:"action"`
	Text   string `json:"text,omitempty"` // a summary's words
}

const (
	actUploaded       = "uploaded"
	actDownloaded     = "downloaded"
	actDeletedHere    = "deleted here"
	actDeletedRemote  = "deleted in OpenCloud"
	actRenamedHere    = "renamed here"
	actRenamedRemote  = "renamed in OpenCloud"
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

// parseChanges reads one successful bisync run's output into changes.
func parseChanges(out, kind string, f KeptFolder, now time.Time) []Change {
	out = reANSI.ReplaceAllString(out, "")
	if kind != "sync" {
		return mergeSummary(out, kind, f, now)
	}
	var root1, root2 string
	type ev struct{ action, path string }
	var evs []ev
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if m := reSynching.FindStringSubmatch(l); m != nil {
			root1, root2 = m[1], m[2]
			continue
		}
		strip := func(side, p string) string {
			root := root1
			if side == "Path2" {
				root = root2
			}
			if root != "" && strings.HasPrefix(p, root) {
				return strings.TrimPrefix(p, root)
			}
			return ""
		}
		if m := reQueueCopy.FindStringSubmatch(l); m != nil {
			act := actUploaded
			if m[1] == "Path1" {
				act = actDownloaded
			}
			if p := strip(m[1], m[2]); !skipChange(p) {
				evs = append(evs, ev{act, p})
			}
			continue
		}
		if m := reQueueDel.FindStringSubmatch(l); m != nil {
			act := actDeletedHere // removed from OpenCloud: it was deleted here
			if m[1] == "Path1" {
				act = actDeletedRemote
			}
			if p := strip(m[1], m[2]); !skipChange(p) {
				evs = append(evs, ev{act, p})
			}
		}
	}
	// Renames: a delete and a copy going the same way.
	used := map[int]bool{}
	pair := func(del, cp string) map[int]int {
		out := map[int]int{}
		for i, d := range evs {
			if d.action != del || used[i] {
				continue
			}
			var match []int
			for j, c := range evs {
				if c.action == cp && !used[j] && path.Base(c.path) == path.Base(d.path) && c.path != d.path {
					match = append(match, j)
				}
			}
			if len(match) == 1 {
				out[i] = match[0]
				used[i], used[match[0]] = true, true
			}
		}
		return out
	}
	here := pair(actDeletedHere, actUploaded)
	remote := pair(actDeletedRemote, actDownloaded)
	var res []Change
	for i, e := range evs {
		if j, ok := here[i]; ok {
			res = append(res, Change{Time: now, Folder: f.Key(), Name: f.DisplayName(), Path: evs[j].path, From: e.path, Action: actRenamedHere})
			continue
		}
		if j, ok := remote[i]; ok {
			res = append(res, Change{Time: now, Folder: f.Key(), Name: f.DisplayName(), Path: evs[j].path, From: e.path, Action: actRenamedRemote})
			continue
		}
		if used[i] {
			continue // the copy half of a rename
		}
		res = append(res, Change{Time: now, Folder: f.Key(), Name: f.DisplayName(), Path: e.path, Action: e.action})
	}
	return res
}

// mergeSummary is a merge's one line: "Repair of Docs: 512 files compared, 2 updated".
func mergeSummary(out, kind string, f KeptFolder, now time.Time) []Change {
	copied := 0
	for _, l := range strings.Split(out, "\n") {
		if m := reCopied.FindStringSubmatch(l); m != nil && !skipChange(m[1]) {
			copied++
		}
	}
	compared := -1
	for _, m := range reChecks.FindAllStringSubmatch(out, -1) {
		compared, _ = strconv.Atoi(m[2])
	}
	what := "Merge"
	switch {
	case strings.Contains(kind, "repair"):
		what = "Repair"
	case strings.Contains(kind, "first sync"):
		what = "First sync"
	case strings.Contains(kind, "excludes"):
		what = "Sync after the excludes changed"
	}
	text := fmt.Sprintf("%s of %s: %d updated", what, f.DisplayName(), copied)
	if compared >= 0 {
		text = fmt.Sprintf("%s of %s: %d files compared, %d updated", what, f.DisplayName(), compared, copied)
	}
	return []Change{{Time: now, Folder: f.Key(), Name: f.DisplayName(), Action: actSummary, Text: text}}
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
		_ = json.Unmarshal(b, &list)
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
	if b, err := json.Marshal(list); err == nil {
		_ = os.MkdirAll(expand(cfg.StateDir), 0o700)
		tmp := changesFile(cfg) + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, changesFile(cfg))
		}
	}
}

// forgetChanges drops a folder's changes (Stop Keeping).
func forgetChanges(cfg Config, key string) {
	changesMu.Lock()
	defer changesMu.Unlock()
	var keep []Change
	for _, c := range loadChangesLocked(cfg) {
		if c.Folder != key {
			keep = append(keep, c)
		}
	}
	changesCache = append([]Change{}, keep...)
	if b, err := json.Marshal(changesCache); err == nil {
		_ = os.WriteFile(changesFile(cfg), b, 0o600)
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
	switch c.Action {
	case actSummary:
		return c.Text
	case actRenamedHere, actRenamedRemote:
		return fmt.Sprintf("%s: %s → %s (%s)", c.Name, c.From, c.Path, c.Action)
	}
	return fmt.Sprintf("%s: %s %s", c.Name, c.Path, c.Action)
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
		if part == "." || part == ".." || hasControl(part) || strings.ContainsAny(part, `\`) {
			return "", fmt.Errorf("not a path in the folder")
		}
		p = filepath.Join(p, part)
	}
	return p, nil
}
