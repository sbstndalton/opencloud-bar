//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Linux: an autostart entry in $XDG_CONFIG_HOME/autostart (default ~/.config/autostart). The
// Mac app isn't this one: on other systems the toggle isn't offered.

const (
	autostartDesktopName = "opencloud-bar.desktop"
	systemdNote          = "Started by the systemd service"
)

// systemdEnabled reports whether the user's systemd service opencloud-bar (packaging/linux,
// the headless mode) starts at login. A variable so the tests can answer for it.
var systemdEnabled = func() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "--user", "is-enabled", "opencloud-bar").Output()
	s := strings.TrimSpace(string(out))
	return s == "enabled" || s == "enabled-runtime"
}

func autostartDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "autostart")
	}
	return filepath.Join(home(), ".config", "autostart")
}

// autostartOS is the system the autostart entry is for (a variable for the tests).
var autostartOS = runtime.GOOS

func osAutostart() autostartInfo {
	if autostartOS != "linux" {
		return autostartInfo{}
	}
	return unixAutostart()
}

func osSetAutostart(on bool) error {
	if autostartOS != "linux" {
		return errors.New("start at sign-in isn't available on this system")
	}
	return unixSetAutostart(on)
}

func unixAutostart() autostartInfo {
	if systemdEnabled() {
		// The service starts the app at login. A second start from the autostart entry would
		// run the app twice, so the entry is never written. One that is already there (from
		// before the service was enabled) can still be removed: the box stays usable then.
		_, err := os.Stat(filepath.Join(autostartDir(), autostartDesktopName))
		return autostartInfo{Supported: true, Enabled: true, Note: systemdNote, Locked: err != nil}
	}
	b, err := os.ReadFile(filepath.Join(autostartDir(), autostartDesktopName))
	if err != nil {
		return autostartInfo{Supported: true}
	}
	hidden := false
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "Hidden=true" || l == "X-GNOME-Autostart-enabled=false" {
			hidden = true
		}
	}
	return autostartInfo{Supported: true, Enabled: !hidden}
}

func unixSetAutostart(on bool) error {
	if on && systemdEnabled() {
		return errors.New(systemdNote + "; turn that off first (systemctl --user disable opencloud-bar)")
	}
	path := filepath.Join(autostartDir(), autostartDesktopName)
	if !on {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Not resolved through symlinks (where the system gives the link's path): a ~/bin link to
	// a versioned folder keeps working after an upgrade.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(autostartDesktop(exe)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// autostartDesktop is packaging/linux/opencloud-bar.desktop with the full path of this
// program (a tarball install isn't on the PATH) and the autostart key.
func autostartDesktop(exe string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=OpenCloud Bar\n" +
		"Comment=OpenCloud drive and synced folders\n" +
		"Exec=" + desktopExec(exe) + " run\n" +
		"Icon=opencloud-bar\n" +
		"Terminal=false\n" +
		"Categories=Network;FileTransfer;\n" +
		"StartupNotify=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
}

// desktopExec quotes a program path for an Exec line (Desktop Entry Specification: a path with
// spaces or other reserved characters goes in double quotes; \ " ` $ get a backslash, which the
// key's own string escaping doubles; % is %%).
func desktopExec(p string) string {
	if !strings.ContainsAny(p, " \t\n\"'\\><~|&;$*?#()`%") {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range p {
		switch r {
		case '\\':
			b.WriteString(`\\\\`)
		case '"', '`', '$':
			b.WriteString(`\\` + string(r))
		case '%':
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
