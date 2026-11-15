package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 1.0.2: one folder can't be kept twice under two spellings, and odd names are refused.
func TestKeptPathNormalisation(t *testing.T) {
	for in, want := range map[string]string{"a//b": "a/b", "/a/b/": "a/b", "a///b//c": "a/b/c", "a": "a"} {
		if got := normalizeKeptPath(in); got != want {
			t.Errorf("normalizeKeptPath(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"a//b", "a/ /b", "a/ b", "a/b ", "a/b.", "a\nb", "x\tz", ".opencloud-bar-backups",
		".OpenCloud-Bar-Backups/x", ".opencloud-bar-backups /x", ".opencloud-bar-backups./x", "a/../b"} {
		if newPathProblem(bad) == "" {
			t.Errorf("newPathProblem(%q) accepted it", bad)
		}
	}
	for _, ok := range []string{"a/b", "My Docs/2026 Taxes", "a.b/c", "x/.opencloud-bar-backups"} {
		if p := newPathProblem(ok); p != "" {
			t.Errorf("pathProblem(%q) = %q", ok, p)
		}
	}

	e := pausedRig(t, "")
	if err := e.Keep("a/b", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.Keep("a//b", ""); err == nil {
		t.Error("a//b kept next to a/b")
	}
	if err := e.Keep("a/b\n", ""); err == nil {
		t.Error("control character accepted")
	}
	if n := len(e.config().Kept); n != 1 {
		t.Errorf("kept %d folders", n)
	}

	// A folder kept by an earlier version with such a name still loads.
	if p := pathProblem("Notes/v1."); p != "" {
		t.Errorf("existing name refused on load: %s", p)
	}
	c := e.config()
	c.Kept = append(c.Kept, KeptFolder{Path: "x", StateName: "x-1", LocalPath: "/tmp/a\nb"})
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "control") {
		t.Errorf("localPath control characters: %v", err)
	}
}

// 1.0.2: a local-copy clash names the folder being added, not the one already kept.
func TestLocalPathClashNamesNewFolder(t *testing.T) {
	testEnv(t)
	root := filepath.Join(home(), "OC")
	c := defaultConfig()
	c.LocalRoot = root
	c.Kept = []KeptFolder{{Path: "Old", StateName: "old-1", LocalPath: filepath.Join(root, "New")},
		{Path: "New", StateName: "new-1"}}
	err := c.Validate()
	if err == nil || !strings.HasPrefix(err.Error(), `kept folder "New"`) || !strings.Contains(err.Error(), `"Old"`) {
		t.Errorf("got %v", err)
	}
}

// 1.0.2: localRoot and mountPoint must be real, separate folders.
func TestFolderSettingsValidated(t *testing.T) {
	testEnv(t)
	cur := defaultConfig()
	try := func(p configPatch) error {
		next := cur
		if p.LocalRoot != nil {
			next.LocalRoot = *p.LocalRoot
		}
		if p.MountPoint != nil {
			next.MountPoint = *p.MountPoint
		}
		return p.validate(next, cur)
	}
	for _, bad := range []string{"", "  ", "/", "relative", "../x", "~/a/../b", " ~/x", "~/x\n"} {
		if try(configPatch{LocalRoot: ptr(bad)}) == nil {
			t.Errorf("localRoot %q accepted", bad)
		}
	}
	for _, bad := range []string{"/", "~", home(), "drive", "~/OpenCloud/Personal", "~/OpenCloud", "~/OpenCloud/Personal/x"} {
		if try(configPatch{MountPoint: ptr(bad)}) == nil {
			t.Errorf("mountPoint %q accepted", bad)
		}
	}
	for _, ok := range []configPatch{{LocalRoot: ptr("~/Sync")}, {MountPoint: ptr("~/Drive")}, {LocalRoot: ptr(filepath.Join(home(), "S"))}} {
		if err := try(ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}

// 1.0.2: an empty settings change doesn't rewrite the file; Pause needs its value.
func TestEmptyPatchAndPauseBody(t *testing.T) {
	r := newUIRig(t)
	r.login()
	before, _ := os.ReadFile(configFile())
	st, _ := os.Stat(configFile())
	for _, body := range []string{`{}`, `null`, `{"localRoot":null,"keptFilters":null}`} {
		resp, b := r.post("/ui/api/config", body)
		if resp.StatusCode != 200 || !strings.Contains(b, `"saved":false`) {
			t.Errorf("%s: %d %s", body, resp.StatusCode, b)
		}
	}
	after, _ := os.ReadFile(configFile())
	st2, _ := os.Stat(configFile())
	if string(before) != string(after) || !st.ModTime().Equal(st2.ModTime()) {
		t.Error("an empty change rewrote config.json")
	}
	if resp, _ := r.post("/ui/api/pause", `{}`); resp.StatusCode != 400 {
		t.Errorf("pause without a value: %d", resp.StatusCode)
	}
	if resp, b := r.post("/ui/api/pause", `{"paused":false}`); resp.StatusCode != 200 {
		t.Errorf("pause false: %d %s", resp.StatusCode, b)
	}
}

func ptr[T any](v T) *T { return &v }
