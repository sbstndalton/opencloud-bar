package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Search: file and folder names across everything the user can see in OpenCloud, the way the
// web UI's search box does it. OpenCloud answers a WebDAV REPORT with an oc:search-files body
// on the spaces root:
//
//	REPORT <dav>/spaces/   (no Depth)
//	  <oc:search-files xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
//	    <d:prop>…</d:prop>
//	    <oc:search><oc:pattern>name:"*term*"</oc:pattern><oc:limit>50</oc:limit></oc:search>
//	  </oc:search-files>
//	→ 207, one response per match: href <dav>/spaces/<space ref>/<path in the space>, with
//	  oc:fileid, oc:file-parent, oc:name, d:getlastmodified, d:resourcetype, d:getcontentlength
//	  (files) / oc:size (folders), oc:privatelink; header Content-Range: rows 0-N/<total>
//
// Sources: opencloud-eu/opencloud services/webdav/pkg/service/v0/search.go (Search:
// readReport takes the search-files element, Search.Pattern is the search service's query and
// Limit its page size; a path /dav/spaces/<id> limits it to one space, /dav/spaces/ searches
// all; matchToPropResponse writes the href as path.Join("/dav/spaces/", <storage id>$<space id>
// [/path]) — for shares <space id>!<share id>/path — and oc:privatelink as <public URL>/f/<id>),
// services/proxy/pkg/config/defaults/defaultconfig.go (REPORT on (/remote.php)?/(web)?dav goes
// to that service; reva's own ocdav report.go answers search-files with 501), and
// opencloud-eu/web packages/web-client/src/webdav/search.ts and client/builders.ts (REPORT on
// '/spaces/', the oc:search-files body above, totalResults from Content-Range) and
// packages/web-pkg/src/composables/search/useSearch.ts (buildSearchTerm: name:"*<term>*", the
// term's quotes doubled by escapeKqlValue). "Show in browser" opens <server>/f/<the item's own id>,
// the web UI's private-link route (packages/web-runtime/src/router/index.ts '/f/:fileId'),
// which opens a file's folder with the file selected, and a folder directly.
//
// The spaces root is the personal space's root.webDavUrl path (/me/drives) with its last
// segment dropped, used against the configured server's origin like the trash. The bearer
// token only goes to the server and no redirect is followed. The macOS app does the same
// (Search.swift).

// searchLimit is how many matches are asked for.
const searchLimit = 50

// SearchResult is one match.
type SearchResult struct {
	Name     string    `json:"name"`
	Folder   string    `json:"folder"` // where it is: "/Docs", "Marketing: /Plans"
	Modified time.Time `json:"modified"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size"`
	WebID    string    `json:"webId"`          // the id "Show in browser" opens (its own)
	Kept     string    `json:"kept,omitempty"` // the kept folder it is in, if it is on this computer
	Rel      string    `json:"rel,omitempty"`  // its path in that kept folder
}

// SearchResults is what the search page shows.
type SearchResults struct {
	Results []SearchResult `json:"results"`
	Total   int            `json:"total"` // matches on the server (more than shown when > len)
}

// searchSpace is a space a match can be in: its id (from /me/drives), name and kept folders.
type searchSpace struct {
	ID, Name string
	kept     []KeptFolder
}

// cleanSearchTerm trims the term and refuses what can't be a name.
func cleanSearchTerm(term string) (string, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return "", errors.New("Type part of a name to search for.")
	}
	if len([]rune(term)) > 200 {
		return "", errors.New("That's too long to search for.")
	}
	if hasControl(term) {
		return "", errors.New("That isn't something a name can contain.")
	}
	return term, nil
}

// searchQuery is the search service's query for names containing term, as the web UI builds it.
func searchQuery(term string) string {
	return `name:"*` + strings.ReplaceAll(term, `"`, `""`) + `*"`
}

// searchBody is the REPORT body.
func searchBody(term string, limit int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><oc:search-files xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop>`)
	b.WriteString(`<oc:fileid/><oc:file-parent/><oc:name/><d:getlastmodified/><d:resourcetype/><d:getcontentlength/><oc:size/>`)
	b.WriteString(`</d:prop><oc:search><oc:pattern>`)
	_ = xml.EscapeText(&b, []byte(searchQuery(term)))
	b.WriteString(`</oc:pattern><oc:limit>` + strconv.Itoa(limit) + `</oc:limit></oc:search></oc:search-files>`)
	return b.String()
}

// spacesRootURL is <dav>/spaces/ for the personal space's root path <dav>/spaces/<id>.
func spacesRootURL(origin, personalDavPath string) (string, error) {
	p := strings.TrimRight(personalDavPath, "/")
	i := strings.LastIndex(p, "/")
	if i < 0 || !strings.HasSuffix(p[:i], "/spaces") || p[i+1:] == "" {
		return "", errors.New("This server's spaces aren't the kind this version understands.")
	}
	return origin + p[:i] + "/", nil
}

// spaceMatches: a match's space ref (from its href) is space id, or the id without the storage
// provider part (shares: <space id>!<share id>, while /me/drives has <provider>$<space id>!<share id>).
func spaceMatches(id, ref string) bool {
	return id != "" && (id == ref || strings.HasSuffix(id, "$"+ref))
}

// validWebID: an id that can go in the /f/ link (one plain segment).
func validWebID(id string) bool {
	return id != "" && len(id) < 300 && !hasControl(id) && !strings.ContainsAny(id, "/?#\\ ")
}

// parseSearch reads the REPORT answer. Matches whose href isn't under /dav/spaces/ or whose
// path isn't plain are left out.
func parseSearch(r io.Reader, spaces []searchSpace, local func(KeptFolder, string) (string, bool)) ([]SearchResult, error) {
	var ms struct {
		Responses []struct {
			Href  string `xml:"href"`
			Props []struct {
				Status        string `xml:"status"`
				FileID        string `xml:"prop>fileid"`
				Name          string `xml:"prop>name"`
				LastModified  string `xml:"prop>getlastmodified"`
				ContentLength string `xml:"prop>getcontentlength"`
				Size          string `xml:"prop>size"`
				ResourceType  struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"prop>resourcetype"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(io.LimitReader(r, 8<<20)).Decode(&ms); err != nil {
		return nil, errors.New("The server's answer wasn't what was expected.")
	}
	out := []SearchResult{}
	seen := map[string]bool{}
	for _, resp := range ms.Responses {
		p := strings.TrimRight(hrefPath(resp.Href), "/")
		i := strings.Index(p, "/dav/spaces/")
		if i < 0 {
			continue
		}
		rest := p[i+len("/dav/spaces/"):]
		ref, inSpace, _ := strings.Cut(rest, "/")
		if ref == "" || hasControl(ref) {
			continue
		}
		var clean string
		if inSpace != "" {
			c, ok := cleanTrashPath(inSpace)
			if !ok {
				continue
			}
			clean = c
		}
		res := SearchResult{Size: -1}
		var fileID string
		for _, ps := range resp.Props {
			if ps.Status != "" && !strings.Contains(ps.Status, " 200") {
				continue
			}
			if v := strings.TrimSpace(ps.FileID); v != "" {
				fileID = v
			}
			if v := strings.TrimSpace(ps.Name); v != "" {
				res.Name = v
			}
			if t := parseHTTPTime(ps.LastModified); !t.IsZero() {
				res.Modified = t.UTC()
			}
			if ps.ResourceType.Collection != nil {
				res.Dir = true
			}
			for _, s := range []string{ps.ContentLength, ps.Size} {
				if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
					res.Size = n
				}
			}
		}
		if fileID != "" {
			if seen[fileID] {
				continue // the web UI drops repeats too
			}
			seen[fileID] = true
		}
		if res.Name == "" || hasControl(res.Name) || strings.ContainsAny(res.Name, "/\\") {
			res.Name = path.Base("/" + clean)
			if clean == "" {
				res.Name = ref
			}
		}
		// The item's own id (never its parent's): the web UI's /f/:fileId opens a file's folder
		// with the file selected, and opens a folder directly.
		if validWebID(fileID) {
			res.WebID = fileID
		}
		dir := "/" + path.Dir("/" + clean)[1:]
		var sp *searchSpace
		for k := range spaces {
			if spaceMatches(spaces[k].ID, ref) {
				sp = &spaces[k]
				break
			}
		}
		if sp != nil && sp.Name != "" && sp.Name != "Personal" {
			res.Folder = sp.Name + ": " + dir
		} else {
			res.Folder = dir
		}
		if sp != nil && clean != "" {
			if f, ok := keptFolderFor(sp.kept, clean); ok {
				rel := strings.TrimPrefix(strings.TrimPrefix(clean, strings.Trim(f.Path, "/")), "/")
				if rel != "" && local != nil {
					if _, ok := local(f, rel); ok {
						res.Kept, res.Rel = f.Key(), rel
					}
				}
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// contentRangeTotal reads the total from "rows 0-49/123"; -1 if not said.
func contentRangeTotal(h string) int {
	_, tot, ok := strings.Cut(strings.TrimSpace(h), "/")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(tot))
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// searchSpaces: the spaces from /me/drives, with the kept folders of each, and the personal
// space's WebDAV root path.
func searchSpaces(d graphDrives, cfg Config) ([]searchSpace, string) {
	var out []searchSpace
	personal := ""
	for _, v := range d.Value {
		if v.ID == "" {
			continue
		}
		sp := searchSpace{ID: v.ID, Name: strings.TrimSpace(v.Name)}
		if v.DriveType == "personal" {
			sp.Name = "Personal"
			if personal == "" {
				personal = personalSpacePath(v.Root.WebDavURL)
				for _, f := range cfg.Kept {
					if f.Space == nil {
						sp.kept = append(sp.kept, f)
					}
				}
			}
		} else if v.DriveType == "virtual" {
			continue // the Shares overview: its shares are listed one by one
		}
		for _, f := range cfg.Kept {
			if f.Space != nil && f.Space.ID == v.ID {
				sp.kept = append(sp.kept, f)
			}
		}
		out = append(out, sp)
	}
	return out, personal
}

// searchLocal is the local copy of rel in kept folder f, if it is there. The path is checked
// by itemLocal (the "item" action's check), so "Show on this computer" is never offered for a
// path that "item" would refuse.
func (e *Engine) searchLocal() func(KeptFolder, string) (string, bool) {
	return func(f KeptFolder, rel string) (string, bool) {
		for _, s := range strings.Split(rel, "/") {
			if isBackupFolder(s) {
				return "", false
			}
		}
		p, err := e.itemLocal(f.Key(), rel)
		if err != nil {
			return "", false
		}
		if _, err := os.Lstat(p); err != nil {
			return "", false
		}
		return p, true
	}
}

// Search searches names in OpenCloud (all spaces), at most searchLimit matches.
func (e *Engine) Search(term string) (SearchResults, error) {
	term, err := cleanSearchTerm(term)
	if err != nil {
		return SearchResults{}, err
	}
	cfg := e.config()
	if !cfg.OIDC() || !cfg.Configured() {
		return SearchResults{}, errors.New("Search needs the browser sign-in.")
	}
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return SearchResults{}, err
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		if errors.Is(err, errSignedOut) {
			return SearchResults{}, errors.New(signInAgainText)
		}
		return SearchResults{}, fmt.Errorf("Couldn't get your sign-in: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var d graphDrives
	if err := ocGet(ctx, base, "/graph/v1.0/me/drives", nil, tok, &d); err != nil {
		return SearchResults{}, fmt.Errorf("Couldn't list the spaces: %w", err)
	}
	spaces, personal := searchSpaces(d, cfg)
	if personal == "" {
		return SearchResults{}, errors.New("The server didn't say where your personal space is.")
	}
	u, err := spacesRootURL(urlOrigin(base), personal)
	if err != nil {
		return SearchResults{}, err
	}
	resp, err := davDo(ctx, "REPORT", u, tok, nil, searchBody(term, searchLimit))
	if err != nil {
		return SearchResults{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 207:
	case 401:
		return SearchResults{}, errors.New(signInAgainText)
	case 400:
		return SearchResults{}, errors.New("OpenCloud couldn't search for that. Try other words.")
	case 501, 404, 405:
		return SearchResults{}, errors.New("This OpenCloud server doesn't offer search.")
	default:
		return SearchResults{}, fmt.Errorf("OpenCloud didn't search (it answered %d).", resp.StatusCode)
	}
	rs, err := parseSearch(resp.Body, spaces, e.searchLocal())
	if err != nil {
		return SearchResults{}, err
	}
	if len(rs) > searchLimit {
		rs = rs[:searchLimit]
	}
	total := contentRangeTotal(resp.Header.Get("Content-Range"))
	if total < len(rs) {
		total = len(rs)
	}
	return SearchResults{Results: rs, Total: total}, nil
}

// searchWebURL is the web UI's link for id (a match's own file id): <server>/f/<id>.
func searchWebURL(cfg Config, id string) (string, error) {
	if !validWebID(id) {
		return "", errors.New("That isn't an item OpenCloud can show.")
	}
	base, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return "", errors.New("No server address is set up yet.")
	}
	if _, err := url.Parse(base); err != nil {
		return "", err
	}
	return strings.TrimRight(base, "/") + "/f/" + url.PathEscape(id), nil
}
