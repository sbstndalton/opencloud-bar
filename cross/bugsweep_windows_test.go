package main

import (
	"os"
	"path/filepath"
	"testing"
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
