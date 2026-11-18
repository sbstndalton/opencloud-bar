package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 2.2: server search, storage quota warnings, and tests that keep out of the real app folder.

// realAppDir is where this computer's OpenCloud Bar keeps its settings and logs (resolved
// before TestMain points the environment elsewhere). No test may use it.
var (
	realAppDir   string
	appDirMisuse atomic.Value // string: the first real app folder a test resolved
)

// underDir: p is root or inside it.
func underDir(p, root string) bool {
	p, root = filepath.Clean(p), filepath.Clean(root)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// TestMain points HOME (and the config folders of each platform) at a temporary folder for the
// whole run. Tests that set their own with t.Setenv get it back afterwards, so an engine a test
// started that still writes a log after the test ended (live sync, a sync finishing) writes it
// there, never into the real ~/Library/Application Support/OpenCloudBar (or ~/.config,
// %AppData%). Every app folder resolved is checked; the run fails if one was the real one.
func TestMain(m *testing.M) {
	realAppDir = appDir()
	tmp, err := os.MkdirTemp("", "ocb-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "no temporary folder:", err)
		os.Exit(2)
	}
	for k, v := range map[string]string{"HOME": tmp, "XDG_CONFIG_HOME": filepath.Join(tmp, "cfg"),
		"APPDATA": filepath.Join(tmp, "cfg"), "LOCALAPPDATA": filepath.Join(tmp, "local"), "USERPROFILE": tmp} {
		_ = os.Setenv(k, v)
	}
	appDirGuard = func(d string) {
		if underDir(d, realAppDir) {
			appDirMisuse.CompareAndSwap(nil, d)
		}
	}
	code := m.Run()
	if d, _ := appDirMisuse.Load().(string); d != "" {
		fmt.Fprintf(os.Stderr, "FAIL: a test used the real app folder %s\n", d)
		code = 1
	}
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

func TestTestsNeverUseTheRealAppFolder(t *testing.T) {
	if realAppDir == "" {
		t.Fatal("TestMain didn't run")
	}
	for _, d := range []string{appDir(), logDir(), configFile(), defaultConfig().StateDir} {
		if underDir(d, realAppDir) {
			t.Errorf("%s is in the real app folder %s", d, realAppDir)
		}
	}
	// A log written after a test's own environment is gone lands in the run's temporary home.
	t.Run("own env", func(t *testing.T) { testEnv(t) })
	f := openLogFile("guard-test.log")
	if f == nil {
		t.Fatal("no log file")
	}
	name := f.Name()
	f.Close()
	if underDir(name, realAppDir) || !underDir(name, os.Getenv("HOME")) {
		t.Errorf("log written to %s", name)
	}
	if !underDir(filepath.Join(realAppDir, "logs", "x.log"), realAppDir) || underDir(realAppDir+"-other", realAppDir) {
		t.Error("underDir")
	}
	if d, _ := appDirMisuse.Load().(string); d != "" {
		t.Errorf("the real app folder was used: %s", d)
	}
}

// ---- search

const searchAnswer = `<?xml version="1.0" encoding="UTF-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
 <d:response><d:href>/dav/spaces/storage-users-1%241111/Docs/report.txt</d:href>
  <d:propstat><d:prop><oc:fileid>storage-users-1$1111!aaaa</oc:fileid><oc:file-parent>storage-users-1$1111!docs</oc:file-parent><oc:name>report.txt</oc:name>
   <d:getlastmodified>Tue, 10 Nov 2026 09:30:00 GMT</d:getlastmodified><d:resourcetype/><d:getcontentlength>7</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/dav/spaces/storage-users-1$1111/Docs/report.txt</d:href>
  <d:propstat><d:prop><oc:fileid>storage-users-1$1111!aaaa</oc:fileid><oc:name>report.txt</oc:name></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/dav/spaces/storage-users-1$1111/Other/Reports</d:href>
  <d:propstat><d:prop><oc:fileid>storage-users-1$1111!rrrr</oc:fileid><oc:file-parent>storage-users-1$1111!other</oc:file-parent><oc:name>Reports</oc:name>
   <d:resourcetype><d:collection/></d:resourcetype><oc:size>4096</oc:size></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/dav/spaces/storage-users-1$2222/Docs/plans/report.md</d:href>
  <d:propstat><d:prop><oc:fileid>storage-users-1$2222!mmmm</oc:fileid><oc:file-parent>storage-users-1$2222!plans</oc:file-parent><oc:name>report.md</oc:name></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/dav/spaces/storage-users-1$1111/../etc/report</d:href>
  <d:propstat><d:prop><oc:fileid>x1</oc:fileid></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/dav/meta/storage-users-1$1111!aaaa/v/report</d:href>
  <d:propstat><d:prop><oc:fileid>x2</oc:fileid></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/dav/spaces/a0ca6a90$a0ca6a90%21share1/report-shared.pdf</d:href>
  <d:propstat><d:prop><oc:fileid>s3</oc:fileid><oc:file-parent>bad/id</oc:file-parent></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`

func TestSearchRequestAndResults(t *testing.T) {
	e, oc := historyRig(t)
	writeReport(t, e, "current")
	var got struct {
		method, depth, ctype string
		body                 string
	}
	oc.report = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/opencloud/dav/spaces/" {
			t.Errorf("REPORT %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		got.method, got.depth, got.ctype, got.body = r.Method, r.Header.Get("Depth"), r.Header.Get("Content-Type"), string(b)
		w.Header().Set("Content-Range", "rows 0-5/120")
		w.WriteHeader(207)
		_, _ = io.WriteString(w, searchAnswer)
	}
	res, err := e.Search(`  rep"ort  `)
	if err != nil {
		t.Fatal(err)
	}
	// The request, as the web UI sends it.
	var body struct {
		XMLName xml.Name
		Props   struct {
			Inner []struct{ XMLName xml.Name } `xml:",any"`
		} `xml:"prop"`
		Pattern string `xml:"search>pattern"`
		Limit   int    `xml:"search>limit"`
	}
	if err := xml.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatalf("body %q: %v", got.body, err)
	}
	if body.XMLName.Local != "search-files" || body.XMLName.Space != "http://owncloud.org/ns" {
		t.Errorf("element %v", body.XMLName)
	}
	if body.Pattern != `name:"*rep""ort*"` || body.Limit != 50 {
		t.Errorf("pattern %q limit %d", body.Pattern, body.Limit)
	}
	if got.depth != "" || !strings.Contains(got.ctype, "xml") {
		t.Errorf("headers depth %q type %q", got.depth, got.ctype)
	}
	// The results: repeats, unsafe paths and hrefs outside the spaces left out.
	if res.Total != 120 || len(res.Results) != 4 {
		t.Fatalf("got %d of %d: %+v", len(res.Results), res.Total, res.Results)
	}
	r0 := res.Results[0]
	if r0.Name != "report.txt" || r0.Folder != "/Docs" || r0.Dir || r0.Size != 7 || r0.WebID != "storage-users-1$1111!aaaa" ||
		!r0.Modified.Equal(reportTime) || r0.Kept != "Docs" || r0.Rel != "report.txt" {
		t.Errorf("kept file: %+v", r0)
	}
	if r := res.Results[1]; r.Name != "Reports" || !r.Dir || r.Folder != "/Other" || r.Kept != "" || r.Size != 4096 || r.WebID != "storage-users-1$1111!rrrr" {
		t.Errorf("folder elsewhere: %+v", r)
	}
	// In a kept folder of a space, but not on this computer: no Show on this computer.
	if r := res.Results[2]; r.Folder != "Marketing: /Docs/plans" || r.Kept != "" || r.WebID != "storage-users-1$2222!mmmm" {
		t.Errorf("space: %+v", r)
	}
	// An unusable parent id doesn't matter: the item's own id is used.
	if r := res.Results[3]; r.Name != "report-shared.pdf" || r.WebID != "s3" || r.Folder != "/" {
		t.Errorf("share: %+v", r)
	}
	// Only the search, and the drives it needs: nothing written.
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if len(oc.writes) != 0 {
		t.Errorf("writes: %v", oc.writes)
	}
	for _, rq := range oc.requests {
		if rq != "GET /opencloud/graph/v1.0/me/drives" && rq != "REPORT /opencloud/dav/spaces/" {
			t.Errorf("request %s", rq)
		}
	}
}

func TestSearchShowsLocalOnlyWhenThere(t *testing.T) {
	e, oc := historyRig(t)
	// Marketing's Docs/plans/report.md on this computer now.
	mk := filepath.Join(e.cfg.Local(e.cfg.Kept[1]), "plans")
	_ = os.MkdirAll(mk, 0o700)
	_ = os.WriteFile(filepath.Join(mk, "report.md"), []byte("x"), 0o600)
	oc.report = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(207)
		_, _ = io.WriteString(w, searchAnswer)
	}
	res, err := e.Search("report")
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Results[0]; r.Kept != "" {
		t.Errorf("report.txt isn't on this computer: %+v", r)
	}
	if r := res.Results[2]; r.Kept != e.cfg.Kept[1].Key() || r.Rel != "plans/report.md" {
		t.Errorf("space file: %+v", r)
	}
	// Total not said: the number shown.
	if res.Total != 4 {
		t.Errorf("total %d", res.Total)
	}
	// The item opens in its folder through the existing "item" kind.
	p, err := e.itemLocal(res.Results[2].Kept, res.Results[2].Rel)
	if err != nil || p != filepath.Join(mk, "report.md") {
		t.Errorf("local %q %v", p, err)
	}
}

func TestSearchErrors(t *testing.T) {
	e, oc := historyRig(t)
	for term, want := range map[string]string{"": "Type part of a name to search for.", "   ": "Type part of a name to search for.",
		"a\x00b": "That isn't something a name can contain.", strings.Repeat("x", 201): "That's too long to search for."} {
		if _, err := e.Search(term); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", term, err, want)
		}
	}
	oc.mu.Lock()
	n := len(oc.requests)
	oc.mu.Unlock()
	if n != 0 {
		t.Errorf("bad terms sent %d requests", n)
	}
	for code, want := range map[int]string{501: "doesn't offer search", 400: "Try other words", 401: "Sign in again", 500: "answered 500"} {
		oc.report = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
		_, err := e.Search("x")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%d: %v", code, err)
			continue
		}
		// Shown to the user: a capitalized full sentence.
		if m := err.Error(); m[0] < 'A' || m[0] > 'Z' || !strings.HasSuffix(m, ".") {
			t.Errorf("%d: %q isn't a capitalized sentence", code, m)
		}
	}
	oc.report = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(207); _, _ = io.WriteString(w, "<nope") }
	if _, err := e.Search("x"); err == nil || err.Error() != "The server's answer wasn't what was expected." {
		t.Errorf("garbage: %v", err)
	}
	// A redirect isn't followed (the token stays with the server).
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("redirect followed to %s", r.URL) }))
	defer other.Close()
	oc.report = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}
	if _, err := e.Search("x"); err == nil {
		t.Error("a redirect counted as results")
	}
}

func TestSearchWebURL(t *testing.T) {
	cfg := Config{ServerURL: "https://cloud.example.com/oc"}
	u, err := searchWebURL(cfg, "storage-users-1$1111!docs")
	if err != nil || u != "https://cloud.example.com/oc/f/storage-users-1$1111%21docs" {
		t.Errorf("%q %v", u, err)
	}
	for _, id := range []string{"", "a/b", "../x", "a?b", "a#b", `a\b`, "a b", "a\nb"} {
		if _, err := searchWebURL(cfg, id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
	if _, err := spacesRootURL("https://h", "/dav/files/sam"); err == nil {
		t.Error("not a spaces path")
	}
	if u, _ := spacesRootURL("https://h", "/oc/dav/spaces/s$1/"); u != "https://h/oc/dav/spaces/" {
		t.Errorf("root %q", u)
	}
	if !spaceMatches("p$s!share", "s!share") || spaceMatches("p$s!share", "x!share") || spaceMatches("", "") {
		t.Error("spaceMatches")
	}
	if contentRangeTotal("rows 0-49/123") != 123 || contentRangeTotal("") != -1 || contentRangeTotal("rows 0-1/x") != -1 {
		t.Error("content range")
	}
}

// ---- quota

func TestQuotaThresholds(t *testing.T) {
	q := func(used, total int64) *quotaInfo { return &quotaInfo{Used: used, Total: total} }
	last := 0
	steps := []struct {
		q    *quotaInfo
		warn int
	}{
		{q(50, 100), 0},
		{q(90, 100), 90},   // warn once
		{q(95, 100), 0},    // no repeat
		{q(92, 100), 0},    // still above 90
		{q(100, 100), 100}, // full
		{q(120, 100), 0},   // over: still full, said already
		{q(96, 100), 0},    // back under 100 but above 90: nothing new
		{q(89, 100), 0},    // below 90: re-armed
		{q(91, 100), 90},   // warns again
		{q(0, 0), 0},       // no limit: no warnings
		{nil, 0},           // the server gave none: nothing to warn about
		{q(100, 100), 100},
	}
	for i, s := range steps {
		w, rem := nextQuotaWarning(last, s.q)
		if w != s.warn {
			t.Errorf("step %d (%+v): warned %d, want %d", i, s.q, w, s.warn)
		}
		last = rem
	}
	if quotaLevel(q(899, 1000)) != 0 || quotaLevel(q(900, 1000)) != 90 || quotaLevel(&quotaInfo{Used: 5, Total: -1}) != 0 {
		t.Error("levels")
	}
	if got := quotaText(quotaInfo{Used: 1536 << 20, Total: 10 << 30}); got != "1.5 GB of 10.0 GB used" {
		t.Errorf("text %q", got)
	}
}

func TestFetchQuota(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oc/graph/v1.0/me/drives" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		query = r.URL.Query().Get("$filter")
		_, _ = io.WriteString(w, `{"value":[{"id":"s$1","driveType":"personal","quota":{"used":950,"total":1000,"remaining":50,"state":"nearing"}}]}`)
	}))
	defer srv.Close()
	q, err := fetchQuota(t.Context(), srv.URL+"/oc", "tok")
	if err != nil || q == nil || q.Used != 950 || q.Total != 1000 || q.Remaining != 50 || q.State != "nearing" {
		t.Fatalf("%+v %v", q, err)
	}
	if query != "driveType eq 'personal'" {
		t.Errorf("filter %q", query)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"value":[{"id":"s$1","driveType":"personal","quota":{"used":950,"remaining":0}}]}`)
	}))
	defer srv2.Close()
	q, err = fetchQuota(t.Context(), srv2.URL, "tok")
	if err != nil || q == nil || q.Total != 0 || quotaLevel(q) != 0 {
		t.Errorf("unlimited: %+v %v", q, err)
	}
}

// signedIn: the rig's engine was made before its sign-in was written.
func signedIn(e *Engine) {
	e.mu.Lock()
	e.signedOut = false
	e.mu.Unlock()
}

func TestQuotaWarningsOncePerCrossing(t *testing.T) {
	e, _ := historyRig(t)
	signedIn(e)
	var mu sync.Mutex
	var shown []string
	done := make(chan struct{}, 10)
	old := showNotification
	showNotification = func(title, body string) error {
		mu.Lock()
		shown = append(shown, title)
		mu.Unlock()
		done <- struct{}{}
		return nil
	}
	t.Cleanup(func() { showNotification = old })
	apply := func(used, total int64, want bool) {
		t.Helper()
		e.applyQuota(e.config(), &quotaInfo{Used: used, Total: total}, time.Now())
		if want {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("no notification at %d/%d", used, total)
			}
		}
	}
	apply(90, 100, true)
	apply(95, 100, false)
	// Restarted: remembered in the state folder.
	if b, _ := os.ReadFile(filepath.Join(expand(e.cfg.StateDir), "quota-warned")); strings.TrimSpace(string(b)) != "90" {
		t.Errorf("remembered %q", b)
	}
	mu.Lock()
	shownBefore := len(shown)
	mu.Unlock()
	changesBefore := len(e.RecentChanges(0))
	e2 := NewEngine()
	defer e2.Shutdown()
	if e2.cfg.StateDir != e.cfg.StateDir {
		t.Fatalf("the restarted engine's state folder is %q, not %q", e2.cfg.StateDir, e.cfg.StateDir)
	}
	e2.applyQuota(e2.config(), &quotaInfo{Used: 96, Total: 100}, time.Now())
	time.Sleep(100 * time.Millisecond) // a (wrong) notification is shown from a goroutine
	mu.Lock()
	if len(shown) != shownBefore {
		t.Errorf("a restart repeated the warning: %q", shown)
	}
	mu.Unlock()
	if n := len(e.RecentChanges(0)); n != changesBefore {
		t.Errorf("a restart added %d change(s)", n-changesBefore)
	}
	apply(100, 100, true)
	apply(100, 100, false)
	apply(10, 100, false) // re-armed
	if exists(filepath.Join(expand(e.cfg.StateDir), "quota-warned")) {
		t.Error("not re-armed")
	}
	apply(0, 0, false) // no limit
	apply(93, 100, true)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	want := []string{"Your OpenCloud storage is almost full", "Your OpenCloud storage is full", "Your OpenCloud storage is almost full"}
	if strings.Join(shown, "|") != strings.Join(want, "|") {
		t.Errorf("shown %q", shown)
	}
	if st := e.Status(); st.Storage == nil || st.Storage.Used != 93 {
		t.Errorf("status storage %+v", st.Storage)
	}
}

func TestQuotaCheckIsDueEvery30Minutes(t *testing.T) {
	e, oc := historyRig(t)
	signedIn(e)
	oc.mu.Lock()
	oc.drives = `{"value":[{"id":"storage-users-1$1111","driveType":"personal","quota":{"used":10,"total":100}}]}`
	oc.mu.Unlock()
	e.mu.Lock()
	e.quotaDueLocked()
	e.mu.Unlock()
	waitFor(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return !e.quotaBusy && e.quotaShown != nil })
	e.mu.Lock()
	e.quotaDueLocked() // just checked: not again
	busy := e.quotaBusy
	e.quotaAt = time.Now().Add(-31 * time.Minute)
	e.quotaDueLocked()
	again := e.quotaBusy
	e.mu.Unlock()
	if busy || !again {
		t.Errorf("busy %v again %v", busy, again)
	}
	waitFor(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return !e.quotaBusy })
	oc.mu.Lock()
	defer oc.mu.Unlock()
	n := 0
	for _, r := range oc.requests {
		if r == "GET /opencloud/graph/v1.0/me/drives" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d checks", n)
	}
}

// ---- 2.2 review round

func TestSearchOnlyTalksToTheConfiguredServer(t *testing.T) {
	e, oc := historyRig(t)
	var other atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Add(1)
		http.Error(w, "no", 400)
	}))
	defer foreign.Close()
	// The server's drives list says the personal space's WebDAV root is on another host.
	oc.mu.Lock()
	oc.drives = `{"value":[{"id":"storage-users-1$1111","name":"Sam","driveType":"personal","root":{"webDavUrl":"` + foreign.URL + `/opencloud/dav/spaces/storage-users-1$1111"}}]}`
	oc.mu.Unlock()
	var reportHost string
	oc.report = func(w http.ResponseWriter, r *http.Request) {
		reportHost = r.Host
		w.WriteHeader(207)
		_, _ = io.WriteString(w, searchAnswer)
	}
	res, err := e.Search("report")
	if err != nil || len(res.Results) == 0 {
		t.Fatalf("%v %+v", err, res)
	}
	oc.mu.Lock()
	gotHost := reportHost
	oc.mu.Unlock()
	if want := strings.TrimPrefix(oc.srv.URL, "http://"); gotHost != want {
		t.Errorf("the search went to %q, not the configured server %q", gotHost, want)
	}
	if n := other.Load(); n != 0 {
		t.Errorf("the other host got %d request(s) (with the bearer token)", n)
	}
}

func TestShowInBrowserUsesTheItemsOwnID(t *testing.T) {
	e, oc := historyRig(t)
	oc.report = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(207)
		_, _ = io.WriteString(w, searchAnswer)
	}
	res, err := e.Search("report")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"report.txt": "storage-users-1$1111!aaaa", "Reports": "storage-users-1$1111!rrrr", "report.md": "storage-users-1$2222!mmmm"}
	for _, r := range res.Results {
		if w, ok := want[r.Name]; ok {
			if r.WebID != w {
				t.Errorf("%s opens %q, want its own id %q", r.Name, r.WebID, w)
			}
			u, err := searchWebURL(e.config(), r.WebID)
			if err != nil || !strings.HasPrefix(u, oc.srv.URL+"/opencloud/f/") || !strings.Contains(u, strings.ReplaceAll(url.PathEscape(w), "%24", "$")) {
				t.Errorf("%s: link %q %v", r.Name, u, err)
			}
		}
		if strings.Contains(r.WebID, "!docs") || strings.Contains(r.WebID, "!other") || strings.Contains(r.WebID, "!plans") {
			t.Errorf("%s opens a parent id %q", r.Name, r.WebID)
		}
	}
	// No usable id of its own: no button, even though a parent id was given.
	noID := `<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/dav/spaces/storage-users-1$1111/Docs/n.txt</d:href>
	 <d:propstat><d:prop><oc:file-parent>storage-users-1$1111!docs</oc:file-parent><oc:name>n.txt</oc:name></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`
	oc.report = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(207); _, _ = io.WriteString(w, noID) }
	res, err = e.Search("n")
	if err != nil || len(res.Results) != 1 || res.Results[0].WebID != "" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestSearchLocalButtonMatchesItemLocal(t *testing.T) {
	e, oc := historyRig(t)
	root := e.cfg.Local(e.cfg.Kept[0])
	names := []string{"ok.txt", "a:b.txt", "sub:dir/f.txt", "sub/g.txt"}
	for _, n := range names {
		p := filepath.Join(root, filepath.FromSlash(n))
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Skipf("this file system can't hold %q: %v", n, err)
		}
	}
	var b strings.Builder
	b.WriteString(`<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">`)
	for i, n := range names {
		fmt.Fprintf(&b, `<d:response><d:href>/dav/spaces/storage-users-1$1111/Docs/%s</d:href><d:propstat><d:prop><oc:fileid>id%d</oc:fileid></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, n, i)
	}
	b.WriteString(`</d:multistatus>`)
	oc.report = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(207); _, _ = io.WriteString(w, b.String()) }
	res, err := e.Search("x")
	if err != nil || len(res.Results) != len(names) {
		t.Fatalf("%v %+v", err, res)
	}
	for i, r := range res.Results {
		_, refused := e.itemLocal(e.cfg.Kept[0].Key(), names[i])
		wantButton := refused == nil
		if (r.Kept != "") != wantButton {
			t.Errorf("%q: Show on This Computer %v, but itemLocal refuses: %v", names[i], r.Kept != "", refused)
		}
		if r.Kept != "" {
			if _, err := e.itemLocal(r.Kept, r.Rel); err != nil {
				t.Errorf("%q: offered a path the item action refuses: %v", names[i], err)
			}
		}
	}
	// The ':' ones are refused by itemLocal, the plain ones are not.
	if _, err := e.itemLocal(e.cfg.Kept[0].Key(), "a:b.txt"); err == nil {
		t.Error("itemLocal accepted a ':' name")
	}
	if res.Results[0].Kept == "" || res.Results[1].Kept != "" || res.Results[2].Kept != "" || res.Results[3].Kept == "" {
		t.Errorf("buttons: %q %q %q %q", res.Results[0].Kept, res.Results[1].Kept, res.Results[2].Kept, res.Results[3].Kept)
	}
}

// quotaRig: a signed-in engine whose notifications are counted.
func quotaRig(t *testing.T) (*Engine, *fakeOC, *atomic.Int32) {
	t.Helper()
	e, oc := historyRig(t)
	signedIn(e)
	var n atomic.Int32
	old := showNotification
	showNotification = func(title, body string) error { n.Add(1); return nil }
	t.Cleanup(func() { showNotification = old })
	return e, oc, &n
}

func TestNoQuotaKeepsTheRememberedLevel(t *testing.T) {
	if w, rem := nextQuotaWarning(90, nil); w != 0 || rem != 90 {
		t.Errorf("nil at 90: %d %d", w, rem)
	}
	if w, rem := nextQuotaWarning(100, nil); w != 0 || rem != 100 {
		t.Errorf("nil at 100: %d %d", w, rem)
	}
	e, _, n := quotaRig(t)
	file := quotaWarnedFile(e.config())
	e.applyQuota(e.config(), &quotaInfo{Used: 95, Total: 100}, time.Now())
	waitFor(t, func() bool { return n.Load() == 1 })
	e.applyQuota(e.config(), nil, time.Now()) // the server gave no quota this time
	if readQuotaWarned(file) != 90 {
		t.Errorf("a missing quota changed the remembered level to %d", readQuotaWarned(file))
	}
	e.applyQuota(e.config(), &quotaInfo{Used: 96, Total: 100}, time.Now())
	time.Sleep(100 * time.Millisecond)
	if n.Load() != 1 {
		t.Errorf("warned again after a missing quota (%d notifications)", n.Load())
	}
	// A real reading below 90% re-arms.
	e.applyQuota(e.config(), &quotaInfo{Used: 10, Total: 100}, time.Now())
	if readQuotaWarned(file) != 0 {
		t.Error("not re-armed by a real reading")
	}
}

// quotaState: puts the engine in "a quota was checked and warned about" state.
func quotaState(t *testing.T, e *Engine) string {
	t.Helper()
	file := quotaWarnedFile(e.config())
	if err := writeQuotaWarned(file, 90); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.quotaShown = &quotaInfo{Used: 95, Total: 100}
	e.quotaAt = time.Now()
	e.mu.Unlock()
	return file
}

func quotaIsReset(t *testing.T, e *Engine, file, why string) {
	t.Helper()
	e.mu.Lock()
	shown, at := e.quotaShown, e.quotaAt
	e.mu.Unlock()
	if shown != nil || !at.IsZero() || exists(file) {
		t.Errorf("%s: shown %+v, checked %v, warned file there %v", why, shown, at, exists(file))
	}
	if e.Status().Storage != nil {
		t.Errorf("%s: status still shows the storage", why)
	}
}

func TestQuotaStateResetsOnSignOutSignInAndServerChange(t *testing.T) {
	e, oc, n := quotaRig(t)
	file := quotaState(t, e)
	if err := e.SignOut(true); err != nil {
		t.Fatal(err)
	}
	quotaIsReset(t, e, file, "sign out")

	// Revoked sign-in.
	signedIn(e)
	file = quotaState(t, e)
	e.markSignedOut()
	quotaIsReset(t, e, file, "revoked sign-in")

	// Signing in as someone else (the next check notices another account).
	base, _ := normalizeServerURL(e.cfg.ServerURL)
	if err := writeAuthState(authState{ServerURL: base, Issuer: base, Username: "sam", Access: "tok", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	signedIn(e)
	e.mu.Lock()
	e.quotaID = "someone-else"
	e.mu.Unlock()
	file = quotaState(t, e)
	oc.mu.Lock()
	oc.drives = `{"value":[{"id":"storage-users-1$1111","driveType":"personal","quota":{"used":95,"total":100}}]}`
	oc.mu.Unlock()
	before := n.Load()
	e.checkQuota(e.quotaGenNow())
	// The old account's remembered warning is gone, so this account is warned.
	waitFor(t, func() bool { return n.Load() == before+1 })
	if readQuotaWarned(file) != 90 {
		t.Errorf("the new reading wasn't remembered: %d", readQuotaWarned(file))
	}

	// Another server, applied by a reload.
	file = quotaState(t, e)
	raw, _ := os.ReadFile(configFile())
	other := strings.Replace(string(raw), oc.srv.URL, "https://other.example.com", 1)
	if other == string(raw) {
		t.Fatal("server not in the config")
	}
	if err := os.WriteFile(configFile(), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	quotaIsReset(t, e, file, "server change")
}

func (e *Engine) quotaGenNow() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.quotaGen
}

func TestFailedQuotaCheckIsRetriedSoon(t *testing.T) {
	e, oc, _ := quotaRig(t)
	oc.mu.Lock()
	oc.drives = `not json`
	oc.mu.Unlock()
	count := func() int {
		oc.mu.Lock()
		defer oc.mu.Unlock()
		n := 0
		for _, r := range oc.requests {
			if r == "GET /opencloud/graph/v1.0/me/drives" {
				n++
			}
		}
		return n
	}
	e.mu.Lock()
	e.quotaDueLocked()
	e.mu.Unlock()
	waitFor(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return !e.quotaBusy })
	e.mu.Lock()
	wait := time.Until(e.quotaAt.Add(quotaEvery))
	e.mu.Unlock()
	if wait <= 0 || wait > quotaRetry {
		t.Fatalf("the next check is in %v, want within %v (not %v)", wait, quotaRetry, quotaEvery)
	}
	// Not yet due...
	e.mu.Lock()
	e.quotaDueLocked()
	busy := e.quotaBusy
	e.mu.Unlock()
	if busy || count() != 1 {
		t.Errorf("checked again at once (%d)", count())
	}
	// ...but due after the retry time, far sooner than 30 minutes.
	e.mu.Lock()
	e.quotaAt = e.quotaAt.Add(-quotaRetry - time.Second) // the 2 minutes have passed
	e.quotaDueLocked()
	again := e.quotaBusy
	e.mu.Unlock()
	if !again {
		t.Error("not retried after the short wait")
	}
	waitFor(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return !e.quotaBusy })
}

func TestApplyQuotaDropsWriteAfterReset(t *testing.T) {
	e, _, n := quotaRig(t)
	file := quotaWarnedFile(e.config())
	gen := e.quotaGenNow()
	afterQuotaRead = func() {
		e.mu.Lock()
		e.resetQuotaLocked(e.cfg)
		e.mu.Unlock()
	}
	defer func() { afterQuotaRead = nil }()
	e.applyQuotaFor(gen, e.config(), &quotaInfo{Used: 95, Total: 100}, time.Now())
	if exists(file) {
		t.Error("a stale check wrote the warned level after a reset")
	}
	time.Sleep(50 * time.Millisecond)
	if n.Load() != 0 {
		t.Errorf("a stale check warned (%d)", n.Load())
	}
}

func TestCheckQuotaDropsFetchAfterReload(t *testing.T) {
	e, oc, _ := quotaRig(t)
	gen := e.quotaGenNow()
	oc.mu.Lock()
	before := len(oc.requests)
	oc.mu.Unlock()
	afterQuotaConfig = func() {
		e.mu.Lock()
		e.resetQuotaLocked(e.cfg)
		e.mu.Unlock()
	}
	defer func() { afterQuotaConfig = nil }()
	e.checkQuota(gen)
	oc.mu.Lock()
	after := len(oc.requests)
	oc.mu.Unlock()
	if after != before {
		t.Errorf("fetched %d times under a stale generation", after-before)
	}
	if e.quotaGenNow() == gen {
		t.Error("the hook did not reset")
	}
}

func TestResetQuotaSkipsRemoveWithoutStateDir(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if err := os.WriteFile("quota-warned", []byte("90"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{}
	e.resetQuotaLocked(Config{})
	if !exists("quota-warned") {
		t.Error("removed a relative quota-warned file for an empty StateDir")
	}
}
