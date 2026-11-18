package main

import (
	"sync"
	"time"
)

// Start at sign-in (Windows and Linux; the Mac app has its own). The state is the operating
// system's, read when asked and never stored in config.json: the installer (Windows), a
// package or the user (Linux) may change it without the app knowing.

// autostartInfo is what the settings page needs for its checkbox.
type autostartInfo struct {
	Supported bool   // false: the checkbox isn't shown
	Enabled   bool   // the app starts when the user signs in
	Note      string // non-empty: the setting can't be changed here, and this says why
}

var autostartCache struct {
	mu   sync.Mutex
	at   time.Time
	info autostartInfo
}

// autostartState is osAutostart, remembered for a few seconds: the settings page asks every 3
// seconds, and on Linux the answer comes from running systemctl.
func autostartState() autostartInfo {
	c := &autostartCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) > 10*time.Second {
		c.info, c.at = osAutostart(), time.Now()
	}
	return c.info
}

// setAutostart turns start at sign-in on or off and forgets the remembered state.
func setAutostart(on bool) error {
	autostartCache.mu.Lock()
	autostartCache.at = time.Time{}
	autostartCache.mu.Unlock()
	return osSetAutostart(on)
}
