package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The command-line program (opencloud-bar.exe) is switched for the windowed one next to it
// (OpenCloud Bar.exe), when that exists: it is what the installer starts. Not platform-bound:
// the function takes the path.
func TestGuiExeNextTo(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "opencloud-bar.exe")
	gui := filepath.Join(dir, "OpenCloud Bar.exe")
	if got := guiExeNextTo(cli); got != cli { // no windowed program there
		t.Errorf("without the windowed program: %q", got)
	}
	if err := os.WriteFile(gui, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := guiExeNextTo(cli); got != gui {
		t.Errorf("with the windowed program: %q, want %q", got, gui)
	}
	upper := filepath.Join(dir, "OPENCLOUD-BAR.EXE") // Windows file names don't care about case
	if got := guiExeNextTo(upper); got != gui {
		t.Errorf("upper case: %q, want %q", got, gui)
	}
	if got := guiExeNextTo(gui); got != gui { // already the windowed one
		t.Errorf("windowed program: %q", got)
	}
	other := filepath.Join(dir, "something-else")
	if got := guiExeNextTo(other); got != other {
		t.Errorf("other program: %q", got)
	}
}
