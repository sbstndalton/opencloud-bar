//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var errNoOSUnmount = errors.New("no OS unmount")

func mountCommand() string {
	if runtime.GOOS == "darwin" {
		return "nfsmount" // no FUSE needed on macOS
	}
	return "mount"
}

func mountExtraArgs() []string { return nil }

func hideWindow(*exec.Cmd) {}

func interrupt(c *exec.Cmd) {
	if c.Process != nil {
		_ = c.Process.Signal(os.Interrupt)
	}
}

// isMounted reports whether target is a mount point, without touching the (possibly dead) mount.
func isMounted(target string) bool {
	target = filepath.Clean(target)
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) > 4 && unescapeMount(f[4]) == target {
				return true
			}
		}
		return false
	}
	out, err := exec.Command("/sbin/mount").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), " on "+target+" (")
}

func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

func unmountOS(target string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("/usr/sbin/diskutil", "unmount", target)
	} else {
		cmd = exec.Command("fusermount3", "-u", target)
		if _, err := exec.LookPath("fusermount3"); err != nil {
			cmd = exec.Command("fusermount", "-u", target)
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func forceUnmount(target string) {
	if runtime.GOOS == "darwin" {
		if exec.Command("/sbin/umount", target).Run() != nil {
			_ = exec.Command("/usr/sbin/diskutil", "unmount", "force", target).Run()
		}
		return
	}
	if exec.Command("fusermount3", "-uz", target).Run() != nil {
		_ = exec.Command("fusermount", "-uz", target).Run()
	}
}

// moveToTrash follows the freedesktop.org trash spec on Linux and uses ~/.Trash on macOS.
// trashName says where moveToTrash puts things, for messages.
func trashName() string { return "the trash" }

func moveToTrash(path string) error {
	if runtime.GOOS == "darwin" {
		return os.Rename(path, uniquePath(filepath.Join(home(), ".Trash", filepath.Base(path))))
	}
	if _, err := exec.LookPath("gio"); err == nil {
		if exec.Command("gio", "trash", path).Run() == nil {
			return nil
		}
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home(), ".local", "share")
	}
	files, info := filepath.Join(dataHome, "Trash", "files"), filepath.Join(dataHome, "Trash", "info")
	if err := os.MkdirAll(files, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(info, 0o700); err != nil {
		return err
	}
	dest := uniquePath(filepath.Join(files, filepath.Base(path)))
	abs, _ := filepath.Abs(path)
	ti := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n", abs, time.Now().Format("2006-01-02T15:04:05"))
	if err := os.WriteFile(filepath.Join(info, filepath.Base(dest)+".trashinfo"), []byte(ti), 0o600); err != nil {
		return err
	}
	return os.Rename(path, dest)
}

func uniquePath(p string) string {
	if !exists(p) {
		return p
	}
	for i := 2; ; i++ {
		q := fmt.Sprintf("%s %d", p, i)
		if !exists(q) {
			return q
		}
	}
}

func openPath(p string) {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", p).Start()
		return
	}
	_ = exec.Command("xdg-open", p).Start()
}

func hasDisplay() bool {
	return runtime.GOOS == "darwin" || os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// Dialogs use zenity on Linux (if installed) and osascript on macOS.
func dialogConfirm(title, text, button string) bool {
	if runtime.GOOS == "darwin" {
		script := fmt.Sprintf(`display dialog %q with title %q buttons {"Cancel", %q} default button "Cancel"`, text, title, button)
		return exec.Command("osascript", "-e", script).Run() == nil
	}
	if _, err := exec.LookPath("zenity"); err != nil {
		return false
	}
	return exec.Command("zenity", "--question", "--title", title, "--text", text, "--ok-label", button, "--cancel-label", "Cancel", "--width", "420").Run() == nil
}

func dialogAlert(title, text string) {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("osascript", "-e", fmt.Sprintf(`display alert %q message %q`, title, text)).Run()
		return
	}
	if _, err := exec.LookPath("zenity"); err == nil {
		_ = exec.Command("zenity", "--info", "--title", title, "--text", text, "--width", "420").Run()
		return
	}
	// No zenity: a notification at least, and the log, so the message isn't lost.
	appLog("message: %s: %s", title, text)
	_ = desktopNotify(title, text)
}

func dialogPickFolder(start, prompt string) (string, bool) {
	var out []byte
	var err error
	if runtime.GOOS == "darwin" {
		out, err = exec.Command("osascript", "-e", fmt.Sprintf(`POSIX path of (choose folder with prompt %q default location POSIX file %q)`, prompt, start)).Output()
	} else {
		if _, lerr := exec.LookPath("zenity"); lerr != nil {
			return "", false
		}
		out, err = exec.Command("zenity", "--file-selection", "--directory", "--title", prompt, "--filename", start+"/").Output()
	}
	if err != nil {
		return "", false
	}
	return strings.TrimRight(strings.TrimSpace(string(out)), "/"), true
}

// openURL opens a web address in the default browser.
func openURL(u string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", u).Start()
	}
	return exec.Command("xdg-open", u).Start()
}

// dialogEntry asks for one line of text. ok is false if cancelled or if no dialog tool is available.
func dialogEntry(title, prompt, def string) (string, bool) {
	if runtime.GOOS == "darwin" {
		script := fmt.Sprintf(`text returned of (display dialog %q with title %q default answer %q buttons {"Cancel", "OK"} default button "OK")`, prompt, title, def)
		out, err := exec.Command("osascript", "-e", script).Output()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(out)), true
	}
	if _, err := exec.LookPath("zenity"); err != nil {
		return "", false
	}
	out, err := exec.Command("zenity", "--entry", "--title", title, "--text", prompt, "--entry-text", def, "--width", "420").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// hasDialogs reports whether dialogs can be shown (Linux needs zenity).
func hasDialogs() bool {
	if runtime.GOOS == "darwin" {
		return true
	}
	_, err := exec.LookPath("zenity")
	return err == nil
}

// desktopNotify shows a notification: notify-send on Linux (part of libnotify; skipped if it
// isn't installed), osascript on macOS (development runs only; the Mac app is the Swift one).
func desktopNotify(title, body string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("osascript", "-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run", title, body).Run()
	}
	p, err := exec.LookPath("notify-send")
	if err != nil {
		return fmt.Errorf("notify-send isn't installed")
	}
	return exec.Command(p, "--app-name=OpenCloud Bar", "--icon=opencloud-bar", "--", title, body).Run()
}
