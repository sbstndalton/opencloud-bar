package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// 2.6: share management against a fake libre-graph server (httptest, nothing else).

const sharedByMeJSON = `{"value":[
 {"id":"st$sp!aaa","name":"Q1 report.pdf","file":{},"permissions":[
   {"id":"link-view","createdDateTime":"2026-11-01T10:00:00Z","expirationDateTime":"2026-12-01T23:59:59Z","hasPassword":false,"link":{"type":"view","webUrl":"https://cloud.example.org/s/AAA"}},
   {"id":"user-1","grantedToV2":{"user":{"id":"u1","displayName":"Ann"}},"roles":["x"]}]},
 {"id":"st$sp!bbb","name":"Docs","folder":{},"permissions":[
   {"id":"link-edit","createdDateTime":"2026-11-05T10:00:00Z","hasPassword":true,"link":{"type":"edit","webUrl":"https://cloud.example.org/s/BBB"}},
   {"id":"group-1","grantedToV2":{"group":{"id":"g1","displayName":"Team"}}}]},
 {"id":"st$sp!ccc","name":"Only people","file":{},"permissions":[
   {"id":"user-2","grantedToV2":{"user":{"id":"u2"}}}]},
 {"id":"st$sp!d/d","name":"Bad item id","file":{},"permissions":[{"id":"x","link":{"type":"view","webUrl":"https://cloud.example.org/s/X"}}]},
 {"id":"st$sp!eee","name":"Bad perm id","file":{},"permissions":[{"id":"a/b","link":{"type":"view","webUrl":"https://cloud.example.org/s/Y"}}]}
]}`

// fakeGraph answers the routes the share code uses and records every request.
type fakeGraph struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string          // "METHOD escaped-path"
	body map[string]string // "METHOD path" -> last body
	// per-permission answer of GET …/permissions/<id>; missing = 404
	perms map[string]string
	// answer of GET …/items/<id>/permissions
	list string
	// sharedByMe answer
	shared string
	// when set, GET …/permissions (the list) answers 404; fileID is what PROPFIND says
	noList bool
	fileID string
	// PATCH: when noApply is set the server answers 200 but changes nothing; otherwise it stores
	// the new expiry, rounded up to 23:59:59 of its UTC day as parseAndFillUpTime does.
	noApply bool
	// when set, createLink answers 400 with this body
	createErr string
}

// The setters change the fake's answers between requests (under the lock the handler takes).
func (f *fakeGraph) setList(v string)    { f.mu.Lock(); f.list = v; f.mu.Unlock() }
func (f *fakeGraph) setShared(v string)  { f.mu.Lock(); f.shared = v; f.mu.Unlock() }
func (f *fakeGraph) setPerm(k, v string) { f.mu.Lock(); f.perms[k] = v; f.mu.Unlock() }
func (f *fakeGraph) setNoList(v bool)    { f.mu.Lock(); f.noList = v; f.mu.Unlock() }
func (f *fakeGraph) setFileID(v string)  { f.mu.Lock(); f.fileID = v; f.mu.Unlock() }
func (f *fakeGraph) setNoApply(v bool)   { f.mu.Lock(); f.noApply = v; f.mu.Unlock() }
func (f *fakeGraph) setCreateErr(v string) {
	f.mu.Lock()
	f.createErr = v
	f.mu.Unlock()
}

// applyPatch stores a PATCH's expirationDateTime in the permission the fake serves.
func (f *fakeGraph) applyPatch(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noApply {
		return
	}
	var in map[string]any
	var perm map[string]any
	if json.Unmarshal([]byte(body), &in) != nil || json.Unmarshal([]byte(f.perms[key]), &perm) != nil {
		return
	}
	switch v := in["expirationDateTime"].(type) {
	case nil:
		delete(perm, "expirationDateTime")
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			y, m, d := t.UTC().Date()
			perm["expirationDateTime"] = time.Date(y, m, d, 23, 59, 59, 0, time.UTC).Format("2006-01-02T15:04:05Z")
		}
	}
	nb, _ := json.Marshal(perm)
	f.perms[key] = string(nb)
}

func newFakeGraph(t *testing.T) *fakeGraph {
	f := &fakeGraph{t: t, body: map[string]string{}, shared: sharedByMeJSON, fileID: "st$sp!aaa", perms: map[string]string{
		"st$sp!aaa/link-view": `{"id":"link-view","expirationDateTime":"2026-12-01T23:59:59Z","link":{"type":"view","webUrl":"https://cloud.example.org/s/AAA"}}`,
		"st$sp!aaa/user-1":    `{"id":"user-1","grantedToV2":{"user":{"id":"u1"}},"roles":["x"]}`,
		"st$sp!bbb/link-edit": `{"id":"link-edit","hasPassword":true,"link":{"type":"edit","webUrl":"https://cloud.example.org/s/BBB"}}`,
		"st$sp!bbb/group-1":   `{"id":"group-1","grantedToV2":{"group":{"id":"g1"}}}`,
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) base() string { return f.srv.URL + "/opencloud" }

func (f *fakeGraph) handle(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	key := r.Method + " " + r.URL.EscapedPath()
	f.reqs = append(f.reqs, key)
	f.body[key] = string(b)
	list, shared, noList, fileID, createErr := f.list, f.shared, f.noList, f.fileID, f.createErr
	perms := make(map[string]string, len(f.perms))
	for k, v := range f.perms {
		perms[k] = v
	}
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "no", 401)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/opencloud")
	switch {
	case r.Method == "GET" && p == "/graph/v1beta1/me/drive/sharedByMe":
		_, _ = io.WriteString(w, shared)
	case r.Method == "PROPFIND":
		w.WriteHeader(207)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/x</d:href><d:propstat><d:prop><oc:fileid>`+fileID+`</oc:fileid></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	case strings.HasPrefix(p, "/graph/v1beta1/drives/st$sp/items/"):
		rest := strings.TrimPrefix(p, "/graph/v1beta1/drives/st$sp/items/")
		item, tail, _ := strings.Cut(rest, "/")
		switch {
		case r.Method == "GET" && tail == "permissions" && noList:
			http.NotFound(w, r)
		case r.Method == "GET" && tail == "permissions":
			_, _ = io.WriteString(w, list)
		case strings.HasPrefix(tail, "permissions/"):
			perm := strings.TrimPrefix(tail, "permissions/")
			ans, ok := perms[item+"/"+perm]
			switch {
			case r.Method == "GET" && ok:
				_, _ = io.WriteString(w, ans)
			case r.Method == "GET":
				http.NotFound(w, r)
			case r.Method == "PATCH":
				f.applyPatch(item+"/"+perm, string(b))
				_, _ = io.WriteString(w, ans)
			case r.Method == "DELETE":
				w.WriteHeader(204)
			}
		case r.Method == "POST" && tail == "createLink" && createErr != "":
			w.WriteHeader(400)
			_, _ = io.WriteString(w, createErr)
		case r.Method == "POST" && tail == "createLink":
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"id":"new","link":{"type":"view","webUrl":"https://cloud.example.org/s/NEW"}}`)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGraph) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *fakeGraph) count(method string) int {
	n := 0
	for _, r := range f.log() {
		if strings.HasPrefix(r, method+" ") {
			n++
		}
	}
	return n
}

func (f *fakeGraph) api() graphAPI { return graphAPI{base: f.base(), tok: "tok"} }

func TestParseSharedByMeLinksOnly(t *testing.T) {
	ls, err := parseSharedByMe(strings.NewReader(sharedByMeJSON))
	if err != nil {
		t.Fatal(err)
	}
	// Only the two links: user and group shares, an item with only people, and entries whose
	// ids aren't one plain segment are left out.
	if len(ls) != 2 {
		t.Fatalf("links: %+v", ls)
	}
	// Sorted by name: Docs, then Q1 report.pdf.
	d, q := ls[0], ls[1]
	if d.Name != "Docs" || !d.Dir || d.Type != "edit" || !d.Password || d.Perm != "link-edit" || d.Item != "st$sp!bbb" || d.Expires != "" || d.Created != "2026-11-05T10:00:00Z" {
		t.Errorf("docs: %+v", d)
	}
	if q.Name != "Q1 report.pdf" || q.Dir || q.Type != "view" || q.Password || q.Perm != "link-view" || q.URL != "https://cloud.example.org/s/AAA" || q.Expires != "2026-12-01T23:59:59Z" {
		t.Errorf("q1: %+v", q)
	}
	if _, err := parseSharedByMe(strings.NewReader("<html>")); err == nil {
		t.Error("not JSON accepted")
	}
	if ls, err := parseSharedByMe(strings.NewReader(`{"value":[{"id":"st$sp!a","name":"n","permissions":[{"id":"p","link":null}]}]}`)); err != nil || len(ls) != 0 {
		t.Errorf("null link: %v %v", ls, err)
	}
}

func TestSharedLinksRequest(t *testing.T) {
	f := newFakeGraph(t)
	ls, err := f.api().sharedLinks(context.Background())
	if err != nil || len(ls) != 2 {
		t.Fatalf("%v %v", ls, err)
	}
	if got := f.log(); len(got) != 1 || got[0] != "GET /opencloud/graph/v1beta1/me/drive/sharedByMe" {
		t.Errorf("requests: %v", got)
	}
}

func TestExistingViewLinkReused(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	f := newFakeGraph(t)
	f.setList(`{"value":[
	 {"id":"u","grantedToV2":{"user":{"id":"u1"}}},
	 {"id":"edit","createdDateTime":"2026-11-09T00:00:00Z","link":{"type":"edit","webUrl":"https://cloud.example.org/s/EDIT"}},
	 {"id":"expired","createdDateTime":"2026-11-10T00:00:00Z","expirationDateTime":"2026-11-17T23:59:59Z","link":{"type":"view","webUrl":"https://cloud.example.org/s/OLD"}},
	 {"id":"pw","createdDateTime":"2026-11-12T00:00:00Z","hasPassword":true,"link":{"type":"view","webUrl":"https://cloud.example.org/s/PW"}},
	 {"id":"plain1","createdDateTime":"2026-11-02T00:00:00Z","link":{"type":"view","webUrl":"https://cloud.example.org/s/ONE"}},
	 {"id":"plain2","createdDateTime":"2026-11-03T00:00:00Z","expirationDateTime":"2026-12-24T23:59:59Z","link":{"type":"view","webUrl":"https://cloud.example.org/s/TWO"}},
	 {"id":"http","createdDateTime":"2026-11-15T00:00:00Z","link":{"type":"view","webUrl":"http://cloud.example.org/s/PLAIN"}}]}`)
	ex, err := f.api().existingViewLink(context.Background(), "st$sp!aaa", now)
	if err != nil || ex == nil {
		t.Fatalf("%v %v", ex, err)
	}
	// Not the edit link, the expired one, the password one or the http one: the newest plain view link.
	if ex.URL != "https://cloud.example.org/s/TWO" || ex.Password || ex.Expires != "2026-12-24T23:59:59Z" {
		t.Errorf("existing: %+v", ex)
	}
	if got := f.log(); len(got) != 1 || got[0] != "GET /opencloud/graph/v1beta1/drives/st$sp/items/st$sp%21aaa/permissions" {
		t.Errorf("requests: %v", got)
	}
	// Only a password link left: it is still offered, and says so.
	f.setList(`{"value":[{"id":"pw","hasPassword":true,"link":{"type":"view","webUrl":"https://cloud.example.org/s/PW"}}]}`)
	if ex, _ := f.api().existingViewLink(context.Background(), "st$sp!aaa", now); ex == nil || !ex.Password {
		t.Errorf("password link: %+v", ex)
	}
	// No view link: nothing to reuse.
	f.setList(`{"value":[{"id":"u","grantedToV2":{"user":{"id":"u1"}}}]}`)
	if ex, err := f.api().existingViewLink(context.Background(), "st$sp!aaa", now); ex != nil || err != nil {
		t.Errorf("none: %+v %v", ex, err)
	}
}

func TestShareOrReuse(t *testing.T) {
	f := newFakeGraph(t)
	f.setList(`{"value":[{"id":"v","createdDateTime":"2026-11-02T00:00:00Z","link":{"type":"view","webUrl":"https://cloud.example.org/s/ONE"}}]}`)
	dav := f.base() + "/dav/files/sam/Q1.pdf"
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	// An existing view link: offered, none made.
	r, err := shareOrReuse(context.Background(), dav, f.api(), ShareOptions{}, false, now, nil)
	if err != nil || !r.Existing || r.URL != "https://cloud.example.org/s/ONE" {
		t.Fatalf("reuse: %+v %v", r, err)
	}
	if f.count("POST") != 0 {
		t.Errorf("a link was made: %v", f.log())
	}
	// Make New Link: the permissions aren't even listed.
	before := len(f.log())
	r, err = shareOrReuse(context.Background(), dav, f.api(), ShareOptions{Days: 7}, true, now, nil)
	if err != nil || r.Existing || r.URL != "https://cloud.example.org/s/NEW" {
		t.Fatalf("new: %+v %v", r, err)
	}
	for _, q := range f.log()[before:] {
		if strings.HasSuffix(q, "/permissions") {
			t.Errorf("listed permissions for Make New Link: %v", q)
		}
	}
	// No view link: made as before.
	f.setList(`{"value":[]}`)
	if r, err = shareOrReuse(context.Background(), dav, f.api(), ShareOptions{}, false, now, nil); err != nil || r.Existing || r.URL == "" {
		t.Errorf("none: %+v %v", r, err)
	}
	// The permissions can't be read (the route answers 404): the link is made, and onCheckErr hears of it.
	f.setNoList(true)
	var heard error
	if r, err = shareOrReuse(context.Background(), dav, f.api(), ShareOptions{}, false, now, func(e error) { heard = e }); err != nil || r.Existing || heard == nil {
		t.Errorf("list failed: %+v %v %v", r, err, heard)
	}
}

func TestSetLinkExpiryPatch(t *testing.T) {
	f := newFakeGraph(t)
	ctx := context.Background()
	exp := time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)
	if err := f.api().setLinkExpiry(ctx, "st$sp!aaa", "link-view", &exp); err != nil {
		t.Fatal(err)
	}
	key := "PATCH /opencloud/graph/v1beta1/drives/st$sp/items/st$sp%21aaa/permissions/link-view"
	if got := f.body[key]; got != `{"expirationDateTime":"2026-12-25T00:00:00Z"}` {
		t.Errorf("body %q; requests %v", got, f.log())
	}
	// No expiry: an explicit null (that is what removes it on the server).
	if err := f.api().setLinkExpiry(ctx, "st$sp!aaa", "link-view", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.body[key]; got != `{"expirationDateTime":null}` {
		t.Errorf("body %q", got)
	}
	// The server looked at the link again before each PATCH.
	log := f.log()
	var seq []string
	for _, q := range log {
		if strings.Contains(q, "/permissions/link-view") || strings.HasSuffix(q, "sharedByMe") {
			seq = append(seq, strings.SplitN(q, " ", 2)[0])
		}
	}
	if strings.Join(seq, ",") != "GET,GET,PATCH,GET,GET,GET,PATCH,GET" {
		t.Errorf("sequence %v", log)
	}
	// A user share's id is never patched.
	f2 := newFakeGraph(t)
	for _, perm := range []string{"user-1", "group-1"} {
		item := "st$sp!aaa"
		if perm == "group-1" {
			item = "st$sp!bbb"
		}
		if err := f2.api().setLinkExpiry(ctx, item, perm, &exp); err == nil {
			t.Errorf("%s patched", perm)
		}
	}
	if f2.count("PATCH") != 0 {
		t.Errorf("PATCH sent for a share: %v", f2.log())
	}
}

func TestExpiryFor(t *testing.T) {
	now := time.Date(2026, 11, 18, 15, 30, 0, 0, time.UTC)
	expiryFor := func(days int, date string, now time.Time) (*time.Time, error) {
		return expiryForIn(days, date, now, time.UTC)
	}
	if e, err := expiryFor(0, "", now); e != nil || err != nil {
		t.Errorf("none: %v %v", e, err)
	}
	if e, err := expiryFor(7, "", now); err != nil || e == nil || e.Format(time.RFC3339) != "2026-11-25T15:30:00Z" {
		t.Errorf("7: %v %v", e, err)
	}
	if e, err := expiryFor(30, "", now); err != nil || e == nil || e.Format("2006-01-02") != "2026-12-18" {
		t.Errorf("30: %v %v", e, err)
	}
	if e, err := expiryFor(0, "2027-01-31", now); err != nil || e == nil || e.UTC().Format(time.RFC3339) != "2027-01-31T23:59:59Z" {
		t.Errorf("date: %v %v", e, err)
	}
	if e, err := expiryFor(0, "2026-11-18", now); err != nil || e == nil { // today is still valid today
		t.Errorf("today: %v %v", e, err)
	}
	for _, c := range []struct {
		days int
		date string
	}{{3, ""}, {-1, ""}, {0, "2026-11-17"}, {0, "tomorrow"}, {0, "2026-13-01"}, {0, "2040-01-01"}} {
		if e, err := expiryFor(c.days, c.date, now); err == nil {
			t.Errorf("%+v accepted: %v", c, e)
		}
	}
}

func TestRemoveLinkRefusedWithoutLinkFacet(t *testing.T) {
	ctx := context.Background()
	// The list says it is a link, but the permission fetched by id has no link facet.
	f := newFakeGraph(t)
	f.setPerm("st$sp!aaa/link-view", `{"id":"link-view","grantedToV2":{"user":{"id":"u1"}},"roles":["x"]}`)
	err := f.api().removeLink(ctx, "st$sp!aaa", "link-view", "https://cloud.example.org/s/AAA")
	if err == nil || !strings.Contains(err.Error(), "isn't a public link") {
		t.Errorf("error: %v", err)
	}
	if f.count("DELETE") != 0 {
		t.Errorf("DELETE sent: %v", f.log())
	}
	// The permission a user share has: it isn't in the list of links, so it never gets that far.
	for _, c := range [][2]string{{"st$sp!aaa", "user-1"}, {"st$sp!bbb", "group-1"}, {"st$sp!ccc", "user-2"}} {
		if err := f.api().removeLink(ctx, c[0], c[1], "https://cloud.example.org/s/AAA"); err == nil {
			t.Errorf("%v removed", c)
		}
	}
	// A permission id the server answers with another id for.
	f.setPerm("st$sp!bbb/link-edit", `{"id":"other","link":{"type":"edit","webUrl":"https://cloud.example.org/s/BBB"}}`)
	if err := f.api().removeLink(ctx, "st$sp!bbb", "link-edit", "https://cloud.example.org/s/BBB"); err == nil {
		t.Error("answer for another permission accepted")
	}
	if f.count("DELETE") != 0 {
		t.Errorf("DELETE sent: %v", f.log())
	}
}

func TestRemoveLinkRefetchesFirst(t *testing.T) {
	f := newFakeGraph(t)
	ctx := context.Background()
	// A different address than the one that was confirmed: nothing is deleted.
	if err := f.api().removeLink(ctx, "st$sp!aaa", "link-view", "https://cloud.example.org/s/OTHER"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("changed link: %v", err)
	}
	if err := f.api().removeLink(ctx, "st$sp!aaa", "link-view", ""); err == nil {
		t.Error("no confirmed address accepted")
	}
	if f.count("DELETE") != 0 {
		t.Fatalf("DELETE sent: %v", f.log())
	}
	if err := f.api().removeLink(ctx, "st$sp!aaa", "link-view", "https://cloud.example.org/s/AAA"); err != nil {
		t.Fatal(err)
	}
	log := f.log()
	n := len(log)
	if n < 3 || log[n-1] != "DELETE /opencloud/graph/v1beta1/drives/st$sp/items/st$sp%21aaa/permissions/link-view" ||
		log[n-2] != "GET /opencloud/graph/v1beta1/drives/st$sp/items/st$sp%21aaa/permissions/link-view" ||
		log[n-3] != "GET /opencloud/graph/v1beta1/me/drive/sharedByMe" {
		t.Errorf("sequence: %v", log)
	}
	if f.count("DELETE") != 1 {
		t.Errorf("deletes: %v", log)
	}
}

func TestCopyLinkReadsFresh(t *testing.T) {
	f := newFakeGraph(t)
	// The list has an old address; the server says another one now: Copy gives the fresh one.
	f.setPerm("st$sp!aaa/link-view", `{"id":"link-view","link":{"type":"view","webUrl":"https://cloud.example.org/s/FRESH"}}`)
	u, err := f.api().linkURL(context.Background(), "st$sp!aaa", "link-view")
	if err != nil || u != "https://cloud.example.org/s/FRESH" {
		t.Errorf("%q %v", u, err)
	}
	f.setPerm("st$sp!aaa/link-view", `{"id":"link-view","link":{"type":"view","webUrl":"http://cloud.example.org/s/PLAIN"}}`)
	if _, err := f.api().linkURL(context.Background(), "st$sp!aaa", "link-view"); err == nil {
		t.Error("an http link was copied")
	}
}

func TestGraphIDsValidated(t *testing.T) {
	for _, id := range []string{"", ".", "..", "a/b", "a?b", "a#b", `a\b`, "a%2Fb", "a b", "a\nb", strings.Repeat("x", 301)} {
		if validGraphID(id) {
			t.Errorf("%q accepted", id)
		}
	}
	for _, id := range []string{"link-view", "st$sp!aaa", "a0ca6a90-a365-4782-871e-d44447bbc668$a0ca6a90!4a5", "YWJj=="} {
		if !validGraphID(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, item := range []string{"nobang", "!x", "x!", "st$sp!a/b", "st$sp!a?b", "st$sp!a#b", `st$sp!a\b`, "../x!y", "a/b!c", "st$sp!.."} {
		if p, err := graphItemPath(item); err == nil {
			t.Errorf("item %q accepted: %s", item, p)
		}
	}
	p, err := graphItemPath("st$sp!aaa")
	if err != nil || p != "/graph/v1beta1/drives/st$sp/items/st$sp%21aaa" {
		t.Errorf("path %q %v", p, err)
	}
}

// Ids that came from the page are checked before anything is sent.
func TestBadIDsSendNothing(t *testing.T) {
	f := newFakeGraph(t)
	ctx := context.Background()
	exp := time.Now().Add(48 * time.Hour)
	for _, c := range [][2]string{{"st$sp!aaa/../bbb", "link-view"}, {"st$sp!aaa", "link-view/../x"}, {"st$sp!aaa?x=1", "p"}, {"st$sp!aaa", "p#f"}, {`st$sp!a\a`, "p"}, {"st$sp!aaa", ""}, {"", "p"}} {
		if err := f.api().removeLink(ctx, c[0], c[1], "u"); err == nil {
			t.Errorf("remove %v", c)
		}
		if err := f.api().setLinkExpiry(ctx, c[0], c[1], &exp); err == nil {
			t.Errorf("expiry %v", c)
		}
		if _, err := f.api().linkURL(ctx, c[0], c[1]); err == nil {
			t.Errorf("copy %v", c)
		}
	}
	if got := f.log(); len(got) != 0 {
		t.Errorf("requests were sent: %v", got)
	}
}

// hostRecorder is a transport that notes the host of every request and refuses any host but one.
type hostRecorder struct {
	mu    sync.Mutex
	hosts []string
	allow string
	next  http.RoundTripper
}

func (h *hostRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.hosts = append(h.hosts, r.URL.Host)
	h.mu.Unlock()
	if r.URL.Host != h.allow {
		return nil, fmt.Errorf("request to %s", r.URL.Host)
	}
	return h.next.RoundTrip(r)
}

func useHostRecorder(t *testing.T, allow string) *hostRecorder {
	rec := &hostRecorder{allow: allow, next: http.DefaultTransport}
	old := accountClient.Transport
	accountClient.Transport = rec
	t.Cleanup(func() { accountClient.Transport = old })
	return rec
}

func TestNoRequestToAnotherHost(t *testing.T) {
	f := newFakeGraph(t)
	u, _ := url.Parse(f.srv.URL)
	rec := useHostRecorder(t, u.Host)
	// The server hands out addresses on other hosts: they are data, never fetched.
	evil := "https://evil.example/s/AAA"
	f.setShared(strings.ReplaceAll(sharedByMeJSON, "https://cloud.example.org/s/AAA", evil))
	f.setPerm("st$sp!aaa/link-view", `{"id":"link-view","link":{"type":"view","webUrl":"`+evil+`"}}`)
	f.setList(`{"value":[{"id":"v","link":{"type":"view","webUrl":"` + evil + `"}}]}`)
	ctx := context.Background()
	g := f.api()
	exp := time.Now().Add(72 * time.Hour)
	if _, err := g.sharedLinks(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := g.linkURL(ctx, "st$sp!aaa", "link-view"); err != nil {
		t.Fatal(err)
	}
	if err := g.setLinkExpiry(ctx, "st$sp!aaa", "link-view", &exp); err != nil {
		t.Fatal(err)
	}
	if _, err := g.existingViewLink(ctx, "st$sp!aaa", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := g.removeLink(ctx, "st$sp!aaa", "link-view", evil); err != nil {
		t.Fatal(err)
	}
	if len(rec.hosts) == 0 {
		t.Fatal("nothing was requested")
	}
	for _, h := range rec.hosts {
		if h != u.Host {
			t.Errorf("request to %s", h)
		}
	}
	// A base that isn't the host an address is built on can't be used to reach another one.
	if _, _, err := (graphAPI{base: "https://good.example", tok: "t"}).do(ctx, "GET", "@evil.example/x", nil); err == nil {
		t.Error("an address on another host was requested")
	}
}

func TestLinkCallsDoNotFollowRedirects(t *testing.T) {
	var evilHits int
	var mu sync.Mutex
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		evilHits++
		mu.Unlock()
	}))
	defer evil.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusTemporaryRedirect) // 307 keeps method and body
	}))
	defer redir.Close()
	g := graphAPI{base: redir.URL, tok: "secret-token"}
	ctx := context.Background()
	exp := time.Now().Add(72 * time.Hour)
	if _, err := g.sharedLinks(ctx); err == nil {
		t.Error("list followed a redirect")
	}
	if _, err := g.existingViewLink(ctx, "st$sp!aaa", time.Now()); err == nil {
		t.Error("permissions followed a redirect")
	}
	if err := g.setLinkExpiry(ctx, "st$sp!aaa", "link-view", &exp); err == nil {
		t.Error("expiry followed a redirect")
	}
	if err := g.removeLink(ctx, "st$sp!aaa", "link-view", "u"); err == nil {
		t.Error("remove followed a redirect")
	}
	if _, err := davFileID(ctx, redir.URL+"/dav/x", "secret-token"); err == nil {
		t.Error("PROPFIND followed a redirect")
	}
	mu.Lock()
	defer mu.Unlock()
	if evilHits != 0 {
		t.Errorf("the other server got %d requests", evilHits)
	}
}

func TestItemWebURL(t *testing.T) {
	f := newFakeGraph(t)
	cfg := spaceCfg(t)
	cfg.ServerURL, cfg.DavPath = f.base(), "/remote.php/dav/files/sam"
	k := KeptFolder{Path: "Docs", StateName: "d"}
	u, err := itemWebURL(context.Background(), cfg, k, "Q1 report.pdf", "tok")
	if err != nil || u != f.base()+"/f/st$sp%21aaa" {
		t.Errorf("%q %v", u, err)
	}
	if got := f.log(); len(got) != 1 || !strings.HasPrefix(got[0], "PROPFIND ") {
		t.Errorf("requests: %v", got)
	}
	// An id that isn't one plain segment can't make an address.
	f.setFileID("a/../b?x#y")
	if u, err := itemWebURL(context.Background(), cfg, k, "x", "tok"); err == nil {
		t.Errorf("odd id accepted: %q", u)
	}
	if _, err := itemWebURL(context.Background(), cfg, k, "../x", "tok"); err == nil {
		t.Error("path outside the folder")
	}
}

func TestSharedLinksNeedSignIn(t *testing.T) {
	e := pausedRig(t, `,"auth":"rclone"`)
	if _, err := e.SharedLinks(); err == nil || !strings.Contains(err.Error(), "browser sign-in") {
		t.Errorf("list: %v", err)
	}
	if err := e.SharedLinkRemove("st$sp!aaa", "p", "u"); err == nil {
		t.Error("remove without sign-in")
	}
	// A date that has passed is refused before the server is asked.
	if err := e.SharedLinkExpiry("st$sp!aaa", "p", 0, "2001-01-01"); err == nil || !strings.Contains(err.Error(), "passed") {
		t.Errorf("expiry: %v", err)
	}
}

// A server that answers only PATCH and DELETE with a 307 (the list and the GETs work): neither
// call follows it, each is sent once, and nothing reaches the other host.
func TestPatchAndDeleteRedirectsNotFollowed(t *testing.T) {
	var mu sync.Mutex
	evilHits := 0
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		evilHits++
		mu.Unlock()
	}))
	defer evil.Close()
	counts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.Method]++
		mu.Unlock()
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/sharedByMe"):
			_, _ = io.WriteString(w, sharedByMeJSON)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/permissions/link-view"):
			_, _ = io.WriteString(w, `{"id":"link-view","expirationDateTime":"2026-12-01T23:59:59Z","link":{"type":"view","webUrl":"https://cloud.example.org/s/AAA"}}`)
		case r.Method == "PATCH" || r.Method == "DELETE":
			http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := graphAPI{base: srv.URL, tok: "secret-token"}
	ctx := context.Background()
	exp := time.Now().Add(72 * time.Hour)
	if err := g.setLinkExpiry(ctx, "st$sp!aaa", "link-view", &exp); err == nil {
		t.Error("PATCH followed or accepted a redirect")
	}
	if err := g.removeLink(ctx, "st$sp!aaa", "link-view", "https://cloud.example.org/s/AAA"); err == nil {
		t.Error("DELETE followed or accepted a redirect")
	}
	mu.Lock()
	defer mu.Unlock()
	if evilHits != 0 {
		t.Errorf("the other server got %d requests", evilHits)
	}
	if counts["PATCH"] != 1 || counts["DELETE"] != 1 {
		t.Errorf("requests %v: each should be sent once", counts)
	}
}

func TestEnforcedPasswordIsTyped(t *testing.T) {
	f := newFakeGraph(t)
	f.setCreateErr(`{"error":{"code":"invalidRequest","message":"Password Protection Is Enforced"}}`)
	_, err := createShareLinkFor(context.Background(), "st$sp!aaa", f.base(), "tok", ShareOptions{Days: 7}, time.Now())
	if !errors.Is(err, errPasswordRequired) {
		t.Fatalf("err %v", err)
	}
	if err.Error() != "This server requires a password for public links." {
		t.Errorf("text %q", err)
	}
	// Other 400s, and the same words with another status, are ordinary errors.
	f.setCreateErr(`{"error":{"code":"invalidRequest","message":"bad expiry"}}`)
	if _, err = createShareLinkFor(context.Background(), "st$sp!aaa", f.base(), "tok", ShareOptions{}, time.Now()); err == nil || errors.Is(err, errPasswordRequired) {
		t.Errorf("other 400: %v", err)
	}
	if passwordEnforced(403, []byte(`{"error":{"message":"password protection is enforced"}}`)) || passwordEnforced(400, []byte("<html>")) {
		t.Error("recognised something else")
	}
	// It comes through shareOrReuse unchanged, and with a password the link is made.
	f.setCreateErr(`{"error":{"code":"invalidRequest","message":"password protection is enforced"}}`)
	dav := f.base() + "/dav/files/sam/Q1.pdf"
	if _, err = shareOrReuse(context.Background(), dav, f.api(), ShareOptions{}, true, time.Now(), nil); !errors.Is(err, errPasswordRequired) {
		t.Errorf("shareOrReuse: %v", err)
	}
	f.setCreateErr("")
	if r, err := shareOrReuse(context.Background(), dav, f.api(), ShareOptions{Password: "pw"}, true, time.Now(), nil); err != nil || r.URL == "" {
		t.Errorf("retry: %+v %v", r, err)
	}
}

func TestExpiryForUsesLocalDay(t *testing.T) {
	nz := time.FixedZone("NZDT", 13*3600)                 // UTC+13
	la := time.FixedZone("PST", -8*3600)                  // UTC-8
	now := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC) // 19 Nov 09:00 in NZ, 18 Nov 12:00 in LA
	// In NZ it is already the 19th: the 18th has passed there, though it hasn't in UTC.
	if e, err := expiryForIn(0, "2026-11-18", now, nz); err == nil {
		t.Errorf("NZ 18th accepted: %v", e)
	}
	e, err := expiryForIn(0, "2026-11-19", now, nz)
	if err != nil || e == nil || e.UTC().Format(time.RFC3339) != "2026-11-19T10:59:59Z" {
		t.Errorf("NZ 19th: %v %v", e, err)
	}
	// In LA it is still the 18th.
	e, err = expiryForIn(0, "2026-11-18", now, la)
	if err != nil || e == nil || e.UTC().Format(time.RFC3339) != "2026-11-19T07:59:59Z" {
		t.Errorf("LA 18th: %v %v", e, err)
	}
	if _, err = expiryForIn(0, "2026-11-17", now, la); err == nil {
		t.Error("LA 17th accepted")
	}
	// The 10-year limit counts local days too.
	if _, err = expiryForIn(0, "2036-11-18", now, la); err != nil {
		t.Errorf("exactly 10 years: %v", err)
	}
	if _, err = expiryForIn(0, "2036-11-19", now, la); err == nil {
		t.Error("beyond 10 years accepted")
	}
}

func TestExpiryReadBackMismatch(t *testing.T) {
	f := newFakeGraph(t)
	f.setNoApply(true) // 200, but the server changed nothing
	ctx := context.Background()
	exp := time.Date(2026, 12, 25, 23, 59, 59, 0, time.UTC)
	err := f.api().setLinkExpiry(ctx, "st$sp!aaa", "link-view", &exp)
	if err == nil || err.Error() != "OpenCloud didn't change the expiry" {
		t.Errorf("set: %v", err)
	}
	if err = f.api().setLinkExpiry(ctx, "st$sp!aaa", "link-view", nil); err == nil || err.Error() != "OpenCloud didn't change the expiry" {
		t.Errorf("remove: %v", err)
	}
	// A server that does apply it passes the check.
	f.setNoApply(false)
	if err = f.api().setLinkExpiry(ctx, "st$sp!aaa", "link-view", nil); err != nil {
		t.Errorf("remove (applied): %v", err)
	}
	if err = f.api().setLinkExpiry(ctx, "st$sp!aaa", "link-view", &exp); err != nil {
		t.Errorf("set (applied): %v", err)
	}
	d := time.Date(2026, 12, 25, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		got  string
		want *time.Time
		ok   bool
	}{
		{"", nil, true}, {"0001-01-01T00:00:00Z", nil, true}, {"2026-12-25T23:59:59Z", nil, false},
		{"2026-12-25T23:59:59Z", &d, true}, {"2026-12-26T23:59:59Z", &d, false}, {"", &d, false}, {"soon", &d, false}, {"soon", nil, false},
	} {
		if got := expiryMatches(c.got, c.want); got != c.ok {
			t.Errorf("expiryMatches(%q, %v) = %v", c.got, c.want, got)
		}
	}
}

func TestUnparseableExpirySkippedForReuse(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	mk := func(id, exp, created string) gPerm {
		p := gPerm{ID: id, Expiration: exp, Created: created}
		p.Link = &struct {
			Type   string `json:"type"`
			WebURL string `json:"webUrl"`
		}{Type: "view", WebURL: "https://cloud.example.org/s/" + id}
		return p
	}
	got := pickExistingView([]gPerm{mk("odd", "next tuesday", "2026-11-17T00:00:00Z"), mk("ok", "2026-12-24T23:59:59Z", "2026-11-01T00:00:00Z")}, now)
	if got == nil || got.URL != "https://cloud.example.org/s/ok" {
		t.Errorf("got %+v", got)
	}
	if got := pickExistingView([]gPerm{mk("odd", "next tuesday", "2026-11-17T00:00:00Z")}, now); got != nil {
		t.Errorf("an unreadable expiry was reused: %+v", got)
	}
}
