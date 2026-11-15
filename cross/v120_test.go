package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A /me/drives answer shaped like OpenCloud's (libre-graph), shortened.
const drivesJSON = `{"value":[
 {"driveAlias":"personal/sam","driveType":"personal","id":"storage-users-1$1111","name":"Sam",
  "root":{"id":"storage-users-1$1111!1111","webDavUrl":"https://cloud.example.com/dav/spaces/storage-users-1$1111"},
  "webUrl":"https://cloud.example.com/f/storage-users-1$1111","quota":{"total":100,"used":1}},
 {"driveAlias":"project/marketing","driveType":"project","id":"storage-users-1$2222","name":"Marketing",
  "root":{"id":"storage-users-1$2222!2222","webDavUrl":"https://cloud.example.com/dav/spaces/storage-users-1$2222",
   "permissions":[{"grantedToV2":{"user":{"displayName":"Sam"}},"roles":["a8d5fe5e-96e3-418d-825b-534dbdf22b99"]}]}},
 {"driveAlias":"project/old","driveType":"project","id":"storage-users-1$3333","name":"Old project",
  "root":{"webDavUrl":"https://cloud.example.com/dav/spaces/storage-users-1$3333","deleted":{"state":"trashed"}}},
 {"driveAlias":"virtual/shares","driveType":"virtual","id":"a0ca6a90-a365-4782-871e-d44447bbc668$a0ca6a90-a365-4782-871e-d44447bbc668","name":"Shares",
  "root":{"webDavUrl":"https://cloud.example.com/dav/spaces/a0ca6a90-a365-4782-871e-d44447bbc668$a0ca6a90-a365-4782-871e-d44447bbc668"}},
 {"driveAlias":"mountpoint/budget","driveType":"mountpoint","id":"a0ca6a90-a365-4782-871e-d44447bbc668$abc!def","name":"Budget 2026",
  "root":{"webDavUrl":"https://cloud.example.com/dav/spaces/a0ca6a90-a365-4782-871e-d44447bbc668$abc!def",
   "remoteItem":{"id":"storage-users-1$4444!5555","name":"Budget 2026","permissions":[{"roles":["b1e2218d-eef8-4d4c-b82d-0f1a1b48f3b5"]}]}}},
 {"driveType":"project","id":"elsewhere","name":"Other server","root":{"webDavUrl":"https://evil.example/dav/spaces/x"}}
]}`

func TestDrivesParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graph/v1.0/me/drives" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		_, _ = w.Write([]byte(strings.ReplaceAll(drivesJSON, "https://cloud.example.com", "http://"+r.Host)))
	}))
	defer srv.Close()
	var d graphDrives
	if err := ocGet(context.Background(), srv.URL, "/graph/v1.0/me/drives", nil, "tok", &d); err != nil {
		t.Fatal(err)
	}
	got := parseSpaces(d, srv.URL)
	if len(got) != 2 {
		t.Fatalf("spaces: %+v", got)
	}
	if got[0].Name != "Budget 2026" || got[0].Type != "mountpoint" || got[0].DavPath != "/dav/spaces/a0ca6a90-a365-4782-871e-d44447bbc668$abc!def" {
		t.Errorf("share: %+v", got[0])
	}
	if got[1].Name != "Marketing" || got[1].ID != "storage-users-1$2222" || got[1].DavPath != "/dav/spaces/storage-users-1$2222" {
		t.Errorf("project: %+v", got[1])
	}
}

func spaceCfg(t *testing.T) Config {
	t.Helper()
	testEnv(t)
	c := defaultConfig()
	c.ServerURL, c.Auth, c.DavPath = "https://cloud.example.com", authOIDC, "/remote.php/dav/files/sam"
	no := false
	c.UseTunnel = &no
	return c
}

var marketing = &SpaceRef{ID: "storage-users-1$2222", Name: "Marketing", DavPath: "/dav/spaces/storage-users-1$2222"}

func TestSpaceRemotesAndPaths(t *testing.T) {
	c := spaceCfg(t)
	personal := KeptFolder{Path: "Docs", StateName: makeStateName("Docs")}
	sp := KeptFolder{Path: "Docs", StateName: makeStateNameIn(marketing.ID, "Docs"), Space: marketing}
	if c.RemoteFor(personal) != c.RemoteSpec() {
		t.Error("personal remote changed")
	}
	if want := ":webdav,vendor=infinitescale,url='https://cloud.example.com/dav/spaces/storage-users-1$2222':"; c.RemoteFor(sp) != want {
		t.Errorf("space remote %q", c.RemoteFor(sp))
	}
	if c.ServerIdentityFor(personal) != c.ServerIdentity() || c.ServerIdentityFor(sp) == c.ServerIdentity() {
		t.Error("server identity")
	}
	if got := c.Local(sp); got != filepath.Join(expand(c.LocalRoot), "Marketing", "Docs") {
		t.Errorf("local %q", got)
	}
	if sp.Key() != marketing.ID+":Docs" || personal.Key() != "Docs" {
		t.Error("keys")
	}
	if a, b, d := makeStateName("Docs"), makeStateNameIn(marketing.ID, "Docs"), makeStateNameIn("other", "Docs"); a == b || b == d || a == d {
		t.Errorf("state names collide: %s %s %s", a, b, d)
	}
	if n := spaceDirName(&SpaceRef{ID: "x$1!2", Name: " a/b:c. "}); n != "a-b-c" {
		t.Errorf("dir name %q", n)
	}
	if n := spaceDirName(&SpaceRef{ID: "x$1!2", Name: ".."}); n != "Space x12" {
		t.Errorf("dir name %q", n)
	}
	// Backups go to the space's own root.
	args := backupArgs(c, sp, "S")
	if args[3] != c.RemoteFor(sp)+".opencloud-bar-backups/"+sp.StateName+"/S" {
		t.Errorf("backup dir %q", args[3])
	}
}

func TestSpaceOverlapChecks(t *testing.T) {
	c := spaceCfg(t)
	c.Kept = []KeptFolder{{Path: "Docs", StateName: "a"}, {Path: "Docs", StateName: "b", Space: marketing}}
	if err := c.Validate(); err != nil {
		t.Errorf("same path in two spaces: %v", err)
	}
	c.Kept = []KeptFolder{{Path: "Docs", StateName: "a", Space: marketing}, {Path: "Docs/x", StateName: "b", Space: marketing}}
	if c.Validate() == nil {
		t.Error("overlap within a space accepted")
	}
	// A personal folder "Marketing/Docs" would land where the space's "Docs" does.
	c.Kept = []KeptFolder{{Path: "Marketing/Docs", StateName: "a"}, {Path: "Docs", StateName: "b", Space: marketing}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "share its folder") {
		t.Errorf("local clash: %v", err)
	}
	// Legacy (rclone remote) configs can't have spaces.
	c.Kept = []KeptFolder{{Path: "Docs", StateName: "b", Space: marketing}}
	c.Auth = authRclone
	c.SSHHost = "host"
	if c.Validate() == nil {
		t.Error("space accepted without OIDC")
	}
}

// Old configs load exactly as before: no space, same keys, state names, local paths, remote.
func TestSpacesBackCompat(t *testing.T) {
	e := pausedRig(t, `,"kept":[{"path":"Docs","stateName":"docs-1","futureFolderKey":"x"}]`)
	f := e.cfg.Kept[0]
	if f.Space != nil || f.Key() != "Docs" || e.cfg.RemoteFor(f) != e.cfg.RemoteSpec() ||
		e.cfg.Local(f) != filepath.Join(expand(e.cfg.LocalRoot), "Docs") || e.cfg.ServerIdentityFor(f) != e.cfg.ServerIdentity() {
		t.Errorf("old folder changed: %+v", f)
	}
	if st := e.Status(); len(st.Folders) != 1 || st.Folders[0].Path != "Docs" || st.Folders[0].Space != "" {
		t.Errorf("status: %+v", st.Folders)
	}
	if err := e.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(configFile())
	if strings.Contains(string(data), `"space"`) {
		t.Errorf("space key written for an old folder: %s", data)
	}
}

// Keeping the same path from a space next to the personal one, and stopping only that one.
func TestKeepSamePathInSpace(t *testing.T) {
	e := pausedRig(t, `,"auth":"oidc","kept":[{"path":"Docs","stateName":"docs-1","futureFolderKey":"x"}]`)
	if err := e.keepIn(marketing, "Docs", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.keepIn(marketing, "Docs/sub", ""); err == nil {
		t.Error("overlap in the space accepted")
	}
	c := e.config()
	if len(c.Kept) != 2 || c.Kept[1].Space == nil || c.Kept[1].StateName == c.Kept[0].StateName {
		t.Fatalf("kept: %+v", c.Kept)
	}
	if _, ok := e.find(marketing.ID + ":Docs"); !ok {
		t.Error("space folder not found by key")
	}
	// The settings page's per-folder excludes address the space folder by its key.
	if _, err := applyConfigPatch(configPatch{KeptFilters: map[string][]string{marketing.ID + ":Docs": {"- *.psd"}}}); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Kept []map[string]any `json:"kept"`
	}
	data, _ := os.ReadFile(configFile())
	_ = json.Unmarshal(data, &raw)
	if raw.Kept[0]["extraFilters"] != nil || raw.Kept[1]["extraFilters"] == nil || raw.Kept[0]["futureFolderKey"] != "x" {
		t.Errorf("excludes went to the wrong folder: %s", data)
	}
}

func TestDavPermissionsReadOnly(t *testing.T) {
	perms := map[string]string{"/dav/spaces/s1/Docs/": "RDNVCK", "/dav/spaces/s1/View/": "S"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.Header.Get("Depth") != "0" {
			http.Error(w, "no", 400)
			return
		}
		p, ok := perms[r.URL.EscapedPath()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(207)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>` + r.URL.Path +
			`</d:href><d:propstat><d:prop><oc:permissions>` + p + `</oc:permissions></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
	}))
	defer srv.Close()
	s := Space{ID: "s1", Name: "S", DavPath: "/dav/spaces/s1"}
	for path, ro := range map[string]bool{"Docs": false, "View": true} {
		p, err := davPermissions(context.Background(), davURLFor(srv.URL, s, path), "tok")
		if err != nil || readOnly(p) != ro {
			t.Errorf("%s: %q %v", path, p, err)
		}
	}
	if _, err := davPermissions(context.Background(), davURLFor(srv.URL, s, "Gone"), "tok"); err == nil {
		t.Error("missing folder")
	}
	if readOnly("") {
		t.Error("unknown permissions count as read-only")
	}
}
