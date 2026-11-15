package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// The settings page opens in its own window, not a browser tab:
//   - Windows: a child process of this program (`settings-window URL`) shows a WebView2 window.
//   - Linux: a Chromium-family browser in --app mode with its own profile folder.
// If neither is possible, the default browser is used. Only one settings window is kept open;
// choosing Settings… again while it is open does nothing.

var (
	winMu   sync.Mutex
	winProc *exec.Cmd
)

// browserCandidates are tried in this order for the Linux app window.
var browserCandidates = []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "brave-browser", "microsoft-edge"}

// appWindowArgs are the arguments that make a Chromium-family browser open url as a plain app window.
func appWindowArgs(url, profileDir string) []string {
	return []string{"--app=" + url, "--class=opencloud-bar", "--window-size=900,640",
		"--user-data-dir=" + profileDir, "--no-first-run", "--no-default-browser-check"}
}

// settingsWindowOpen reports whether our settings window is still running.
func settingsWindowOpen() bool {
	winMu.Lock()
	defer winMu.Unlock()
	return winProc != nil
}

func track(cmd *exec.Cmd) {
	winMu.Lock()
	winProc = cmd
	winMu.Unlock()
	go func() {
		_ = cmd.Wait()
		winMu.Lock()
		if winProc == cmd {
			winProc = nil
		}
		winMu.Unlock()
	}()
}

// launchSettingsWindow starts the window for the one-time login url. It reports what it did.
func launchSettingsWindow(url string) (how string, err error) {
	switch runtime.GOOS {
	case "windows":
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		cmd := exec.Command(exe, "settings-window", url)
		hideWindow(cmd)
		if err := cmd.Start(); err != nil {
			return "", err
		}
		track(cmd)
		return "window", nil
	case "linux":
		profile := filepath.Join(appDir(), "settings-browser")
		_ = os.MkdirAll(profile, 0o700)
		for _, name := range browserCandidates {
			path, err := exec.LookPath(name)
			if err != nil {
				continue
			}
			cmd := exec.Command(path, appWindowArgs(url, profile)...)
			if err := cmd.Start(); err != nil {
				continue
			}
			track(cmd)
			return name, nil
		}
	}
	return "", fmt.Errorf("no app window available")
}

// openSettings opens (or leaves open) the settings window. section is "" or a page section.
func openSettings(section string) {
	if settingsUI.getPort() == 0 {
		dialogAlert("OpenCloud Bar", "The settings page isn't available yet.")
		return
	}
	if settingsWindowOpen() {
		settingsUI.requestSection(section) // the open window switches to it
		return
	}
	u := settingsUI.loginURL(section)
	how, err := launchSettingsWindow(u)
	if err == nil {
		appLog("settings opened (%s)", how)
		return
	}
	appLog("settings window not available (%v); using the default browser", err)
	if err := openURL(u); err != nil {
		dialogAlert("OpenCloud Bar", "Couldn't open the settings: "+err.Error())
	}
}

// cmdSettingsWindow is the hidden `settings-window URL` subcommand: a window for the settings
// page in this process. It only accepts the local address.
func cmdSettingsWindow(args []string) {
	if len(args) != 1 || !strings.HasPrefix(args[0], "http://127.0.0.1:") || !strings.Contains(args[0], "/ui/login?") {
		os.Exit(2)
	}
	if err := runSettingsWindow(args[0]); err != nil {
		appLog("settings window failed: %v; using the default browser", err)
		_ = openURL(args[0])
	}
}
