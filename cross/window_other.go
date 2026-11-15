//go:build !windows

package main

import "errors"

// Only Windows has an embedded web view (the other platforms use a browser app window).
func runSettingsWindow(string) error { return errors.New("no embedded window on this platform") }
