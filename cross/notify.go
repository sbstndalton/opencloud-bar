package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Desktop notifications: one per change of state, never repeated while the state lasts, and
// folders that change the same way at the same time share one notification. The macOS app
// does the same in AppModel.swift (Notifier).

// note is one notification. section is the settings page it is about.
type note struct {
	Title, Body, Section string
}

// notifyState is what has already been announced.
type notifyState struct {
	seeded    bool
	folders   map[string]string // folder path -> problem kind announced ("" = none)
	signedOut bool
}

// problemKind is the notification kind for a folder state. keep is true while a folder is
// busy or was skipped: its last kind stands, so a failure that repeats every sync (failed,
// syncing, failed…) is announced once.
func problemKind(f FolderStatus) (kind string, keep bool) {
	switch f.State {
	case StSyncing, StSkipped:
		return "", true
	case StNeedsRepair:
		return "repair", false
	case StBigDeletion:
		return "deletion", false
	case StFailed:
		if f.Detail == missingRemoteText {
			return "missing", false
		}
		if f.Detail == "signed out" {
			return "", false // the sign-out notification covers it
		}
		return "failed", false
	}
	return "", false
}

// planNotifications compares st with what was announced and returns what to show now.
// autoMerged names folders merged automatically since the last call. The first call only
// records the current state: problems that were there at start-up show in the menu.
func planNotifications(ns *notifyState, st Status, autoMerged []string) []note {
	if ns.folders == nil {
		ns.folders = map[string]string{}
	}
	fresh := map[string][]FolderStatus{} // kind -> folders that just got it
	seen := map[string]bool{}
	for _, f := range st.Folders {
		seen[f.Path] = true
		kind, keep := problemKind(f)
		if keep {
			continue
		}
		if kind != "" && ns.folders[f.Path] != kind && ns.seeded {
			fresh[kind] = append(fresh[kind], f)
		}
		ns.folders[f.Path] = kind
	}
	for p := range ns.folders {
		if !seen[p] {
			delete(ns.folders, p)
		}
	}
	wasOut := ns.signedOut
	ns.signedOut = st.SignedOut
	if !ns.seeded {
		ns.seeded = true
		return nil
	}
	var out []note
	if st.SignedOut && !wasOut {
		out = append(out, note{"Signed out of OpenCloud", "Syncing has stopped. Choose Set Up / Sign In… in the OpenCloud Bar menu to continue.", "account"})
	}
	for _, kind := range []string{"missing", "repair", "deletion", "failed"} {
		if fs := fresh[kind]; len(fs) > 0 {
			out = append(out, folderNote(kind, fs))
		}
	}
	if len(autoMerged) > 0 {
		out = append(out, note{
			Title:   plural(autoMerged, "“%s” was merged automatically", "%d folders were merged automatically"),
			Body:    "They were out of sync. The newer copy of each file was kept; older copies are kept in the backups (" + backupFolderName + " in OpenCloud, and on this computer).",
			Section: "folders",
		})
		if len(autoMerged) == 1 {
			out[len(out)-1].Body = "It was out of sync. The newer copy of each file was kept; older copies are kept in the backups (" + backupFolderName + " in OpenCloud, and on this computer)."
		}
	}
	return out
}

func folderNote(kind string, fs []FolderStatus) note {
	n := folderNoteText(kind, fs)
	n.Body = strings.TrimSpace(n.Body)
	return n
}

func folderNoteText(kind string, fs []FolderStatus) note {
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = f.Name
	}
	sort.Strings(names)
	one := len(fs) == 1
	detail := func(alt string) string {
		if one && fs[0].Detail != "" {
			return capitalize(fs[0].Detail) + "."
		}
		return alt
	}
	list := "“" + strings.Join(names, "”, “") + "”"
	switch kind {
	case "missing":
		return note{plural(names, "“%s” is missing or empty in OpenCloud", "%d folders are missing or empty in OpenCloud"),
			"Syncing them stopped, so nothing is deleted on this computer. Check the folder in OpenCloud. " + ifMany(one, list), "folders"}
	case "repair":
		return note{plural(names, "“%s” needs repair", "%d folders need repair"),
			detail("Open Settings ▸ Folders and choose Repair. " + list), "folders"}
	case "deletion":
		return note{plural(names, "Large deletion blocked in “%s”", "Large deletions blocked in %d folders"),
			"More than half of the files were deleted " + deletionSide(fs, one) + ". Check, then allow the deletion or restore the files. " + ifMany(one, list), "folders"}
	}
	return note{plural(names, "Sync failed for “%s”", "Sync failed for %d folders"), detail("OpenCloud Bar tries again at the next sync. " + list), "folders"}
}

// deletionSide names where a blocked deletion happened ("on this computer", "in OpenCloud").
func deletionSide(fs []FolderStatus, one bool) string {
	if one && fs[0].Detail != "" {
		return fs[0].Detail
	}
	return "on one side"
}

// conflictNote announces conflict copies found since the last check (one note for all folders).
func conflictNote(bs []conflictBatch) *note {
	if len(bs) == 0 {
		return nil
	}
	n, names := 0, []string{}
	for _, b := range bs {
		n += b.n
		names = append(names, b.folder)
	}
	title := conflictsText(n) + " in “" + names[0] + "”"
	if len(names) > 1 {
		title = conflictsText(n) + " in " + fmt.Sprint(len(names)) + " folders"
	}
	saved := "as “" + bs[0].example + "”"
	if n > 1 {
		saved = "with .conflict1 (or a higher number) added to its name, like “" + bs[0].example + "”"
	}
	return &note{title, "A file changed on both sides. The newer copy kept the name; the other is saved next to it " + saved + ". Open Settings ▸ Conflicts to keep one.", "conflicts"}
}

func plural(names []string, one, many string) string {
	if len(names) == 1 {
		return fmt.Sprintf(one, names[0])
	}
	return fmt.Sprintf(many, len(names))
}

func ifMany(one bool, list string) string {
	if one {
		return ""
	}
	return list
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// updateNote announces a release once per version, remembered on disk across restarts.
func updateNote(u UpdateState, file string) *note {
	if !u.Available || u.Latest == "" {
		return nil
	}
	if b, err := os.ReadFile(file); err == nil && strings.TrimSpace(string(b)) == u.Latest {
		return nil
	}
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	_ = os.WriteFile(file, []byte(u.Latest), 0o600)
	return &note{"OpenCloud Bar " + u.Latest + " is available", "You have " + u.Running + ". Choose Update Available in the menu to download it.", "general"}
}

// showNotification is desktopNotify; tests replace it.
var showNotification = desktopNotify

// checkNotifications runs on every tick: it shows what changed since the last check.
func (e *Engine) checkNotifications() {
	st := e.Status()
	e.mu.Lock()
	merged := e.autoMerged
	e.autoMerged = nil
	conflicts := e.newConflicts
	e.newConflicts = nil
	notes := planNotifications(&e.notes, st, merged)
	on := e.cfg.NotificationsOn() && e.cfgErr == nil
	e.mu.Unlock()
	if !on {
		return // the update notice waits: it is shown once notifications are on again
	}
	if n := updateNote(st.Update, filepath.Join(appDir(), "notified-update")); n != nil {
		notes = append(notes, *n)
	}
	if n := conflictNote(conflicts); n != nil {
		notes = append(notes, *n)
	}
	for _, n := range notes {
		e.logLine("notification: %s", n.Title)
		go func(n note) {
			if err := showNotification(n.Title, n.Body); err != nil {
				appLog("notification not shown: %v", err)
			}
		}(n)
	}
}
