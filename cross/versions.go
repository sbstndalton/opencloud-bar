package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// File history: the previous versions OpenCloud keeps of a file in a kept folder, and
// restoring one. OpenCloud (reva's ocdav) serves them under the WebDAV "meta" tree, by file id:
//
//	PROPFIND <dav>/meta/<fileid>/v, Depth 1   → 207; the first response is the "v" folder
//	         itself, then one per version: href …/meta/<fileid>/v/<key>, with
//	         d:getlastmodified, d:getcontentlength, d:getetag
//	COPY     <dav>/meta/<fileid>/v/<key>      → 204; the version becomes the current file
//
// Sources: opencloud-eu/reva internal/http/services/owncloud/ocdav/versions.go (PROPFIND
// with an empty key → doListVersions, 207; COPY with a key → doRestore → RestoreFileVersion,
// 204; the Destination header isn't read, "cs3api has no destination"), dav.go (the "meta"
// segment), and opencloud-eu/web packages/web-client/src/webdav/listFileVersions.ts and
// restoreFileVersion.ts (the web UI: urlJoin('meta', fileId, 'v'[, versionId]), the first
// PROPFIND entry dropped, and dav.copy(source, <the file's files/ path>)). The app sends that
// Destination too, as the web UI does. <dav> is the WebDAV root the folder's path lives under:
// /remote.php/dav for /remote.php/dav/files/<user>, /dav for /dav/spaces/<id>.
//
// The file id comes from a PROPFIND of the file (davFileID, as for share links). The bearer
// token only goes to the server (accountClient follows no redirects). The macOS app does the
// same (History.swift).

// FileVersion is one earlier version of a file.
type FileVersion struct {
	Key      string    `json:"key"`
	Modified time.Time `json:"modified"`
	Size     int64     `json:"size"`
}

// davRootOf is the WebDAV root a folder's DAV path lives under: "/remote.php/dav/files/sam" →
// "/remote.php/dav", "/opencloud/dav/spaces/x" → "/opencloud/dav".
func davRootOf(davPath string) (string, error) {
	p := strings.TrimRight(davPath, "/")
	for _, seg := range []string{"/files/", "/spaces/"} {
		if i := strings.LastIndex(p, seg); i >= 0 && i+len(seg) < len(p) {
			return p[:i], nil
		}
	}
	return "", errors.New("this server's WebDAV paths aren't the kind this version understands")
}

// metaVersionsURL is <dav>/meta/<fileid>/v for a kept folder's space.
func metaVersionsURL(cfg Config, f KeptFolder, fileID string) (string, error) {
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return "", err
	}
	if fileID == "" || hasControl(fileID) || strings.ContainsAny(fileID, "/?#\\") {
		return "", errors.New("the server's file id isn't one this version understands")
	}
	var root string
	if f.Space != nil {
		r, err := davRootOf(f.Space.DavPath)
		if err != nil {
			return "", err
		}
		root = urlOrigin(base) + r
	} else {
		r, err := davRootOf(cfg.DavPath)
		if err != nil {
			return "", err
		}
		root = strings.TrimRight(base, "/") + r
	}
	return root + "/meta/" + url.PathEscape(fileID) + "/v", nil
}

// davMultistatus is the part of a PROPFIND answer the history and trash read. Element names
// match in any namespace (d: DAV:, oc: http://owncloud.org/ns).
type davMultistatus struct {
	Responses []struct {
		Href  string `xml:"href"`
		Props []struct {
			Status        string `xml:"status"`
			LastModified  string `xml:"prop>getlastmodified"`
			ContentLength string `xml:"prop>getcontentlength"`
			Size          string `xml:"prop>size"`
			ResourceType  struct {
				Collection *struct{} `xml:"collection"`
			} `xml:"prop>resourcetype"`
			OrigName  string `xml:"prop>trashbin-original-filename"`
			OrigLoc   string `xml:"prop>trashbin-original-location"`
			DeletedAt string `xml:"prop>trashbin-delete-datetime"`
			DeletedTS string `xml:"prop>trashbin-delete-timestamp"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// hrefPath is a PROPFIND href's path, decoded (an href may be a path or a whole URL).
func hrefPath(href string) string {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return ""
	}
	return u.Path
}

// parseHTTPTime reads a WebDAV date (RFC 1123, as getlastmodified has it).
func parseHTTPTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{http.TimeFormat, time.RFC1123, time.RFC1123Z, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseVersions reads the versions PROPFIND answer: newest first, the "v" folder itself left
// out, and only keys that are one plain path segment.
func parseVersions(r io.Reader) ([]FileVersion, error) {
	var ms davMultistatus
	if err := xml.NewDecoder(io.LimitReader(r, 4<<20)).Decode(&ms); err != nil {
		return nil, errors.New("the server's answer wasn't what was expected")
	}
	out := []FileVersion{}
	for _, resp := range ms.Responses {
		p := strings.TrimRight(hrefPath(resp.Href), "/")
		i := strings.LastIndex(p, "/v/")
		if i < 0 {
			continue // the "v" folder itself
		}
		key := p[i+3:]
		if key == "" || key == "." || key == ".." || strings.ContainsAny(key, "/\\") || hasControl(key) {
			continue
		}
		v := FileVersion{Key: key, Size: -1}
		for _, ps := range resp.Props {
			if ps.Status != "" && !strings.Contains(ps.Status, " 200") {
				continue
			}
			if t := parseHTTPTime(ps.LastModified); !t.IsZero() {
				v.Modified = t
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(ps.ContentLength), 10, 64); err == nil {
				v.Size = n
			}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// davDo sends one WebDAV request with the bearer token (no redirects followed).
func davDo(ctx context.Context, method, u, tok string, hdr map[string]string, body string) (*http.Response, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/xml")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := accountClient.Do(req)
	if err != nil {
		return nil, errors.New("the server didn't answer")
	}
	return resp, nil
}

const versionsPropfind = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getlastmodified/><d:getcontentlength/><d:getetag/></d:prop></d:propfind>`

// listVersions lists the versions at metaURL (<dav>/meta/<fileid>/v).
func listVersions(ctx context.Context, metaURL, tok string) ([]FileVersion, error) {
	resp, err := davDo(ctx, "PROPFIND", metaURL, tok, map[string]string{"Depth": "1"}, versionsPropfind)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 207:
	case 403:
		return nil, errors.New("you can't see this file's versions")
	case 404:
		return nil, errors.New("OpenCloud has no versions of this file")
	default:
		return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	return parseVersions(resp.Body)
}

// restoreVersion makes version key the current file (COPY …/v/<key>; Destination: the file).
func restoreVersion(ctx context.Context, metaURL, key, fileDavURL, tok string) error {
	if key == "" || key == "." || key == ".." || strings.ContainsAny(key, "/\\") || hasControl(key) {
		return errors.New("not a version of this file")
	}
	resp, err := davDo(ctx, "COPY", metaURL+"/"+url.PathEscape(key), tok, map[string]string{"Destination": fileDavURL}, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == 403:
		return errors.New("you can't restore versions of this file")
	case resp.StatusCode == 404:
		return errors.New("that version isn't there any more")
	}
	return fmt.Errorf("OpenCloud didn't restore it (it answered %d)", resp.StatusCode)
}

// cleanItemRel checks a file's path in a kept folder ("a/b.txt"): no "", ".", "..", "\",
// control characters, conflict copies or the backups folder.
func cleanItemRel(rel string) (string, error) {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" {
		return "", errors.New("choose a file in the folder")
	}
	for _, p := range strings.Split(rel, "/") {
		// ":" only on Windows, where it means a drive or a stream.
		if p == "" || p == "." || p == ".." || hasControl(p) || (runtime.GOOS == "windows" && strings.Contains(p, ":")) || isBackupFolder(p) {
			return "", errors.New("not a path in the folder")
		}
	}
	if skipChange(rel) {
		return "", errors.New("that file has no history here")
	}
	return rel, nil
}

// davFile is what a PROPFIND (depth 0) says about the current file in OpenCloud.
type davFile struct {
	ID       string
	Size     int64     // -1: not said
	Modified time.Time // zero: not said
}

// davFileInfo reads a file's oc:fileid, size and modification time in one PROPFIND.
func davFileInfo(ctx context.Context, davURL, tok string) (davFile, error) {
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:fileid/><d:getcontentlength/><d:getlastmodified/></d:prop></d:propfind>`
	resp, err := davDo(ctx, "PROPFIND", davURL, tok, map[string]string{"Depth": "0"}, body)
	if err != nil {
		return davFile{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 207:
	case 404:
		return davFile{}, errors.New("it isn't in OpenCloud yet; wait for the folder to sync, then try again")
	default:
		return davFile{}, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	var ms struct {
		Responses []struct {
			Props []struct {
				Status   string `xml:"status"`
				FileID   string `xml:"prop>fileid"`
				Length   string `xml:"prop>getcontentlength"`
				Modified string `xml:"prop>getlastmodified"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ms); err != nil {
		return davFile{}, errors.New("the server's answer wasn't what was expected")
	}
	out := davFile{Size: -1}
	for _, r := range ms.Responses {
		for _, p := range r.Props {
			if p.Status != "" && !strings.Contains(p.Status, " 200") {
				continue
			}
			if id := strings.TrimSpace(p.FileID); id != "" && out.ID == "" {
				out.ID = id
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(p.Length), 10, 64); err == nil {
				out.Size = n
			}
			if t := parseHTTPTime(p.Modified); !t.IsZero() {
				out.Modified = t
			}
		}
		break // depth 0: the file itself
	}
	if out.ID == "" {
		return davFile{}, errors.New("the server didn't say which file that is")
	}
	return out, nil
}

const unsyncedText = "This file has changes that haven't synced yet. Sync first, then restore."

// localMatches: the local copy is the server's current file as far as size and (when the
// server says) modification time go, within 2 seconds (WebDAV dates are whole seconds). A
// missing local copy doesn't match: a deletion here hasn't synced yet.
func localMatches(local string, srv davFile) bool {
	st, err := os.Stat(local)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if srv.Size >= 0 && st.Size() != srv.Size {
		return false
	}
	if !srv.Modified.IsZero() {
		d := st.ModTime().Sub(srv.Modified)
		if d < -2*time.Second || d > 2*time.Second {
			return false
		}
	}
	return true
}

// folderSyncing: that folder's sync runs (or is starting), or an exclusive action does.
func (e *Engine) folderSyncing(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active[key] != nil || e.startingKeys[key] > 0 || e.exclOn
}

// historyTarget resolves a file in a kept folder for the history: the folder, the file's path,
// its local copy, its WebDAV URL, its versions URL and a token.
type historyTarget struct {
	cfg     Config
	f       KeptFolder
	rel     string
	local   string
	davURL  string
	metaURL string
	server  davFile
	tok     string
}

func (e *Engine) historyFor(ctx context.Context, key, rel string) (historyTarget, error) {
	var t historyTarget
	f, ok := e.find(key)
	if !ok {
		return t, fmt.Errorf("%q isn't a kept folder", key)
	}
	cfg := e.config()
	if !cfg.OIDC() {
		return t, errors.New("file history needs the browser sign-in")
	}
	if !exists(cfg.statePath(f, ".initialised")) {
		return t, fmt.Errorf("“%s” hasn't synced yet", f.DisplayName())
	}
	rel, err := cleanItemRel(rel)
	if err != nil {
		return t, err
	}
	root := cfg.Local(f)
	local := filepath.Join(root, filepath.FromSlash(rel))
	if r, err := filepath.Rel(root, local); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return t, errors.New("not a path in the folder")
	}
	if st, err := os.Lstat(local); err == nil && !st.Mode().IsRegular() {
		return t, errors.New("only files have a history")
	}
	davURL, err := itemDavURL(cfg, f, rel)
	if err != nil {
		return t, err
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return t, fmt.Errorf("signed out: %w", err)
	}
	info, err := davFileInfo(ctx, davURL, tok)
	if err != nil {
		return t, err
	}
	metaURL, err := metaVersionsURL(cfg, f, info.ID)
	if err != nil {
		return t, err
	}
	return historyTarget{cfg: cfg, f: f, rel: rel, local: local, davURL: davURL, metaURL: metaURL, tok: tok, server: info}, nil
}

// FileVersions lists the earlier versions of rel in kept folder key, newest first.
func (e *Engine) FileVersions(key, rel string) ([]FileVersion, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t, err := e.historyFor(ctx, key, rel)
	if err != nil {
		return nil, err
	}
	return listVersions(ctx, t.metaURL, t.tok)
}

// RestoreVersion makes version vkey of rel the current file in OpenCloud. Refused while that
// folder syncs, and when the local copy differs from the server's current file (changes not
// synced yet would otherwise be overwritten, or would undo the restore). The local copy is
// copied to the local backups first (<stateDir>/backups/<stateName>/<stamp>/<rel>); then the
// folder syncs, which brings the restored version here.
func (e *Engine) RestoreVersion(key, rel, vkey string) error {
	f, ok := e.find(key)
	if ok && e.folderSyncing(f.Key()) {
		return fmt.Errorf("“%s” is syncing; try again when it finishes", f.DisplayName())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t, err := e.historyFor(ctx, key, rel)
	if err != nil {
		cancel()
		return err
	}
	vs, err := listVersions(ctx, t.metaURL, t.tok)
	cancel()
	if err != nil {
		return err
	}
	found := false
	for _, v := range vs {
		found = found || v.Key == vkey
	}
	if !found {
		return errors.New("that version isn't there any more; reload the list")
	}
	if !localMatches(t.local, t.server) {
		return errors.New(unsyncedText)
	}
	if _, err := backupLocalCopy(t.local, t.cfg.StateDir, t.f.StateName, t.rel, time.Now()); err != nil {
		return fmt.Errorf("couldn't keep the current file in the backups, so nothing was restored: %w", err)
	}
	// Checked again: a sync may have started while the backup was made.
	if e.folderSyncing(t.f.Key()) {
		return fmt.Errorf("“%s” started syncing; try again when it finishes", t.f.DisplayName())
	}
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second) // after the backup
	defer cancel()
	if err := restoreVersion(ctx, t.metaURL, vkey, t.davURL, t.tok); err != nil {
		return err
	}
	e.logLine("restored an earlier version of %s/%s", t.f.DisplayName(), t.rel)
	e.syncSoon(t.f)
	return nil
}

// backupLocalCopy copies a local file into the local backups (never replacing another backup).
// A file that isn't there (not synced here yet) is nothing to keep: "", nil.
func backupLocalCopy(local, stateDir, stateName, rel string, now time.Time) (string, error) {
	src, err := os.Open(local)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("not a file")
	}
	dest := freePath(filepath.Join(localBackupDir(stateDir, stateName, backupStamp(now)), filepath.FromSlash(path.Clean(rel))))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		_ = os.Remove(dest)
		return "", err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dest)
		return "", err
	}
	_ = os.Chtimes(dest, st.ModTime(), st.ModTime())
	return dest, nil
}

// syncSoon queues one kept folder's sync (after a restore), ahead of the interval's folders.
func (e *Engine) syncSoon(f KeptFolder) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queueLocked(f.Key())
	e.startQueuedLocked()
}
