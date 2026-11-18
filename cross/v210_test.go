package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 2.1: file history (versions) and deleted files (trash). Fake OpenCloud servers only.

const versionsAnswer = `<?xml version="1.0" encoding="UTF-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns">
 <d:response><d:href>/opencloud/remote.php/dav/meta/storage-users-1$1111%21aaaa/v/</d:href>
  <d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/opencloud/remote.php/dav/meta/storage-users-1$1111%21aaaa/v/v1731500000.000001</d:href>
  <d:propstat><d:prop><d:getlastmodified>Wed, 13 Nov 2024 12:13:20 GMT</d:getlastmodified><d:getcontentlength>120</d:getcontentlength><d:getetag>"e1"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/opencloud/remote.php/dav/meta/storage-users-1$1111%21aaaa/v/v1731600000.000002</d:href>
  <d:propstat><d:prop><d:getlastmodified>Thu, 14 Nov 2024 16:00:00 GMT</d:getlastmodified><d:getcontentlength>300</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  <d:propstat><d:prop><d:getetag/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response>
 <d:response><d:href>/opencloud/remote.php/dav/meta/storage-users-1$1111%21aaaa/v/..</d:href>
  <d:propstat><d:prop><d:getcontentlength>1</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`

func TestParseVersions(t *testing.T) {
	vs, err := parseVersions(strings.NewReader(versionsAnswer))
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("got %+v", vs)
	}
	// Newest first, the "v" folder and an unsafe key left out.
	if vs[0].Key != "v1731600000.000002" || vs[0].Size != 300 || !vs[0].Modified.Equal(time.Date(2024, 11, 14, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("first: %+v", vs[0])
	}
	if vs[1].Key != "v1731500000.000001" || vs[1].Size != 120 {
		t.Errorf("second: %+v", vs[1])
	}
	if _, err := parseVersions(strings.NewReader("not xml")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestDavRootAndMetaURL(t *testing.T) {
	for in, want := range map[string]string{
		"/remote.php/dav/files/sam":         "/remote.php/dav",
		"/opencloud/dav/spaces/x$y":         "/opencloud/dav",
		"/remote.php/dav/files/sam/":        "/remote.php/dav",
		"/dav/spaces/storage-users-1$2222/": "/dav",
	} {
		if got, err := davRootOf(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := davRootOf("/webdav"); err == nil {
		t.Error("unknown layout accepted")
	}
	cfg := Config{ServerURL: "https://cloud.example.com/oc", DavPath: "/remote.php/dav/files/sam"}
	u, err := metaVersionsURL(cfg, KeptFolder{Path: "Docs"}, "s$1!a")
	if err != nil || u != "https://cloud.example.com/oc/remote.php/dav/meta/s$1%21a/v" {
		t.Errorf("personal: %q %v", u, err)
	}
	u, err = metaVersionsURL(cfg, KeptFolder{Path: "Docs", Space: marketing}, "s$2!b")
	if err != nil || u != "https://cloud.example.com/dav/meta/s$2%21b/v" {
		t.Errorf("space: %q %v", u, err)
	}
	if _, err := metaVersionsURL(cfg, KeptFolder{Path: "Docs"}, "a/../b"); err == nil {
		t.Error("file id with a slash accepted")
	}
}

// fakeOC is a fake OpenCloud with one personal space (sam, behind /opencloud) and the
// Marketing project space. It records every request, and those that change something.
type fakeOC struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	writes   []string // "METHOD path | Destination | Overwrite"
	requests []string // every request: "METHOD path"
	exists   map[string]bool
	trash    map[string]string // space path → multistatus answer
	drives   string            // the /me/drives answer ("" for the usual one)
	report   http.HandlerFunc  // answers REPORT (search, 2.2); called with mu held
}

// reportTime is the server's modification time of Docs/report.txt (size 7, "current").
var reportTime = time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)

// reportSize is its size in OpenCloud (changed by a test, under fakeOC.mu).
var reportSize = 7

func newFakeOC(t *testing.T) *fakeOC {
	f := &fakeOC{t: t, exists: map[string]bool{}, trash: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOC) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "no", 401)
		return
	}
	p := r.URL.Path
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+p)
	switch {
	case r.Method == "GET" && p == "/opencloud/graph/v1.0/me/drives" && f.drives != "":
		_, _ = io.WriteString(w, f.drives)
	case r.Method == "GET" && p == "/opencloud/graph/v1.0/me/drives":
		_, _ = io.WriteString(w, `{"value":[{"id":"storage-users-1$1111","name":"Sam","driveType":"personal","root":{"webDavUrl":"`+f.srv.URL+`/opencloud/dav/spaces/storage-users-1$1111"}},
		  {"id":"storage-users-1$2222","name":"Marketing","driveType":"project","root":{"webDavUrl":"`+f.srv.URL+`/dav/spaces/storage-users-1$2222"}}]}`)
	case r.Method == "PROPFIND" && p == "/opencloud/remote.php/dav/files/sam/Docs/report.txt":
		w.WriteHeader(207)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/x</d:href><d:propstat><d:prop><oc:fileid>storage-users-1$1111!aaaa</oc:fileid><d:getcontentlength>`+strconv.Itoa(reportSize)+`</d:getcontentlength><d:getlastmodified>`+reportTime.Format(http.TimeFormat)+`</d:getlastmodified></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	case r.Method == "PROPFIND" && p == "/opencloud/remote.php/dav/meta/storage-users-1$1111!aaaa/v":
		if r.Header.Get("Depth") != "1" {
			f.t.Errorf("versions depth %q", r.Header.Get("Depth"))
		}
		w.WriteHeader(207)
		_, _ = io.WriteString(w, versionsAnswer)
	case r.Method == "PROPFIND" && strings.Contains(p, "/trash-bin/"):
		ans, ok := f.trash[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(207)
		_, _ = io.WriteString(w, ans)
	case r.Method == "PROPFIND" && r.Header.Get("Depth") == "0":
		if f.exists[p] {
			w.WriteHeader(207)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"/>`)
			return
		}
		http.NotFound(w, r)
	case r.Method == "REPORT" && f.report != nil:
		f.report(w, r)
	case r.Method == "COPY" || r.Method == "MOVE":
		f.writes = append(f.writes, r.Method+" "+r.URL.EscapedPath()+" | "+r.Header.Get("Destination")+" | "+r.Header.Get("Overwrite"))
		if r.Method == "MOVE" {
			w.WriteHeader(201)
		} else {
			w.WriteHeader(204)
		}
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.EscapedPath())
		http.Error(w, "no", 400)
	}
}

// historyRig: an engine signed in (browser sign-in) to the fake server, paused, with a
// personal folder Docs and Marketing's Docs, both synced once.
func historyRig(t *testing.T) (*Engine, *fakeOC) {
	t.Helper()
	oc := newFakeOC(t)
	e := pausedRig(t, `,"auth":"oidc","paused":true,"serverURL":"`+oc.srv.URL+`/opencloud","kept":[{"path":"Docs","stateName":"d"},
	  {"path":"Docs","stateName":"m","space":{"id":"storage-users-1$2222","name":"Marketing","davPath":"/dav/spaces/storage-users-1$2222"}}]`)
	base, _ := normalizeServerURL(e.cfg.ServerURL)
	if err := writeAuthState(authState{ServerURL: base, Issuer: base, Username: "sam", Access: "tok", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(expand(e.cfg.StateDir), 0o700)
	for _, f := range e.cfg.Kept {
		_ = os.WriteFile(e.cfg.statePath(f, ".initialised"), nil, 0o600)
		_ = os.MkdirAll(e.cfg.Local(f), 0o700)
	}
	return e, oc
}

// writeReport writes the synced local copy of Docs/report.txt (same size and time as the server's).
func writeReport(t *testing.T, e *Engine, content string) string {
	t.Helper()
	local := filepath.Join(e.cfg.Local(e.cfg.Kept[0]), "report.txt")
	if err := os.WriteFile(local, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(local, reportTime, reportTime); err != nil {
		t.Fatal(err)
	}
	return local
}

func (f *fakeOC) copies() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.writes {
		if strings.HasPrefix(w, "COPY ") {
			n++
		}
	}
	return n
}

func TestFileVersionsAndRestore(t *testing.T) {
	e, oc := historyRig(t)
	local := writeReport(t, e, "current")
	vs, err := e.FileVersions("Docs", "report.txt")
	if err != nil || len(vs) != 2 {
		t.Fatalf("versions: %+v %v", vs, err)
	}
	if err := e.RestoreVersion("Docs", "report.txt", "v-unknown"); err == nil {
		t.Error("an unlisted version was restored")
	}
	if len(oc.writes) != 0 {
		t.Fatalf("wrote: %v", oc.writes)
	}
	if err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001"); err != nil {
		t.Fatal(err)
	}
	// The request: COPY of the version onto the file (Destination as the web UI sends it).
	want := "COPY /opencloud/remote.php/dav/meta/storage-users-1$1111%21aaaa/v/v1731500000.000001 | " + oc.srv.URL + "/opencloud/remote.php/dav/files/sam/Docs/report.txt | "
	if len(oc.writes) != 1 || oc.writes[0] != want {
		t.Errorf("requests:\n%v\nwant\n%s", oc.writes, want)
	}
	// The current file was copied to the local backups first, and stays in place.
	matches, _ := filepath.Glob(filepath.Join(expand(e.cfg.StateDir), "backups", "d", "*", "report.txt"))
	if len(matches) != 1 {
		t.Fatalf("backup: %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "current" {
		t.Errorf("backup holds %q", b)
	}
	if b, _ := os.ReadFile(local); string(b) != "current" {
		t.Errorf("local file changed: %q", b)
	}
	// The folder's sync is queued (paused: it waits).
	e.mu.Lock()
	q := e.queued["Docs"]
	e.mu.Unlock()
	if !q {
		t.Error("no sync queued after the restore")
	}
}

func TestFileVersionsRefusesPathsOutsideKeptFolders(t *testing.T) {
	e, oc := historyRig(t)
	writeReport(t, e, "current")
	for _, rel := range []string{"", "../secret.txt", "a/../../b", ".opencloud-bar-backups/x", "x.conflict1.txt", "a\x01b", `..\up.txt`, "../report.txt", "/../Docs/report.txt/.."} {
		if _, err := e.FileVersions("Docs", rel); err == nil {
			t.Errorf("%q accepted", rel)
		}
		if err := e.RestoreVersion("Docs", rel, "v1731500000.000001"); err == nil {
			t.Errorf("restore %q accepted", rel)
		}
	}
	if _, err := e.FileVersions("Elsewhere", "report.txt"); err == nil {
		t.Error("a folder that isn't kept")
	}
	if err := e.RestoreVersion("Elsewhere", "report.txt", "v1731500000.000001"); err == nil {
		t.Error("restore in a folder that isn't kept")
	}
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if len(oc.requests) != 0 {
		t.Errorf("requests sent: %v", oc.requests)
	}
}

// A backup that can't be made: no COPY, an error.
func TestRestoreVersionBackupFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a read-only folder (chmod 0500) still accepts new files on Windows; only Unix permissions can make this backup fail")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write into a read-only folder")
	}
	e, oc := historyRig(t)
	writeReport(t, e, "current")
	backups := filepath.Join(expand(e.cfg.StateDir), "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backups, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(backups, 0o700) })
	err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001")
	if err == nil || !strings.Contains(err.Error(), "nothing was restored") {
		t.Errorf("err %v", err)
	}
	if n := oc.copies(); n != 0 {
		t.Errorf("%d COPY sent", n)
	}
}

// Refused while the folder syncs, and when the local copy differs from the server's file.
func TestRestoreVersionRefusesBusyOrUnsynced(t *testing.T) {
	e, oc := historyRig(t)
	local := writeReport(t, e, "current")
	e.mu.Lock()
	e.active = map[string]*activeRun{"Docs": {name: "Docs"}}
	e.mu.Unlock()
	if err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001"); err == nil || !strings.Contains(err.Error(), "is syncing") {
		t.Errorf("busy: %v", err)
	}
	e.mu.Lock()
	e.active = nil
	e.mu.Unlock()

	// Changed here (another size), not synced yet.
	writeReport(t, e, "changed here")
	if err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001"); err == nil || err.Error() != unsyncedText {
		t.Errorf("size differs: %v", err)
	}
	// Same size, but a newer time.
	writeReport(t, e, "CURRENT")
	later := reportTime.Add(time.Minute)
	_ = os.Chtimes(local, later, later)
	if err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001"); err == nil || err.Error() != unsyncedText {
		t.Errorf("time differs: %v", err)
	}
	// Deleted here, not synced yet.
	_ = os.Remove(local)
	if err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001"); err == nil || err.Error() != unsyncedText {
		t.Errorf("missing here: %v", err)
	}
	if n := oc.copies(); n != 0 {
		t.Errorf("%d COPY sent", n)
	}
	// Within 2 seconds of the server's time is the same file.
	writeReport(t, e, "current")
	near := reportTime.Add(1500 * time.Millisecond)
	_ = os.Chtimes(local, near, near)
	if err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001"); err != nil {
		t.Errorf("synced file refused: %v", err)
	}
	if n := oc.copies(); n != 1 {
		t.Errorf("%d COPY sent, want 1", n)
	}
}

// Queued or waiting counts as syncing; so does a sync starting while the backup is made.
func TestRestoreVersionRefusesQueuedOrBusyAfterBackup(t *testing.T) {
	e, oc := historyRig(t)
	writeReport(t, e, "current")
	e.mu.Lock()
	e.queueLocked("Docs") // paused: it stays queued
	e.mu.Unlock()
	if err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001"); err == nil || !strings.Contains(err.Error(), "is syncing") {
		t.Errorf("queued: %v", err)
	}
	e.mu.Lock()
	e.queued = nil
	e.mu.Unlock()

	for _, mark := range []func(){
		func() { e.startingKeys["Docs"]++ },
		func() { e.exclOn = true },
	} {
		afterVersionBackup = func() {
			e.mu.Lock()
			if e.startingKeys == nil {
				e.startingKeys = map[string]int{}
			}
			mark()
			e.mu.Unlock()
		}
		err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001")
		afterVersionBackup = nil
		if err == nil || !strings.Contains(err.Error(), "started syncing") {
			t.Errorf("busy after backup: %v", err)
		}
		e.mu.Lock()
		e.startingKeys, e.exclOn = nil, false
		e.mu.Unlock()
	}
	if n := oc.copies(); n != 0 {
		t.Errorf("%d COPY sent", n)
	}
}

// The file changed in OpenCloud (another client) while the version was chosen: no COPY.
func TestRestoreVersionRechecksBeforeCopy(t *testing.T) {
	e, oc := historyRig(t)
	writeReport(t, e, "current")
	afterVersionBackup = func() {
		oc.mu.Lock()
		reportSize = 99
		oc.mu.Unlock()
	}
	defer func() { afterVersionBackup = nil; reportSize = 7 }()
	if err := e.RestoreVersion("Docs", "report.txt", "v1731500000.000001"); err == nil || err.Error() != unsyncedText {
		t.Errorf("changed meanwhile: %v", err)
	}
	if n := oc.copies(); n != 0 {
		t.Errorf("%d COPY sent", n)
	}
}

const personalTrash = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
 <d:response><d:href>/opencloud/dav/spaces/trash-bin/storage-users-1$1111/</d:href>
  <d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/opencloud/dav/spaces/trash-bin/storage-users-1$1111/k-report</d:href>
  <d:propstat><d:prop><oc:trashbin-original-filename>report.txt</oc:trashbin-original-filename><oc:trashbin-original-location>Docs/sub/report.txt</oc:trashbin-original-location>
   <oc:trashbin-delete-timestamp>1763200000</oc:trashbin-delete-timestamp><oc:trashbin-delete-datetime>Sat, 15 Nov 2025 09:46:40 +0000</oc:trashbin-delete-datetime>
   <d:getcontentlength>42</d:getcontentlength><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  <d:propstat><d:prop><oc:size/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response>
 <d:response><d:href>/opencloud/dav/spaces/trash-bin/storage-users-1$1111/k-old/</d:href>
  <d:propstat><d:prop><oc:trashbin-original-filename>Old</oc:trashbin-original-filename><oc:trashbin-original-location>Docs/Old</oc:trashbin-original-location>
   <oc:trashbin-delete-datetime>Fri, 14 Nov 2025 08:00:00 +0000</oc:trashbin-delete-datetime>
   <oc:size>4096</oc:size><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/opencloud/dav/spaces/trash-bin/storage-users-1$1111/k-photo</d:href>
  <d:propstat><d:prop><oc:trashbin-original-filename>photo.jpg</oc:trashbin-original-filename><oc:trashbin-original-location>Pictures/photo.jpg</oc:trashbin-original-location>
   <oc:trashbin-delete-timestamp>1763100000</oc:trashbin-delete-timestamp><d:getcontentlength>9</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/opencloud/dav/spaces/trash-bin/storage-users-1$1111/k-docsx</d:href>
  <d:propstat><d:prop><oc:trashbin-original-location>DocsX/a.txt</oc:trashbin-original-location><oc:trashbin-delete-timestamp>1763000000</oc:trashbin-delete-timestamp></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
 <d:response><d:href>/opencloud/dav/spaces/trash-bin/storage-users-1$1111/k-evil</d:href>
  <d:propstat><d:prop><oc:trashbin-original-location>Docs/../../etc/passwd</oc:trashbin-original-location></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`

const marketingTrash = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
 <d:response><d:href>/dav/spaces/trash-bin/storage-users-1%242222/k-plan</d:href>
  <d:propstat><d:prop><oc:trashbin-original-location>Docs/plan.odt</oc:trashbin-original-location><oc:trashbin-delete-timestamp>1763300000</oc:trashbin-delete-timestamp><d:getcontentlength>7</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`

func TestParseTrash(t *testing.T) {
	sp := trashSpace{ID: "storage-users-1$1111", Name: "Personal", kept: []KeptFolder{{Path: "Docs", StateName: "d"}}}
	items, err := parseTrash(strings.NewReader(personalTrash), sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("items: %+v", items)
	}
	r := items[0]
	if r.Key != "k-report" || r.Name != "report.txt" || r.Path != "Docs/sub/report.txt" || r.Size != 42 || r.Dir || r.Folder != "Docs" ||
		!r.Deleted.Equal(time.Unix(1763200000, 0)) || r.SpaceID != sp.ID {
		t.Errorf("report: %+v", r)
	}
	o := items[1]
	if o.Key != "k-old" || !o.Dir || o.Size != 4096 || o.Folder != "Docs" || !o.Deleted.Equal(time.Date(2025, 11, 14, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("folder: %+v", o)
	}
	if items[2].Folder != "" || items[2].Path != "Pictures/photo.jpg" {
		t.Errorf("outside: %+v", items[2])
	}
	// "DocsX" isn't inside "Docs"; a name is made from the path when the server gives none.
	if items[3].Folder != "" || items[3].Name != "a.txt" {
		t.Errorf("prefix: %+v", items[3])
	}
	for _, it := range items {
		if strings.Contains(it.Path, "..") {
			t.Errorf("unsafe path kept: %+v", it)
		}
	}
	if u, err := trashURL("https://h", "/opencloud/dav/spaces/s$1"); err != nil || u != "https://h/opencloud/dav/spaces/trash-bin/s$1" {
		t.Errorf("trash url %q %v", u, err)
	}
	if _, err := trashURL("https://h", "/remote.php/dav/files/sam"); err == nil {
		t.Error("a files/ path accepted as a space")
	}
}

func TestListAndRestoreTrash(t *testing.T) {
	e, oc := historyRig(t)
	oc.trash["/opencloud/dav/spaces/trash-bin/storage-users-1$1111"] = personalTrash
	oc.trash["/dav/spaces/trash-bin/storage-users-1$2222"] = marketingTrash

	l, err := e.ListTrash(false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range l.Items {
		got = append(got, it.SpaceName+":"+it.Path+":"+it.Folder)
	}
	want := []string{"Marketing:Docs/plan.odt:storage-users-1$2222:Docs", "Personal:Docs/sub/report.txt:Docs", "Personal:Docs/Old:Docs"}
	if strings.Join(got, ",") != strings.Join(want, ",") || l.Hidden != 2 || len(l.Problems) != 0 {
		t.Errorf("kept only: %v hidden %d problems %v", got, l.Hidden, l.Problems)
	}
	all, err := e.ListTrash(true)
	if err != nil || len(all.Items) != 5 || all.Hidden != 0 {
		t.Errorf("all: %+v %v", all, err)
	}

	// Restore: a MOVE to the original place in the same space, never overwriting.
	if err := e.RestoreTrash("storage-users-1$1111", "k-report"); err != nil {
		t.Fatal(err)
	}
	wantMove := "MOVE /opencloud/dav/spaces/trash-bin/storage-users-1$1111/k-report | " + oc.srv.URL + "/opencloud/dav/spaces/storage-users-1$1111/Docs/sub/report.txt | F"
	if len(oc.writes) != 1 || oc.writes[0] != wantMove {
		t.Errorf("requests:\n%v\nwant\n%s", oc.writes, wantMove)
	}
	e.mu.Lock()
	q := e.queued["Docs"]
	e.mu.Unlock()
	if !q {
		t.Error("no sync queued after the restore")
	}
	if err := e.RestoreTrash("storage-users-1$2222", "k-plan"); err != nil {
		t.Fatal(err)
	}
	if len(oc.writes) != 2 || !strings.HasSuffix(oc.writes[1], "/dav/spaces/storage-users-1$2222/Docs/plan.odt | F") {
		t.Errorf("space restore: %v", oc.writes)
	}
}

func TestRestoreTrashRefusals(t *testing.T) {
	e, oc := historyRig(t)
	oc.trash["/opencloud/dav/spaces/trash-bin/storage-users-1$1111"] = personalTrash
	oc.trash["/dav/spaces/trash-bin/storage-users-1$2222"] = marketingTrash

	// Something is at the original place in OpenCloud again: refused, nothing moved.
	oc.exists["/opencloud/dav/spaces/storage-users-1$1111/Docs/Old"] = true
	if err := e.RestoreTrash("storage-users-1$1111", "k-old"); err == nil || !strings.Contains(err.Error(), "in OpenCloud again") {
		t.Errorf("conflict: %v", err)
	}
	// Something is there on this computer (not synced yet).
	local := filepath.Join(e.cfg.Local(e.cfg.Kept[0]), "sub")
	_ = os.MkdirAll(local, 0o700)
	_ = os.WriteFile(filepath.Join(local, "report.txt"), []byte("x"), 0o600)
	// Paused (the rig): resume first.
	if err := e.RestoreTrash("storage-users-1$1111", "k-report"); err == nil || !strings.Contains(err.Error(), "Resume syncing and wait for the folder to sync, then restore") {
		t.Errorf("local conflict, paused: %v", err)
	}
	e.mu.Lock()
	e.cfg.Paused = false
	e.mu.Unlock()
	if err := e.RestoreTrash("storage-users-1$1111", "k-report"); err == nil || !strings.Contains(err.Error(), ". Wait for the folder to sync, then restore") {
		t.Errorf("local conflict: %v", err)
	}
	e.mu.Lock()
	e.cfg.Paused = true
	e.mu.Unlock()
	// Not in a kept folder (shown with Show all, restored in the browser only).
	if err := e.RestoreTrash("storage-users-1$1111", "k-photo"); err == nil || !strings.Contains(err.Error(), "wasn't in a kept folder") {
		t.Errorf("outside: %v", err)
	}
	if err := e.RestoreTrash("storage-users-1$1111", "k-docsx"); err == nil || !strings.Contains(err.Error(), "wasn't in a kept folder") {
		t.Errorf("prefix: %v", err)
	}
	// An unsafe original location isn't listed, so it can't be restored.
	if err := e.RestoreTrash("storage-users-1$1111", "k-evil"); err == nil {
		t.Error("unsafe location restored")
	}
	if err := e.RestoreTrash("storage-users-1$1111", "k-gone"); err == nil || !strings.Contains(err.Error(), "isn't in the trash") {
		t.Errorf("gone: %v", err)
	}
	if err := e.RestoreTrash("storage-users-1$9999", "k-plan"); err == nil {
		t.Error("a space no folder is kept from")
	}
	if len(oc.writes) != 0 {
		t.Errorf("wrote: %v", oc.writes)
	}

	// The server's 412 (Overwrite: F, the place taken meanwhile) is the same refusal.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Overwrite") != "F" {
			t.Errorf("overwrite %q", r.Header.Get("Overwrite"))
		}
		w.WriteHeader(412)
	}))
	defer srv.Close()
	sp := trashSpace{ID: "s$1", Name: "Personal", DavPath: "/dav/spaces/s$1", kept: []KeptFolder{{Path: "Docs"}}}
	err := e.restoreTrashChecked(t.Context(), e.config(), srv.URL, sp, TrashItem{Key: "k", Path: "Docs/new.txt"}, "tok")
	if err == nil || !strings.Contains(err.Error(), "nothing was overwritten") {
		t.Errorf("412: %v", err)
	}
}

// The personal space's webDavUrl: only its path is used, against the configured server.
func TestPersonalTrashURLFromDrives(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherHits.Add(1) }))
	defer other.Close()
	e, oc := historyRig(t)
	oc.trash["/opencloud/dav/spaces/trash-bin/storage-users-1$1111"] = personalTrash
	oc.trash["/dav/spaces/trash-bin/storage-users-1$2222"] = marketingTrash

	// Another host in webDavUrl: its path goes to the configured server.
	oc.drives = `{"value":[{"id":"storage-users-1$1111","driveType":"personal","root":{"webDavUrl":"` + other.URL + `/opencloud/dav/spaces/storage-users-1$1111"}}]}`
	l, err := e.ListTrash(false)
	if err != nil || len(l.Problems) != 0 || len(l.Items) != 3 {
		t.Errorf("other host: %+v %v", l, err)
	}
	if err := e.RestoreTrash("storage-users-1$1111", "k-report"); err != nil {
		t.Errorf("restore: %v", err)
	}
	if otherHits.Load() != 0 {
		t.Error("a request went to the host in webDavUrl")
	}
	// A relative or unusable path: a problem line; the other spaces still listed.
	for _, bad := range []string{"dav/spaces/storage-users-1$1111", "", "https://h/dav/spaces/it's", "%zz"} {
		oc.drives = `{"value":[{"id":"storage-users-1$1111","driveType":"personal","root":{"webDavUrl":"` + bad + `"}}]}`
		l, err := e.ListTrash(true)
		if err != nil || len(l.Problems) != 1 || l.Problems[0] != personalProblem || len(l.Items) != 1 || l.Items[0].SpaceName != "Marketing" {
			t.Errorf("%q: %+v %v", bad, l, err)
		}
	}
	oc.drives = `{"value":[]}`
	if l, err := e.ListTrash(true); err != nil || len(l.Problems) != 1 {
		t.Errorf("no personal space: %+v %v", l, err)
	}
	if otherHits.Load() != 0 {
		t.Error("a request went to the host in webDavUrl")
	}
}

func TestRedirectsNotFollowedForHistory(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if _, err := listVersions(t.Context(), srv.URL+"/dav/meta/x/v", "tok"); err == nil {
		t.Error("listVersions: redirect accepted")
	}
	if err := restoreVersion(t.Context(), srv.URL+"/dav/meta/x/v", "v1", srv.URL+"/dav/files/sam/a.txt", "tok"); err == nil {
		t.Error("restoreVersion: redirect accepted")
	}
	if _, err := davFileID(t.Context(), srv.URL+"/dav/files/sam/a.txt", "tok"); err == nil {
		t.Error("davFileID: redirect accepted")
	}
	if _, err := davFileInfo(t.Context(), srv.URL+"/dav/files/sam/a.txt", "tok"); err == nil {
		t.Error("davFileInfo: redirect accepted")
	}
	sp := trashSpace{ID: "s", DavPath: "/dav/spaces/s"}
	if _, err := listTrash(t.Context(), srv.URL, sp, "tok"); err == nil {
		t.Error("listTrash: redirect accepted")
	}
	if err := restoreTrashItem(t.Context(), srv.URL, sp, TrashItem{Key: "k", Path: "Docs/a.txt"}, "tok"); err == nil {
		t.Error("restoreTrashItem: redirect accepted")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the token went to another host %d times", n)
	}
}
