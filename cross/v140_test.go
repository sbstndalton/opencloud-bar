package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Output of a real rclone v1.76 bisync (local temp folders and a WebDAV remote), shortened.
const bisyncOut = "2026/11/15 16:55:06 INFO  : Synching Path1 \"/home/sam/OpenCloud/Docs/\" with Path2 \":webdav{1sYgV}:Docs/\"\n" +
	"2026/11/15 16:55:06 INFO  : - Path1    File changed: size (larger), time (newer) - keep.txt\n" +
	"2026/11/15 16:55:06 INFO  : - Path1    File was deleted          - old.txt\n" +
	"2026/11/15 16:55:06 INFO  : Applying changes\n" +
	"2026/11/15 16:55:06 INFO  : - Path1    Queue copy to Path2       - :webdav{1sYgV}:Docs/keep.txt\n" +
	"2026/11/15 16:55:06 INFO  : - Path1    Queue copy to Path2       - :webdav{1sYgV}:Docs/new file.txt\n" +
	"2026/11/15 16:55:06 INFO  : - Path2    Queue delete              - :webdav{1sYgV}:Docs/old.txt\n" +
	"2026/11/15 16:55:06 INFO  : - Path1    Queue copy to Path2       - :webdav{1sYgV}:Docs/sub/renamed.txt\n" +
	"2026/11/15 16:55:06 INFO  : - Path2    Queue delete              - :webdav{1sYgV}:Docs/sub/s.txt\n" +
	"2026/11/15 16:55:06 INFO  : - Path1    Queue copy to Path2       - :webdav{1sYgV}:Docs/a/moved.pdf\n" +
	"2026/11/15 16:55:06 INFO  : - Path2    Queue delete              - :webdav{1sYgV}:Docs/b/moved.pdf\n" +
	"2026/11/15 16:55:06 INFO  : - Path1    Queue delete              - /home/sam/OpenCloud/Docs/del.txt\n" +
	"2026/11/15 16:55:06 INFO  : - Path2    Queue copy to Path1       - /home/sam/OpenCloud/Docs/fromb.txt\n" +
	"2026/11/15 16:55:06 INFO  : - Path2    Queue copy to Path1       - /home/sam/OpenCloud/Docs/report.conflict1.docx\n" +
	"2026/11/15 16:55:06 INFO  : - Path1    Queue copy to Path2       - :webdav{1sYgV}:Docs/.opencloud-bar-backups/x/y.txt\n" +
	"2026/11/15 16:55:06 INFO  : fromb.txt: Copied (server-side copy)\n" +
	"2026/11/15 16:55:06 INFO  : Bisync successful\n"

func TestParseChanges(t *testing.T) {
	f := KeptFolder{Path: "Docs", StateName: "d"}
	now := time.Date(2026, 11, 15, 17, 0, 0, 0, time.UTC)
	got := parseChanges(bisyncOut, "sync", f, now)
	var lines []string
	for _, c := range got {
		if c.Folder != "Docs" || c.Name != "Docs" || !c.Time.Equal(now) {
			t.Errorf("change %+v", c)
		}
		lines = append(lines, c.Action+": "+c.From+">"+c.Path)
	}
	want := []string{
		"uploaded: >keep.txt",
		"uploaded: >new file.txt",
		"deleted here: >old.txt",
		"uploaded: >sub/renamed.txt",
		"deleted here: >sub/s.txt",
		"renamed here: b/moved.pdf>a/moved.pdf",
		"deleted in OpenCloud: >del.txt",
		"downloaded: >fromb.txt",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	// A space folder's changes are keyed like the folder.
	sp := KeptFolder{Path: "Docs", StateName: "m", Space: marketing}
	if c := parseChanges(bisyncOut, "sync", sp, now); len(c) == 0 || c[0].Folder != marketing.ID+":Docs" {
		t.Errorf("space key: %+v", c)
	}
	// Nothing changed: nothing recorded.
	if c := parseChanges("2026/11/15 INFO  : Bisync successful\n", "sync", f, now); len(c) != 0 {
		t.Errorf("empty run: %+v", c)
	}
}

func TestMergeIsOneSummary(t *testing.T) {
	f := KeptFolder{Path: "School/Notes", StateName: "d"}
	var b strings.Builder
	b.WriteString("INFO  : Synching Path1 \"/x/\" with Path2 \":webdav{a}:School/Notes/\"\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "2026/11/15 16:55:06 INFO  : f%d.txt: Copied (new)\n", i)
	}
	b.WriteString("2026/11/15 16:55:06 INFO  : r.conflict2.txt: Copied (new)\n")
	b.WriteString("Checks:               100 / 100, 100%\nChecks:               512 / 512, 100%, Listed 1024\n")
	got := parseChanges(b.String(), "merge (repair)", f, time.Now())
	if len(got) != 1 || got[0].Action != actSummary || got[0].Text != "Repair of Notes: 512 files compared, 40 updated" {
		t.Errorf("summary: %+v", got)
	}
	if got := parseChanges("", "merge (first sync)", f, time.Now()); got[0].Text != "First sync of Notes: 0 updated" {
		t.Errorf("first sync: %+v", got)
	}
}

func TestChangesStoreBounded(t *testing.T) {
	testEnv(t)
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	f := KeptFolder{Path: "Docs", StateName: "d"}
	for i := 0; i < 520; i++ {
		addChanges(cfg, []Change{{Time: time.Unix(int64(i), 0), Folder: f.Key(), Name: "Docs", Path: fmt.Sprintf("f%d", i), Action: actUploaded}})
	}
	changesMu.Lock()
	changesCache, changesKey = nil, "" // read it back from the file
	changesMu.Unlock()
	list := loadChanges(cfg)
	if len(list) != maxChanges || list[0].Path != "f20" || list[len(list)-1].Path != "f519" {
		t.Fatalf("stored %d, first %v", len(list), list[0].Path)
	}
	data, _ := os.ReadFile(changesFile(cfg))
	var raw []map[string]any
	if json.Unmarshal(data, &raw) != nil || len(raw) != maxChanges {
		t.Errorf("file: %d", len(raw))
	}
	forgetChanges(cfg, "Docs")
	if len(loadChanges(cfg)) != 0 {
		t.Error("not forgotten")
	}
}

// A fake OpenCloud: WebDAV PROPFIND for oc:fileid and Graph createLink. Never a real server.
func fakeShareServer(t *testing.T, got *map[string]any, status int, answer string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		switch {
		case r.Method == "PROPFIND" && r.URL.EscapedPath() == "/opencloud/remote.php/dav/files/sam/Docs/Q1%20report.pdf":
			w.WriteHeader(207)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/x</d:href><d:propstat><d:prop><oc:fileid>storage-users-1$1111!aaaa</oc:fileid></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
		case r.Method == "PROPFIND":
			http.NotFound(w, r)
		case r.Method == "POST" && r.URL.EscapedPath() == "/opencloud/graph/v1beta1/drives/storage-users-1$1111/items/storage-users-1$1111%21aaaa/createLink",
			r.Method == "POST" && r.URL.EscapedPath() == "/opencloud/graph/v1beta1/drives/storage-users-1$1111/items/storage-users-1$1111!aaaa/createLink":
			_ = json.NewDecoder(r.Body).Decode(got)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, answer)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.EscapedPath())
			http.Error(w, "no", 400)
		}
	}))
}

func TestCreateShareLink(t *testing.T) {
	var body map[string]any
	srv := fakeShareServer(t, &body, 200, `{"id":"p1","roles":["x"],"link":{"type":"view","webUrl":"https://cloud.example.org/s/YlaySZdHCRHUDeE"}}`)
	defer srv.Close()
	cfg := spaceCfg(t)
	cfg.ServerURL, cfg.DavPath = srv.URL+"/opencloud", "/remote.php/dav/files/sam"
	f := KeptFolder{Path: "Docs", StateName: "d"}
	dav, err := itemDavURL(cfg, f, "Q1 report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	link, err := createShareLink(context.Background(), dav, cfg.ServerURL, "tok", ShareOptions{Days: 7, Password: "s3cret-Pass"}, now)
	if err != nil || link != "https://cloud.example.org/s/YlaySZdHCRHUDeE" {
		t.Fatalf("link %q %v", link, err)
	}
	if body["type"] != "view" || body["expirationDateTime"] != "2026-11-22T10:00:00Z" || body["password"] != "s3cret-Pass" {
		t.Errorf("request: %v", body)
	}
	// No expiry, no password: neither is sent.
	body = nil
	if _, err := createShareLink(context.Background(), dav, cfg.ServerURL, "tok", ShareOptions{}, now); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["expirationDateTime"]; ok || body["password"] != nil {
		t.Errorf("request: %v", body)
	}
	// Not on the server yet.
	missing, _ := itemDavURL(cfg, f, "new.txt")
	if _, err := createShareLink(context.Background(), missing, cfg.ServerURL, "tok", ShareOptions{}, now); err == nil || !strings.Contains(err.Error(), "isn't in OpenCloud yet") {
		t.Errorf("missing: %v", err)
	}
	if _, err := itemDavURL(cfg, f, "../etc/passwd"); err == nil {
		t.Error("path outside the folder")
	}
}

func TestCreateShareLinkServerRefuses(t *testing.T) {
	var body map[string]any
	srv := fakeShareServer(t, &body, 400, `{"error":{"code":"invalidRequest","message":"password protection is enforced"}}`)
	defer srv.Close()
	cfg := spaceCfg(t)
	cfg.ServerURL, cfg.DavPath = srv.URL+"/opencloud", "/remote.php/dav/files/sam"
	dav, _ := itemDavURL(cfg, KeptFolder{Path: "Docs"}, "Q1 report.pdf")
	_, err := createShareLink(context.Background(), dav, cfg.ServerURL, "tok", ShareOptions{}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "password protection is enforced") {
		t.Errorf("error: %v", err)
	}
}

func TestShareLinkChecks(t *testing.T) {
	e := pausedRig(t, `,"auth":"oidc","kept":[{"path":"Docs","stateName":"d"}]`)
	if _, err := e.ShareLink("Docs", "a.txt", ShareOptions{}); err == nil || !strings.Contains(err.Error(), "hasn't synced") {
		t.Errorf("unsynced folder: %v", err)
	}
	_ = os.MkdirAll(expand(e.cfg.StateDir), 0o700)
	_ = os.WriteFile(e.cfg.statePath(e.cfg.Kept[0], ".initialised"), nil, 0o600)
	if _, err := e.ShareLink("Docs", "a.txt", ShareOptions{Days: 3}); err == nil {
		t.Error("3 days accepted")
	}
	if _, err := e.ShareLink("Nope", "a.txt", ShareOptions{}); err == nil {
		t.Error("unknown folder")
	}
	legacy := pausedRig(t, `,"kept":[{"path":"Docs","stateName":"d"}]`)
	if _, err := legacy.ShareLink("Docs", "a.txt", ShareOptions{}); err == nil || !strings.Contains(err.Error(), "browser sign-in") {
		t.Errorf("legacy: %v", err)
	}
}
