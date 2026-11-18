//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Windows: the per-user Run key, under the name the installer writes
// (packaging/windows/opencloud-bar.iss, [Registry]; it removes the value when uninstalling).
// The installer's value is the quoted path of "OpenCloud Bar.exe" and nothing else.
var (
	autostartRunKey  = `Software\Microsoft\Windows\CurrentVersion\Run`
	autostartValue   = "OpenCloud Bar"
	autostartExeFile = "OpenCloud Bar.exe" // the program without a console window
	autostartCLIFile = "opencloud-bar.exe"
)

func osAutostart() autostartInfo {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return autostartInfo{Supported: true}
	}
	defer k.Close()
	v, _, err := k.GetStringValue(autostartValue)
	return autostartInfo{Supported: true, Enabled: err == nil && strings.TrimSpace(v) != ""}
}

func osSetAutostart(on bool) error {
	if !on {
		k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer k.Close()
		if err := k.DeleteValue(autostartValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := autostartExe()
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(autostartValue, `"`+exe+`"`)
}

// autostartExe is this program, or when it is the command-line one, the windowed program
// next to it, which is what the installer starts.
func autostartExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if strings.EqualFold(filepath.Base(exe), autostartCLIFile) {
		gui := filepath.Join(filepath.Dir(exe), autostartExeFile)
		if _, err := os.Stat(gui); err == nil {
			return gui, nil
		}
	}
	return exe, nil
}
