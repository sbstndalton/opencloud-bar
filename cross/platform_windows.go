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

	"golang.org/x/sys/windows/registry"
)

var errNoOSUnmount = errors.New("no OS unmount")

func mountCommand() string { return "mount" } // needs WinFsp (https://winfsp.dev)

// mountProblem says why the drive can't start, or "". rclone's drive letter needs WinFsp; it
// is found the way rclone (cgofuse) finds it: InstallDir under HKLM\SOFTWARE\WinFsp in the 32-bit
// registry view (the 64-bit view too, in case), with the DLL present.
func mountProblem() string {
	for _, view := range []uint32{registry.WOW64_32KEY, registry.WOW64_64KEY} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WinFsp`, registry.QUERY_VALUE|view)
		if err != nil {
			continue
		}
		dir, _, err := k.GetStringValue("InstallDir")
		k.Close()
		if err == nil && exists(filepath.Join(dir, "bin", "winfsp-x64.dll")) {
			return ""
		}
	}
	return "The drive needs WinFsp, which isn't installed. Run the OpenCloud Bar installer again (it installs WinFsp), or get it from winfsp.dev. Kept folders still sync."
}

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
		_, err := os.Stat(t + `\`)
		return err == nil
	}
	// A folder mount point is a reparse point while WinFsp has it mounted; a plain folder
	// at that path is not a mount.
	p, err := syscall.UTF16PtrFromString(t)
	if err != nil {
		return false
	}
	a, err := syscall.GetFileAttributes(p)
	return err == nil && a&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// prepareMountPoint: WinFsp wants a folder mount point not to exist yet (rclone: "mountpoint
// path already exists"), only its parent. An empty folder left from earlier is removed.
func prepareMountPoint(target string) string {
	if len(target) == 2 && target[1] == ':' {
		return ""
	}
	_ = os.MkdirAll(filepath.Dir(target), 0o700)
	if fi, err := os.Lstat(target); err == nil {
		if !fi.IsDir() || os.Remove(target) != nil {
			return "The drive folder " + target + " must be empty or not exist yet — choose another in Settings ▸ Drive"
		}
	}
	return ""
}

func unmountOS(string) error { return errNoOSUnmount }
func forceUnmount(string)    {}

// ps runs a PowerShell script. All text the script shows comes in through environment
// variables (OCB_*), never pasted into the script, so quotes or $ in a folder name or a
// server's error message can't change what the script does.
func ps(script string, env map[string]string) *exec.Cmd {
	c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	c.Env = os.Environ()
	for k, v := range env {
		c.Env = append(c.Env, "OCB_"+k+"="+v)
	}
	hideWindow(c)
	return c
}

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
	_ = startReaped(exec.Command("explorer.exe", p))
}

func hasDisplay() bool { return true }

func dialogConfirm(title, text, button string) bool {
	script := "Add-Type -AssemblyName System.Windows.Forms; " +
		"$r = [System.Windows.Forms.MessageBox]::Show($env:OCB_TEXT, $env:OCB_TITLE, 'OKCancel', 'Warning'); " +
		"if ($r -eq 'OK') { exit 0 } else { exit 1 }"
	return ps(script, map[string]string{"TITLE": title, "TEXT": text + "\n\nChoose OK to " + strings.ToLower(button) + "."}).Run() == nil
}

func dialogAlert(title, text string) {
	script := "Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.MessageBox]::Show($env:OCB_TEXT, $env:OCB_TITLE) | Out-Null"
	_ = ps(script, map[string]string{"TITLE": title, "TEXT": text}).Run()
}

func dialogPickFolder(start, prompt string) (string, bool) {
	if len(start) == 2 && start[1] == ':' {
		start += `\`
	}
	script := "Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; " +
		"$d.Description = $env:OCB_PROMPT; $d.SelectedPath = $env:OCB_START; " +
		"if ($d.ShowDialog() -eq 'OK') { [Console]::OutputEncoding = [Text.Encoding]::UTF8; $d.SelectedPath } else { exit 1 }"
	out, err := ps(script, map[string]string{"PROMPT": prompt, "START": start}).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func hasDialogs() bool { return true }

// desktopNotify shows a toast. Toasts need an app ID that has a Start menu shortcut: the
// installer gives its shortcut OpenCloud Bar's ID; without it (a copy run from a folder)
// PowerShell's own ID is used, so the toast still shows.
func desktopNotify(title, body string) error {
	appID := windowsAppID
	if pd := os.Getenv("ProgramData"); pd == "" || !exists(filepath.Join(pd, `Microsoft\Windows\Start Menu\Programs\OpenCloud Bar\OpenCloud Bar.lnk`)) {
		appID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`
	}
	script := "$ErrorActionPreference = 'Stop'; " +
		"[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null; " +
		"$x = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02); " +
		"$t = $x.GetElementsByTagName('text'); " +
		"$t.Item(0).AppendChild($x.CreateTextNode($env:OCB_TITLE)) > $null; " +
		"$t.Item(1).AppendChild($x.CreateTextNode($env:OCB_TEXT)) > $null; " +
		"[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($env:OCB_APPID).Show([Windows.UI.Notifications.ToastNotification]::new($x))"
	return ps(script, map[string]string{"TITLE": title, "TEXT": body, "APPID": appID}).Run()
}

// windowsAppID is the AppUserModelID the installer puts on the Start menu shortcut.
const windowsAppID = "OpenCloudBar.OpenCloudBar"

// openURL opens a web address in the default browser. rundll32 passes the whole URL through
// untouched (explorer.exe mangles URLs containing & in some cases).
func openURL(u string) error {
	return startReaped(exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", u))
}

// dialogEntry asks for one line of text with a Visual Basic input box. The box returns an empty
// string when cancelled, so empty means cancelled.
func dialogEntry(title, prompt, def string) (string, bool) {
	script := "Add-Type -AssemblyName Microsoft.VisualBasic; [Console]::OutputEncoding = [Text.Encoding]::UTF8; " +
		"[Microsoft.VisualBasic.Interaction]::InputBox($env:OCB_PROMPT, $env:OCB_TITLE, $env:OCB_DEF)"
	out, err := ps(script, map[string]string{"TITLE": title, "PROMPT": prompt, "DEF": def}).Output()
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(out))
	return v, v != ""
}
