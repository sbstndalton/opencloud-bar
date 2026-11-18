package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Share links: a public, read-only link to a file or folder in a kept folder, made with
// OpenCloud's libre-graph API:
//
//	PROPFIND <its WebDAV URL>, Depth 0, oc:fileid  → "<storage>$<space>!<node>"
//	POST /graph/v1beta1/drives/<storage>$<space>/items/<fileid>/createLink
//	     {"type": "view", "expirationDateTime": "…Z", "password": "…"}
//	→ a permission: {"link": {"webUrl": "https://…/s/…"}, …}
//
// The drive id is the file id up to its "!". The password is typed for this one link and
// never stored or logged. The macOS app does the same (Shares.swift).

// errPasswordRequired: the server enforces a password on public links (a createLink without one
// answers 400 "password protection is enforced"). The pages then ask for one and try again.
var errPasswordRequired = errors.New("This server requires a password for public links.")

// passwordEnforced reports whether a createLink answer is that 400.
func passwordEnforced(status int, data []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var ge struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal(data, &ge) == nil && strings.Contains(strings.ToLower(ge.Error.Message), "password protection is enforced")
}

// ShareOptions: Days 0 means no expiry; Password "" means none.
type ShareOptions struct {
	Days     int
	Password string
}

// davFileID reads a resource's oc:fileid (PROPFIND, depth 0).
func davFileID(ctx context.Context, davURL, tok string) (string, error) {
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:fileid/></d:prop></d:propfind>`
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
		return "", errors.New("it isn't in OpenCloud yet; wait for the folder to sync, then try again")
	default:
		return "", fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	var ms struct {
		Responses []struct {
			Props []struct {
				FileID string `xml:"prop>fileid"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ms); err != nil {
		return "", errors.New("the server's answer wasn't what was expected")
	}
	for _, r := range ms.Responses {
		for _, p := range r.Props {
			if id := strings.TrimSpace(p.FileID); id != "" {
				return id, nil
			}
		}
	}
	return "", errors.New("the server didn't say which file that is")
}

// shareDrive checks a file id ("<storage>$<space>!<node>") and returns its drive id.
func shareDrive(id string) (string, error) {
	i := strings.LastIndex(id, "!")
	if i <= 0 || strings.ContainsAny(id, "/?#") {
		return "", errors.New("the server's file id isn't one this version understands")
	}
	return id[:i], nil
}

// createShareLink makes a view link for the resource at davURL; graphBase is the server
// address (https://host[/prefix]).
func createShareLink(ctx context.Context, davURL, graphBase, tok string, o ShareOptions, now time.Time) (string, error) {
	id, err := davFileID(ctx, davURL, tok)
	if err != nil {
		return "", err
	}
	return createShareLinkFor(ctx, id, graphBase, tok, o, now)
}

// createShareLinkFor makes the view link for a file id the server gave.
func createShareLinkFor(ctx context.Context, id, graphBase, tok string, o ShareOptions, now time.Time) (string, error) {
	drive, err := shareDrive(id)
	if err != nil {
		return "", err
	}
	body := map[string]any{"type": "view"}
	if o.Days > 0 {
		body["expirationDateTime"] = now.UTC().Add(time.Duration(o.Days) * 24 * time.Hour).Format("2006-01-02T15:04:05Z")
	}
	if o.Password != "" {
		body["password"] = o.Password
	}
	b, _ := json.Marshal(body)
	u := strings.TrimRight(graphBase, "/") + "/graph/v1beta1/drives/" + url.PathEscape(drive) + "/items/" + url.PathEscape(id) + "/createLink"
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := accountClient.Do(req)
	if err != nil {
		return "", errors.New("the server didn't answer")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		if passwordEnforced(resp.StatusCode, data) {
			return "", errPasswordRequired
		}
		var ge struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &ge) == nil && ge.Error.Message != "" {
			return "", fmt.Errorf("OpenCloud didn't make the link: %s", cleanServerText(ge.Error.Message))
		}
		return "", fmt.Errorf("OpenCloud didn't make the link (it answered %d)", resp.StatusCode)
	}
	var perm struct {
		Link struct {
			WebURL string `json:"webUrl"`
		} `json:"link"`
	}
	if err := json.Unmarshal(data, &perm); err != nil || perm.Link.WebURL == "" {
		return "", errors.New("the server's answer had no link")
	}
	// A public link must be https (OpenCloud may give it its own public host name).
	if pu, err := url.Parse(perm.Link.WebURL); err != nil || pu.Scheme != "https" || pu.Host == "" {
		return "", errors.New("the server's answer had no https link")
	}
	return perm.Link.WebURL, nil
}

// cleanServerText keeps a server message short and printable.
func cleanServerText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// itemDavURL is the WebDAV URL (for the server's API, not rclone) of rel in kept folder f.
func itemDavURL(cfg Config, f KeptFolder, rel string) (string, error) {
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return "", err
	}
	root := strings.TrimRight(base, "/") + cfg.DavPath
	if f.Space != nil {
		root = urlOrigin(base) + f.Space.DavPath
	}
	u := root
	for _, part := range strings.Split(f.Path+"/"+rel, "/") {
		if part == "" {
			continue
		}
		if part == "." || part == ".." || hasControl(part) || strings.Contains(part, `\`) {
			return "", errors.New("not a path in the folder")
		}
		u += "/" + url.PathEscape(part)
	}
	return u, nil
}

// ShareResult is what Copy Public Link found or made: Existing is true when the item already
// has a view link and none was made (the page offers Copy Existing Link or Make New Link).
type ShareResult struct {
	URL      string `json:"url"`
	Existing bool   `json:"existing,omitempty"`
	Expires  string `json:"expires,omitempty"`
	Created  string `json:"created,omitempty"`
	Password bool   `json:"password,omitempty"`
}

// ShareLink makes a public, read-only link to rel ("" for the folder itself) in a kept folder,
// always a new one. Only for a folder that has synced, with the browser sign-in.
func (e *Engine) ShareLink(key, rel string, o ShareOptions) (string, error) {
	r, err := e.ShareLinkReuse(key, rel, o, true)
	return r.URL, err
}

// ShareLinkReuse is ShareLink, but unless newLink is set it first lists the item's permissions
// (GET …/items/<id>/permissions) and, if a view link is already there, returns that one
// (Existing) instead of making another, so public links don't pile up. If the list can't be read
// the link is made as before.
func (e *Engine) ShareLinkReuse(key, rel string, o ShareOptions, newLink bool) (ShareResult, error) {
	f, ok := e.find(key)
	if !ok {
		return ShareResult{}, fmt.Errorf("%q isn't a kept folder", key)
	}
	cfg := e.config()
	if !cfg.OIDC() {
		return ShareResult{}, errors.New("share links need the browser sign-in")
	}
	if !exists(cfg.statePath(f, ".initialised")) {
		return ShareResult{}, fmt.Errorf("“%s” hasn't synced yet; share it after its first sync", f.DisplayName())
	}
	switch o.Days {
	case 0, 1, 7, 30:
	default:
		return ShareResult{}, errors.New("the link can expire after 1, 7 or 30 days, or never")
	}
	if len(o.Password) > 200 || hasControl(o.Password) {
		return ShareResult{}, errors.New("that password can't be used")
	}
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if skipChange(rel) && rel != "" {
		return ShareResult{}, errors.New("that file can't be shared from here")
	}
	davURL, err := itemDavURL(cfg, f, rel)
	if err != nil {
		return ShareResult{}, err
	}
	base, _ := normalizeServerURL(cfg.ServerURL)
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return ShareResult{}, fmt.Errorf("signed out: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := f.DisplayName()
	if rel != "" {
		name += "/" + rel
	}
	res, err := shareOrReuse(ctx, davURL, graphAPI{base: base, tok: tok}, o, newLink, time.Now(), func(err error) {
		e.logLine("couldn't check %s for an existing link: %v", name, err)
	})
	if err != nil {
		return ShareResult{}, err
	}
	if res.Existing {
		e.logLine("share link for %s: an existing view link found", name)
	} else {
		e.logLine("share link made for %s", name) // never the link's password
	}
	return res, nil
}

// shareOrReuse asks the server for the item's id, then (unless newLink) for its permissions: a
// view link that is already there is returned as it is. Otherwise a new view link is made. A
// failed look at the permissions (told to onCheckErr) doesn't stop the link being made.
func shareOrReuse(ctx context.Context, davURL string, g graphAPI, o ShareOptions, newLink bool, now time.Time, onCheckErr func(error)) (ShareResult, error) {
	id, err := davFileID(ctx, davURL, g.tok)
	if err != nil {
		return ShareResult{}, err
	}
	if !newLink {
		ex, err := g.existingViewLink(ctx, id, now)
		switch {
		case err != nil:
			if onCheckErr != nil {
				onCheckErr(err)
			}
		case ex != nil:
			return ShareResult{URL: ex.URL, Existing: true, Expires: ex.Expires, Created: ex.Created, Password: ex.Password}, nil
		}
	}
	link, err := createShareLinkFor(ctx, id, g.base, g.tok, o, now)
	if err != nil {
		return ShareResult{}, err
	}
	return ShareResult{URL: link}, nil
}

// Item is one entry of a kept folder's listing for the share picker.
type Item struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

// ListItems lists files and folders directly in sub of a kept folder, on this computer (what
// has synced is what can be shared).
func (e *Engine) ListItems(key, sub string) ([]Item, error) {
	f, ok := e.find(key)
	if !ok {
		return nil, fmt.Errorf("%q isn't a kept folder", key)
	}
	sub = strings.Trim(sub, "/")
	for _, p := range strings.Split(sub, "/") {
		// "\" too: a separator on Windows.
		if p == ".." || p == "." || hasControl(p) || strings.ContainsAny(p, `\:`) {
			return nil, errors.New("not a path in the folder")
		}
	}
	dir := e.config().Local(f)
	if sub != "" {
		dir += string(os.PathSeparator) + strings.ReplaceAll(sub, "/", string(os.PathSeparator))
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.New("couldn't list that folder")
	}
	out := []Item{}
	for _, en := range ents {
		n := en.Name()
		rel := n
		if sub != "" {
			rel = sub + "/" + n
		}
		if strings.HasPrefix(n, ".") || skipChange(rel) {
			continue
		}
		out = append(out, Item{Name: n, Dir: en.IsDir()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return foldKey(out[i].Name) < foldKey(out[j].Name)
	})
	return out, nil
}
