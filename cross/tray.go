//go:build !notray

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"
)

// trayFolderSlots is how many folders the "Kept Folders" submenu lists. The settings page has no limit.
const trayFolderSlots = 10

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
	update := systray.AddMenuItem("Update Available…", "Open the release page")
	update.Hide()
	systray.AddSeparator()
	openDrive := systray.AddMenuItem("Open OpenCloud Drive", "")
	toggleDrive := systray.AddMenuItem("Connect Drive", "")
	systray.AddSeparator()
	kept := systray.AddMenuItem("Kept Folders (0)", "")
	slots := make([]*systray.MenuItem, trayFolderSlots)
	for i := range slots {
		slots[i] = kept.AddSubMenuItem("", "")
		slots[i].Hide()
	}
	manage := kept.AddSubMenuItem("Manage Folders…", "")
	keep := systray.AddMenuItem("Keep a Folder Here…", "")
	systray.AddSeparator()
	syncNow := systray.AddMenuItem("Sync Now", "")
	pause := systray.AddMenuItem("Pause Syncing", "Stop syncing kept folders; the drive keeps working")
	settings := systray.AddMenuItem("Settings…", "")
	checkUpdates := systray.AddMenuItem("Check for Updates…", "")
	about := systray.AddMenuItem("About OpenCloud Bar", "")
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit OpenCloud Bar", "")

	var shown []FolderStatus // what the slots currently stand for
	var shownMu sync.Mutex
	lockShown, unlockShown := shownMu.Lock, shownMu.Unlock

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
		configured := e.config().Configured()
		needSignIn := !configured || st.SignedOut
		show(setup, needSignIn)
		if st.Update.Available {
			update.SetTitle("Update Available: v" + st.Update.Latest + "…")
			update.Show()
		} else {
			update.Hide()
		}
		if st.Paused {
			pause.SetTitle("Resume Syncing")
		} else {
			pause.SetTitle("Pause Syncing")
		}
		enable(pause, configured && st.ConfigError == "")
		if !e.config().Mounting() {
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
		kept.SetTitle(fmt.Sprintf("Kept Folders (%d)", len(st.Folders)))
		order := attentionFirst(st.Folders)
		lockShown()
		shown = order
		unlockShown()
		for i, s := range slots {
			if i >= len(order) {
				s.Hide()
				continue
			}
			s.SetTitle(folderLabel(order[i]))
			if needsAttention(order[i].State) {
				s.SetTooltip(attentionReason(order[i]) + " — click to fix it in Settings")
			} else {
				s.SetTooltip("Open the folder")
			}
			s.Show()
		}
		if len(order) > len(slots) {
			manage.SetTitle(fmt.Sprintf("Manage All %d Folders…", len(order)))
		} else {
			manage.SetTitle("Manage Folders…")
		}
		enable(keep, !needSignIn && !st.Syncing)
		enable(syncNow, !st.Syncing && !st.Paused && !needSignIn)
	}
	refresh()
	// First launch (nothing set up yet): start the set-up right away.
	if e.Status().ConfigError == "" && !e.config().Configured() {
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

	for i := range slots {
		i, s := i, slots[i]
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-s.ClickedCh:
					lockShown()
					var f FolderStatus
					ok := i < len(shown)
					if ok {
						f = shown[i]
					}
					unlockShown()
					switch {
					case !ok:
					case needsAttention(f.State):
						openSettings("folders") // where Repair and the other fixes are
					default:
						openPath(f.Local)
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
			case <-keep.ClickedCh:
				pickAndKeep(e)
			case <-syncNow.ClickedCh:
				e.SyncNow()
			case <-pause.ClickedCh:
				alertErr(e.SetPaused(!e.Status().Paused))
			case <-update.ClickedCh:
				openRelease(e.Status().Update)
			case <-about.ClickedCh:
				openSettings("about")
			case <-checkUpdates.ClickedCh:
				go checkUpdatesNow(e) // a network call: don't block the menu
			case <-settings.ClickedCh:
				openSettings("")
			case <-manage.ClickedCh:
				openSettings("folders")
			case <-quitItem.ClickedCh:
				if !confirmUploads(e, "Quit Anyway") {
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
