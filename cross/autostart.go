package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Start at sign-in (Windows and Linux; the Mac app has its own). The state is the operating
// system's, read when asked and never stored in config.json: the installer (Windows), a
// package or the user (Linux) may change it without the app knowing.

// autostartInfo is what the settings page needs for its checkbox.
type autostartInfo struct {
	Supported bool   // false: the checkbox isn't shown
	Enabled   bool   // the app starts when the user signs in
	Note      string // shown next to the checkbox when non-empty
	Locked    bool   // the checkbox can't be changed here (Note says why)
}

// Windows: the per-user Run key and value, the names the installer writes
// (packaging/windows/opencloud-bar.iss, [Registry]; a test checks that they match).
// Declared here, not in autostart_windows.go, so that test runs on every system.
var (
	autostartRunKey  = `Software\Microsoft\Windows\CurrentVersion\Run`
	autostartValue   = "OpenCloud Bar"
	autostartExeFile = "OpenCloud Bar.exe" // the program without a console window
	autostartCLIFile = "opencloud-bar.exe"
)

// guiExeNextTo is exe, or when exe is the command-line program, the windowed program next to
// it if there is one, which is what the installer starts.
func guiExeNextTo(exe string) string {
	if strings.EqualFold(filepath.Base(exe), autostartCLIFile) {
		gui := filepath.Join(filepath.Dir(exe), autostartExeFile)
		if _, err := os.Stat(gui); err == nil {
			return gui
		}
	}
	return exe
}

var autostartCache struct {
	mu   sync.Mutex
	gen  int // bumped by every change, so a read that began before it can't store its answer
	at   time.Time
	info autostartInfo
}

// resetAutostartCache forgets the remembered state.
func resetAutostartCache() {
	c := &autostartCache
	c.mu.Lock()
	c.gen++
	c.at = time.Time{}
	c.mu.Unlock()
}

// autostartState is osAutostart, remembered for a few seconds: the settings page asks every 3
// seconds, and on Linux the answer comes from running systemctl. The lock isn't held while the
// system is asked, so a slow systemctl doesn't block other callers.
func autostartState() autostartInfo {
	c := &autostartCache
	c.mu.Lock()
	if !c.at.IsZero() && time.Since(c.at) <= 10*time.Second {
		info := c.info
		c.mu.Unlock()
		return info
	}
	gen := c.gen
	c.mu.Unlock()
	info := osAutostart()
	c.mu.Lock()
	if c.gen == gen { // not changed meanwhile: else the answer may be stale, and isn't kept
		c.info, c.at = info, time.Now()
	}
	c.mu.Unlock()
	return info
}

// setAutostart turns start at sign-in on or off. The remembered state is forgotten after the
// system was written (also when that failed, which may have changed it in part), so the next
// read sees the new state.
func setAutostart(on bool) error {
	err := osSetAutostart(on)
	resetAutostartCache()
	return err
}
