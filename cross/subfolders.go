package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Choose Subfolders (selective sync): a checklist of a kept folder's subfolders, two levels
// deep. An unchecked subfolder is one exclude rule `- /<Sub>/**` (or `- /<Sub>/<Child>/**`)
// in the folder's extraFilters. The checklist manages only rules of exactly that form; every
// other rule (hand-written globs, includes, comments) stays as it is, in place. A change
// goes the same way as Change Excludes: rclone then wants the folder's sync history started
// over, so the folder asks for a Repair (a merge that never deletes anything). Nothing on the
// server is removed, and an excluded subfolder's copy on this computer stays there, unsynced.
// The macOS app does the same (Subfolders.swift).

const globMeta = `*?[]{}\`

// escapeGlob makes a folder name match only itself in an rclone filter rule.
func escapeGlob(name string) string {
	var b strings.Builder
	for _, r := range name {
		if strings.ContainsRune(globMeta, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unescapeGlob undoes escapeGlob; ok is false if s has an unescaped glob character (a
// hand-written pattern, not a folder name) or a stray backslash.
func unescapeGlob(s string) (string, bool) {
	var b strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case esc:
			if !strings.ContainsRune(globMeta, r) {
				return "", false
			}
			b.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
		case strings.ContainsRune(globMeta, r):
			return "", false
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), !esc
}

// subfolderName can be written as a rule (and is a single folder name).
func subfolderName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/") && !hasControl(n) &&
		strings.TrimSpace(n) == n // the rules file is read line by line, trimmed
}

// subfolderRule is the exclude rule for sub ("A" or "A/B", relative to the kept folder).
func subfolderRule(sub string) string {
	parts := strings.Split(sub, "/")
	for i, p := range parts {
		parts[i] = escapeGlob(p)
	}
	return "- /" + strings.Join(parts, "/") + "/**"
}

// parseSubfolderRule reads a rule of exactly the checklist's form back into its subfolder
// ("A" or "A/B"); ok is false for every other rule.
func parseSubfolderRule(line string) (string, bool) {
	l := strings.TrimSpace(line)
	if !strings.HasPrefix(l, "- /") || !strings.HasSuffix(l, "/**") || len(l) <= len("- //**") {
		return "", false
	}
	mid := l[len("- /") : len(l)-len("/**")]
	parts := strings.Split(mid, "/")
	if len(parts) > 2 {
		return "", false
	}
	for i, p := range parts {
		n, ok := unescapeGlob(p)
		if !ok || !subfolderName(n) {
			return "", false
		}
		parts[i] = n
	}
	return strings.Join(parts, "/"), true
}

// excludedSubfolders is the set of subfolders the rules exclude in the checklist's form.
func excludedSubfolders(rules []string) map[string]bool {
	out := map[string]bool{}
	for _, l := range rules {
		if s, ok := parseSubfolderRule(l); ok {
			out[s] = true
		}
	}
	return out
}

// setSubfolders applies the checklist's changes (subfolder → included) to rules. The rules
// of the checklist's form come first, as one block (rclone uses the first rule that matches,
// so an unchecked subfolder stays unsynced whatever the other rules say); the other rules
// follow in their order, unchanged. In the block, rules for subfolders not in changes keep
// their order and new ones go at its end. Unchecking a folder also drops the rules for its
// own subfolders (it excludes them all already). Names match exactly, case included: Photos
// and photos are two folders on the server.
func setSubfolders(rules []string, changes map[string]bool) ([]string, error) {
	for sub := range changes {
		parts := strings.Split(sub, "/")
		if len(parts) > 2 {
			return nil, fmt.Errorf("%q: only two levels of subfolders can be chosen", sub)
		}
		for _, p := range parts {
			if !subfolderName(p) {
				return nil, fmt.Errorf("%q can't be chosen here (edit the excludes instead)", sub)
			}
		}
	}
	parentOff := func(s string) bool {
		if i := strings.Index(s, "/"); i > 0 {
			inc, ok := changes[s[:i]]
			return ok && !inc
		}
		return false
	}
	var managed, other []string
	have := map[string]bool{}
	for _, l := range rules {
		s, ok := parseSubfolderRule(l)
		if !ok {
			other = append(other, l)
			continue
		}
		if inc, changed := changes[s]; (changed && inc) || parentOff(s) || have[s] {
			continue // included now, inside a folder now excluded as a whole, or a duplicate
		}
		have[s] = true
		managed = append(managed, l)
	}
	var add []string
	for sub, inc := range changes {
		if inc || have[sub] || parentOff(sub) {
			continue
		}
		if i := strings.Index(sub, "/"); i > 0 && have[sub[:i]] {
			continue // its parent is excluded already: this rule would add nothing
		}
		add = append(add, sub)
	}
	sort.Strings(add)
	for _, s := range add {
		managed = append(managed, subfolderRule(s))
	}
	return append(managed, other...), nil
}

// Subfolder is one line of the checklist.
type Subfolder struct {
	Name     string `json:"name"`
	Excluded bool   `json:"excluded"`
	// Choosable is false for a name the checklist can't write a rule for (control characters,
	// leading or trailing spaces): it is shown but can't be unchecked.
	Choosable bool `json:"choosable"`
}

// listDirs lists the folders directly in path (relative to the space root) of f's space.
func (e *Engine) listDirs(cfg Config, f KeptFolder, path string) ([]string, error) {
	if hasControl(path) || strings.Contains(path, "'") {
		return nil, errors.New("not a folder path")
	}
	out, err := e.run(time.Minute, nil, cfg.Rclone(), "lsf", "--dirs-only", "--max-depth", "1", cfg.RemoteFor(f)+path)
	if err != nil {
		return nil, errors.New("couldn't list that folder")
	}
	var dirs []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if !strings.HasSuffix(l, "/") { // rclone's own messages share the output
			continue
		}
		if l = strings.TrimSuffix(l, "/"); l != "" && !isBackupFolder(l) {
			dirs = append(dirs, l)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return foldKey(dirs[i]) < foldKey(dirs[j]) })
	return dirs, nil
}

// Subfolders lists a kept folder's subfolders at sub ("" for the top level, or one top-level
// subfolder) from the server, with whether the folder's rules exclude each.
func (e *Engine) Subfolders(key, sub string) ([]Subfolder, error) {
	f, ok := e.find(key)
	if !ok {
		return nil, fmt.Errorf("%q isn't a kept folder", key)
	}
	sub = strings.Trim(sub, "/")
	if strings.Contains(sub, "/") {
		return nil, errors.New("only two levels of subfolders can be chosen")
	}
	if sub != "" && !subfolderName(sub) { // "..", ".", control characters
		return nil, fmt.Errorf("%q isn't a subfolder name", sub)
	}
	p := f.Path
	if sub != "" {
		p += "/" + sub
	}
	dirs, err := e.listDirs(e.config(), f, p)
	if err != nil {
		return nil, err
	}
	// The rules as they are in the file: a change saved while a sync runs is applied (Reload)
	// only when it finishes, and the checklist must show it already.
	rules := f.ExtraFilters
	configMu.Lock()
	raw, err := readConfigRaw()
	configMu.Unlock()
	if err == nil {
		if kept, err := rawKept(raw); err == nil {
			for _, k := range kept {
				if rawKey(k) == f.Key() {
					rules = nil
					_ = json.Unmarshal(k["extraFilters"], &rules)
				}
			}
		}
	}
	ex := excludedSubfolders(rules)
	out := []Subfolder{}
	for _, d := range dirs {
		rel := d
		if sub != "" {
			rel = sub + "/" + d
		}
		out = append(out, Subfolder{Name: d, Excluded: ex[rel], Choosable: subfolderName(d)})
	}
	return out, nil
}

// serveSubfolders lists ?folder=<key>&sub=<top-level subfolder or empty>. Changes are saved
// through the settings (keptSubfolders), the same path as Save Excludes.
func (e *Engine) serveSubfolders(w http.ResponseWriter, r *http.Request) {
	list, err := e.Subfolders(r.URL.Query().Get("folder"), r.URL.Query().Get("sub"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, 200, map[string]any{"folders": list})
}
