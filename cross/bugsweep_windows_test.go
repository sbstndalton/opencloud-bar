package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// WinFsp refuses a folder mount point that already exists: a plain folder isn't "mounted",
// and an empty one is cleared so rclone can create it.
func TestWindowsFolderMountPoint(t *testing.T) {
	target := filepath.Join(t.TempDir(), "parent", "OpenCloud")
	if p := prepareMountPoint(target); p != "" {
		t.Fatalf("missing folder: %s", p)
	}
	if _, err := os.Stat(filepath.Dir(target)); err != nil {
		t.Fatal("parent not created")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("mount point itself must not be created")
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if isMounted(target) {
		t.Fatal("a plain folder isn't a mount")
	}
	if p := prepareMountPoint(target); p != "" || exists(target) {
		t.Fatalf("empty folder should be removed: %q", p)
	}
	_ = os.Mkdir(target, 0o700)
	_ = os.WriteFile(filepath.Join(target, "x"), nil, 0o600)
	if p := prepareMountPoint(target); p == "" {
		t.Fatal("non-empty folder must be refused")
	}
}

// Live check on a PC with WinFsp and rclone (OCB_LIVE_MOUNT=1): a real folder mount is seen
// as mounted, and goes away when rclone stops.
func TestWindowsFolderMountLive(t *testing.T) {
	if os.Getenv("OCB_LIVE_MOUNT") != "1" {
		t.Skip("set OCB_LIVE_MOUNT=1 on a PC with WinFsp and rclone")
	}
	target := filepath.Join(t.TempDir(), "drive")
	_ = os.Mkdir(target, 0o700) // what older versions left behind
	if p := prepareMountPoint(target); p != "" {
		t.Fatal(p)
	}
	c := exec.Command("rclone", "mount", ":memory:", target)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	for i := 0; i < 40 && !isMounted(target); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if !isMounted(target) {
		t.Fatal("rclone folder mount not detected")
	}
	_ = c.Process.Kill()
	_ = c.Wait()
	for i := 0; i < 40 && isMounted(target); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if isMounted(target) {
		t.Fatal("still mounted after rclone stopped")
	}
}
