package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Public links you made: reuse an existing one, and list, copy, re-date and remove them. All
// with OpenCloud's libre-graph API (opencloud-eu/opencloud, services/graph):
//
//	GET    /graph/v1beta1/drives/<drive>/items/<item>/permissions
//	       (service.go: r.Route("/v1beta1") ▸ "/drives/{driveID}/items/{itemID}/permissions" ▸ ListPermissions;
//	       api_driveitem_permissions.go ListPermissions). {"value": [permission…]}; a permission with
//	       a "link" facet is a public link, one with "grantedToV2" a share with a person or group.
//	GET    /graph/v1beta1/me/drive/sharedByMe
//	       (service.go: "/me" ▸ "/drive" ▸ r.Get("/sharedByMe", svc.GetSharedByMe); sharedbyme.go).
//	       {"value": [driveItem with permissions[]]}: user shares and links. The web client keeps the
//	       items whose permissions have a link (opencloud-eu/web, packages/web-pkg/src/services/folder/
//	       loaders/loaderSharedViaLink.ts), and so do we.
//	GET    …/items/<item>/permissions/<perm>   (GetPermission), fetched fresh before every change
//	PATCH  …/items/<item>/permissions/<perm>   {"expirationDateTime": "…Z" | null}
//	       (UpdatePermission ▸ updatePublicLinkPermission in api_driveitem_permissions_links.go; null
//	       removes the expiry: libre-graph-api-go model_permission.go has NullableTime, so an explicit null
//	       is "set", and parseAndFillUpTime turns a zero time into no expiry. The server rounds the
//	       time to 23:59:59 of the day it is sent (its UTC day, for a "…Z" time). opencloud-eu/web, packages/web-pkg/src/composables/
//	       piniaStores/shares/shares.ts updateLink sends the same.)
//	DELETE …/items/<item>/permissions/<perm>   (DeletePermission, 204)
//
// Safety. Removing a link ends it for everyone who has it, and the PATCH route also updates a
// share with a person, so nothing here trusts an id it was handed: the ids only pick an entry of
// the server's own sharedByMe answer; the ids used in the request come from that answer; the
// permission is then fetched again by id and must have a "link" facet, or nothing is changed. The
// token goes only to the configured server (accountClient follows no redirects; every request
// address is built from the configured address and checked against its host). The macOS app does
// the same (SharedLinks.swift).

// validGraphID: an id that is one plain path segment, safe to put in a request address.
func validGraphID(id string) bool {
	if id == "" || len(id) > 300 || id == "." || id == ".." || hasControl(id) {
		return false
	}
	return !strings.ContainsAny(id, "/?#\\% ")
}

// graphItemPath is /graph/v1beta1/drives/<drive>/items/<item> for an item id
// "<storage>$<space>!<node>"; the drive id is the item id up to its "!".
func graphItemPath(item string) (string, error) {
	i := strings.LastIndex(item, "!")
	if !validGraphID(item) || i <= 0 || i == len(item)-1 {
		return "", errors.New("the server's item id isn't one this version understands")
	}
	drive := item[:i]
	if !validGraphID(drive) || !validGraphID(item[i+1:]) {
		return "", errors.New("the server's item id isn't one this version understands")
	}
	return "/graph/v1beta1/drives/" + url.PathEscape(drive) + "/items/" + url.PathEscape(item), nil
}

// graphAPI talks to one server (base is https://host[/prefix]) with one token.
type graphAPI struct{ base, tok string }

func (g graphAPI) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	base, err := url.Parse(g.base)
	if err != nil || base.Host == "" {
		return 0, nil, errors.New("the server address isn't valid")
	}
	u := strings.TrimRight(g.base, "/") + path
	pu, err := url.Parse(u)
	if err != nil || pu.Host != base.Host || pu.Scheme != base.Scheme {
		return 0, nil, errors.New("that isn't an address on your OpenCloud server")
	}
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+g.tok)
	resp, err := accountClient.Do(req) // no redirects: the token stays with the server
	if err != nil {
		return 0, nil, errors.New("the server didn't answer")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, data, nil
}

// graphFail turns an unexpected answer into a short message (the server's own, if it gave one).
func graphFail(what string, status int, data []byte) error {
	var ge struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &ge) == nil && ge.Error.Message != "" {
		return fmt.Errorf("%s: %s", what, cleanServerText(ge.Error.Message))
	}
	if status >= 300 && status < 400 {
		return fmt.Errorf("%s (the server sent us somewhere else, which isn't followed)", what)
	}
	return fmt.Errorf("%s (it answered %d)", what, status)
}

// gPerm is the part of a libre-graph permission we read. Link is nil for a share with a person
// or group.
type gPerm struct {
	ID   string `json:"id"`
	Link *struct {
		Type   string `json:"type"`
		WebURL string `json:"webUrl"`
	} `json:"link"`
	Expiration  string `json:"expirationDateTime"`
	Created     string `json:"createdDateTime"`
	HasPassword bool   `json:"hasPassword"`
}

// ExistingLink is a public view link an item already has.
type ExistingLink struct {
	URL      string `json:"url"`
	Expires  string `json:"expires"`
	Created  string `json:"created"`
	Password bool   `json:"password"`
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// pickExistingView chooses the view link to offer again: not expired (nor with an expiry that
// can't be read), https; one without a
// password first (it is the one a plain "Copy Public Link" would make), then the newest.
func pickExistingView(perms []gPerm, now time.Time) *ExistingLink {
	var best *gPerm
	for i := range perms {
		p := &perms[i]
		if p.Link == nil || p.Link.Type != "view" || !httpsURL(p.Link.WebURL) {
			continue
		}
		if p.Expiration != "" {
			// An expiry that can't be read might be in the past: don't offer that link again.
			t, err := time.Parse(time.RFC3339, p.Expiration)
			if err != nil || !t.After(now) {
				continue
			}
		}
		if best == nil || (best.HasPassword && !p.HasPassword) || (best.HasPassword == p.HasPassword && p.Created > best.Created) {
			best = p
		}
	}
	if best == nil {
		return nil
	}
	return &ExistingLink{URL: best.Link.WebURL, Expires: best.Expiration, Created: best.Created, Password: best.HasPassword}
}

// existingViewLink lists the item's permissions and returns the view link to reuse, if any.
func (g graphAPI) existingViewLink(ctx context.Context, item string, now time.Time) (*ExistingLink, error) {
	p, err := graphItemPath(item)
	if err != nil {
		return nil, err
	}
	status, data, err := g.do(ctx, "GET", p+"/permissions", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, graphFail("OpenCloud didn't list the item's links", status, data)
	}
	var doc struct {
		Value []gPerm `json:"value"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil, errors.New("the server's answer wasn't what was expected")
	}
	return pickExistingView(doc.Value, now), nil
}

// SharedLink is one public link in the Shared Links list.
type SharedLink struct {
	Item     string `json:"item"` // the item's id as the server gave it
	Perm     string `json:"perm"` // the link's id (a permission id)
	Name     string `json:"name"`
	Dir      bool   `json:"dir"`
	Type     string `json:"type"` // view, edit, upload, createOnly, blocksDownload, internal
	URL      string `json:"url"`
	Expires  string `json:"expires"`
	Created  string `json:"created"`
	Password bool   `json:"password"`
}

// parseSharedByMe reads the sharedByMe answer: only links (permissions with a "link" facet), not
// shares with a person or group; entries whose ids aren't plain segments are left out.
func parseSharedByMe(r io.Reader) ([]SharedLink, error) {
	var doc struct {
		Value []struct {
			ID          string          `json:"id"`
			Name        string          `json:"name"`
			Folder      json.RawMessage `json:"folder"`
			Permissions []gPerm         `json:"permissions"`
		} `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 8<<20)).Decode(&doc); err != nil {
		return nil, errors.New("the server's answer wasn't what was expected")
	}
	out := []SharedLink{}
	for _, it := range doc.Value {
		if _, err := graphItemPath(it.ID); err != nil {
			continue
		}
		dir := len(it.Folder) > 0 && string(it.Folder) != "null"
		for _, p := range it.Permissions {
			if p.Link == nil || !validGraphID(p.ID) {
				continue
			}
			out = append(out, SharedLink{Item: it.ID, Perm: p.ID, Name: cleanServerText(it.Name), Dir: dir, Type: cleanServerText(p.Link.Type),
				URL: p.Link.WebURL, Expires: p.Expiration, Created: p.Created, Password: p.HasPassword})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := foldKey(out[i].Name), foldKey(out[j].Name); a != b {
			return a < b
		}
		return out[i].Created > out[j].Created
	})
	return out, nil
}

// sharedLinks lists the links you made (GET /me/drive/sharedByMe).
func (g graphAPI) sharedLinks(ctx context.Context) ([]SharedLink, error) {
	status, data, err := g.do(ctx, "GET", "/graph/v1beta1/me/drive/sharedByMe", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, graphFail("OpenCloud didn't list your links", status, data)
	}
	return parseSharedByMe(strings.NewReader(string(data)))
}

var errNotLink = errors.New("that isn't a public link (it is a share with a person or group), so it was left alone")

// linkPermission fetches a permission by id and insists on its "link" facet.
func (g graphAPI) linkPermission(ctx context.Context, item, perm string) (gPerm, error) {
	p, err := graphItemPath(item)
	if err != nil {
		return gPerm{}, err
	}
	if !validGraphID(perm) {
		return gPerm{}, errors.New("the server's link id isn't one this version understands")
	}
	status, data, err := g.do(ctx, "GET", p+"/permissions/"+url.PathEscape(perm), nil)
	if err != nil {
		return gPerm{}, err
	}
	switch status {
	case 200:
	case 404:
		return gPerm{}, errors.New("that link doesn't exist any more")
	default:
		return gPerm{}, graphFail("OpenCloud didn't look up the link", status, data)
	}
	var fresh gPerm
	if json.Unmarshal(data, &fresh) != nil {
		return gPerm{}, errors.New("the server's answer wasn't what was expected")
	}
	if fresh.ID != perm {
		return gPerm{}, errors.New("the server answered about another permission, so nothing was changed")
	}
	if fresh.Link == nil {
		return gPerm{}, errNotLink
	}
	return fresh, nil
}

// resolveLink finds (item, perm) in the server's own list of your links, then fetches the
// permission again by id. What it returns is what the server says now; the caller's strings
// were only used to pick the entry.
func (g graphAPI) resolveLink(ctx context.Context, item, perm string) (SharedLink, error) {
	if !validGraphID(item) || !validGraphID(perm) {
		return SharedLink{}, errors.New("that isn't a link this version understands")
	}
	list, err := g.sharedLinks(ctx)
	if err != nil {
		return SharedLink{}, err
	}
	for _, l := range list {
		if l.Item != item || l.Perm != perm {
			continue
		}
		fresh, err := g.linkPermission(ctx, l.Item, l.Perm)
		if err != nil {
			return SharedLink{}, err
		}
		l.URL, l.Type, l.Expires, l.Password = fresh.Link.WebURL, fresh.Link.Type, fresh.Expiration, fresh.HasPassword
		if fresh.Created != "" {
			l.Created = fresh.Created
		}
		return l, nil
	}
	return SharedLink{}, errors.New("that link isn't in your shared links any more; reload the list")
}

// linkURL is the link's address as the server says now (for Copy).
func (g graphAPI) linkURL(ctx context.Context, item, perm string) (string, error) {
	l, err := g.resolveLink(ctx, item, perm)
	if err != nil {
		return "", err
	}
	if !httpsURL(l.URL) {
		return "", errors.New("the server's answer had no https link")
	}
	return l.URL, nil
}

// setLinkExpiry sets the link's expiry; nil removes it.
func (g graphAPI) setLinkExpiry(ctx context.Context, item, perm string, exp *time.Time) error {
	l, err := g.resolveLink(ctx, item, perm)
	if err != nil {
		return err
	}
	var v any // null removes the expiry
	if exp != nil {
		v = exp.UTC().Format("2006-01-02T15:04:05Z")
	}
	body, _ := json.Marshal(map[string]any{"expirationDateTime": v})
	p, _ := graphItemPath(l.Item)
	status, data, err := g.do(ctx, "PATCH", p+"/permissions/"+url.PathEscape(l.Perm), body)
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		return graphFail("OpenCloud didn't change the link's expiry", status, data)
	}
	// Read it back: a 200 isn't proof the expiry changed.
	fresh, err := g.linkPermission(ctx, l.Item, l.Perm)
	if err != nil {
		return fmt.Errorf("OpenCloud didn't change the expiry (it can't be checked: %w)", err)
	}
	if !expiryMatches(fresh.Expiration, exp) {
		return errors.New("OpenCloud didn't change the expiry")
	}
	return nil
}

// expiryMatches: after setting an expiry, the link has none (want nil) or an expiry on the day
// that was sent. The server rounds a time up to the end of its UTC day, so days are compared.
func expiryMatches(got string, want *time.Time) bool {
	var t time.Time
	has := false
	if got != "" {
		var err error
		if t, err = time.Parse(time.RFC3339, got); err != nil {
			return false
		}
		has = t.Year() > 1970 // the zero time means no expiry
	}
	if want == nil {
		return !has
	}
	if !has {
		return false
	}
	y1, m1, d1 := t.UTC().Date()
	y2, m2, d2 := want.UTC().Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// removeLink deletes the link. wantURL is the address the user was shown when they confirmed;
// if the link now has another address, nothing is deleted.
func (g graphAPI) removeLink(ctx context.Context, item, perm, wantURL string) error {
	l, err := g.resolveLink(ctx, item, perm)
	if err != nil {
		return err
	}
	if wantURL == "" || l.URL != wantURL {
		return errors.New("that link changed since the list was loaded, so it was not removed; reload the list")
	}
	p, _ := graphItemPath(l.Item)
	status, data, err := g.do(ctx, "DELETE", p+"/permissions/"+url.PathEscape(l.Perm), nil)
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		return graphFail("OpenCloud didn't remove the link", status, data)
	}
	return nil
}

// expiryFor is the new expiry for a Change expiry choice: days 7 or 30 from now, or a date
// (YYYY-MM-DD, in this computer's time zone, not in the past, within 10 years); neither is "no
// expiry" (nil). A date becomes the end of that day (23:59:59) in the local time zone; the caller
// sends it as UTC.
//
// Source read (opencloud-eu/opencloud, main, services/graph/pkg/service/v0/
// api_driveitem_permissions_links.go): parseAndFillUpTime takes the date parts of the time it is
// sent, in the time's own location, and adds 23:59:59 to that day's midnight "so the link is valid
// for the whole day". We send a UTC time ("…Z"), so the server's day is the UTC day of the instant
// we send. That is not always the local day: the link can end some hours after the local day does
// (see the wording in the pages and README).
func expiryFor(days int, date string, now time.Time) (*time.Time, error) {
	return expiryForIn(days, date, now, time.Local)
}

func expiryForIn(days int, date string, now time.Time, loc *time.Location) (*time.Time, error) {
	switch {
	case date != "":
		d, err := time.ParseInLocation("2006-01-02", date, loc)
		if err != nil {
			return nil, errors.New("that isn't a date (use year-month-day)")
		}
		y, m, dd := now.In(loc).Date()
		today := time.Date(y, m, dd, 0, 0, 0, 0, loc)
		if d.Before(today) {
			return nil, errors.New("that date has passed")
		}
		if d.After(today.AddDate(10, 0, 0)) {
			return nil, errors.New("that date is too far away (10 years at most)")
		}
		end := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc)
		return &end, nil
	case days == 0:
		return nil, nil
	case days == 7 || days == 30:
		t := now.UTC().Add(time.Duration(days) * 24 * time.Hour)
		return &t, nil
	}
	return nil, errors.New("the expiry can be none, 7 days, 30 days or a date")
}

// linksAPI is the server and token for the Shared Links calls (browser sign-in only).
func (e *Engine) linksAPI() (graphAPI, error) {
	cfg := e.config()
	if !cfg.OIDC() {
		return graphAPI{}, errors.New("shared links need the browser sign-in")
	}
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return graphAPI{}, err
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return graphAPI{}, fmt.Errorf("signed out: %w", err)
	}
	return graphAPI{base: base, tok: tok}, nil
}

func linksCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// SharedLinks lists the public links you made.
func (e *Engine) SharedLinks() ([]SharedLink, error) {
	g, err := e.linksAPI()
	if err != nil {
		return nil, err
	}
	ctx, cancel := linksCtx()
	defer cancel()
	return g.sharedLinks(ctx)
}

// SharedLinkURL is the link's address, read from the server again, for Copy.
func (e *Engine) SharedLinkURL(item, perm string) (string, error) {
	g, err := e.linksAPI()
	if err != nil {
		return "", err
	}
	ctx, cancel := linksCtx()
	defer cancel()
	return g.linkURL(ctx, item, perm)
}

// SharedLinkExpiry changes a link's expiry: days 0 and no date remove it.
func (e *Engine) SharedLinkExpiry(item, perm string, days int, date string) error {
	exp, err := expiryFor(days, date, time.Now())
	if err != nil {
		return err
	}
	g, err := e.linksAPI()
	if err != nil {
		return err
	}
	ctx, cancel := linksCtx()
	defer cancel()
	if err := g.setLinkExpiry(ctx, item, perm, exp); err != nil {
		return err
	}
	e.logLine("shared link expiry changed")
	return nil
}

// SharedLinkRemove removes a link for good. confirmedURL is the address shown in the confirm
// dialog.
func (e *Engine) SharedLinkRemove(item, perm, confirmedURL string) error {
	g, err := e.linksAPI()
	if err != nil {
		return err
	}
	ctx, cancel := linksCtx()
	defer cancel()
	if err := g.removeLink(ctx, item, perm, confirmedURL); err != nil {
		return err
	}
	e.logLine("shared link removed")
	return nil
}

// itemWebURL is the web UI's address of a file or folder in a kept folder (<server>/f/<file id>):
// its oc:fileid by PROPFIND, then the same link as a search match's.
func itemWebURL(ctx context.Context, cfg Config, f KeptFolder, rel, tok string) (string, error) {
	davURL, err := itemDavURL(cfg, f, rel)
	if err != nil {
		return "", err
	}
	id, err := davFileID(ctx, davURL, tok)
	if err != nil {
		return "", err
	}
	return searchWebURL(cfg, id)
}

// ItemWebURL is that for the Open OpenCloud buttons; rel "" is the folder itself. Without the
// browser sign-in it is the web interface's front page.
func (e *Engine) ItemWebURL(key, rel string) (string, error) {
	f, ok := e.find(key)
	if !ok {
		return "", fmt.Errorf("%q isn't a kept folder", key)
	}
	cfg := e.config()
	if !cfg.OIDC() {
		// No token to ask the server which item it is: open the web interface as before.
		return normalizeServerURL(cfg.ServerURL)
	}
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return "", fmt.Errorf("signed out: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return itemWebURL(ctx, cfg, f, rel, tok)
}
