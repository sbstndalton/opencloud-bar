package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Deleted files: OpenCloud's trash of the personal space and of the spaces folders are kept
// from, and restoring an item to where it was. Each space has its trash next to its WebDAV
// root (reva ocdav, spaces handler, segment "trash-bin"):
//
//	PROPFIND <dav>/spaces/trash-bin/<space id>, Depth 1 → 207, one response per trashed item:
//	         href …/trash-bin/<space id>/<key>, oc:trashbin-original-filename,
//	         oc:trashbin-original-location (its path in the space, no leading "/"),
//	         oc:trashbin-delete-timestamp (Unix seconds), oc:trashbin-delete-datetime
//	         (RFC 1123Z), d:resourcetype, d:getcontentlength (files) / oc:size (folders)
//	MOVE     <dav>/spaces/trash-bin/<space id>/<key>
//	         Destination: <dav>/spaces/<space id>/<original location>, Overwrite: F
//	         → 201 restored; 412 the destination exists (with Overwrite F); 409 its parent
//	         folder is gone
//
// Sources: opencloud-eu/reva internal/http/services/owncloud/ocdav/spaces.go (segment
// _trashbinPath → handleSpacesTrashbin: PROPFIND → listTrashbin, MOVE → restore, DELETE →
// delete; the Destination parsed against <base URI>/<space id>, the same space), dav.go (the
// "spaces" segment sets that base URI) and trashbin.go (the property names; restore: 412 when
// the destination exists and Overwrite isn't T, 409 when the parent is missing, 201/204).
//
// The app never deletes from the trash. It refuses to restore over something that is at the
// original place again (checked first with a PROPFIND, and Overwrite: F as well), and it only
// restores items that were inside a kept folder of that space. The space URLs are the ones
// the server gave (/graph/v1.0/me/drives root.webDavUrl, like spaces.go); the bearer token
// only goes to the server and no redirect is followed. The macOS app does the same (History.swift).

// TrashItem is one item in a space's trash.
type TrashItem struct {
	SpaceID   string    `json:"spaceId"`
	SpaceName string    `json:"spaceName"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Path      string    `json:"path"` // where it was in the space
	Deleted   time.Time `json:"deleted"`
	Size      int64     `json:"size"`
	Dir       bool      `json:"dir"`
	Folder    string    `json:"folder"` // the kept folder it was in (its key), "" if none
}

// trashSpace is a space whose trash is listed, with the kept folders from it.
type trashSpace struct {
	ID, Name string
	DavPath  string // the space root's WebDAV path, e.g. /dav/spaces/<id>
	kept     []KeptFolder
}

// trashURL is <dav>/spaces/trash-bin/<space id> for a space root path <dav>/spaces/<space id>.
func trashURL(origin, davPath string) (string, error) {
	p := strings.TrimRight(davPath, "/")
	i := strings.LastIndex(p, "/")
	if i < 0 || !strings.HasSuffix(p[:i], "/spaces") || p[i+1:] == "" {
		return "", errors.New("this server's spaces aren't the kind this version understands")
	}
	return origin + p[:i] + "/trash-bin/" + p[i+1:], nil
}

// cleanTrashPath checks an original location: a relative path of plain segments.
func cleanTrashPath(p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." || strings.Contains(s, `\`) || hasControl(s) {
			return "", false
		}
	}
	return p, true
}

// keptFolderFor is the kept folder of the space whose path is p or contains it.
func keptFolderFor(kept []KeptFolder, p string) (KeptFolder, bool) {
	best := KeptFolder{}
	found := false
	for _, f := range kept {
		fp := strings.Trim(f.Path, "/")
		if fp == "" || p == fp || strings.HasPrefix(p, fp+"/") {
			if !found || len(fp) > len(strings.Trim(best.Path, "/")) {
				best, found = f, true
			}
		}
	}
	return best, found
}

// parseTrash reads a trash-bin PROPFIND answer for space sp. Items whose key or original
// location isn't a plain path are left out.
func parseTrash(r io.Reader, sp trashSpace) ([]TrashItem, error) {
	var ms davMultistatus
	if err := xml.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&ms); err != nil {
		return nil, errors.New("the server's answer wasn't what was expected")
	}
	out := []TrashItem{}
	for _, resp := range ms.Responses {
		p := strings.TrimRight(hrefPath(resp.Href), "/")
		i := strings.Index(p, "/trash-bin/")
		if i < 0 {
			continue
		}
		rest := p[i+len("/trash-bin/"):] // <space id>/<key>
		j := strings.Index(rest, "/")
		if j < 0 {
			continue // the trash itself
		}
		key := rest[j+1:]
		if _, ok := cleanTrashPath(key); !ok {
			continue
		}
		it := TrashItem{SpaceID: sp.ID, SpaceName: sp.Name, Key: key, Size: -1}
		var loc string
		for _, ps := range resp.Props {
			if ps.Status != "" && !strings.Contains(ps.Status, " 200") {
				continue
			}
			if ps.OrigLoc != "" {
				loc = ps.OrigLoc
			}
			if ps.OrigName != "" {
				it.Name = strings.TrimSpace(ps.OrigName)
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(ps.DeletedTS), 10, 64); err == nil && n > 0 {
				it.Deleted = time.Unix(n, 0).UTC()
			} else if t := parseHTTPTime(ps.DeletedAt); !t.IsZero() && it.Deleted.IsZero() {
				it.Deleted = t.UTC()
			}
			if ps.ResourceType.Collection != nil {
				it.Dir = true
			}
			for _, s := range []string{ps.ContentLength, ps.Size} {
				if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
					it.Size = n
				}
			}
		}
		clean, ok := cleanTrashPath(loc)
		if !ok {
			continue
		}
		it.Path = clean
		if it.Name == "" || hasControl(it.Name) {
			it.Name = clean[strings.LastIndex(clean, "/")+1:]
		}
		if f, ok := keptFolderFor(sp.kept, clean); ok {
			it.Folder = f.Key()
		}
		out = append(out, it)
	}
	return out, nil
}

const trashPropfind = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop>` +
	`<oc:trashbin-original-filename/><oc:trashbin-original-location/><oc:trashbin-delete-datetime/>` +
	`<oc:trashbin-delete-timestamp/><d:getcontentlength/><oc:size/><d:resourcetype/></d:prop></d:propfind>`

// listTrash lists one space's trash.
func listTrash(ctx context.Context, origin string, sp trashSpace, tok string) ([]TrashItem, error) {
	u, err := trashURL(origin, sp.DavPath)
	if err != nil {
		return nil, err
	}
	resp, err := davDo(ctx, "PROPFIND", u, tok, map[string]string{"Depth": "1"}, trashPropfind)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 207:
	case 403, 404:
		return nil, errors.New("its trash isn't available to you")
	default:
		return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	return parseTrash(resp.Body, sp)
}

// davURLOf is origin + root + the escaped segments of rel.
func davURLOf(origin, root, rel string) string {
	u := origin + strings.TrimRight(root, "/")
	for _, s := range strings.Split(rel, "/") {
		if s != "" {
			u += "/" + url.PathEscape(s)
		}
	}
	return u
}

var errRestoreExists = errors.New("exists")

// restoreTrashItem restores it to its original place in space sp, never over something there.
func restoreTrashItem(ctx context.Context, origin string, sp trashSpace, it TrashItem, tok string) error {
	loc, ok := cleanTrashPath(it.Path)
	key, ok2 := cleanTrashPath(it.Key)
	if !ok || !ok2 {
		return errors.New("that item's place isn't a path this version can restore to")
	}
	tu, err := trashURL(origin, sp.DavPath)
	if err != nil {
		return err
	}
	dest := davURLOf(origin, sp.DavPath, loc)
	// Is something at the original place again? Then refuse rather than overwrite it.
	resp, err := davDo(ctx, "PROPFIND", dest, tok, map[string]string{"Depth": "0"},
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	switch resp.StatusCode {
	case 404:
	case 207, 200:
		return errRestoreExists
	default:
		return fmt.Errorf("couldn't check its original place (the server answered %d)", resp.StatusCode)
	}
	resp, err = davDo(ctx, "MOVE", davURLOf(tu, "", key), tok, map[string]string{"Destination": dest, "Overwrite": "F"}, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == 201 || resp.StatusCode == 204 || resp.StatusCode == 200:
		return nil
	case resp.StatusCode == 412:
		return errRestoreExists
	case resp.StatusCode == 409:
		return errors.New("the folder it was in is gone from OpenCloud; restore that folder first")
	case resp.StatusCode == 403:
		return errors.New("you can't restore items in this space")
	case resp.StatusCode == 404:
		return errors.New("it isn't in the trash any more")
	}
	return fmt.Errorf("OpenCloud didn't restore it (it answered %d)", resp.StatusCode)
}

// personalSpacePath is the path of the personal space's root.webDavUrl, used against the
// configured server's origin (its host is ignored). Same check as parseSpaces; "" if unusable.
func personalSpacePath(webDavURL string) string {
	u, err := url.Parse(webDavURL)
	if err != nil {
		return ""
	}
	p := strings.TrimSuffix(u.EscapedPath(), "/")
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "' ") {
		return ""
	}
	return p
}

const personalProblem = "Personal: the server didn't say where your personal space is"

// trashSpaces: the personal space (from /me/drives) and the spaces folders are kept from,
// and a problem line when the personal space can't be used.
func trashSpaces(ctx context.Context, cfg Config, base, tok string) ([]trashSpace, []string, error) {
	var d graphDrives
	if err := ocGet(ctx, base, "/graph/v1.0/me/drives", nil, tok, &d); err != nil {
		return nil, nil, fmt.Errorf("couldn't list the spaces: %w", err)
	}
	var out []trashSpace
	var problems []string
	found := false
	for _, v := range d.Value {
		if v.DriveType != "personal" {
			continue
		}
		found = true
		p := personalSpacePath(v.Root.WebDavURL)
		if v.ID == "" || p == "" {
			problems = append(problems, personalProblem)
			break
		}
		sp := trashSpace{ID: v.ID, Name: "Personal", DavPath: p}
		for _, f := range cfg.Kept {
			if f.Space == nil {
				sp.kept = append(sp.kept, f)
			}
		}
		out = append(out, sp)
		break
	}
	if !found {
		problems = append(problems, personalProblem)
	}
	byID := map[string]int{}
	for _, f := range cfg.Kept {
		if f.Space == nil {
			continue
		}
		if i, ok := byID[f.Space.ID]; ok {
			out[i].kept = append(out[i].kept, f)
			continue
		}
		byID[f.Space.ID] = len(out)
		out = append(out, trashSpace{ID: f.Space.ID, Name: f.Space.Name, DavPath: strings.TrimSuffix(f.Space.DavPath, "/"), kept: []KeptFolder{f}})
	}
	return out, problems, nil
}

// TrashList is what the Deleted Files page shows.
type TrashList struct {
	Items    []TrashItem `json:"items"`
	Problems []string    `json:"problems"` // spaces whose trash couldn't be read
	Hidden   int         `json:"hidden"`   // items not in a kept folder, left out
}

// trashLimit caps the list (the page and the menu stay usable).
const trashLimit = 2000

// ListTrash lists deleted items, newest first. Without all, only items that were in a kept
// folder.
func (e *Engine) ListTrash(all bool) (TrashList, error) {
	cfg := e.config()
	if !cfg.OIDC() || !cfg.Configured() {
		return TrashList{}, errors.New("deleted files need the browser sign-in")
	}
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return TrashList{}, err
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return TrashList{}, fmt.Errorf("signed out: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	spaces, problems, err := trashSpaces(ctx, cfg, base, tok)
	if err != nil {
		return TrashList{}, err
	}
	res := TrashList{Items: []TrashItem{}, Problems: append([]string{}, problems...)}
	for _, sp := range spaces {
		items, err := listTrash(ctx, urlOrigin(base), sp, tok)
		if err != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("%s: %v", sp.Name, err))
			continue
		}
		for _, it := range items {
			if !all && it.Folder == "" {
				res.Hidden++
				continue
			}
			res.Items = append(res.Items, it)
		}
	}
	sort.SliceStable(res.Items, func(i, j int) bool { return res.Items[i].Deleted.After(res.Items[j].Deleted) })
	if len(res.Items) > trashLimit {
		res.Items = res.Items[:trashLimit]
	}
	return res, nil
}

// RestoreTrash restores one deleted item (by its space and key, looked up on the server
// again) to its original place, if that was inside a kept folder and nothing is there now in
// OpenCloud or on this computer. The folder then syncs.
func (e *Engine) RestoreTrash(spaceID, key string) error {
	cfg := e.config()
	if !cfg.OIDC() || !cfg.Configured() {
		return errors.New("deleted files need the browser sign-in")
	}
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return err
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return fmt.Errorf("signed out: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	spaces, _, err := trashSpaces(ctx, cfg, base, tok)
	if err != nil {
		return err
	}
	var sp *trashSpace
	for i := range spaces {
		if spaces[i].ID == spaceID {
			sp = &spaces[i]
		}
	}
	if sp == nil {
		return errors.New("that space isn't one folders are kept from")
	}
	items, err := listTrash(ctx, urlOrigin(base), *sp, tok)
	if err != nil {
		return err
	}
	var it *TrashItem
	for i := range items {
		if items[i].Key == key {
			it = &items[i]
		}
	}
	if it == nil {
		return errors.New("it isn't in the trash any more; reload the list")
	}
	return e.restoreTrashChecked(ctx, cfg, urlOrigin(base), *sp, *it, tok)
}

// restoreTrashChecked: the checks around restoreTrashItem, and the sync after it.
func (e *Engine) restoreTrashChecked(ctx context.Context, cfg Config, origin string, sp trashSpace, it TrashItem, tok string) error {
	loc, ok := cleanTrashPath(it.Path)
	if !ok {
		return errors.New("that item's place isn't a path this version can restore to")
	}
	f, ok := keptFolderFor(sp.kept, loc)
	if !ok {
		return fmt.Errorf("“%s” wasn't in a kept folder; restore it in OpenCloud in the browser", loc)
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(loc, strings.Trim(f.Path, "/")), "/")
	for _, s := range strings.Split(rel, "/") {
		if isBackupFolder(s) {
			return errors.New("that item can't be restored from here")
		}
	}
	local := cfg.Local(f)
	if rel != "" {
		local = filepath.Join(local, filepath.FromSlash(rel))
	}
	if r, err := filepath.Rel(cfg.Local(f), local); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return errors.New("not a path in the folder")
	}
	// Something here that isn't in OpenCloud (not synced yet) would meet the restored item in
	// the next sync; leave that to the user. The kept folder itself is allowed: the sync merges.
	if _, err := os.Lstat(local); err == nil && rel != "" {
		if cfg.Paused {
			return fmt.Errorf("something is at “%s” on this computer. Resume syncing and wait for the folder to sync, then restore", loc)
		}
		return fmt.Errorf("something is at “%s” on this computer. Wait for the folder to sync, then restore", loc)
	}
	if err := restoreTrashItem(ctx, origin, sp, it, tok); err != nil {
		if errors.Is(err, errRestoreExists) {
			return fmt.Errorf("something is at “%s” in OpenCloud again; rename or move it first, then restore (nothing was overwritten)", loc)
		}
		return err
	}
	e.logLine("restored %s from the trash of %s", loc, sp.Name)
	e.syncSoon(f)
	return nil
}
