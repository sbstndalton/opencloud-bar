package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSubfolderRuleEscaping(t *testing.T) {
	for _, name := range []string{"Photos", "a*b", "what?", "[draft]", "{x,y}", `back\slash`, "Q1 2026 (final)", "naïve"} {
		r := subfolderRule(name)
		if got, ok := parseSubfolderRule(r); !ok || got != name {
			t.Errorf("%q: rule %q reads back as %q %v", name, r, got, ok)
		}
	}
	if r := subfolderRule("a*b/[c]"); r != `- /a\*b/\[c\]/**` {
		t.Errorf("nested rule %q", r)
	}
	if got, ok := parseSubfolderRule(`- /Media/Raw\?/**`); !ok || got != "Media/Raw?" {
		t.Errorf("nested parse %q %v", got, ok)
	}
	// Not the checklist's form: hand-written globs, includes, deeper paths, unanchored.
	for _, l := range []string{"- /*.tmp/**", "+ /Photos/**", "- Photos/**", "- /Photos/*", "- /A/B/C/**",
		"- /**", `- /a\b/**`, "- /a[b]/**", "# - /Photos/**", "- / x/**", "- /x /**"} {
		if s, ok := parseSubfolderRule(l); ok {
			t.Errorf("%q managed as %q", l, s)
		}
	}
}

func TestSetSubfoldersKeepsHandRules(t *testing.T) {
	rules := []string{"+ /keep.txt", "- /Old/**", "- *.iso", `- /Arch\*/**`, "- /Media/**", "- /Media/Raw/**", "- /Gone/**"}
	got, err := setSubfolders(rules, map[string]bool{
		"Old": true, "New": false, "Arch*": true, "Media": true, "Music/Live": false, "Photos": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"+ /keep.txt", "- *.iso", "- /Media/Raw/**", "- /Gone/**", "- /Music/Live/**", "- /New/**"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	// Unchecking a folder drops its subfolders' rules (it excludes them already) and adds one.
	got, _ = setSubfolders(want, map[string]bool{"Music": false, "Music/Other": false})
	if want := []string{"+ /keep.txt", "- *.iso", "- /Media/Raw/**", "- /Gone/**", "- /New/**", "- /Music/**"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parent: %q", got)
	}
	// No change: the same rules, in the same order; nothing is added twice.
	if again, _ := setSubfolders(got, map[string]bool{"New": false}); !reflect.DeepEqual(again, got) {
		t.Errorf("not idempotent: %q", again)
	}
	for _, bad := range []string{"a/b/c", "", " x", "a\nb"} {
		if _, err := setSubfolders(nil, map[string]bool{bad: false}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Through the settings (as the page saves it): only that folder's excludes change, hand-written
// rules and unknown keys stay.
func TestSubfoldersSavedLikeExcludes(t *testing.T) {
	e := pausedRig(t, `,"auth":"oidc","kept":[{"path":"Docs","stateName":"d","futureFolderKey":"x","extraFilters":["- *.psd","- /Old/**"]},
	  {"path":"Docs","stateName":"m","space":{"id":"s1","name":"M","davPath":"/dav/spaces/s1"}}]`)
	if _, err := applyConfigPatch(configPatch{KeptSubfolders: map[string]map[string]bool{"Docs": {"Old": true, "Big [1]": false}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	c := e.config()
	if want := []string{"- *.psd", `- /Big \[1\]/**`}; !reflect.DeepEqual(c.Kept[0].ExtraFilters, want) || len(c.Kept[1].ExtraFilters) != 0 {
		t.Errorf("filters: %q / %q", c.Kept[0].ExtraFilters, c.Kept[1].ExtraFilters)
	}
	data, _ := os.ReadFile(configFile())
	var raw struct{ Kept []map[string]any }
	_ = json.Unmarshal(data, &raw)
	if raw.Kept[0]["futureFolderKey"] != "x" || !strings.Contains(string(data), `"futureKey": 7`) {
		t.Errorf("other keys lost: %s", data)
	}
	if _, err := applyConfigPatch(configPatch{KeptSubfolders: map[string]map[string]bool{"nope": {"A": false}}}); err == nil {
		t.Error("unknown folder accepted")
	}
}
