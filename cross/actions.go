package main

import (
	"context"
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
