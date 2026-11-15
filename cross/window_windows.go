//go:build windows

package main

import (
	"errors"
	"path/filepath"

	"github.com/jchv/go-webview2"
)

// runSettingsWindow shows url in a WebView2 window and returns when it is closed. It runs in its
// own process (see cmdSettingsWindow) so it doesn't share a message loop with the tray.
func runSettingsWindow(url string) error {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  filepath.Join(appDir(), "webview2"), // the default (next to the exe) may not be writable
		WindowOptions: webview2.WindowOptions{
			Title:  "OpenCloud Bar",
			Width:  900,
			Height: 640,
			Center: true,
		},
	})
	if w == nil {
		return errors.New("WebView2 couldn't start (is the Microsoft Edge WebView2 Runtime installed?)")
	}
	defer w.Destroy()
	w.Navigate(url)
	w.Run()
	return nil
}
