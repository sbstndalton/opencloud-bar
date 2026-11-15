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

// createShareLink makes a view link for the resource at davURL; graphBase is the server
// address (https://host[/prefix]).
func createShareLink(ctx context.Context, davURL, graphBase, tok string, o ShareOptions, now time.Time) (string, error) {
	id, err := davFileID(ctx, davURL, tok)
	if err != nil {
		return "", err
	}
	i := strings.LastIndex(id, "!")
	if i <= 0 || strings.ContainsAny(id, "/?#") {
		return "", errors.New("the server's file id isn't one this version understands")
	}
	drive := id[:i]
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

// ShareLink makes a public, read-only link to rel ("" for the folder itself) in a kept folder.
// Only for a folder that has synced, with the browser sign-in.
func (e *Engine) ShareLink(key, rel string, o ShareOptions) (string, error) {
	f, ok := e.find(key)
	if !ok {
		return "", fmt.Errorf("%q isn't a kept folder", key)
	}
	cfg := e.config()
	if !cfg.OIDC() {
		return "", errors.New("share links need the browser sign-in")
	}
	if !exists(cfg.statePath(f, ".initialised")) {
		return "", fmt.Errorf("“%s” hasn't synced yet; share it after its first sync", f.DisplayName())
	}
	switch o.Days {
	case 0, 1, 7, 30:
	default:
		return "", errors.New("the link can expire after 1, 7 or 30 days, or never")
	}
	if len(o.Password) > 200 || hasControl(o.Password) {
		return "", errors.New("that password can't be used")
	}
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if skipChange(rel) && rel != "" {
		return "", errors.New("that file can't be shared from here")
	}
	davURL, err := itemDavURL(cfg, f, rel)
	if err != nil {
		return "", err
	}
	base, _ := normalizeServerURL(cfg.ServerURL)
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return "", fmt.Errorf("signed out: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	link, err := createShareLink(ctx, davURL, base, tok, o, time.Now())
	if err != nil {
		return "", err
	}
	name := f.DisplayName()
	if rel != "" {
		name += "/" + rel
	}
	e.logLine("share link made for %s", name) // never the link's password
	return link, nil
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
