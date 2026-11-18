package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Conflicts. Kept folders sync with --conflict-resolve newer --conflict-loser num: when both
// sides changed a file, the newer copy keeps the name and the other is renamed. rclone v1.76
// appends ".conflict<N>" to the whole name ("report.pdf" -> "report.pdf.conflict1", checked
// with a local bisync). Those copies sync to both sides like any file, so the app lists them
// and lets the user keep one or discard it. The macOS app does the same (Conflicts.swift).

// conflictRe matches a conflict loser's base name; group 1 is the winner's name.
var conflictRe = regexp.MustCompile(`^(.+)\.conflict([1-9][0-9]*)$`)

// Conflict is one losing copy, relative to the kept folder's local copy (slash-separated).
type Conflict struct {
	File     string    `json:"file"`     // the losing copy, e.g. "Notes/todo.txt.conflict1"
	Original string    `json:"original"` // the file it lost against, e.g. "Notes/todo.txt"
	Modified time.Time `json:"modified"`
	Size     int64     `json:"size"`
}

const (
	maxConflicts    = 200     // listed per folder
	maxConflictWalk = 200_000 // entries looked at per folder
)

// conflictOriginal returns the winner's name for a conflict loser's base name ("" if it isn't one).
func conflictOriginal(base string) string {
	if m := conflictRe.FindStringSubmatch(base); m != nil {
		return m[1]
	}
	return ""
}

// skippedDirs are folder names the filters exclude everywhere ("- **/node_modules/**"), plus the
// backup folder: the scan doesn't walk into them (nothing in them syncs, so no conflicts).
func skippedDirs(filters []string) map[string]bool {
	out := map[string]bool{backupFolderName: true}
	for _, l := range filters {
		l = strings.TrimSpace(l)
		if name, ok := strings.CutPrefix(l, "- **/"); ok {
			if name, ok := strings.CutSuffix(name, "/**"); ok && !strings.ContainsAny(name, "*?[{/") {
				out[name] = true
			}
		}
	}
	return out
}

// scanConflicts lists the conflict copies under dir, newest first (capped).
func scanConflicts(dir string, filters []string) []Conflict {
	skip := skippedDirs(filters)
	var out []Conflict
	seen := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > maxConflictWalk {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if p != dir && skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		orig := conflictOriginal(d.Name())
		if orig == "" || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		out = append(out, Conflict{File: rel, Original: pathpkg.Join(pathpkg.Dir(rel), orig),
			Modified: info.ModTime(), Size: info.Size()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	if len(out) > maxConflicts {
		out = out[:maxConflicts]
	}
	return out
}

func (c Config) conflictsPath(f KeptFolder) string { return c.statePath(f, ".conflicts.json") }

func loadConflicts(file string) []Conflict {
	var out []Conflict
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// updateConflicts rescans a folder after a sync (or an action) and remembers the list. It
// returns the copies that weren't listed before, for the notification.
func (e *Engine) updateConflicts(cfg Config, f KeptFolder) []Conflict {
	list := scanConflicts(cfg.Local(f), append(append([]string{}, f.ExtraFilters...), cfg.CommonFilters...))
	file := cfg.conflictsPath(f)
	old := map[string]bool{}
	for _, c := range loadConflicts(file) {
		old[c.File] = true
	}
	var fresh []Conflict
	for _, c := range list {
		if !old[c.File] {
			fresh = append(fresh, c)
		}
	}
	if len(list) == 0 {
		_ = os.Remove(file)
	} else if b, err := json.MarshalIndent(list, "", "  "); err == nil {
		_ = os.WriteFile(file, b, 0o600)
	}
	e.mu.Lock()
	if s := e.status[f.Key()]; s != nil {
		s.Conflicts = list
	}
	if len(fresh) > 0 {
		e.newConflicts = append(e.newConflicts, conflictBatch{f.DisplayName(), len(fresh), pathpkg.Base(fresh[0].File)})
	}
	e.mu.Unlock()
	return fresh
}

type conflictBatch struct {
	folder  string
	n       int
	example string // one conflict copy's file name, for the notification
}

// conflictTarget checks a conflict action's file: it must be listed, be a conflict copy, and
// stay inside the folder. It returns the loser's and the winner's full paths.
func conflictTarget(local, rel string, list []Conflict) (loser, winner, origRel string, err error) {
	clean := pathpkg.Clean(strings.ReplaceAll(rel, `\`, "/"))
	if clean != rel || strings.HasPrefix(clean, "../") || clean == ".." || pathpkg.IsAbs(clean) {
		return "", "", "", fmt.Errorf("%q isn't a file in this folder", rel)
	}
	listed := false
	for _, c := range list {
		if c.File == rel {
			listed = true
		}
	}
	orig := conflictOriginal(pathpkg.Base(clean))
	if !listed || orig == "" {
		return "", "", "", fmt.Errorf("%q isn't a listed conflict copy", rel)
	}
	origRel = pathpkg.Join(pathpkg.Dir(clean), orig)
	return filepath.Join(local, filepath.FromSlash(clean)), filepath.Join(local, filepath.FromSlash(origRel)), origRel, nil
}

// keepConflictVersion makes the conflict copy the file: the current file goes to the local
// backups (like a merge's), then the copy takes its name. The next sync sends it to OpenCloud.
func keepConflictVersion(local, stateDir, stateName, rel string, list []Conflict, now time.Time) error {
	loser, winner, origRel, err := conflictTarget(local, rel, list)
	if err != nil {
		return err
	}
	if _, err := os.Stat(loser); err != nil {
		return fmt.Errorf("the conflict copy is gone (synced or removed meanwhile)")
	}
	if _, err := os.Stat(winner); err == nil {
		dest := freePath(filepath.Join(localBackupDir(stateDir, stateName, backupStamp(now)), filepath.FromSlash(origRel)))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		if err := moveFile(winner, dest); err != nil {
			return fmt.Errorf("couldn't move the current version to the backups: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(loser, winner)
}

// freePath is p, or "name (2).ext", "name (3).ext"… if p exists: a backup never replaces
// another (two Keeps of the same file within one second share the stamp folder).
func freePath(p string) string {
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		q := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Lstat(q); errors.Is(err, os.ErrNotExist) {
			return q
		}
	}
}

// moveFile renames, or copies and removes when the backups are on another disk.
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	st, _ := os.Stat(from)
	if err := os.WriteFile(to, b, 0o600); err != nil {
		return err
	}
	if st != nil {
		_ = os.Chtimes(to, st.ModTime(), st.ModTime())
	}
	return os.Remove(from)
}

// ConflictAction is the settings page's Keep This Version / Discard / Reveal for one copy.
// Keep and Discard run as the only sync activity: at once when no sync runs, else when the
// running syncs end (the copy is looked up again then; if it is gone the action fails and says
// so in the log). Reveal changes nothing, so it runs at once.
func (e *Engine) ConflictAction(folder, file, action string) error {
	f, ok := e.find(folder)
	if !ok {
		return fmt.Errorf("%q isn't a kept folder", folder)
	}
	switch action {
	case "keep", "discard", "reveal":
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	// The conflict list and settings as they are when the action runs.
	current := func() (Config, []Conflict) {
		e.mu.Lock()
		defer e.mu.Unlock()
		var list []Conflict
		if s := e.status[f.Key()]; s != nil {
			list = s.Conflicts
		}
		return e.cfg, list
	}
	if action == "reveal" {
		cfg, list := current()
		loser, _, _, err := conflictTarget(cfg.Local(f), file, list)
		if err == nil {
			openPath(filepath.Dir(loser))
		}
		return err
	}
	done := make(chan error, 1)
	queued, err := e.exclusiveQ(func() {
		cfg, list := current()
		local := cfg.Local(f)
		var err error
		if action == "keep" {
			err = keepConflictVersion(local, cfg.StateDir, f.StateName, file, list, time.Now())
		} else {
			var loser string
			if loser, _, _, err = conflictTarget(local, file, list); err == nil {
				err = moveToTrash(loser)
			}
		}
		if err == nil {
			e.logLine("%s: conflict %s: %s", f.Path, action, file)
		} else {
			e.logLine("%s: conflict %s of %s failed: %v", f.Path, action, file, err)
		}
		e.updateConflicts(cfg, f)
		done <- err
	})
	if err != nil || queued {
		return err // queued: it runs after the sync; a failure then is in the log
	}
	return <-done
}

// conflictCount is the number of conflict copies over all kept folders.
func conflictCount(fs []FolderStatus) int {
	n := 0
	for _, f := range fs {
		n += len(f.Conflicts)
	}
	return n
}

func conflictsText(n int) string {
	if n == 1 {
		return "1 conflict"
	}
	return fmt.Sprintf("%d conflicts", n)
}
