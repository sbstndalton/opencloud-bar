//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

func moveToTrash(path string) error {
	abs, _ := filepath.Abs(path)
	script := "Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteDirectory(" +
		psQuote(abs) + ", 'OnlyErrorDialogs', 'SendToRecycleBin')"
	return ps(script).Run()
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
