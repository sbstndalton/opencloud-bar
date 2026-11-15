package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// configPatch is the set of settings the page may change. Anything else in the body is refused
// (the server address, davPath and auth belong to sign-in). A nil field is left as it is.
type configPatch struct {
	SyncIntervalSeconds   *int                `json:"syncIntervalSeconds"`
	CommonFilters         *[]string           `json:"commonFilters"`
	LocalRoot             *string             `json:"localRoot"`
	MountPoint            *string             `json:"mountPoint"`
	MountEnabled          *bool               `json:"mountEnabled"`
	VolumeName            *string             `json:"volumeName"`
	CacheMaxSize          *string             `json:"cacheMaxSize"`
	CacheMaxAge           *string             `json:"cacheMaxAge"`
	MinServerAvailableMiB *int                `json:"minServerAvailableMiB"`
	Transfers             *int                `json:"transfers"`
	Checkers              *int                `json:"checkers"`
	UseTunnel             *bool               `json:"useTunnel"`
	SSHHost               *string             `json:"sshHost"`
	ServerPort            *int                `json:"serverPort"`
	LocalPort             *int                `json:"localPort"`
	RclonePath            *string             `json:"rclonePath"`
	CheckForUpdates       *bool               `json:"checkForUpdates"`
	Notifications         *bool               `json:"notifications"`
	KeptFilters           map[string][]string `json:"keptFilters"` // kept folder path -> its own excludes
}

var (
	cacheSizeRe = regexp.MustCompile(`^[1-9][0-9]{0,5}[KMGT]?$`)
	cacheAgeRe  = regexp.MustCompile(`^[1-9][0-9]{0,4}(s|m|h|d|w)$`)
	driveRe     = regexp.MustCompile(`^[A-Za-z]:$`)
)

func cleanText(name, v string, max int) error {
	if len(v) > max {
		return fmt.Errorf("%s is too long", name)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s can't contain control characters", name)
		}
	}
	return nil
}

// checkFilters validates rclone filter lines and returns them cleaned.
func checkFilters(name string, in []string) ([]string, error) {
	out := []string{}
	if len(in) > 1000 {
		return nil, fmt.Errorf("%s has too many lines", name)
	}
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if err := cleanText(name, l, 300); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "+ ") {
			return nil, fmt.Errorf("%s: %q must start with \"- \" (exclude) or \"+ \" (include)", name, l)
		}
		out = append(out, l)
	}
	return out, nil
}

// validatePatch checks the values being changed, beyond what Config.Validate covers.
func (p configPatch) validate(next Config, current Config) error {
	in := func(name string, v *int, lo, hi int) error {
		if v != nil && (*v < lo || *v > hi) {
			return fmt.Errorf("%s must be between %d and %d", name, lo, hi)
		}
		return nil
	}
	for _, err := range []error{
		in("Sync interval", p.SyncIntervalSeconds, 60, 7*24*3600),
		in("Transfers", p.Transfers, 1, 32),
		in("Checkers", p.Checkers, 1, 64),
		in("Memory limit", p.MinServerAvailableMiB, 0, 1<<22),
		in("Server port", p.ServerPort, 1, 65535),
		in("Local port", p.LocalPort, 1024, 65000), // the control port is this +101
	} {
		if err != nil {
			return err
		}
	}
	str := func(name string, v *string, max int) error {
		if v == nil {
			return nil
		}
		return cleanText(name, *v, max)
	}
	for _, err := range []error{str("Mount location", p.MountPoint, 300), str("Local folder", p.LocalRoot, 300),
		str("Volume name", p.VolumeName, 60), str("SSH host", p.SSHHost, 200), str("rclone path", p.RclonePath, 400)} {
		if err != nil {
			return err
		}
	}
	if p.SSHHost != nil {
		h := *p.SSHHost
		if strings.HasPrefix(h, "-") || strings.ContainsAny(h, " \t'\"") {
			return errors.New("SSH host can't start with - or contain spaces or quotes")
		}
	}
	if p.MountPoint != nil && strings.TrimSpace(*p.MountPoint) == "" {
		return errors.New("Mount location can't be empty")
	}
	if p.MountPoint != nil && strings.HasSuffix(*p.MountPoint, ":") && !driveRe.MatchString(*p.MountPoint) {
		return errors.New("a drive letter looks like O:")
	}
	if p.VolumeName != nil && strings.TrimSpace(*p.VolumeName) == "" {
		return errors.New("Volume name can't be empty")
	}
	if p.CacheMaxSize != nil && !cacheSizeRe.MatchString(*p.CacheMaxSize) {
		return errors.New("Cache size looks like 20G")
	}
	if p.CacheMaxAge != nil && !cacheAgeRe.MatchString(*p.CacheMaxAge) {
		return errors.New("Cache age looks like 72h")
	}
	if p.LocalRoot != nil {
		if err := folderSettingProblem("Local folder", *p.LocalRoot); err != nil {
			return err
		}
	}
	if p.MountPoint != nil && !driveRe.MatchString(*p.MountPoint) {
		if err := folderSettingProblem("Mount location", *p.MountPoint); err != nil {
			return err
		}
		if samePath(filepath.Clean(expand(*p.MountPoint)), filepath.Clean(home())) {
			return errors.New("Mount location can't be your home folder")
		}
	}
	if (p.LocalRoot != nil || p.MountPoint != nil) && !driveRe.MatchString(next.MountPoint) {
		// The drive shows the server's files; the local folder holds the synced copies. One
		// inside the other would sync the drive into itself.
		m, l := filepath.Clean(expand(next.MountPoint)), filepath.Clean(expand(next.LocalRoot))
		if samePath(m, l) || within(m, l) || within(l, m) {
			return errors.New("Mount location and Local folder can't be the same, or one inside the other")
		}
	}
	if p.LocalRoot != nil && len(current.Kept) > 0 && *p.LocalRoot != current.LocalRoot {
		return errors.New("the local folder can't change while folders are kept; stop keeping them first")
	}
	if p.RclonePath != nil && *p.RclonePath != "" {
		path := expand(*p.RclonePath)
		if st, err := os.Stat(path); err != nil || st.IsDir() {
			if _, err := exec.LookPath(path); err != nil {
				return errors.New("rclone isn't at that path")
			}
		}
	}
	if next.LocalPort+101 > 65535 {
		return errors.New("Local port is too high")
	}
	return nil
}

// applyConfigPatch edits config.json as generic JSON (so keys this version doesn't know are
// kept), validates the result with Config.Validate, and writes it. It doesn't touch the running
// app; the caller calls Engine.Reload.
// isEmpty: nothing to change ({}, null, or only nulls).
func (p configPatch) isEmpty() bool { return reflect.DeepEqual(p, configPatch{}) }

func applyConfigPatch(p configPatch) (restartPort bool, err error) {
	configMu.Lock()
	defer configMu.Unlock()
	raw, err := readConfigRaw()
	if err != nil {
		return false, err
	}
	current := defaultConfig()
	if err := json.Unmarshal(mustJSON(raw), &current); err != nil {
		return false, err
	}
	set := func(k string, v any) { raw[k] = mustJSON(v) }
	if p.SyncIntervalSeconds != nil {
		set("syncIntervalSeconds", *p.SyncIntervalSeconds)
	}
	if p.CommonFilters != nil {
		f, err := checkFilters("Excludes", *p.CommonFilters)
		if err != nil {
			return false, err
		}
		set("commonFilters", f)
	}
	if p.LocalRoot != nil {
		set("localRoot", *p.LocalRoot)
	}
	if p.MountPoint != nil {
		set("mountPoint", *p.MountPoint)
	}
	if p.MountEnabled != nil {
		set("mountEnabled", *p.MountEnabled)
	}
	if p.VolumeName != nil {
		set("volumeName", *p.VolumeName)
	}
	if p.CacheMaxSize != nil {
		set("cacheMaxSize", *p.CacheMaxSize)
	}
	if p.CacheMaxAge != nil {
		set("cacheMaxAge", *p.CacheMaxAge)
	}
	if p.MinServerAvailableMiB != nil {
		set("minServerAvailableMiB", *p.MinServerAvailableMiB)
	}
	if p.Transfers != nil {
		set("transfers", *p.Transfers)
	}
	if p.Checkers != nil {
		set("checkers", *p.Checkers)
	}
	if p.UseTunnel != nil {
		set("useTunnel", *p.UseTunnel)
	}
	if p.SSHHost != nil {
		set("sshHost", strings.TrimSpace(*p.SSHHost))
	}
	if p.ServerPort != nil {
		set("serverPort", *p.ServerPort)
	}
	if p.LocalPort != nil {
		set("localPort", *p.LocalPort)
	}
	if p.RclonePath != nil {
		set("rclonePath", strings.TrimSpace(*p.RclonePath))
	}
	if p.CheckForUpdates != nil {
		set("checkForUpdates", *p.CheckForUpdates)
	}
	if p.Notifications != nil {
		set("notifications", *p.Notifications)
	}
	if p.KeptFilters != nil {
		kept, err := rawKept(raw) // per-folder unknown keys survive too
		if err != nil {
			return false, err
		}
		for path, lines := range p.KeptFilters {
			f, err := checkFilters("Excludes", lines)
			if err != nil {
				return false, err
			}
			found := false
			for _, k := range kept {
				var kp string
				_ = json.Unmarshal(k["path"], &kp)
				if kp != path {
					continue
				}
				found = true
				if len(f) == 0 {
					delete(k, "extraFilters")
				} else {
					k["extraFilters"] = mustJSON(f)
				}
			}
			if !found {
				return false, fmt.Errorf("%q isn't a kept folder", path)
			}
		}
		set("kept", kept)
	}
	next := defaultConfig()
	if err := json.Unmarshal(mustJSON(raw), &next); err != nil {
		return false, fmt.Errorf("those settings don't fit the settings file: %w", err)
	}
	if err := p.validate(next, current); err != nil {
		return false, err
	}
	if err := next.Validate(); err != nil {
		return false, err
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeConfigBytes(out); err != nil {
		return false, err
	}
	return next.LocalPort != current.LocalPort, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

var reloadRetrying atomic.Bool

// serveConfigPost saves edited settings and applies them with Engine.Reload.
func (e *Engine) serveConfigPost(w http.ResponseWriter, r *http.Request) {
	var p configPatch
	if !readJSON(w, r, &p) {
		return
	}
	if p.isEmpty() {
		writeJSON(w, 200, map[string]any{"saved": false}) // nothing changed: the file stays as it is
		return
	}
	restart, err := applyConfigPatch(p)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err)
		return
	}
	resp := map[string]any{"saved": true, "restartRequired": restart}
	if err := e.Reload(); err != nil {
		if !e.Status().Syncing {
			resp["error"] = "Saved, but couldn't apply: " + err.Error()
			writeJSON(w, http.StatusConflict, resp)
			return
		}
		// Reload waits for the running sync. Saved already; apply as soon as it finishes.
		resp["pending"] = true
		e.reloadWhenIdle()
	}
	writeJSON(w, 200, resp)
}

func (e *Engine) reloadWhenIdle() {
	if !reloadRetrying.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer reloadRetrying.Store(false)
		for i := 0; i < 2000; i++ { // about 3 hours, the longest a sync may run
			time.Sleep(5 * time.Second)
			e.mu.Lock()
			q := e.quitting
			e.mu.Unlock()
			if q {
				return
			}
			if err := e.Reload(); err == nil {
				e.logLine("settings applied after the sync finished")
				e.notify()
				return
			} else if !e.Status().Syncing {
				e.logLine("couldn't apply saved settings: %v", err)
				return
			}
		}
	}()
}
