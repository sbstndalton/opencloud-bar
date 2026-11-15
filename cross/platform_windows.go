//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var errNoOSUnmount = errors.New("no OS unmount")

func mountCommand() string { return "mount" } // needs WinFsp (https://winfsp.dev)

// Show the drive in Explorer like a network drive.
func mountExtraArgs() []string { return []string{"--network-mode"} }

const createNoWindow = 0x08000000

func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// Windows has no SIGINT for other processes; bisync's --recover handles an interrupted run.
func interrupt(c *exec.Cmd) {
	if c.Process != nil {
		_ = c.Process.Kill()
	}
}

func isMounted(target string) bool {
	t := target
	if len(t) == 2 && t[1] == ':' {
		t += `\`
	}
	_, err := os.Stat(t)
	return err == nil
}

func unmountOS(string) error { return errNoOSUnmount }
func forceUnmount(string)    {}

func ps(script string, args ...string) *exec.Cmd {
	c := exec.Command("powershell.exe", append([]string{"-NoProfile", "-NonInteractive", "-Command", script}, args...)...)
	hideWindow(c)
	return c
}

// psQuote makes a PowerShell single-quoted string literal.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// moveToTrash moves a folder that is no longer kept into "Removed by OpenCloud Bar" in the
// user's profile. The Recycle Bin isn't used: outside a desktop session Windows can delete
// permanently without reporting it, and a plain rename on the same drive can't lose data.
// trashName says where moveToTrash puts things, for messages.
func trashName() string { return `the "Removed by OpenCloud Bar" folder in your user folder` }

func moveToTrash(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, "Removed by OpenCloud Bar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := filepath.Base(abs) + " (removed " + time.Now().Format("2006-01-02 1504") + ")"
	dest := filepath.Join(dir, base)
	for i := 2; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(dir, fmt.Sprintf("%s %d", base, i))
	}
	if err := os.Rename(abs, dest); err != nil {
		return fmt.Errorf("couldn't move %s to %s: %w", abs, dir, err)
	}
	return nil
}

func openPath(p string) {
	if len(p) == 2 && p[1] == ':' {
		p += `\`
	}
	_ = exec.Command("explorer.exe", p).Start()
}

func hasDisplay() bool { return true }

func dialogConfirm(title, text, button string) bool {
	script := "Add-Type -AssemblyName System.Windows.Forms; " +
		"$r = [System.Windows.Forms.MessageBox]::Show(" + psQuote(text+"\n\nChoose OK to "+strings.ToLower(button)+".") + ", " + psQuote(title) + ", 'OKCancel', 'Warning'); " +
		"if ($r -eq 'OK') { exit 0 } else { exit 1 }"
	return ps(script).Run() == nil
}

func dialogAlert(title, text string) {
	script := "Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.MessageBox]::Show(" + psQuote(text) + ", " + psQuote(title) + ") | Out-Null"
	_ = ps(script).Run()
}

func dialogPickFolder(start, prompt string) (string, bool) {
	if len(start) == 2 && start[1] == ':' {
		start += `\`
	}
	script := "Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; " +
		"$d.Description = " + psQuote(prompt) + "; $d.SelectedPath = " + psQuote(start) + "; " +
		"if ($d.ShowDialog() -eq 'OK') { $d.SelectedPath } else { exit 1 }"
	out, err := ps(script).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// openURL opens a web address in the default browser. rundll32 passes the whole URL through
// untouched (explorer.exe mangles URLs containing & in some cases).
func openURL(u string) error {
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", u).Start()
}

// dialogEntry asks for one line of text with a Visual Basic input box. The box returns an empty
// string when cancelled, so empty means cancelled.
func dialogEntry(title, prompt, def string) (string, bool) {
	script := "Add-Type -AssemblyName Microsoft.VisualBasic; " +
		"[Microsoft.VisualBasic.Interaction]::InputBox(" + psQuote(prompt) + ", " + psQuote(title) + ", " + psQuote(def) + ")"
	out, err := ps(script).Output()
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(out))
	return v, v != ""
}
