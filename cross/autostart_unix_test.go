//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stubSystemd(t *testing.T, enabled bool) {
	t.Helper()
	old := systemdEnabled
	systemdEnabled = func() bool { return enabled }
	t.Cleanup(func() { systemdEnabled = old })
}

func TestAutostartDesktopRoundTrip(t *testing.T) {
	testEnv(t)
	xdg := filepath.Join(t.TempDir(), "config home")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	stubSystemd(t, false)
	file := filepath.Join(xdg, "autostart", "opencloud-bar.desktop")

	if unixAutostart().Enabled {
		t.Fatal("enabled before anything was written")
	}
	if err := unixSetAutostart(true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Desktop Entry]", "Type=Application", "Name=OpenCloud Bar", " run\n", "X-GNOME-Autostart-enabled=true"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("desktop file lacks %q:\n%s", want, b)
		}
	}
	if st := unixAutostart(); !st.Supported || !st.Enabled || st.Note != "" {
		t.Errorf("after enabling: %+v", st)
	}
	if err := unixSetAutostart(true); err != nil { // again: still fine
		t.Fatal(err)
	}
	if err := unixSetAutostart(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("file still there: %v", err)
	}
	if unixAutostart().Enabled {
		t.Error("still enabled after removing")
	}
	if err := unixSetAutostart(false); err != nil { // already off: not an error
		t.Error(err)
	}
}

func TestAutostartDefaultsToHomeConfig(t *testing.T) {
	testEnv(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := autostartDir(), filepath.Join(home(), ".config", "autostart"); got != want {
		t.Errorf("%s, want %s", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative/path") // the spec: ignored unless absolute
	if got, want := autostartDir(), filepath.Join(home(), ".config", "autostart"); got != want {
		t.Errorf("%s, want %s", got, want)
	}
}

func TestAutostartRefusedWhenSystemdStartsTheApp(t *testing.T) {
	testEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stubSystemd(t, true)
	st := unixAutostart()
	if !st.Supported || !st.Enabled || st.Note != "Started by the systemd service" {
		t.Errorf("state: %+v", st)
	}
	if err := unixSetAutostart(true); err == nil {
		t.Error("enabling next to the systemd service wasn't refused")
	}
	if _, err := os.Stat(filepath.Join(autostartDir(), "opencloud-bar.desktop")); !os.IsNotExist(err) {
		t.Errorf("a desktop file was written: %v", err)
	}
}

func TestAutostartHiddenEntryIsOff(t *testing.T) {
	testEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stubSystemd(t, false)
	if err := os.MkdirAll(autostartDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(autostartDir(), "opencloud-bar.desktop")
	if err := os.WriteFile(f, []byte("[Desktop Entry]\nExec=x\nHidden=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if unixAutostart().Enabled {
		t.Error("a Hidden=true entry counts as on")
	}
}

func TestDesktopExecQuoting(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/opencloud-bar": "/usr/bin/opencloud-bar",
		"/opt/Open Cloud/bar":    `"/opt/Open Cloud/bar"`,
		`/opt/a"b$c`:             `"/opt/a\\"b\\$c"`,
		"/opt/100%/bar":          `"/opt/100%%/bar"`,
	} {
		if got := desktopExec(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// The settings page's route: the state reports the operating system's setting, saving it
// writes the autostart entry, and config.json never gets an openAtLogin key.
func TestAutostartThroughTheSettingsAPI(t *testing.T) {
	r := newUIRig(t)
	r.login()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := autostartOS
	autostartOS = "linux"
	t.Cleanup(func() {
		autostartOS = old
		autostartCache.mu.Lock()
		autostartCache.at = time.Time{}
		autostartCache.mu.Unlock()
	})
	autostartCache.mu.Lock()
	autostartCache.at = time.Time{}
	autostartCache.mu.Unlock()
	stubSystemd(t, false)
	file := filepath.Join(autostartDir(), "opencloud-bar.desktop")

	cfgOf := func() map[string]any {
		_, body := r.do("GET", "/ui/api/state", "", "", nil)
		var st struct{ Config map[string]any }
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatal(err)
		}
		return st.Config
	}
	if c := cfgOf(); c["openAtLogin"] != false || c["openAtLoginAvailable"] != true || c["openAtLoginNote"] != "" {
		t.Fatalf("state before: %v", c)
	}
	if resp, body := r.post("/ui/api/config", `{"openAtLogin":true}`); resp.StatusCode != 200 {
		t.Fatalf("enable: %d %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("no autostart entry: %v", err)
	}
	if c := cfgOf(); c["openAtLogin"] != true {
		t.Errorf("state after: %v", c)
	}
	if b, err := os.ReadFile(configFile()); err == nil && strings.Contains(string(b), "openAtLogin") {
		t.Errorf("openAtLogin was stored in config.json:\n%s", b)
	}
	if resp, body := r.post("/ui/api/config", `{"openAtLogin":false}`); resp.StatusCode != 200 {
		t.Fatalf("disable: %d %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("entry still there: %v", err)
	}
	// systemd starts the app: refused, and nothing is written.
	stubSystemd(t, true)
	autostartCache.mu.Lock()
	autostartCache.at = time.Time{}
	autostartCache.mu.Unlock()
	if c := cfgOf(); c["openAtLoginNote"] != "Started by the systemd service" {
		t.Errorf("state with systemd: %v", c)
	}
	if resp, _ := r.post("/ui/api/config", `{"openAtLogin":true}`); resp.StatusCode != 400 {
		t.Errorf("enable next to systemd: %d", resp.StatusCode)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("entry written next to systemd: %v", err)
	}
}
