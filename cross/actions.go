package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Actions shared by the tray menu and the settings page. Nothing here needs the systray library.

var signingIn atomic.Bool

// setUpAndSignIn asks for the server address, signs in with the browser, then offers to keep a folder.
func setUpAndSignIn(e *Engine) {
	if !signingIn.CompareAndSwap(false, true) {
		return
	}
	defer signingIn.Store(false)
	e.mu.Lock()
	def := e.cfg.ServerURL
	e.mu.Unlock()
	addr, ok := dialogEntry("Set Up OpenCloud Bar", "Your OpenCloud server address, for example https://cloud.example.com", def)
	if !ok || addr == "" {
		return
	}
	e.setMessage("Waiting for you to sign in in the browser…")
	e.notify()
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout+time.Minute)
	defer cancel()
	user, err := signIn(ctx, addr, nil)
	e.setMessage("")
	if err != nil {
		e.notify()
		dialogAlert("Sign-in failed", err.Error())
		return
	}
	if err := e.Reload(); err != nil {
		dialogAlert("OpenCloud Bar", "Signed in as "+user+", but the new settings couldn't be applied yet: "+err.Error())
		return
	}
	if !dialogConfirm("Signed in as "+user, "OpenCloud Bar is connecting. Choose a folder from OpenCloud to keep on this computer now?", "Keep a Folder…") {
		return
	}
	// The folder picker browses the drive, which needs a moment to mount.
	for i := 0; i < 30 && !e.Status().DriveMounted && e.cfg.Mounting(); i++ {
		time.Sleep(time.Second)
	}
	pickAndKeep(e)
}

func pickAndKeep(e *Engine) {
	st := e.Status()
	target := e.mountTarget()
	if !st.DriveMounted {
		dialogAlert("Connect the OpenCloud drive first", "Folders are chosen from the drive. Choose Connect Drive, then try again.")
		return
	}
	picked, ok := dialogPickFolder(target, "Choose a folder in OpenCloud to keep on this computer")
	if !ok {
		return
	}
	root := filepath.Clean(target)
	if len(root) == 2 && root[1] == ':' {
		root += `\`
	}
	rel, err := filepath.Rel(root, filepath.Clean(picked))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		dialogAlert("That folder isn't in OpenCloud", "Choose a folder inside the OpenCloud drive.")
		return
	}
	alertErr(e.Keep(filepath.ToSlash(rel), ""))
}

func alertErr(err error) {
	if err != nil {
		dialogAlert("OpenCloud Bar", err.Error())
	}
}

// folderLabel is a folder's line in the tray's Kept Folders menu: why it needs attention, or
// when it last synced.
func folderLabel(f FolderStatus) string {
	return folderLabelAt(f, time.Now())
}

func folderLabelAt(f FolderStatus, now time.Time) string {
	switch f.State { // short: the full reason is the item's tooltip and the menu's first line
	case StNeedsRepair:
		return "⚠ " + f.Name + " — needs repair"
	case StBigDeletion:
		return "⚠ " + f.Name + " — large deletion blocked"
	case StFailed:
		if f.Detail == missingRemoteText {
			return "⚠ " + f.Name + " — missing or empty in OpenCloud"
		}
		return "⚠ " + f.Name + " — sync failed"
	}
	switch f.State {
	case StSyncing:
		return f.Name + " — syncing…"
	case StSkipped:
		s := f.Name + " — skipped"
		if f.Detail != "" {
			s += " (" + f.Detail + ")"
		}
		return s
	}
	if !f.LastSuccess.IsZero() {
		return f.Name + " — synced " + agoAt(f.LastSuccess, now)
	}
	return f.Name + " — waiting for first sync"
}

// confirmUploads asks before quitting or signing out while files may still be uploading.
// It fails closed: an unknown count asks too.
func confirmUploads(e *Engine, button string) bool {
	switch n := e.PendingUploads(); {
	case n > 0:
		return dialogConfirm("Files are still uploading",
			fmt.Sprintf("%d file(s) written to the OpenCloud drive haven't finished uploading. They upload the next time OpenCloud Bar runs.", n), button)
	case n < 0:
		return dialogConfirm("Uploads couldn't be checked",
			"OpenCloud Bar couldn't check whether files written to the OpenCloud drive have finished uploading. Anything not uploaded yet uploads the next time OpenCloud Bar runs.", button)
	}
	return true
}

// checkUpdatesNow is the menu's Check for Updates…: always says what it found.
func checkUpdatesNow(e *Engine) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u := e.CheckForUpdates(ctx)
	switch {
	case u.Error != "":
		dialogAlert("Couldn't check for updates", u.Error)
	case u.Available:
		if dialogConfirm("OpenCloud Bar "+u.Latest+" is available", "You have "+u.Running+". Open the release page to download it?", "Open Release Page") {
			openRelease(u)
		}
	case u.DevBuild:
		dialogAlert("Development build", "This is a development build ("+u.Running+"), so it isn't compared with releases. The latest release is "+u.Latest+".")
	default:
		dialogAlert("OpenCloud Bar is up to date", "You have "+u.Running+", the latest release.")
	}
}

func openRelease(u UpdateState) {
	link := u.URL
	if !strings.HasPrefix(link, releasesPagePrefix) {
		link = releasesPagePrefix + "latest"
	}
	if err := openURL(link); err != nil {
		dialogAlert("OpenCloud Bar", "Open this page in your browser: "+link)
	}
}
