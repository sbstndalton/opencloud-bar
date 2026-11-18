package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 2.7: keeping a folder from the file manager, the size check, OpenCloud notifications.

func newJar() http.CookieJar { j, _ := cookiejar.New(nil); return j }

// ---- driveRel

func TestDriveRel(t *testing.T) {
	type c struct {
		root, path, want string
		ok               bool
	}
	var cases []c
	if runtime.GOOS == "windows" {
		cases = []c{
			{`O:`, `O:\Docs\Work`, "Docs/Work", true},
			{`O:\`, `O:\Docs`, "Docs", true},
			{`O:`, `o:\docs\work`, "docs/work", true}, // letter and case don't matter
			{`o:`, `O:\Docs`, "Docs", true},
			{`O:`, `O:/Docs/Work/`, "Docs/Work", true},
			{`O:`, `O:\..foo`, "..foo", true}, // a folder named ..foo is inside
			{`O:`, `O:\`, "", false},          // the drive itself
			{`O:`, `P:\Docs`, "", false},      // another drive
			{`O:`, `O:\Docs\..\..`, "", false},
			{`O:`, `C:\Users\sam\Docs`, "", false},
			{`O:`, `Docs`, "", false}, // not absolute
			{`O:`, ``, "", false},
		}
	} else {
		cases = []c{
			{"/mnt/oc", "/mnt/oc/Docs/Work", "Docs/Work", true},
			{"/mnt/oc/", "/mnt/oc/Docs", "Docs", true},
			{"/mnt/oc", "/mnt/oc/Docs/Work/", "Docs/Work", true},
			{"/mnt/oc", "/mnt/oc/..foo", "..foo", true},
			{"/mnt/oc", "/mnt/oc", "", false},
			{"/mnt/oc", "/mnt/ocx/Docs", "", false}, // same prefix, another folder
			{"/mnt/oc", "/mnt/OC/Docs", "", false},  // case counts here
			{"/mnt/oc", "/mnt/oc/Docs/../../etc", "", false},
			{"/mnt/oc", "/home/sam/Docs", "", false},
			{"/mnt/oc", "Docs", "", false},
			{"/mnt/oc", "", "", false},
		}
	}
	for _, x := range cases {
		got, ok := driveRel(x.root, x.path)
		if got != x.want || ok != x.ok {
			t.Errorf("driveRel(%q, %q) = %q, %v; want %q, %v", x.root, x.path, got, ok, x.want, x.ok)
		}
	}
}

// ---- keep-drive

type kdRig struct {
	mount, outside string
	out, errw      bytes.Buffer
	alerts         []string
	confirms       int
	answer         bool
	calls          []bool // the confirm flag of each keep call
	rels           []string
	check          SizeCheck
	keepErr        error
	env            keepDriveEnv
}

func newKDRig(t *testing.T, dialogs bool) *kdRig {
	t.Helper()
	k := &kdRig{mount: t.TempDir(), outside: t.TempDir(), check: SizeCheck{Size: -1, Free: -1}}
	k.env = keepDriveEnv{mount: k.mount, out: &k.out, errw: &k.errw, dialogs: dialogs,
		alert:   func(title, text string) { k.alerts = append(k.alerts, title+": "+text) },
		confirm: func(title, text, button string) bool { k.confirms++; return k.answer },
		keep: func(rel string, confirm bool) (SizeCheck, error) {
			k.rels = append(k.rels, rel)
			k.calls = append(k.calls, confirm)
			if k.keepErr != nil {
				return SizeCheck{}, k.keepErr
			}
			if confirm {
				return SizeCheck{Size: -1, Free: -1}, nil
			}
			return k.check, nil
		}}
	return k
}

func TestKeepDriveRefusesPathsOutsideTheDrive(t *testing.T) {
	for _, dialogs := range []bool{false, true} {
		k := newKDRig(t, dialogs)
		file := filepath.Join(k.mount, "note.txt")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, path := range map[string]string{"outside": k.outside, "the drive itself": k.mount, "a file": file, "relative": "Docs"} {
			k.alerts, k.errw = nil, bytes.Buffer{}
			if code := k.env.run([]string{path}); code == 0 {
				t.Errorf("dialogs=%v %s: accepted (exit 0)", dialogs, name)
			}
			if len(k.rels) != 0 {
				t.Fatalf("dialogs=%v %s: the app was asked to keep %v", dialogs, name, k.rels)
			}
			if k.errw.Len() == 0 {
				t.Errorf("dialogs=%v %s: nothing printed", dialogs, name)
			}
			if (len(k.alerts) == 1) != dialogs {
				t.Errorf("dialogs=%v %s: alerts %v", dialogs, name, k.alerts)
			}
		}
		if code := k.env.run(nil); code != 2 {
			t.Errorf("no path: exit %d, want 2", code)
		}
		if code := k.env.run([]string{"--bogus", k.mount}); code != 2 {
			t.Errorf("unknown flag: exit %d, want 2", code)
		}
	}
	k := newKDRig(t, false)
	k.env.run([]string{k.outside})
	if !strings.Contains(k.errw.String(), "isn't in") {
		t.Errorf("the refusal doesn't say why: %q", k.errw.String())
	}
}

func TestKeepDriveKeepsAFolderInsideTheDrive(t *testing.T) {
	k := newKDRig(t, false)
	dir := filepath.Join(k.mount, "Docs", "Work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if code := k.env.run([]string{dir}); code != 0 {
		t.Fatalf("exit %d: %s", code, k.errw.String())
	}
	if len(k.rels) != 1 || k.rels[0] != "Docs/Work" || k.calls[0] {
		t.Errorf("calls %v %v", k.rels, k.calls)
	}
	// A folder that isn't there (a mount that isn't visible to this check) still maps.
	k = newKDRig(t, false)
	if code := k.env.run([]string{filepath.Join(k.mount, "Later")}); code != 0 || k.rels[0] != "Later" {
		t.Errorf("exit %d, %v", code, k.rels)
	}
	// The app not running (or refusing) is an error.
	k = newKDRig(t, true)
	k.keepErr = errors.New("opencloud-bar isn't running")
	if code := k.env.run([]string{filepath.Join(k.mount, "Later")}); code == 0 || len(k.alerts) != 1 {
		t.Errorf("exit %d, alerts %v", code, k.alerts)
	}
}

func TestKeepDriveBigFolderNeedsYesOrADialog(t *testing.T) {
	big := newSizeCheck("Big", 6<<30, 100<<30)
	if !big.NeedsConfirm {
		t.Fatal("rig: not big")
	}
	// No terminal dialogs: refuse without --yes, keep with it.
	k := newKDRig(t, false)
	k.check = big
	dir := filepath.Join(k.mount, "Big")
	if code := k.env.run([]string{dir}); code == 0 || len(k.rels) != 1 {
		t.Fatalf("without --yes: exit %d, calls %v", code, k.calls)
	}
	if !strings.Contains(k.errw.String(), "--yes") {
		t.Errorf("doesn't say about --yes: %q", k.errw.String())
	}
	k.errw.Reset()
	if code := k.env.run([]string{dir, "--yes"}); code != 0 || !k.calls[len(k.calls)-1] {
		t.Fatalf("with --yes: exit %d, calls %v", code, k.calls)
	}
	// Dialogs: cancel keeps nothing, OK keeps.
	k = newKDRig(t, true)
	k.check = big
	dir = filepath.Join(k.mount, "Big")
	if code := k.env.run([]string{dir}); code == 0 || k.confirms != 1 || len(k.calls) != 1 {
		t.Fatalf("cancelled: exit %d, confirms %d, calls %v", code, k.confirms, k.calls)
	}
	k.answer = true
	if code := k.env.run([]string{dir}); code != 0 || k.confirms != 2 || !k.calls[len(k.calls)-1] {
		t.Fatalf("accepted: exit %d, confirms %d, calls %v", code, k.confirms, k.calls)
	}
}

// ---- the size check

func TestSizeVerdict(t *testing.T) {
	const gb = int64(1 << 30)
	for _, c := range []struct {
		size, free int64
		want       bool
		why        string
	}{
		{1 * gb, 10 * gb, false, "small and fits"},
		{5 * gb, 100 * gb, false, "exactly 5 GB is not more than 5 GB"},
		{5*gb + 1, 100 * gb, true, "more than 5 GB"},
		{20 * gb, 500 * gb, true, "big, lots free"},
		{3 * gb, 2 * gb, true, "more than the free space"},
		{2 * gb, 2 * gb, false, "exactly the free space"},
		{2*gb + 1, 2 * gb, true, "one byte more than the free space"},
		{0, 0, false, "empty folder, no space"},
		{-1, 0, false, "unknown size, no free space"},
		{-1, -1, false, "nothing known"},
		{100 * 1024, -1, false, "small, free space unknown"},
		{6 * gb, -1, true, "big, free space unknown"},
	} {
		if got := sizeVerdict(c.size, c.free); got != c.want {
			t.Errorf("%s: sizeVerdict(%d, %d) = %v", c.why, c.size, c.free, got)
		}
	}
	sc := newSizeCheck("Photos", 8<<30, 2<<30)
	if !sc.NeedsConfirm || !strings.Contains(sc.Message, "8.0 GB") || !strings.Contains(sc.Message, "2.0 GB is free") {
		t.Errorf("message: %+v", sc)
	}
	if sc := newSizeCheck("Photos", 8<<30, 100<<30); !strings.Contains(sc.Message, "a lot") {
		t.Errorf("message for a big folder that fits: %q", sc.Message)
	}
	if sc := newSizeCheck("Photos", -1, 1<<30); sc.NeedsConfirm || sc.Message != "" {
		t.Errorf("unknown size: %+v", sc)
	}
}

// fakeDAV answers PROPFIND for the folder's oc:size and records what it is asked.
type fakeDAV struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
	hdr  []http.Header
	body []string
	// answer: status and body
	status int
	xml    string
	// redirect: answer 307 to this address
	redirect string
}

func newFakeDAV(t *testing.T) *fakeDAV {
	f := &fakeDAV{status: 207}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.hdr = append(f.hdr, r.Header.Clone())
		f.body = append(f.body, string(b))
		if f.redirect != "" {
			http.Redirect(w, r, f.redirect, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.xml)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func sizeXML(size string) string {
	return `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/dav/spaces/s/Big/</d:href><d:propstat><d:prop><oc:size>` + size + `</oc:size></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`
}

func TestDavFolderSize(t *testing.T) {
	f := newFakeDAV(t)
	f.xml = sizeXML("123456789012")
	n, err := davFolderSize(context.Background(), f.srv.URL+"/dav/spaces/s/Big/", "tok")
	if err != nil || n != 123456789012 {
		t.Fatalf("%d %v", n, err)
	}
	if f.reqs[0] != "PROPFIND /dav/spaces/s/Big/" || f.hdr[0].Get("Depth") != "0" || f.hdr[0].Get("Authorization") != "Bearer tok" ||
		!strings.Contains(f.body[0], "<oc:size/>") {
		t.Errorf("request: %v %v %q", f.reqs, f.hdr[0], f.body[0])
	}
	// Anything that isn't a size is "unknown" (an error), never a number.
	for name, c := range map[string]struct {
		status int
		xml    string
	}{
		"404":           {404, ""},
		"500":           {500, "boom"},
		"401":           {401, ""},
		"not xml":       {207, "<<<"},
		"no prop":       {207, `<d:multistatus xmlns:d="DAV:"></d:multistatus>`},
		"empty size":    {207, sizeXML("")},
		"not a number":  {207, sizeXML("lots")},
		"negative":      {207, sizeXML("-5")},
		"not found 404": {207, strings.Replace(sizeXML("77"), "200 OK", "404 Not Found", 1)},
	} {
		f.mu.Lock()
		f.status, f.xml = c.status, c.xml
		f.mu.Unlock()
		if n, err := davFolderSize(context.Background(), f.srv.URL+"/x/", "tok"); err == nil || n != -1 {
			t.Errorf("%s: %d %v", name, n, err)
		}
	}
}

func TestDavFolderSizeFollowsNoRedirect(t *testing.T) {
	other := newFakeDAV(t)
	other.xml = sizeXML("5")
	f := newFakeDAV(t)
	f.redirect = other.srv.URL + "/steal"
	if n, err := davFolderSize(context.Background(), f.srv.URL+"/x/", "tok"); err == nil || n != -1 {
		t.Errorf("a redirect gave a size: %d %v", n, err)
	}
	if len(other.reqs) != 0 {
		t.Errorf("the redirect was followed: %v", other.reqs)
	}
	if len(f.reqs) != 1 {
		t.Errorf("sent %d times", len(f.reqs))
	}
}

// sizeRig: the history rig (signed in, the fake server) with the size and free space faked.
func sizeRig(t *testing.T, size int64, sizeErr error, free int64) (*Engine, *int) {
	t.Helper()
	e, _ := historyRig(t)
	signedIn(e)
	asked := new(int)
	oldS, oldF := keepSizeFn, freeAtFn
	keepSizeFn = func(ctx context.Context, davURL, tok string) (int64, error) {
		*asked++
		if tok != "tok" || !strings.Contains(davURL, "/remote.php/dav/files/sam/") && !strings.Contains(davURL, "/dav/spaces/") {
			t.Errorf("size asked of %q with %q", davURL, tok)
		}
		return size, sizeErr
	}
	freeAtFn = func(string) int64 { return free }
	t.Cleanup(func() { keepSizeFn, freeAtFn = oldS, oldF })
	return e, asked
}

func keptPaths(e *Engine) string {
	var p []string
	for _, f := range e.config().Kept {
		p = append(p, f.Path)
	}
	return strings.Join(p, ",")
}

func TestKeepAsksBeforeABigFolder(t *testing.T) {
	const gb = int64(1 << 30)
	// Bigger than 5 GB: not kept until confirmed.
	e, asked := sizeRig(t, 6*gb, nil, 500*gb)
	sc, err := e.KeepChecked("", "Big", "", false)
	if err != nil || !sc.NeedsConfirm || sc.Size != 6*gb || sc.Free != 500*gb || *asked != 1 {
		t.Fatalf("%+v %v asked %d", sc, err, *asked)
	}
	if strings.Contains(keptPaths(e), "Big") {
		t.Fatal("kept before it was confirmed")
	}
	if sc, err = e.KeepChecked("", "Big", "", true); err != nil || sc.NeedsConfirm || !strings.Contains(keptPaths(e), "Big") {
		t.Fatalf("confirmed: %+v %v %s", sc, err, keptPaths(e))
	}
	if *asked != 1 {
		t.Errorf("a confirmed keep asked the size again (%d)", *asked)
	}

	// More than the free space.
	e, _ = sizeRig(t, 3*gb, nil, 2*gb)
	if sc, err = e.KeepChecked("", "Photos", "", false); err != nil || !sc.NeedsConfirm || strings.Contains(keptPaths(e), "Photos") {
		t.Fatalf("%+v %v", sc, err)
	}

	// Small and fits: kept straight away.
	e, _ = sizeRig(t, 1*gb, nil, 500*gb)
	if sc, err = e.KeepChecked("", "Small", "", false); err != nil || sc.NeedsConfirm || !strings.Contains(keptPaths(e), "Small") {
		t.Fatalf("%+v %v", sc, err)
	}

	// An unknown size never blocks, however it comes to be unknown.
	e, _ = sizeRig(t, -1, errors.New("the server didn't answer"), 0)
	if sc, err = e.KeepChecked("", "Unknown", "", false); err != nil || sc.NeedsConfirm || !strings.Contains(keptPaths(e), "Unknown") {
		t.Fatalf("%+v %v", sc, err)
	}
	// An unknown free space doesn't block a small folder either.
	e, _ = sizeRig(t, 1*gb, nil, -1)
	if sc, err = e.KeepChecked("", "NoFree", "", false); err != nil || sc.NeedsConfirm {
		t.Fatalf("%+v %v", sc, err)
	}
	// A bad path is refused before anything is asked of the server.
	e, asked = sizeRig(t, 6*gb, nil, 500*gb)
	if _, err = e.KeepChecked("", "../x", "", false); err == nil || *asked != 0 {
		t.Errorf("bad path: %v, asked %d", err, *asked)
	}
}

func TestKeepSizeCheckNeedsTheBrowserSignIn(t *testing.T) {
	e := pausedRig(t, `,"kept":[]`) // auth "rclone": no token to ask with
	asked := 0
	old := keepSizeFn
	keepSizeFn = func(context.Context, string, string) (int64, error) { asked++; return 99 << 30, nil }
	t.Cleanup(func() { keepSizeFn = old })
	sc, err := e.KeepChecked("", "Big", "", false)
	if err != nil || sc.NeedsConfirm || asked != 0 {
		t.Errorf("%+v %v asked %d", sc, err, asked)
	}
}

func TestKeepAPIReturnsNeedsConfirmUntilConfirmed(t *testing.T) {
	const gb = int64(1 << 30)
	e, _ := sizeRig(t, 7*gb, nil, 500*gb)
	ui := newWebUI()
	srv := httptest.NewServer(apiHandler(e, func() {}, "tok", ui))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	ui.setPort(port)
	r := &uiRig{t: t, e: e, ui: ui, srv: srv, port: port, clock: time.Now(),
		client: &http.Client{Jar: newJar(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	r.login()
	resp, body := r.post("/ui/api/keep", `{"path":"Big"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"needsConfirm":true`) || !strings.Contains(body, `"size":7516192768`) || !strings.Contains(body, `"free":536870912000`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if strings.Contains(keptPaths(e), "Big") {
		t.Fatal("kept before confirming")
	}
	resp, body = r.post("/ui/api/keep", `{"path":"Big","confirm":true}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"needsConfirm":false`) || !strings.Contains(keptPaths(e), "Big") {
		t.Fatalf("%d %s %s", resp.StatusCode, body, keptPaths(e))
	}
	// The control API (CLI) says the same.
	e2, _ := sizeRig(t, 7*gb, nil, 500*gb)
	srv2 := httptest.NewServer(apiHandler(e2, func() {}, "tok", newWebUI()))
	t.Cleanup(srv2.Close)
	post := func(q string) string {
		req, _ := http.NewRequest("POST", srv2.URL+"/keep?"+q, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if b := post("path=Big"); !strings.Contains(b, `"needsConfirm":true`) || strings.Contains(keptPaths(e2), "Big") {
		t.Errorf("CLI without confirm: %s %s", b, keptPaths(e2))
	}
	if b := post("path=Big&confirm=1"); !strings.Contains(b, `"needsConfirm":false`) || !strings.Contains(keptPaths(e2), "Big") {
		t.Errorf("CLI with confirm: %s %s", b, keptPaths(e2))
	}
}

func TestFreeAtFindsAnExistingParent(t *testing.T) {
	dir := t.TempDir()
	if n := freeAt(filepath.Join(dir, "not", "there", "yet")); n < 0 {
		t.Errorf("free space of a folder that isn't made yet: %d", n)
	}
	if n := freeAt(dir); n <= 0 {
		t.Errorf("free space: %d", n)
	}
}

// ---- the installer

func TestInstallerAddsTheKeepVerbAndRemovesItOnUninstall(t *testing.T) {
	const key = `Software\Classes\Directory\shell\OpenCloudBarKeep`
	var verb, command issEntry
	deletes := false
	for _, e := range issRegistryEntries(t) {
		if e["Root"] != "HKCU" {
			continue
		}
		switch strings.ToLower(e["Subkey"]) {
		case strings.ToLower(key):
			if e["ValueName"] == "" {
				verb = e
			}
			if strings.Contains(e["Flags"], "uninsdeletekey") {
				deletes = true
			}
		case strings.ToLower(key + `\command`):
			command = e
		}
	}
	if verb == nil || command == nil {
		t.Fatalf("the .iss has no HKCU %s verb (%v) and command (%v)", key, verb, command)
	}
	if verb["ValueData"] != "Keep on this PC with OpenCloud Bar" {
		t.Errorf("menu text %q", verb["ValueData"])
	}
	if want := `"{app}\opencloud-bar.exe" keep-drive "%V"`; command["ValueData"] != want {
		t.Errorf("command %q, want %q", command["ValueData"], want)
	}
	if !deletes {
		t.Error("the verb key has no uninsdeletekey flag: uninstalling would leave the menu item behind")
	}
	// The verb sits under the verb key, so deleting it takes the command with it (not above it).
	if !strings.HasPrefix(strings.ToLower(command["Subkey"]), strings.ToLower(key)+`\`) {
		t.Errorf("command key %q isn't under the verb key", command["Subkey"])
	}
}

// ---- OpenCloud notifications

func TestParseSrvNotes(t *testing.T) {
	got, err := parseSrvNotes(strings.NewReader(`{"ocs":{"meta":{"status":"ok","statuscode":200},"data":[
	  {"notification_id":"n1","app":"userlog","subject":"Ann shared “x” with you","message":"A new share","datetime":"2026-11-18T10:00:00Z"},
	  {"notification_id":42,"subject":"numeric"},
	  {"notification_id":"n1","subject":"repeated"},
	  {"subject":"no id"},
	  {"notification_id":"  ","subject":"blank id"}]}}`))
	if err != nil || len(got) != 2 || got[0] != (srvNote{"n1", "Ann shared “x” with you", "A new share"}) || got[1].ID != "42" {
		t.Fatalf("%+v %v", got, err)
	}
	for name, body := range map[string]string{"null data": `{"ocs":{"data":null}}`, "empty list": `{"ocs":{"data":[]}}`, "empty body": ``, "blank body": " \n"} {
		if got, err := parseSrvNotes(strings.NewReader(body)); err != nil || len(got) != 0 {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	for name, body := range map[string]string{"html": "<html>", "wrong shape": `{"ocs":{"data":"x"}}`, "array": `[]`} {
		if _, err := parseSrvNotes(strings.NewReader(body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func ids(ns ...string) []srvNote {
	var out []srvNote
	for _, n := range ns {
		out = append(out, srvNote{ID: n, Subject: "s " + n})
	}
	return out
}

func TestPlanSrvNotes(t *testing.T) {
	// The first fetch only seeds.
	show, st := planSrvNotes(srvSeen{}, ids("a", "b"))
	if len(show) != 0 || !st.Seeded || strings.Join(st.IDs, ",") != "a,b" {
		t.Fatalf("first: %v %+v", show, st)
	}
	// Even an empty first fetch seeds: what comes later is new.
	show, st0 := planSrvNotes(srvSeen{}, nil)
	if len(show) != 0 || !st0.Seeded {
		t.Fatalf("first, empty: %v %+v", show, st0)
	}
	if show, _ = planSrvNotes(st0, ids("x")); len(show) != 1 {
		t.Errorf("after an empty seed: %v", show)
	}
	// A new id is shown once.
	show, st = planSrvNotes(st, ids("a", "b", "c"))
	if len(show) != 1 || show[0].ID != "c" {
		t.Fatalf("new: %v", show)
	}
	if show, st = planSrvNotes(st, ids("a", "b", "c")); len(show) != 0 {
		t.Fatalf("again: %v", show)
	}
	// One read in OpenCloud (gone from the list) stays remembered, so it isn't new if it comes back.
	show, st = planSrvNotes(st, ids("a", "c"))
	if len(show) != 0 {
		t.Fatalf("one fewer: %v", show)
	}
	if show, _ = planSrvNotes(st, ids("a", "b", "c")); len(show) != 0 {
		t.Errorf("an id that went and came back was shown: %v", show)
	}
}

func TestSeenIDsAreCapped(t *testing.T) {
	st := srvSeen{Seeded: true}
	for batch := 0; batch < 12; batch++ {
		var got []srvNote
		for i := 0; i < 100; i++ {
			got = append(got, srvNote{ID: fmt.Sprintf("n%d-%d", batch, i)})
		}
		var show []srvNote
		show, st = planSrvNotes(st, got)
		if len(show) != 100 {
			t.Fatalf("batch %d: %d shown", batch, len(show))
		}
		if len(st.IDs) > srvNotesMaxSeen {
			t.Fatalf("batch %d: %d ids kept", batch, len(st.IDs))
		}
	}
	if len(st.IDs) != srvNotesMaxSeen {
		t.Errorf("%d ids kept, want %d", len(st.IDs), srvNotesMaxSeen)
	}
	if st.IDs[len(st.IDs)-1] != "n11-99" || st.IDs[0] != "n7-0" {
		t.Errorf("not the newest ones: %s … %s", st.IDs[0], st.IDs[len(st.IDs)-1])
	}
	// Ids the server still lists are never dropped, or they would be shown again.
	var big []srvNote
	for i := 0; i < 700; i++ {
		big = append(big, srvNote{ID: "b" + strconv.Itoa(i)})
	}
	_, st = planSrvNotes(srvSeen{Seeded: true}, big)
	if len(st.IDs) != 700 {
		t.Errorf("kept %d of 700 listed", len(st.IDs))
	}
	if show, _ := planSrvNotes(st, big); len(show) != 0 {
		t.Errorf("%d shown again", len(show))
	}
}

func TestSrvNotesBackoff(t *testing.T) {
	want := []time.Duration{3, 6, 12, 24, 30, 30, 30}
	for fails, w := range want {
		if got := srvNotesBackoff(fails); got != w*time.Minute {
			t.Errorf("%d errors: %v, want %v", fails, got, w*time.Minute)
		}
	}
	if srvNotesBackoff(1000) != 30*time.Minute {
		t.Error("not capped")
	}
}

func TestSrvToasts(t *testing.T) {
	n := srvToasts([]srvNote{{ID: "1", Subject: "  Ann\tshared\n“x”  ", Message: "line1\r\nline2\x07"}})
	if len(n) != 1 || n[0].Title != "Ann shared “x”" || n[0].Body != "line1 line2" {
		t.Errorf("%+v", n)
	}
	if n = srvToasts([]srvNote{{ID: "1"}}); n[0].Title != "OpenCloud" || n[0].Body == "" {
		t.Errorf("empty notification: %+v", n)
	}
	long := strings.Repeat("é", 500)
	if n = srvToasts([]srvNote{{ID: "1", Subject: long, Message: long}}); len([]rune(n[0].Title)) > 100 || len([]rune(n[0].Body)) > 240 {
		t.Errorf("not shortened: %d %d", len([]rune(n[0].Title)), len([]rune(n[0].Body)))
	}
	many := srvToasts(ids("1", "2", "3", "4", "5", "6", "7"))
	if len(many) != srvNotesMaxToasts || !strings.Contains(many[len(many)-1].Body, "3 more") {
		t.Errorf("a flood: %+v", many)
	}
	if got := srvToasts(ids("1", "2", "3", "4", "5")); len(got) != 5 || strings.Contains(got[4].Body, "more") {
		t.Errorf("exactly the limit: %+v", got)
	}
}

// fakeNotes is an OpenCloud server's notifications route.
type fakeNotes struct {
	srv      *httptest.Server
	mu       sync.Mutex
	status   int
	body     string
	reqs     []string // "METHOD path?query"
	auth     []string
	redirect string
}

func newFakeNotes(t *testing.T) *fakeNotes {
	f := &fakeNotes{status: 200, body: notesJSON()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if f.redirect != "" {
			http.Redirect(w, r, f.redirect, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func notesJSON(idList ...string) string {
	var items []string
	for _, id := range idList {
		items = append(items, fmt.Sprintf(`{"notification_id":%q,"app":"userlog","subject":"Subject %s","message":"Message %s"}`, id, id, id))
	}
	return `{"ocs":{"meta":{"status":"ok","statuscode":200,"message":""},"data":[` + strings.Join(items, ",") + `]}}`
}

func (f *fakeNotes) set(status int, body string) {
	f.mu.Lock()
	f.status, f.body = status, body
	f.mu.Unlock()
}

func (f *fakeNotes) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

type notesRig struct {
	t     *testing.T
	e     *Engine
	f     *fakeNotes
	mu    sync.Mutex
	shown []string
}

func (r *notesRig) titles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.shown...)
}

func signInTo(t *testing.T, serverURL string) {
	t.Helper()
	base, _ := normalizeServerURL(serverURL)
	if err := writeAuthState(authState{ServerURL: base, Issuer: base, Username: "sam", Access: "tok", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func newNotesRig(t *testing.T, extra string) *notesRig {
	t.Helper()
	f := newFakeNotes(t)
	e := pausedRig(t, `,"auth":"oidc","serverURL":"`+f.srv.URL+`"`+extra)
	signInTo(t, f.srv.URL)
	_ = os.MkdirAll(expand(e.cfg.StateDir), 0o700)
	signedIn(e)
	r := &notesRig{t: t, e: e, f: f}
	old := showNotification
	showNotification = func(title, body string) error {
		r.mu.Lock()
		r.shown = append(r.shown, title+" | "+body)
		r.mu.Unlock()
		return nil
	}
	t.Cleanup(func() { showNotification = old })
	return r
}

// poll runs one poll now.
func (r *notesRig) poll() {
	r.t.Helper()
	r.e.mu.Lock()
	gen := r.e.snGen
	r.e.mu.Unlock()
	r.e.pollServerNotes(gen) // shows what is new before it returns
}

func (r *notesRig) state() (seen srvSeen, at time.Time, fails int, off bool) {
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	return r.e.snSeen, r.e.snAt, r.e.snFails, r.e.snOff
}

func TestFirstFetchSeedsAndANewIdIsShownOnce(t *testing.T) {
	r := newNotesRig(t, "")
	file := srvNotesFile(r.e.config())
	r.f.set(200, notesJSON("n1", "n2"))
	r.poll()
	if got := r.titles(); len(got) != 0 {
		t.Fatalf("the first fetch showed %v", got)
	}
	if seen := readSrvSeen(file); !seen.Seeded || strings.Join(seen.IDs, ",") != "n1,n2" {
		t.Fatalf("not recorded: %+v", seen)
	}
	r.poll()
	if got := r.titles(); len(got) != 0 {
		t.Fatalf("the same ids showed %v", got)
	}
	r.f.set(200, notesJSON("n1", "n2", "n3"))
	r.poll()
	got := r.titles()
	if len(got) != 1 || got[0] != "Subject n3 | Message n3" {
		t.Fatalf("a new id: %v", got)
	}
	r.poll()
	r.poll()
	if got := r.titles(); len(got) != 1 {
		t.Fatalf("shown again: %v", got)
	}
	// A restart (a new engine) knows what was seen: no flood, and n3 isn't shown again.
	e2 := NewEngine()
	signedIn(e2)
	e2.mu.Lock()
	gen := e2.snGen
	e2.mu.Unlock()
	e2.pollServerNotes(gen)
	if got := r.titles(); len(got) != 1 {
		t.Fatalf("after a restart: %v", got)
	}
}

func TestSeedingWorksWithAnEmptyServerAndWithNotificationsOff(t *testing.T) {
	r := newNotesRig(t, "")
	r.f.set(200, notesJSON())
	r.poll()
	r.f.set(200, notesJSON("a"))
	r.poll()
	if got := r.titles(); len(got) != 1 {
		t.Errorf("a first notification after an empty seed: %v", got)
	}

	// With notifications off nothing is shown, but the ids are recorded: turning them on later
	// doesn't show a flood.
	r = newNotesRig(t, `,"notifications":false`)
	r.f.set(200, notesJSON("a"))
	r.poll() // seeds
	r.f.set(200, notesJSON("a", "b"))
	r.poll()
	if got := r.titles(); len(got) != 0 {
		t.Fatalf("shown with notifications off: %v", got)
	}
	if seen := readSrvSeen(srvNotesFile(r.e.config())); strings.Join(seen.IDs, ",") != "a,b" {
		t.Errorf("not recorded: %+v", seen)
	}
}

func TestOnlyTheConfiguredHostIsContactedWithGETOnly(t *testing.T) {
	r := newNotesRig(t, "")
	decoy := newFakeNotes(t)
	r.f.set(200, notesJSON("n1"))
	r.poll()
	r.f.set(200, notesJSON("n1", "n2"))
	r.poll()
	reqs := r.f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%v", reqs)
	}
	for i, q := range reqs {
		if q != "GET "+srvNotesPath+"?format=json" {
			t.Errorf("request %d: %q", i, q)
		}
		if r.f.auth[i] != "Bearer tok" {
			t.Errorf("request %d auth %q", i, r.f.auth[i])
		}
	}
	if len(decoy.requests()) != 0 {
		t.Errorf("another host was contacted: %v", decoy.requests())
	}
}

func TestNoRedirectIsFollowed(t *testing.T) {
	other := newFakeNotes(t)
	r := newNotesRig(t, "")
	r.f.mu.Lock()
	r.f.redirect = other.srv.URL + "/steal"
	r.f.mu.Unlock()
	r.poll()
	r.poll()
	if got := other.requests(); len(got) != 0 {
		t.Fatalf("the redirect was followed (the token went to another host): %v", got)
	}
	if got := r.titles(); len(got) != 0 {
		t.Errorf("shown: %v", got)
	}
	if _, _, fails, off := r.state(); fails != 2 || off {
		t.Errorf("a redirect should be an error that backs off: fails %d, off %v", fails, off)
	}
	if _, err := fetchSrvNotes(context.Background(), r.f.srv.URL, "tok"); err == nil {
		t.Error("fetch followed a redirect")
	}
}

func TestA404DisablesPollingForTheSession(t *testing.T) {
	r := newNotesRig(t, "")
	r.f.set(404, "")
	r.poll()
	if _, _, fails, off := r.state(); !off || fails != 0 {
		t.Fatalf("off %v, fails %d", off, fails)
	}
	if got := r.titles(); len(got) != 0 {
		t.Errorf("shown: %v", got)
	}
	// Due again, but it doesn't poll.
	before := len(r.f.requests())
	r.e.mu.Lock()
	r.e.snAt = time.Time{}
	r.e.srvNotesDueLocked()
	busy := r.e.snBusy
	r.e.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if busy || len(r.f.requests()) != before {
		t.Errorf("polled again after a 404 (busy %v, %d requests)", busy, len(r.f.requests())-before)
	}
	// A new sign-in (or another server) is a new session.
	r.e.mu.Lock()
	r.e.resetQuotaLocked(r.e.cfg)
	off := r.e.snOff
	r.e.mu.Unlock()
	if off {
		t.Error("still off after a reset")
	}
}

func TestErrorsBackOffAndASuccessResets(t *testing.T) {
	r := newNotesRig(t, "")
	r.f.set(500, "boom")
	for i, want := range []time.Duration{6, 12, 24, 30, 30} {
		before := time.Now()
		r.poll()
		_, at, fails, _ := r.state()
		d := at.Sub(before)
		if fails != i+1 || d < want*time.Minute-time.Second || d > want*time.Minute+time.Minute {
			t.Fatalf("after error %d: fails %d, next in %v, want %v", i+1, fails, d, want*time.Minute)
		}
	}
	r.f.set(200, notesJSON("a"))
	before := time.Now()
	r.poll()
	_, at, fails, _ := r.state()
	if d := at.Sub(before); fails != 0 || d < srvNotesEvery-time.Second || d > srvNotesEvery+time.Minute {
		t.Errorf("after a success: fails %d, next in %v", fails, d)
	}
	// Garbage in a 200 is an error too.
	r.f.set(200, "<html>")
	r.poll()
	if _, _, fails, _ = r.state(); fails != 1 {
		t.Errorf("garbage: fails %d", fails)
	}
}

func TestPollingOnlyWhenItShould(t *testing.T) {
	// Each case changes one thing from "polls".
	for name, c := range map[string]struct {
		extra string
		mut   func(e *Engine)
		polls bool
	}{
		"default":              {"", nil, true},
		"setting off":          {`,"serverNotifications":false`, nil, false},
		"paused":               {`,"paused":true`, nil, false},
		"signed out":           {"", func(e *Engine) { e.signedOut = true }, false},
		"quitting":             {"", func(e *Engine) { e.quitting = true }, false},
		"not yet due":          {"", func(e *Engine) { e.snAt = time.Now().Add(time.Minute) }, false},
		"already polling":      {"", func(e *Engine) { e.snBusy = true }, false},
		"browser sign-in only": {`,"auth":"rclone"`, nil, false},
	} {
		r := newNotesRig(t, c.extra)
		r.e.mu.Lock()
		if c.mut != nil {
			c.mut(r.e)
		}
		r.e.srvNotesDueLocked()
		r.e.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		if got := len(r.f.requests()) > 0; got != c.polls {
			t.Errorf("%s: polled %v, want %v", name, got, c.polls)
		}
	}
}

func TestNotificationsStateResetsOnSignOutAndServerChange(t *testing.T) {
	r := newNotesRig(t, "")
	file := srvNotesFile(r.e.config())
	seed := func() {
		r.f.set(200, notesJSON("n1"))
		r.poll()
		if !exists(file) {
			t.Fatal("not recorded")
		}
	}
	gone := func(why string) {
		t.Helper()
		seen, at, fails, off := r.state()
		if exists(file) || seen.Seeded || len(seen.IDs) != 0 || !at.IsZero() || fails != 0 || off {
			t.Errorf("%s: file %v, seen %+v, at %v, fails %d, off %v", why, exists(file), seen, at, fails, off)
		}
	}
	seed()
	if err := r.e.SignOut(true); err != nil {
		t.Fatal(err)
	}
	gone("sign out")

	signedIn(r.e)
	signInTo(t, r.f.srv.URL)
	seed()
	r.e.markSignedOut()
	gone("revoked sign-in")

	// Another account is noticed by the next poll: the old ids aren't this account's.
	signedIn(r.e)
	signInTo(t, r.f.srv.URL)
	seed()
	r.e.mu.Lock()
	r.e.snID = "someone-else"
	r.e.mu.Unlock()
	r.f.set(200, notesJSON("n1", "n2"))
	r.poll() // forgets, then seeds: nothing shown
	if got := r.titles(); len(got) != 0 {
		t.Errorf("another account: shown %v", got)
	}
	if seen := readSrvSeen(file); strings.Join(seen.IDs, ",") != "n1,n2" {
		t.Errorf("after the other account's seed: %+v", seen)
	}

	// Another server, applied by a reload: forgotten, and the first fetch from the new server only seeds.
	other := newFakeNotes(t)
	other.set(200, notesJSON("x1", "x2"))
	raw, _ := os.ReadFile(configFile())
	changed := strings.Replace(string(raw), r.f.srv.URL, other.srv.URL, 1)
	if changed == string(raw) {
		t.Fatal("server not in the config")
	}
	if err := os.WriteFile(configFile(), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	signInTo(t, other.srv.URL)
	if err := r.e.Reload(); err != nil {
		t.Fatal(err)
	}
	gone("server change")
	r.poll()
	if got := r.titles(); len(got) != 0 {
		t.Errorf("first fetch from the new server showed %v", got)
	}
	if n := len(other.requests()); n != 1 {
		t.Errorf("new server asked %d times", n)
	}
	other.set(200, notesJSON("x1", "x2", "x3"))
	r.poll()
	if got := r.titles(); len(got) != 1 || !strings.Contains(got[0], "x3") {
		t.Errorf("after seeding: %v", got)
	}
}

func TestAPollThatStartedBeforeAResetIsDropped(t *testing.T) {
	r := newNotesRig(t, "")
	file := srvNotesFile(r.e.config())
	r.e.mu.Lock()
	gen := r.e.snGen
	r.e.mu.Unlock()
	cfg := r.e.config()
	r.e.mu.Lock()
	r.e.resetQuotaLocked(cfg) // sign-out happens while the fetch is on its way
	r.e.mu.Unlock()
	r.e.applySrvNotes(gen, cfg, ids("old"), time.Now())
	if exists(file) {
		t.Error("an old answer was recorded after the reset")
	}
}

func TestConfigToggleServerNotifications(t *testing.T) {
	r := newNotesRig(t, "")
	if !r.e.config().ServerNotesOn() {
		t.Fatal("not on by default")
	}
	ui := newWebUI()
	srv := httptest.NewServer(apiHandler(r.e, func() {}, "tok", ui))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	ui.setPort(port)
	rig := &uiRig{t: t, e: r.e, ui: ui, srv: srv, port: port, clock: time.Now(),
		client: &http.Client{Jar: newJar(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	rig.login()
	if resp, body := rig.post("/ui/api/config", `{"serverNotifications":false}`); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if r.e.config().ServerNotesOn() {
		t.Error("not turned off")
	}
	if b, _ := os.ReadFile(configFile()); !strings.Contains(string(b), `"serverNotifications": false`) {
		t.Errorf("not saved: %s", b)
	}
	if _, body := rig.do("GET", "/ui/api/state", "", "", nil); !strings.Contains(body, `"serverNotifications":false`) {
		t.Errorf("the page isn't told: %.300s", body)
	}
}
