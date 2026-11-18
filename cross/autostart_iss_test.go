package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// issEntry is one line of the installer's [Registry] section: its parameters by name.
type issEntry map[string]string

// issSplit cuts a line at the semicolons outside double quotes ("" inside quotes is a quote).
func issSplit(line string) []string {
	var parts []string
	var cur strings.Builder
	inQ := false
	for _, r := range line {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case r == ';' && !inQ:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(parts, cur.String())
}

// issRunEntries returns the installer's [Registry] entries for HKCU's Run key.
func issRunEntries(t *testing.T) []issEntry {
	t.Helper()
	var out []issEntry
	for _, e := range issRegistryEntries(t) {
		if e["Root"] == "HKCU" && strings.EqualFold(e["Subkey"], autostartRunKey) {
			out = append(out, e)
		}
	}
	return out
}

// issRegistryEntries returns every entry of the installer's [Registry] section.
func issRegistryEntries(t *testing.T) []issEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "packaging", "windows", "opencloud-bar.iss"))
	if err != nil {
		t.Fatal(err)
	}
	var out []issEntry
	inRegistry := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inRegistry = strings.EqualFold(line, "[Registry]")
			continue
		}
		if !inRegistry || line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		e := issEntry{}
		for _, part := range issSplit(line) {
			k, v, ok := strings.Cut(part, ":")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
				v = strings.ReplaceAll(v[1:len(v)-1], `""`, `"`)
			}
			e[strings.TrimSpace(k)] = v
		}
		out = append(out, e)
	}
	return out
}

// installerRunValueName is the Run value's name the installer writes, read from the .iss file.
func installerRunValueName(t *testing.T) string {
	t.Helper()
	es := issRunEntries(t)
	if len(es) == 0 {
		t.Fatal("no HKCU Run entry in opencloud-bar.iss")
	}
	return es[0]["ValueName"]
}

// The installer and the app must name the same Run value, and uninstalling must always remove
// it: one entry writes it (first install, task ticked), another with no task logs its removal.
func TestInstallerRunEntriesMatchTheApp(t *testing.T) {
	es := issRunEntries(t)
	if len(es) != 2 {
		t.Fatalf("%d Run entries in the installer, want 2: %v", len(es), es)
	}
	var writer, remover issEntry
	for _, e := range es {
		if e["ValueName"] != autostartValue {
			t.Errorf("installer's value name %q, the app's is %q", e["ValueName"], autostartValue)
		}
		if !strings.Contains(e["Flags"], "uninsdeletevalue") {
			t.Errorf("entry without uninsdeletevalue: %v", e)
		}
		switch e["ValueType"] {
		case "string":
			writer = e
		case "none":
			remover = e
		}
	}
	if writer == nil || remover == nil {
		t.Fatalf("want one string and one none entry: %v", es)
	}
	if writer["Tasks"] != "startup" || writer["Check"] != "FirstInstall" {
		t.Errorf("the entry that writes the value is for the first install with the task ticked: %v", writer)
	}
	if want := `"{app}\` + autostartExeFile + `"`; writer["ValueData"] != want {
		t.Errorf("value data %q, want %q", writer["ValueData"], want)
	}
	if remover["Tasks"] != "" || remover["Check"] != "" {
		t.Errorf("the removal entry must have no task and no Check: %v", remover)
	}
}
