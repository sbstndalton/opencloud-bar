package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Spaces: OpenCloud keeps project spaces and accepted shares as drives next to the personal
// one. GET /graph/v1.0/me/drives lists them (libre-graph): each has an id, a name, a driveType
// ("personal", "project", "virtual" — the Shares overview — or "mountpoint", one accepted
// share) and root.webDavUrl, the WebDAV URL of its root. Kept folders from a space sync with
// the same on-the-fly WebDAV remote as the personal space, pointed at that URL's path.
// The macOS app does the same (Spaces.swift).

// Space is one space a folder can be kept from.
type Space struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"` // "project" or "mountpoint" (a share)
	DavPath string `json:"davPath"`
}

func (s Space) Ref() *SpaceRef { return &SpaceRef{ID: s.ID, Name: s.Name, DavPath: s.DavPath} }

// graphDrives is the part of the /me/drives answer the app reads.
type graphDrives struct {
	Value []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		DriveType string `json:"driveType"`
		Root      struct {
			WebDavURL string          `json:"webDavUrl"`
			Deleted   json.RawMessage `json:"deleted"` // a disabled (trashed) project space
		} `json:"root"`
	} `json:"value"`
}

// parseSpaces turns a /me/drives answer into the spaces that can be kept from: project spaces
// and accepted shares that aren't disabled, on this server, sorted by name.
func parseSpaces(d graphDrives, base string) []Space {
	bu, _ := url.Parse(base)
	var out []Space
	for _, v := range d.Value {
		if v.DriveType != "project" && v.DriveType != "mountpoint" {
			continue
		}
		if len(v.Root.Deleted) > 0 && string(v.Root.Deleted) != "null" {
			continue
		}
		u, err := url.Parse(v.Root.WebDavURL)
		if err != nil || v.ID == "" || u.Path == "" || (bu != nil && u.Host != "" && !strings.EqualFold(u.Host, bu.Host)) {
			continue
		}
		p := strings.TrimSuffix(u.EscapedPath(), "/")
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "' ") {
			continue
		}
		name := strings.TrimSpace(v.Name)
		if name == "" {
			name = v.ID
		}
		out = append(out, Space{ID: v.ID, Name: name, Type: v.DriveType, DavPath: p})
	}
	sort.Slice(out, func(i, j int) bool { return foldKey(out[i].Name) < foldKey(out[j].Name) })
	return out
}

// listSpaces asks the server for the spaces the user can keep folders from.
func listSpaces(ctx context.Context, cfg Config) ([]Space, error) {
	if !cfg.OIDC() || !cfg.Configured() {
		return nil, errors.New("spaces need the browser sign-in (Set Up / Sign In…)")
	}
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return nil, err
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return nil, fmt.Errorf("signed out: %w", err)
	}
	var d graphDrives
	if err := ocGet(ctx, base, "/graph/v1.0/me/drives", nil, tok, &d); err != nil {
		return nil, fmt.Errorf("couldn't list the spaces: %w", err)
	}
	return parseSpaces(d, base), nil
}

// davPermissions reads a folder's oc:permissions over WebDAV (PROPFIND, depth 0): letters
// like "RDNVCK", as OpenCloud's desktop client reads them. "" if the server didn't say.
func davPermissions(ctx context.Context, davURL, tok string) (string, error) {
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:permissions/></d:prop></d:propfind>`
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", davURL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := accountClient.Do(req)
	if err != nil {
		return "", errors.New("the server didn't answer")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 207:
	case 404:
		return "", errors.New("that folder doesn't exist in the space")
	default:
		return "", fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	var ms struct {
		Responses []struct {
			Props []struct {
				Permissions string `xml:"prop>permissions"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ms); err != nil {
		return "", errors.New("the server's answer wasn't what was expected")
	}
	for _, r := range ms.Responses {
		for _, p := range r.Props {
			if p.Permissions != "" {
				return p.Permissions, nil
			}
		}
	}
	return "", nil
}

// readOnly is true when the permissions say files can't be added or changed there (no
// C/K create, no W write). Unknown ("") isn't read-only: the first sync would tell.
func readOnly(perms string) bool {
	return perms != "" && !strings.ContainsAny(perms, "CKW")
}

// davURLFor is the WebDAV URL of a folder in a space, for the server's API (not rclone).
func davURLFor(base string, s Space, path string) string {
	u := strings.TrimSuffix(base, "/") + s.DavPath
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			u += "/" + url.PathEscape(part)
		}
	}
	return u + "/"
}

// KeepInSpace keeps a folder from a space other than the personal one. The space is looked up
// on the server (the page only sends its id), and a read-only folder is refused: a two-way
// sync can't send changes back, and would keep failing.
func (e *Engine) KeepInSpace(spaceID, path, localPath string) error {
	if spaceID == "" {
		return e.Keep(path, localPath)
	}
	cfg := e.config()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spaces, err := listSpaces(ctx, cfg)
	if err != nil {
		return err
	}
	var sp *Space
	for i := range spaces {
		if spaces[i].ID == spaceID {
			sp = &spaces[i]
		}
	}
	if sp == nil {
		return errors.New("that space isn't available (any more); reload the list")
	}
	path = normalizeKeptPath(path)
	if p := newPathProblem(path); p != "" {
		return errors.New(p)
	}
	base, _ := normalizeServerURL(cfg.ServerURL)
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return fmt.Errorf("signed out: %w", err)
	}
	perms, err := davPermissions(ctx, davURLFor(base, *sp, path), tok)
	if err != nil {
		return fmt.Errorf("couldn't check “%s” in %s: %w", path, sp.Name, err)
	}
	if readOnly(perms) {
		return fmt.Errorf("“%s” in %s is read-only for you, so it can't be kept here (a kept folder syncs both ways). Open it on the drive or in the browser instead", path, sp.Name)
	}
	return e.keepIn(sp.Ref(), path, localPath)
}

// BrowseSpace lists the folders directly in path of a space ("" for the personal one), with
// rclone and the folder's remote, for the settings page's folder list.
func (e *Engine) BrowseSpace(spaceID, path string) ([]string, error) {
	cfg := e.config()
	f := KeptFolder{Path: normalizeKeptPath(path)}
	if spaceID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		spaces, err := listSpaces(ctx, cfg)
		if err != nil {
			return nil, err
		}
		for _, s := range spaces {
			if s.ID == spaceID {
				f.Space = s.Ref()
			}
		}
		if f.Space == nil {
			return nil, errors.New("that space isn't available (any more)")
		}
	}
	if hasControl(f.Path) || strings.Contains(f.Path, "'") {
		return nil, errors.New("not a folder path")
	}
	out, err := e.run(time.Minute, nil, cfg.Rclone(), "lsf", "--dirs-only", "--max-depth", "1", cfg.RemoteFor(f)+f.Path)
	if err != nil {
		return nil, errors.New("couldn't list that folder")
	}
	var dirs []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
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
