package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"fyne.io/systray"
)

const folderSlots = 12

type slot struct {
	root, open, repair, allow, stop *systray.MenuItem
}

// runTray shows the tray icon and blocks until Quit. quit stops the engine cleanly.
func runTray(e *Engine, ctx context.Context, quit func()) {
	systray.Run(func() { trayReady(e, ctx, quit) }, func() {})
}

func trayReady(e *Engine, ctx context.Context, quit func()) {
	systray.SetIcon(trayIcon())
	systray.SetTooltip("OpenCloud Bar")

	summary := systray.AddMenuItem("Starting…", "")
	summary.Disable()
	message := systray.AddMenuItem("", "")
	message.Disable()
	message.Hide()
	setup := systray.AddMenuItem("Set Up / Sign In…", "")
	setup.Hide()
	systray.AddSeparator()
	openDrive := systray.AddMenuItem("Open OpenCloud Drive", "")
	toggleDrive := systray.AddMenuItem("Connect Drive", "")
	systray.AddSeparator()
	header := systray.AddMenuItem("Kept on this computer", "")
	header.Disable()
	slots := make([]slot, folderSlots)
	for i := range slots {
		r := systray.AddMenuItem("", "")
		slots[i] = slot{root: r,
			open:   r.AddSubMenuItem("Open Folder", ""),
			repair: r.AddSubMenuItem("Repair (Merge Both Sides)…", ""),
			allow:  r.AddSubMenuItem("Allow Large Deletion…", ""),
			stop:   r.AddSubMenuItem("Stop Keeping Here…", ""),
		}
		r.Hide()
	}
	keep := systray.AddMenuItem("Keep a Folder Here…", "")
	systray.AddSeparator()
	syncNow := systray.AddMenuItem("Sync Now", "")
	settings := systray.AddMenuItem("Edit Settings File", "")
	logs := systray.AddMenuItem("Show Logs", "")
	reload := systray.AddMenuItem("Reload Settings", "")
	signOut := systray.AddMenuItem("Sign Out", "")
	signOut.Hide()
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit OpenCloud Bar", "")

	var shown []FolderStatus
	refresh := func() {
		st := e.Status()
		summary.SetTitle(st.Summary)
		if st.Message != "" {
			message.SetTitle(st.Message)
			message.Show()
		} else {
			message.Hide()
		}
		systray.SetTooltip("OpenCloud Bar — " + st.Summary)
		configured := e.cfg.Configured()
		needSignIn := !configured || st.SignedOut
		show(setup, needSignIn)
		show(signOut, e.cfg.OIDC() && configured && !st.SignedOut)
		if !e.cfg.Mounting() {
			openDrive.Hide()
			toggleDrive.Hide()
		} else if st.DriveMounted {
			toggleDrive.SetTitle("Eject Drive")
			toggleDrive.Enable()
		} else if st.DriveWanted {
			toggleDrive.SetTitle("Connecting…")
			toggleDrive.Disable()
		} else {
			toggleDrive.SetTitle("Connect Drive")
			toggleDrive.Enable()
		}
		shown = st.Folders
		for i, s := range slots {
			if i >= len(st.Folders) {
				s.root.Hide()
				continue
			}
			f := st.Folders[i]
			s.root.SetTitle(folderLabel(f))
			s.root.Show()
			show(s.repair, f.State == StNeedsRepair)
			show(s.allow, f.State == StBigDeletion)
			enable(s.stop, !st.Syncing)
			enable(s.repair, !st.Syncing)
			enable(s.allow, !st.Syncing)
		}
		enable(keep, !needSignIn && !st.Syncing && len(st.Folders) < folderSlots)
		enable(syncNow, !st.Syncing)
	}
	refresh()
	// First launch (nothing set up yet): start the set-up right away.
	if e.Status().ConfigError == "" && !e.cfg.Configured() {
		go setUpAndSignIn(e)
	}

	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			case <-e.changed:
				refresh()
			}
		}
	}()

	folderAt := func(i int) (FolderStatus, bool) {
		if i < len(shown) {
			return shown[i], true
		}
		return FolderStatus{}, false
	}
	for i := range slots {
		i, s := i, slots[i]
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-s.open.ClickedCh:
					if f, ok := folderAt(i); ok {
						openPath(f.Local)
					}
				case <-s.repair.ClickedCh:
					if f, ok := folderAt(i); ok && dialogConfirm("Repair "+f.Name+"?",
						"This merges this computer's copy with OpenCloud. For each file the newer copy wins, and nothing is deleted. Files deleted since the last good sync will come back.", "Repair") {
						alertErr(e.Repair(f.Path))
					}
				case <-s.allow.ClickedCh:
					if f, ok := folderAt(i); ok && dialogConfirm("Allow a large deletion in "+f.Name+"?",
						"More than half of this folder's files were deleted on one side. If that was on purpose, the deletion will be applied to the other side too.", "Apply Deletion") {
						alertErr(e.AllowBigDeletion(f.Path))
					}
				case <-s.stop.ClickedCh:
					if f, ok := folderAt(i); ok {
						text := "Your changes are synced to OpenCloud first. Then the copy on this computer is moved to " + trashName() + ". The folder stays in OpenCloud."
						if n := e.UnsyncedCount(f.Path); n > 0 {
							text += fmt.Sprintf("\n\n%d file(s) in it are never synced (build folders, excluded items) and exist only here. They move with it.", n)
						}
						if dialogConfirm("Stop keeping "+f.Name+" here?", text, "Stop Keeping") {
							alertErr(e.Unkeep(f.Path, alertErr))
						}
					}
				}
			}
		}()
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				systray.Quit()
				return
			case <-openDrive.ClickedCh:
				if !e.Status().DriveMounted {
					e.Connect()
					time.Sleep(3 * time.Second)
				}
				openPath(e.mountTarget())
			case <-toggleDrive.ClickedCh:
				if e.Status().DriveMounted {
					alertErr(e.Eject())
				} else {
					e.Connect()
				}
			case <-setup.ClickedCh:
				go setUpAndSignIn(e) // minutes in the browser: don't block the menu
			case <-signOut.ClickedCh:
				if dialogConfirm("Sign out of OpenCloud?", "The drive is disconnected and syncing stops. Your kept folders stay on this computer. Sign in again to resume.", "Sign Out") {
					e.SignOut()
				}
			case <-keep.ClickedCh:
				pickAndKeep(e)
			case <-syncNow.ClickedCh:
				e.SyncNow()
			case <-settings.ClickedCh:
				openPath(configFile())
			case <-logs.ClickedCh:
				openPath(logDir())
			case <-reload.ClickedCh:
				alertErr(e.Reload())
			case <-quitItem.ClickedCh:
				if n := e.PendingUploads(); n > 0 && !dialogConfirm("Files are still uploading",
					fmt.Sprintf("%d file(s) written to the OpenCloud drive haven't finished uploading. They upload next time OpenCloud Bar runs.", n), "Quit Anyway") {
					continue
				}
				summary.SetTitle("Finishing sync…")
				quit()
				systray.Quit()
				return
			}
		}
	}()
}

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

func show(m *systray.MenuItem, on bool) {
	if on {
		m.Show()
	} else {
		m.Hide()
	}
}

func enable(m *systray.MenuItem, on bool) {
	if on {
		m.Enable()
	} else {
		m.Disable()
	}
}

func folderLabel(f FolderStatus) string {
	switch f.State {
	case StSyncing:
		return f.Name + " — syncing…"
	case StFailed:
		return f.Name + " — " + f.Detail
	case StNeedsRepair:
		return f.Name + " — needs repair (" + f.Detail + ")"
	case StBigDeletion:
		return f.Name + " — large deletion blocked"
	case StSkipped:
		return f.Name + " — skipped (" + f.Detail + ")"
	}
	if !f.LastSuccess.IsZero() {
		return f.Name + " — " + ago(f.LastSuccess)
	}
	return f.Name + " — waiting"
}

// trayIcon draws a small cloud; Windows wants ICO, others PNG.
func trayIcon() []byte {
	const n = 32
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	c := color.NRGBA{0x3b, 0x82, 0xf6, 0xff}
	circle := func(cx, cy, r float64) {
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				dx, dy := float64(x)-cx, float64(y)-cy
				if dx*dx+dy*dy <= r*r {
					img.Set(x, y, c)
				}
			}
		}
	}
	circle(11, 19, 7)
	circle(18, 14, 9)
	circle(24, 20, 6)
	for y := 19; y < 26; y++ {
		for x := 11; x < 25; x++ {
			img.Set(x, y, c)
		}
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	if runtime.GOOS != "windows" {
		return pngBuf.Bytes()
	}
	// ICO container holding one PNG image
	var ico bytes.Buffer
	_ = binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, 1})
	ico.Write([]byte{n, n, 0, 0})
	_ = binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})
	_ = binary.Write(&ico, binary.LittleEndian, []uint32{uint32(pngBuf.Len()), 22})
	ico.Write(pngBuf.Bytes())
	return ico.Bytes()
}
