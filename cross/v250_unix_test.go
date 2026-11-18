//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder's transfers show while its bisync runs (read from the progress blocks, with the
// whole file name asked for) and are gone when the run ends.
func TestTransfersLiveOnlyWhileTheRunGoes(t *testing.T) {
	e, fr, f := syncRig(t, false)
	dir := filepath.Dir(fr.path())
	script := `#!/bin/sh
d="` + dir + `"
echo "$*" >> "$d/calls"
case "$1" in
bisync)
  printf '2026/11/18 02:23:32 NOTICE: \nTransferred:            0 / 1, 0%%\nTransferring:\n * Docs/report, final: v2.pdf: 34%% / 2.861 MiB, 1019.990 KiB/s, 1s\n\n'
  touch "$d/printed"
  while [ -f "$d/hold" ]; do sleep 0.05; done ;;
esac
exit 0
`
	if err := os.WriteFile(fr.path(), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "hold"), nil, 0o600)
	done := make(chan bool)
	go func() { done <- e.syncFolder(f, modeNormal) }()
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.transfers[f.Key()]) == 1
	})
	page := e.transfersForPage()
	list, _ := page["folders"].([]folderTransfer)
	if len(list) != 1 || list[0].Name != "Docs/report, final: v2.pdf" || list[0].Percent != 34 || list[0].FolderName != "Docs" || list[0].Folder != f.Key() {
		t.Errorf("while syncing: %+v", page["folders"])
	}
	_ = os.Remove(filepath.Join(dir, "hold"))
	<-done
	e.mu.Lock()
	left := len(e.transfers)
	e.mu.Unlock()
	if left != 0 {
		t.Errorf("transfers left after the run: %v", e.transfers)
	}
	if !strings.Contains(fr.calls(), "--stats-file-name-length 0") || !strings.Contains(fr.calls(), "--stats 2s") || strings.Contains(fr.calls(), "--stats-one-line") {
		t.Errorf("bisync stats arguments: %s", fr.calls())
	}
}
