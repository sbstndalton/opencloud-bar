package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Folder states, mirroring the macOS app.
const (
	StIdle        = "idle"
	StSyncing     = "syncing"
	StOK          = "ok"
	StSkipped     = "skipped"
	StFailed      = "failed"
	StNeedsRepair = "needs-repair"   // user must choose repair (a merge of both sides)
	StBigDeletion = "large-deletion" // bisync refused: more than half the files would be deleted
)

type FolderStatus struct {
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Detail      string    `json:"detail,omitempty"`
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	Local       string    `json:"local"`
}

type Status struct {
	Summary      string         `json:"summary"`
	Message      string         `json:"message,omitempty"`
	TunnelUp     bool           `json:"tunnelUp"`
	DriveMounted bool           `json:"driveMounted"`
	DriveWanted  bool           `json:"driveWanted"`
	Syncing      bool           `json:"syncing"`
	UseTunnel    bool           `json:"useTunnel"`
	SignedOut    bool           `json:"signedOut,omitempty"` // browser sign-in is missing or was revoked
	Folders      []FolderStatus `json:"folders"`
	ConfigError  string         `json:"configError,omitempty"`
	Paused       bool           `json:"paused,omitempty"` // kept folders don't sync until resumed
	Update       UpdateState    `json:"update"`
}

// Engine runs the SSH tunnel, the on-demand drive (rclone mount) and the two-way sync
// (rclone bisync). One mutex guards all state; only one bisync runs at a time.
type Engine struct {
	mu       sync.Mutex
	cfg      Config
	cfgErr   error
	status   map[string]*FolderStatus
	message  string
	syncing  bool
	quitting bool

	signedOut   bool      // OIDC only: no usable sign-in; nothing runs until the user signs in
	authRetryAt time.Time // after a transient token-refresh failure, don't retry every tick

	tunnel        *exec.Cmd
	tunnelDone    chan struct{}
	tunnelRetryAt time.Time

	mount          *exec.Cmd
	mountWanted    bool
	mountStarted   time.Time
	mountRetryAt   time.Time
	mountBackoff   time.Duration
	unmountRequest bool

	currentSync *exec.Cmd
	nextSyncAt  time.Time
	changed     chan struct{} // pinged whenever status changes (for the tray)

	// The drive's remote-control port only answers with these (random each start).
	rcUser, rcPass string

	updates *updater

	notes      notifyState // what has been announced (notify.go)
	autoMerged []string    // folders merged automatically since the last notification check

	// What the running sync is doing, for "Syncing “X” (2 of 3) · 12 files…".
	activeName           string
	activeIndex, activeN int
	activeFiles          int
}

const syncTimeout = 3 * time.Hour

func NewEngine() *Engine {
	cfg, err := loadConfig()
	e := &Engine{cfg: cfg, cfgErr: err, status: map[string]*FolderStatus{},
		mountWanted: cfg.Mounting(), mountBackoff: 5 * time.Second, changed: make(chan struct{}, 1),
		rcUser: "ocb-" + randomToken()[:12], rcPass: randomToken(), updates: newUpdater()}
	e.signedOut = cfg.OIDC() && cfg.Configured() && !haveCredentials(cfg)
	e.refreshStatusLocked()
	return e
}

const signedOutMessage = "Signed out: choose Set Up / Sign In…"

// markSignedOut stops all activity until the user signs in again.
func (e *Engine) markSignedOut() {
	e.mu.Lock()
	e.signedOut = true
	e.message = signedOutMessage
	e.mu.Unlock()
	e.logLine("signed out (sign-in missing or revoked)")
	e.notify()
}

// authReady is true when work may proceed. In OIDC mode it also keeps the cached access token
// fresh (rclone only asks for a new one after a 401), and notices a revoked sign-in.
func (e *Engine) authReady() bool {
	e.mu.Lock()
	cfg := e.cfg
	skip := e.quitting || e.cfgErr != nil || !cfg.OIDC() || !cfg.Configured()
	out, retry := e.signedOut, e.authRetryAt
	e.mu.Unlock()
	if skip {
		return true
	}
	if out {
		return false
	}
	if time.Now().Before(retry) {
		return true
	}
	if _, err := accessToken(cfg.ServerURL); errors.Is(err, errSignedOut) {
		e.markSignedOut()
		return false
	} else if err != nil {
		e.logLine("token refresh failed: %v", err)
		e.mu.Lock()
		e.authRetryAt = time.Now().Add(30 * time.Second)
		e.mu.Unlock()
	}
	return true
}

// errUploadsUnknown: Quit and Sign Out ask first when the upload count can't be read (fail closed).
var errUploadsUnknown = errors.New("couldn't check whether files written to the drive have finished uploading")

// uploadsBlock returns an error when files may still be uploading: a count, or "unknown".
func (e *Engine) uploadsBlock() error {
	switch n := e.PendingUploads(); {
	case n < 0:
		return errUploadsUnknown
	case n > 0:
		return fmt.Errorf("%d file(s) written to the drive haven't finished uploading", n)
	}
	return nil
}

// SignOut forgets the sign-in, stops the drive and waits for the next sign-in. Unless force is
// set it refuses while files may still be uploading (the caller asks the user, then forces).
func (e *Engine) SignOut(force bool) error {
	if !force {
		if err := e.uploadsBlock(); err != nil {
			return err
		}
	}
	logout()
	e.mu.Lock()
	e.signedOut = true
	e.message = signedOutMessage
	m, t := e.mount, e.tunnel
	if m != nil {
		e.unmountRequest = true
	}
	e.mu.Unlock()
	if m != nil {
		_ = e.unmount(m)
	}
	if t != nil && t.Process != nil {
		_ = t.Process.Kill()
	}
	e.notify()
	return nil
}

func (e *Engine) notify() {
	select {
	case e.changed <- struct{}{}:
	default:
	}
}

func (e *Engine) refreshStatusLocked() {
	keep := map[string]bool{}
	var saved map[string]time.Time
	for _, f := range e.cfg.Kept {
		keep[f.Path] = true
		if e.status[f.Path] == nil {
			s := &FolderStatus{Path: f.Path, Name: f.DisplayName(), State: StIdle, Local: e.cfg.Local(f)}
			if saved == nil {
				saved = e.loadLastSuccess()
			}
			s.LastSuccess = saved[f.StateName]
			if r := readReason(e.cfg.statePath(f, ".needs-repair")); r != "" {
				s.State, s.Detail = StNeedsRepair, r
			}
			e.status[f.Path] = s
		}
	}
	for p := range e.status {
		if !keep[p] {
			delete(e.status, p)
		}
	}
}

// Run supervises everything until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	e.tick()
	for {
		select {
		case <-ctx.Done():
			e.Shutdown()
			return
		case <-t.C:
			e.tick()
		}
	}
}

func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (e *Engine) tunnelOurs() bool {
	return e.tunnel != nil && e.tunnel.ProcessState == nil
}

func (e *Engine) tick() {
	e.checkNotifications()
	if !e.authReady() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.quitting && e.cfgErr == nil && e.cfg.UpdateChecks() && e.updates.due() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if st := e.updates.Check(ctx); st.Available {
				e.logLine("update available: %s", st.Latest)
			}
			e.notify()
		}()
	}
	if e.quitting || e.cfgErr != nil || !e.cfg.Configured() {
		return
	}
	up := true // without a tunnel the server is reached directly
	if e.cfg.Tunnel() {
		open := portOpen(e.cfg.LocalPort)
		if !e.tunnelOurs() && time.Now().After(e.tunnelRetryAt) {
			if open {
				e.message = fmt.Sprintf("Port %d is in use by another program; waiting for it to close.", e.cfg.LocalPort)
				e.tunnelRetryAt = time.Now().Add(30 * time.Second)
			} else {
				e.startTunnelLocked()
			}
		}
		up = e.tunnelOurs() && open
	}
	if e.cfg.Mounting() && e.mountWanted && up && e.mount == nil && time.Now().After(e.mountRetryAt) {
		e.startMountLocked()
	}
	if !e.syncing && !e.cfg.Paused && up && len(e.cfg.Kept) > 0 && time.Now().After(e.nextSyncAt) {
		e.startSyncAllLocked(false)
	}
}

func (e *Engine) logFile(name string) *os.File { return openLogFile(name) }

// appLog writes to app.log from code that has no Engine (sign-in, token command).
func appLog(format string, a ...any) {
	if f := openLogFile("app.log"); f != nil {
		fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
		f.Close()
	}
}

func openLogFile(name string) *os.File {
	_ = os.MkdirAll(logDir(), 0o700)
	p := filepath.Join(logDir(), name)
	if st, err := os.Stat(p); err == nil && st.Size() > 5_000_000 {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	return f
}

func (e *Engine) logLine(format string, a ...any) {
	if f := e.logFile("app.log"); f != nil {
		fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
		f.Close()
	}
}

// envFor is the environment for rclone with these settings.
func envFor(cfg Config) []string {
	env := os.Environ()
	if cfg.RcloneConfig != "" {
		env = append(env, "RCLONE_CONFIG="+expand(cfg.RcloneConfig))
	}
	if cfg.OIDC() {
		// rclone runs this at start and again after a 401; see accessToken in auth.go.
		env = append(env, "RCLONE_WEBDAV_BEARER_TOKEN_COMMAND="+tokenCommand())
	}
	return env
}

// MARK: tunnel

func (e *Engine) startTunnelLocked() {
	args := []string{"-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		"-N", "-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", e.cfg.LocalPort, e.cfg.ServerPort), e.cfg.SSHHost}
	cmd := exec.Command(e.cfg.SSH(), args...)
	hideWindow(cmd)
	if f := e.logFile("tunnel.log"); f != nil {
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		e.message = "Could not start ssh: " + err.Error()
		e.tunnelRetryAt = time.Now().Add(30 * time.Second)
		return
	}
	e.tunnel = cmd
	done := make(chan struct{})
	e.tunnelDone = done
	e.logLine("tunnel started")
	go func() {
		err := cmd.Wait()
		close(done)
		e.mu.Lock()
		if e.tunnel == cmd {
			e.tunnel = nil
		}
		e.tunnelRetryAt = time.Now().Add(15 * time.Second)
		q := e.quitting
		e.mu.Unlock()
		if !q {
			e.logLine("tunnel exited (%v), retrying in 15 s", err)
		}
		e.notify()
	}()
}

func (e *Engine) waitForTunnel() bool {
	cfg := e.config()
	if !cfg.Tunnel() {
		return true
	}
	for i := 0; i < 20; i++ {
		e.mu.Lock()
		ok := e.tunnelOurs()
		e.mu.Unlock()
		if ok && portOpen(cfg.LocalPort) {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

// MARK: drive

// mountTarget is where the drive appears. mountTargetLocked is the same for callers holding e.mu.
func (e *Engine) mountTarget() string       { return expand(e.config().MountPoint) }
func (e *Engine) mountTargetLocked() string { return expand(e.cfg.MountPoint) }

// isDriveLetter is true for a Windows drive letter mount point like "O:".
func isDriveLetter(t string) bool { return driveRe.MatchString(t) }

func driveLetterInUse(t string) string {
	return "Drive letter " + strings.ToUpper(t) + " is already in use — choose another in Settings ▸ Drive"
}

func (e *Engine) startMountLocked() {
	if p := mountProblem(); p != "" {
		if e.message != p {
			e.logLine("drive: %s", p)
		}
		e.message = p
		e.mountRetryAt = time.Now().Add(time.Minute)
		return
	}
	target := e.mountTargetLocked()
	if isMounted(target) {
		if isDriveLetter(target) {
			// Our drive isn't running, so something else has this letter (a USB stick, a
			// network drive). Windows has no way to take it over: say so instead of retrying silently.
			e.message = driveLetterInUse(target)
			e.mountRetryAt = time.Now().Add(30 * time.Second)
			return
		}
		go forceUnmount(target)
		e.mountRetryAt = time.Now().Add(10 * time.Second)
		return
	}
	if strings.HasPrefix(e.message, "Drive letter ") || strings.HasPrefix(e.message, "The drive needs WinFsp") {
		e.message = ""
	}
	if runtime.GOOS != "windows" || len(target) > 2 {
		_ = os.MkdirAll(target, 0o700)
	}
	cache := filepath.Join(appDir(), "vfs-cache")
	_ = os.MkdirAll(cache, 0o700)
	args := []string{mountCommand(), e.cfg.RemoteSpec(), target,
		"--volname", e.cfg.VolumeName,
		"--vfs-cache-mode", "full", "--cache-dir", cache,
		"--vfs-cache-max-size", e.cfg.CacheMaxSize, "--vfs-cache-max-age", e.cfg.CacheMaxAge,
		"--dir-cache-time", "30s", "--vfs-read-chunk-size", "16M", "--buffer-size", "16M",
		"--transfers", "4", "--checkers", "8", "--low-level-retries", "10",
		"--exclude", ".DS_Store", "--exclude", "._*",
		"--rc", "--rc-addr", fmt.Sprintf("127.0.0.1:%d", e.cfg.RcPort()),
		"--log-level", "NOTICE"}
	args = append(args, mountExtraArgs()...)
	cmd := exec.Command(e.cfg.Rclone(), args...)
	// The rc login goes in the environment, not the command line, so other users can't read it.
	cmd.Env = append(envFor(e.cfg), "RCLONE_RC_USER="+e.rcUser, "RCLONE_RC_PASS="+e.rcPass)
	hideWindow(cmd)
	if f := e.logFile("drive.log"); f != nil {
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		e.message = "Could not start rclone: " + err.Error()
		e.mountWanted = false
		return
	}
	e.mount, e.mountStarted, e.unmountRequest = cmd, time.Now(), false
	e.logLine("drive mount started at %s", target)
	go func() {
		err := cmd.Wait()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		defer e.notify()
		e.mount = nil
		still := isMounted(target)
		if e.quitting {
			return
		}
		if e.unmountRequest {
			e.logLine("drive ejected from the menu")
			return
		}
		if !still && code == 0 {
			e.mountWanted = false
			e.mountBackoff = 5 * time.Second
			e.logLine("drive unmounted outside the app; stays off until Connect")
			return
		}
		if time.Since(e.mountStarted) > 5*time.Minute {
			e.mountBackoff = 5 * time.Second
		}
		e.logLine("drive mount exited (%v); retrying in %s", err, e.mountBackoff)
		e.mountRetryAt = time.Now().Add(e.mountBackoff)
		if e.mountBackoff < 5*time.Minute {
			e.mountBackoff *= 2
		}
		if still {
			go forceUnmount(target)
		}
	}()
}

// unmount stops the drive: the OS unmount where there is one, else rclone's own quit,
// then waits for rclone to exit. Fails (and leaves rclone running) if the drive is busy.
func (e *Engine) unmount(m *exec.Cmd) error {
	err := unmountOS(e.mountTarget())
	if err == errNoOSUnmount {
		_, err = e.rc("core/quit", 15*time.Second)
	}
	if err != nil {
		return err
	}
	for i := 0; i < 150; i++ {
		e.mu.Lock()
		gone := e.mount != m
		e.mu.Unlock()
		if gone {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("rclone did not exit after unmount")
}

// PendingUploads returns files written to the drive that aren't uploaded yet (-1 if unknown).
func (e *Engine) PendingUploads() int {
	e.mu.Lock()
	running := e.mount != nil
	e.mu.Unlock()
	if !running {
		return 0
	}
	out, err := e.rc("vfs/stats", 10*time.Second)
	if err != nil {
		return -1
	}
	var v struct {
		DiskCache *struct {
			UploadsInProgress int `json:"uploadsInProgress"`
			UploadsQueued     int `json:"uploadsQueued"`
		} `json:"diskCache"`
	}
	if json.Unmarshal(out, &v) != nil || v.DiskCache == nil {
		return -1
	}
	return v.DiskCache.UploadsInProgress + v.DiskCache.UploadsQueued
}

// rc calls the drive's rclone remote control (127.0.0.1 only, with this run's login).
func (e *Engine) rc(method string, timeout time.Duration) ([]byte, error) {
	e.mu.Lock()
	port, user, pass := e.cfg.RcPort(), e.rcUser, e.rcPass
	e.mu.Unlock()
	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/%s", port, method), strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, pass)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rclone rc %s: %s", method, resp.Status)
	}
	return body, nil
}

func (e *Engine) Connect() {
	e.mu.Lock()
	e.mountWanted, e.mountRetryAt = true, time.Time{}
	e.mu.Unlock()
	e.tick()
}

// Eject unmounts the drive. It refuses while uploads are pending or files are open.
func (e *Engine) Eject() error {
	if n := e.PendingUploads(); n > 0 {
		return fmt.Errorf("%d file(s) are still uploading; try again in a moment", n)
	}
	e.mu.Lock()
	m := e.mount
	e.unmountRequest = true
	e.mu.Unlock()
	if m == nil {
		return nil
	}
	if err := e.unmount(m); err != nil {
		e.mu.Lock()
		e.unmountRequest = false
		e.mu.Unlock()
		return fmt.Errorf("the drive is in use; close files using it and try again")
	}
	e.mu.Lock()
	e.mountWanted = false
	e.mu.Unlock()
	return nil
}

// MARK: sync

func (c Config) statePath(f KeptFolder, suffix string) string {
	return filepath.Join(expand(c.StateDir), f.StateName+suffix)
}
func (c Config) workPath(f KeptFolder) string {
	return filepath.Join(expand(c.StateDir), "work", f.StateName)
}
func (c Config) filtersPath(f KeptFolder) string {
	return filepath.Join(expand(c.StateDir), "filters-"+f.StateName+".txt")
}

// stateFile, workDir and filtersFile read the settings themselves: don't call them holding e.mu.
func (e *Engine) stateFile(f KeptFolder, suffix string) string {
	return e.config().statePath(f, suffix)
}
func (e *Engine) workDir(f KeptFolder) string     { return e.config().workPath(f) }
func (e *Engine) filtersFile(f KeptFolder) string { return e.config().filtersPath(f) }

func (e *Engine) needsRepairReason(f KeptFolder) string {
	return readReason(e.stateFile(f, ".needs-repair"))
}

func readReason(file string) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (e *Engine) setState(f KeptFolder, state, detail string) {
	e.mu.Lock()
	ok := false
	if s := e.status[f.Path]; s != nil {
		s.State, s.Detail = state, detail
		if state == StOK {
			s.LastSuccess = time.Now()
			ok = true
		}
	}
	e.mu.Unlock()
	if ok {
		e.saveLastSuccess()
	}
	e.notify()
}

// lastSuccessFile: the caller holds e.mu.
func (e *Engine) lastSuccessFile() string {
	return filepath.Join(expand(e.cfg.StateDir), "last-success.json")
}

// loadLastSuccess reads "last synced" times (stateName -> time), so they survive restarts.
// Same file and format as the macOS app.
func (e *Engine) loadLastSuccess() map[string]time.Time {
	out := map[string]time.Time{}
	b, err := os.ReadFile(e.lastSuccessFile())
	if err != nil {
		return out
	}
	var raw map[string]string
	if json.Unmarshal(b, &raw) != nil {
		return out
	}
	for k, v := range raw {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out[k] = t
		}
	}
	return out
}

func (e *Engine) saveLastSuccess() {
	e.mu.Lock()
	raw := map[string]string{}
	for _, f := range e.cfg.Kept {
		if s := e.status[f.Path]; s != nil && !s.LastSuccess.IsZero() {
			raw[f.StateName] = s.LastSuccess.UTC().Format(time.RFC3339)
		}
	}
	file := e.lastSuccessFile()
	e.mu.Unlock()
	if b, err := json.MarshalIndent(raw, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(file), 0o700)
		_ = os.WriteFile(file, b, 0o600)
	}
}

func (e *Engine) markNeedsRepair(f KeptFolder, reason string) {
	_ = os.WriteFile(e.stateFile(f, ".needs-repair"), []byte(reason), 0o600)
	e.setState(f, StNeedsRepair, reason)
	e.logLine("%s: needs repair: %s", f.Path, reason)
}

// parkDrift marks a folder whose listings keep drifting. It still waits for Repair, but each
// sync interval tries a plain sync (no merge); once that succeeds the folder is fine again.
func (e *Engine) parkDrift(f KeptFolder, reason string) {
	_ = os.WriteFile(e.stateFile(f, ".drift-parked"), nil, 0o600)
	e.markNeedsRepair(f, reason)
}

// SyncNow syncs every kept folder now (not while paused). It reads each folder's repair state
// from disk, so a folder repaired meanwhile isn't skipped because of what the app remembered,
// and it tries a blocked large deletion again (it is blocked again if nothing changed).
func (e *Engine) SyncNow() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.syncing && e.cfgErr == nil && e.cfg.Configured() && !e.signedOut && !e.cfg.Paused {
		e.startSyncAllLocked(true)
	}
}

func (e *Engine) startSyncAllLocked(manual bool) {
	iv := time.Duration(max(60, e.cfg.SyncIntervalSeconds)) * time.Second
	e.nextSyncAt = time.Now().Add(iv)
	e.syncing = true
	folders := append([]KeptFolder(nil), e.cfg.Kept...)
	go func() {
		defer func() { e.mu.Lock(); e.syncing = false; e.mu.Unlock(); e.notify() }()
		if reason := e.serverTooBusy(); reason != "" {
			for _, f := range folders {
				e.setState(f, StSkipped, reason)
			}
			return
		}
		defer func() { e.mu.Lock(); e.activeName, e.activeIndex, e.activeN = "", 0, 0; e.mu.Unlock() }()
		for i, f := range folders {
			e.mu.Lock()
			e.activeIndex, e.activeN = i+1, len(folders)
			q := e.quitting
			s := e.status[f.Path]
			state := ""
			if s != nil {
				state = s.State
			}
			e.mu.Unlock()
			if q || s == nil {
				continue
			}
			if r := e.needsRepairReason(f); r != "" {
				if exists(e.stateFile(f, ".drift-parked")) {
					e.syncFolder(f, modeDriftRetry)
				} else {
					e.setState(f, StNeedsRepair, r)
				}
				continue
			}
			if state == StBigDeletion && !manual {
				continue
			}
			e.syncFolder(f, modeNormal)
		}
	}()
}

func (e *Engine) serverTooBusy() string {
	cfg := e.config()
	// The memory check runs over ssh, so it only applies with the tunnel.
	if !cfg.Tunnel() || cfg.MinServerAvailableMiB <= 0 {
		return ""
	}
	out, err := e.run(20*time.Second, nil, cfg.SSH(), "-n", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		cfg.SSHHost, "grep MemAvailable /proc/meminfo")
	// Look for "MemAvailable:   12345678 kB"; ssh may add warning lines (stdout and stderr are combined).
	// No quotes or $ in the remote command: Windows ssh mangles them.
	var mib int
	var perr error = fmt.Errorf("no MemAvailable line")
	for _, line := range strings.Split(out, "\n") {
		if fs := strings.Fields(line); len(fs) >= 2 && fs[0] == "MemAvailable:" {
			var kb int
			kb, perr = strconv.Atoi(fs[1])
			mib = kb / 1024
			break
		}
	}
	// The value is what matters: ssh on Windows can linger after printing it.
	if perr == nil {
		err = nil
	}
	if err != nil || perr != nil {
		e.logLine("server check failed: %v %v: %s", err, perr, strings.TrimSpace(out))
		e.setMessage("Server unreachable; sync skipped")
		return "server unreachable"
	}
	if mib < cfg.MinServerAvailableMiB {
		e.setMessage(fmt.Sprintf("Server low on memory (%d MiB); sync skipped", mib))
		return "server busy"
	}
	e.setMessage("")
	return ""
}

func (e *Engine) setMessage(m string) {
	e.mu.Lock()
	e.message = m
	e.mu.Unlock()
}

type syncMode int

const (
	modeNormal syncMode = iota
	modeRepair
	modeAllowBigDeletion
	modeDriftRetry // a folder parked after drift: a plain sync, no merge
)

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// folderRun is one kept folder's paths for a sync.
type folderRun struct {
	cfg                              Config // the settings when the sync started (a copy)
	f                                KeptFolder
	local, work, filters, serverFile string
	marker                           string
}

// syncFolder runs one bisync. It never creates, recreates or resyncs a folder unless this is
// its first sync, the user chose repair, or the listings drifted apart (one automatic merge
// per folder per 24 hours; after that the user decides).
func (e *Engine) syncFolder(f KeptFolder, mode syncMode) bool {
	e.mu.Lock()
	cfg := e.cfg // this sync's settings: a Reload or settings change meanwhile doesn't touch them
	e.activeName, e.activeFiles = f.DisplayName(), 0
	single := e.activeN == 0 // Repair, Stop Keeping…: one folder on its own
	if single {
		e.activeIndex, e.activeN = 1, 1
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.activeName, e.activeFiles = "", 0
		if single {
			e.activeIndex, e.activeN = 0, 0
		}
		e.mu.Unlock()
	}()
	parkedReason := ""
	if mode == modeDriftRetry {
		parkedReason = readReason(cfg.statePath(f, ".needs-repair"))
	}
	e.setState(f, StSyncing, "")
	marker := cfg.statePath(f, ".initialised")
	initialised := exists(marker)
	first := !initialised || mode == modeRepair
	local := cfg.Local(f)

	if mode == modeNormal {
		if r := readReason(cfg.statePath(f, ".needs-repair")); r != "" {
			e.setState(f, StNeedsRepair, r)
			return false
		}
	}
	if mode == modeDriftRetry && (!initialised || parkedReason == "") {
		_ = os.Remove(cfg.statePath(f, ".drift-parked"))
		if parkedReason != "" {
			e.setState(f, StNeedsRepair, parkedReason)
		} else {
			e.setState(f, StIdle, "")
		}
		return false
	}
	// stillParked keeps a parked folder waiting for Repair whatever stopped this try.
	stillParked := func() bool {
		if mode == modeDriftRetry {
			e.setState(f, StNeedsRepair, parkedReason)
		}
		return false
	}
	if initialised && f.LocalPath == "" && !exists(expand(cfg.LocalRoot)) {
		e.setState(f, StFailed, cfg.LocalRoot+" is missing")
		return stillParked()
	}
	if initialised && !exists(local) {
		e.setState(f, StFailed, missingLocalText(f.DisplayName()))
		return stillParked()
	}
	work := cfg.workPath(f)
	serverFile := filepath.Join(work, "server.txt")
	if initialised && mode != modeRepair {
		if b, err := os.ReadFile(serverFile); err == nil && string(b) != cfg.ServerIdentity() {
			e.markNeedsRepair(f, "server or account settings changed")
			return false
		}
	}
	if !e.waitForTunnel() {
		e.setState(f, StFailed, "no connection to server")
		return stillParked()
	}
	if first {
		_ = os.MkdirAll(local, 0o700)
	}
	_ = os.MkdirAll(work, 0o700)
	if !exists(serverFile) {
		_ = os.WriteFile(serverFile, []byte(cfg.ServerIdentity()), 0o600)
	}
	filters := cfg.filtersPath(f)
	if b, _ := os.ReadFile(filters); string(b) != filtersText(cfg, f) {
		_ = os.WriteFile(filters, []byte(filtersText(cfg, f)), 0o600)
	}
	r := folderRun{cfg: cfg, f: f, local: local, work: work, filters: filters, serverFile: serverFile, marker: marker}

	if first {
		kind := "merge (first sync)"
		if mode == modeRepair {
			kind = "merge (repair)"
		}
		ok, _ := e.merge(r, kind, "")
		return ok
	}
	var extra []string
	if mode == modeAllowBigDeletion {
		// Check both sides again: an emptied copy (an unplugged disk, a server folder that
		// vanished) must never be "applied" to the other side.
		if localMissingOrEmpty(local) {
			e.setState(f, StFailed, missingLocalText(f.DisplayName()))
			return false
		}
		if e.remoteMissingOrEmpty(cfg, f) {
			e.setState(f, StFailed, missingRemoteText)
			return false
		}
		extra = []string{"--force"}
	}
	out, err := e.bisync(r, "sync", extra...)
	if err == nil {
		if mode == modeDriftRetry {
			e.logLine("%s: syncs again without a merge; no repair needed", f.Path)
		}
		e.syncSucceeded(r)
		return true
	}
	res := classifyBisync(out, err == context.DeadlineExceeded)
	if mode == modeDriftRetry {
		// Still not settled: it keeps waiting for Repair, and the next interval tries again.
		e.logLine("%s: still needs repair (plain sync failed)", f.Path)
		return stillParked()
	}
	if res.outcome != outcomeDrift {
		e.syncFailed(r, res, out)
		return false
	}
	// The two sides' listings disagree. bisync won't continue (and --recover just falls back
	// to listings that disagree the same way); a newer-wins merge brings both sides together.
	auto := cfg.statePath(f, ".auto-merge")
	if !canAutoMerge(auto, time.Now()) {
		e.parkDrift(f, driftReason(res.path, "already merged automatically today; choose Repair"))
		return false
	}
	// The once-a-day limit only holds if it is on disk: no record, no automatic merge.
	if err := recordAutoMerge(auto, time.Now()); err != nil {
		e.logLine("%s: couldn't record the automatic merge (%v); not merging", f.Path, err)
		e.markNeedsRepair(f, driftReason(res.path, "choose Repair"))
		return false
	}
	e.logLine("%s: out of sync (%s); merging automatically, newer copy wins", f.Path, orText(res.path, "listings differ"))
	ok, outcome := e.merge(r, "automatic merge (out of sync)", "the automatic merge didn't settle it; choose Repair")
	if !ok {
		if outcome == outcomeInterrupted {
			_ = os.Remove(auto) // it didn't happen: the next sync may merge
		}
		return false
	}
	e.setMessage(fmt.Sprintf("“%s” was out of sync and was merged automatically (the newer copy of each file was kept; older copies are in the backups)", f.DisplayName()))
	e.mu.Lock()
	e.autoMerged = append(e.autoMerged, f.DisplayName())
	e.mu.Unlock()
	return true
}

// filtersText is the folder's filters file: its own excludes, the common ones, and the
// backup folder.
func filtersText(cfg Config, f KeptFolder) string {
	lines := append(append(append([]string{}, f.ExtraFilters...), cfg.CommonFilters...), backupFilter)
	return strings.Join(lines, "\n") + "\n"
}

func orText(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// merge runs `bisync --resync --resync-mode newer`: for each file the newer copy wins and no
// file is deleted. Every copy a merge overwrites is moved to a dated backup folder first (on
// this computer under the state folder, in OpenCloud under .opencloud-bar-backups).
// --check-sync stays on, so a merge that didn't bring both sides together fails now instead of
// breaking the next sync. Files that change during the merge make that check fail; one more
// pass usually settles them. It also returns how the last run ended.
func (e *Engine) merge(r folderRun, kind, driftNote string) (bool, syncOutcome) {
	if driftNote == "" {
		driftNote = "choose Repair when it's quiet"
	}
	now := time.Now()
	pruneBackups(r.cfg.StateDir, r.f.StateName, now)
	backups := backupArgs(r.cfg, r.f, backupStamp(now))
	for pass := 1; pass <= 2; pass++ {
		out, err := e.bisync(r, kind, append([]string{"--resync", "--resync-mode", "newer"}, backups...)...)
		if err == nil {
			e.syncSucceeded(r)
			return true, outcomeOK
		}
		res := classifyBisync(out, err == context.DeadlineExceeded)
		if res.outcome != outcomeDrift || pass == 2 {
			if res.outcome == outcomeDrift {
				e.parkDrift(r.f, driftReason(res.path, driftNote))
			} else {
				e.syncFailed(r, res, out)
			}
			return false, res.outcome
		}
		e.logLine("%s: files changed during the merge (%s); merging once more", r.f.Path, orText(res.path, "listings differ"))
		kind = "merge (second pass)"
	}
	return false, outcomeFailed
}

// backupArgs sends the copies a merge overwrites to this computer's and OpenCloud's backup folders.
func backupArgs(cfg Config, f KeptFolder, stamp string) []string {
	return []string{"--backup-dir1", localBackupDir(cfg.StateDir, f.StateName, stamp),
		"--backup-dir2", remoteBackupDir(cfg.RemoteSpec(), f.StateName, stamp)}
}

// bisync runs one rclone bisync for the folder with the common flags plus extra.
func (e *Engine) bisync(r folderRun, kind string, extra ...string) (string, error) {
	tr, ck := r.cfg.Transfers, r.cfg.Checkers
	if tr <= 0 {
		tr = 4
	}
	if ck <= 0 {
		ck = 6
	}
	level := "NOTICE"
	if kind != "sync" {
		level = "INFO" // merges are rare: log every copy so a merge can be checked afterwards
	}
	args := []string{"bisync", r.local, r.cfg.RemoteSpec() + r.f.Path,
		"--workdir", r.work, "--filters-file", r.filters,
		"--create-empty-src-dirs", "--compare", "size,modtime",
		"--resilient", "--recover", "--max-lock", "10m",
		"--conflict-resolve", "newer", "--conflict-loser", "num",
		"--transfers", strconv.Itoa(tr), "--checkers", strconv.Itoa(ck), "--log-level", level,
		"--stats", "2s", "--stats-log-level", "NOTICE"} // progress for the menu (activity.go)
	args = append(args, extra...)
	sf := &statsFilter{onFiles: func(n int) {
		e.mu.Lock()
		changed := e.activeFiles != n
		e.activeFiles = n
		e.mu.Unlock()
		if changed {
			e.notify()
		}
	}}
	if lf := e.logFile("sync-" + r.f.StateName + ".log"); lf != nil {
		fmt.Fprintf(lf, "\n=== %s %s of %s\n", time.Now().Format(time.RFC3339), kind, r.f.Path)
		defer lf.Close()
		sf.w = lf
	}
	defer sf.Flush()
	e.mu.Lock()
	e.activeFiles = 0
	e.mu.Unlock()
	return e.run(syncTimeout, sf, r.cfg.Rclone(), args...)
}

func (e *Engine) syncSucceeded(r folderRun) {
	_ = os.WriteFile(r.marker, nil, 0o600)
	_ = os.Remove(r.cfg.statePath(r.f, ".needs-repair"))
	_ = os.Remove(r.cfg.statePath(r.f, ".drift-parked"))
	_ = os.WriteFile(r.serverFile, []byte(r.cfg.ServerIdentity()), 0o600)
	e.setState(r.f, StOK, "")
}

// syncFailed shows why a run failed (everything except drift, which the caller handles).
func (e *Engine) syncFailed(r folderRun, res bisyncResult, out string) {
	f := r.f
	switch res.outcome {
	case outcomeSignedOut:
		if r.cfg.OIDC() {
			// The token command said the sign-in is gone; don't show rclone's raw error.
			e.markSignedOut()
			e.setState(f, StFailed, "signed out")
			return
		}
		e.setState(f, StFailed, "rclone couldn't sign in; will retry")
	case outcomeInterrupted:
		e.mu.Lock()
		q := e.quitting
		e.mu.Unlock()
		if res.timeout && !q {
			e.setState(f, StFailed, "timed out; will retry")
		} else {
			e.setState(f, StSkipped, "interrupted; continues with the next sync")
		}
	case outcomeBigChange:
		// An emptied copy isn't a deletion the user made: never offer to apply it to the other side.
		if res.side != "Path2" && localMissingOrEmpty(r.local) {
			e.setState(f, StFailed, missingLocalText(f.DisplayName()))
			return
		}
		if res.side != "Path1" && e.remoteMissingOrEmpty(r.cfg, f) {
			e.setState(f, StFailed, missingRemoteText)
			return
		}
		e.setState(f, StBigDeletion, bigDeletionSide(res.side))
	case outcomeNeedsResync:
		if strings.Contains(out, "directory not found") && e.remoteMissingOrEmpty(r.cfg, f) {
			e.setState(f, StFailed, missingRemoteText)
			return
		}
		e.markNeedsRepair(f, "sync history is out of date")
	case outcomeDrift:
		e.parkDrift(f, driftReason(res.path, "choose Repair when it's quiet"))
	default:
		if strings.Contains(out, "directory not found") && e.remoteMissingOrEmpty(r.cfg, f) {
			e.setState(f, StFailed, missingRemoteText)
			return
		}
		e.setState(f, StFailed, "rclone failed; will retry (see the sync log)")
	}
}

// remoteMissingOrEmpty is true when the folder is gone from OpenCloud or holds nothing. A
// "safety abort" then means the server copy vanished, not that the user deleted files: the
// large-deletion override must not be offered, or it would empty this computer's copy too.
func (e *Engine) remoteMissingOrEmpty(cfg Config, f KeptFolder) bool {
	out, err := e.run(time.Minute, nil, cfg.Rclone(), "lsf", "--max-depth", "1", "--exclude", ".DS_Store", cfg.RemoteSpec()+f.Path)
	if err != nil {
		return strings.Contains(out, "directory not found") || strings.Contains(out, "not found")
	}
	return strings.TrimSpace(out) == ""
}

// bigDeletionSide is where the files were deleted, for the folder's detail text.
func bigDeletionSide(side string) string {
	switch side {
	case "Path1":
		return "on this computer"
	case "Path2":
		return "in OpenCloud"
	}
	return ""
}

// run executes a command, optionally teeing output to a log, and tracks it so Shutdown can stop it.
func (e *Engine) run(timeout time.Duration, logTo io.Writer, name string, args ...string) (string, error) {
	cfg := e.config()
	cmd := exec.Command(name, args...)
	cmd.Env = envFor(cfg)
	hideWindow(cmd)
	var buf bytes.Buffer
	var w io.Writer = &buf
	if logTo != nil {
		w = io.MultiWriter(&buf, logTo)
	}
	cmd.Stdout, cmd.Stderr = w, w
	// Don't wait forever for output pipes that a finished process left open (seen with ssh on Windows).
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return "", err
	}
	tracked := name == cfg.Rclone() && len(args) > 0 && args[0] == "bisync"
	if tracked {
		e.mu.Lock()
		e.currentSync = cmd
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.currentSync = nil; e.mu.Unlock() }()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(timeout):
		interrupt(cmd) // bisync finishes the current file and saves its state
		select {
		case <-done:
		case <-time.After(time.Minute):
			_ = cmd.Process.Kill()
			<-done
		}
		return buf.String(), context.DeadlineExceeded
	}
}

// exclusive runs body as the only sync activity.
func (e *Engine) exclusive(body func()) error {
	e.mu.Lock()
	if e.syncing {
		e.mu.Unlock()
		return fmt.Errorf("a sync is running; try again when it finishes")
	}
	e.syncing = true
	e.mu.Unlock()
	go func() {
		defer func() { e.mu.Lock(); e.syncing = false; e.mu.Unlock(); e.notify() }()
		body()
	}()
	return nil
}

func (e *Engine) find(path string) (KeptFolder, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.cfg.Kept {
		if foldKey(f.Path) == foldKey(path) {
			return f, true
		}
	}
	return KeptFolder{}, false
}

func (e *Engine) Repair(path string) error {
	f, ok := e.find(path)
	if !ok {
		return fmt.Errorf("%q isn't a kept folder", path)
	}
	return e.exclusive(func() {
		_ = os.Remove(e.stateFile(f, ".needs-repair"))
		// A repair the user chose also resets the daily automatic-merge allowance.
		_ = os.Remove(e.stateFile(f, ".auto-merge"))
		_ = os.Remove(e.stateFile(f, ".drift-parked"))
		e.syncFolder(f, modeRepair)
	})
}

// AllowBigDeletion applies a blocked large deletion. It is only offered (and only accepted)
// while the folder is blocked for that reason, never when the server copy is missing.
func (e *Engine) AllowBigDeletion(path string) error {
	f, ok := e.find(path)
	if !ok {
		return fmt.Errorf("%q isn't a kept folder", path)
	}
	e.mu.Lock()
	s := e.status[f.Path]
	blocked := s != nil && s.State == StBigDeletion
	e.mu.Unlock()
	if !blocked {
		return fmt.Errorf("%q has no blocked deletion to apply", f.DisplayName())
	}
	return e.exclusive(func() { e.syncFolder(f, modeAllowBigDeletion) })
}

// SetPaused pauses or resumes syncing of kept folders and remembers it in the settings.
// The drive keeps working. A sync that is already running finishes first.
func (e *Engine) SetPaused(paused bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfgErr != nil {
		return fmt.Errorf("the settings file has a problem; fix it first")
	}
	if e.cfg.Paused == paused {
		return nil
	}
	// Only "paused" changes in the file; everything else stays as it is there.
	if _, err := editConfig(func(raw map[string]json.RawMessage) error {
		if paused {
			raw["paused"] = mustJSON(true)
		} else {
			delete(raw, "paused") // the default, as the macOS app writes it
		}
		return nil
	}); err != nil {
		return err
	}
	cfg := e.cfg
	cfg.Paused = paused
	e.cfg = cfg
	if !paused {
		e.nextSyncAt = time.Time{} // sync right away
	}
	go e.notify()
	return nil
}

// CheckForUpdates asks GitHub now (the menu's Check for Updates…).
func (e *Engine) CheckForUpdates(ctx context.Context) UpdateState {
	st := e.updates.Check(ctx)
	e.notify()
	return st
}

// Keep adds a kept folder. localPath is optional (absolute).
func (e *Engine) Keep(path, localPath string) error {
	path = strings.Trim(filepath.ToSlash(path), "/")
	if p := pathProblem(path); p != "" {
		return fmt.Errorf("%s", p)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfgErr != nil {
		return fmt.Errorf("the settings file has a problem; fix it first")
	}
	if e.syncing {
		return fmt.Errorf("a sync is running; try again when it finishes")
	}
	for _, g := range e.cfg.Kept {
		if overlaps(path, g.Path) {
			return fmt.Errorf("%q overlaps %q, which is already kept", path, g.Path)
		}
	}
	f := KeptFolder{Path: path, StateName: makeStateName(path), LocalPath: localPath}
	// Add one entry to the file's kept list; the other entries (and their keys this version
	// doesn't know) stay as they are.
	next, err := editConfig(func(raw map[string]json.RawMessage) error {
		kept, err := rawKept(raw)
		if err != nil {
			return err
		}
		taken := map[string]bool{}
		for _, g := range e.cfg.Kept {
			taken[g.StateName] = true
		}
		for _, k := range kept {
			if p := rawString(k, "path"); overlaps(path, p) {
				return fmt.Errorf("%q overlaps %q, which is already kept", path, p)
			}
			taken[rawString(k, "stateName")] = true
		}
		for taken[f.StateName] {
			f.StateName += "x"
		}
		var entry map[string]json.RawMessage
		_ = json.Unmarshal(mustJSON(f), &entry)
		raw["kept"] = mustJSON(append(kept, entry))
		return nil
	})
	if err != nil {
		return err
	}
	cfg := e.cfg
	cfg.Kept = next.Kept
	e.cfg = cfg
	e.refreshStatusLocked()
	e.nextSyncAt = time.Time{}
	return nil
}

// UnsyncedCount is the number of local files the filters exclude (-1 if unknown).
func (e *Engine) UnsyncedCount(path string) int {
	f, ok := e.find(path)
	if !ok || !exists(e.filtersFile(f)) {
		return -1
	}
	cfg := e.config()
	local := cfg.Local(f)
	all, err1 := e.run(5*time.Minute, nil, cfg.Rclone(), "lsf", "-R", "--files-only", local)
	synced, err2 := e.run(5*time.Minute, nil, cfg.Rclone(), "lsf", "-R", "--files-only", "--filters-file", e.filtersFile(f), local)
	if err1 != nil || err2 != nil {
		return -1
	}
	n := len(strings.Split(strings.TrimSpace(all), "\n")) - len(strings.Split(strings.TrimSpace(synced), "\n"))
	return max(0, n)
}

// Unkeep does a final sync, then moves the local copy to the trash (or a holding folder).
func (e *Engine) Unkeep(path string, result func(error)) error {
	f, ok := e.find(path)
	if !ok {
		return fmt.Errorf("%q isn't a kept folder", path)
	}
	e.mu.Lock()
	bad := e.cfgErr
	e.mu.Unlock()
	if bad != nil {
		return fmt.Errorf("the settings file has a problem; fix it first")
	}
	return e.exclusive(func() {
		if !e.syncFolder(f, modeNormal) {
			result(fmt.Errorf("couldn't sync %q; nothing was removed", f.DisplayName()))
			return
		}
		if err := moveToTrash(e.config().Local(f)); err != nil {
			result(fmt.Errorf("couldn't move it to %s: %w", trashName(), err))
			return
		}
		_ = os.RemoveAll(e.workDir(f))
		for _, s := range []string{".initialised", ".needs-repair", ".auto-merge", ".drift-parked"} {
			_ = os.Remove(e.stateFile(f, s))
		}
		_ = os.Remove(e.filtersFile(f))
		e.mu.Lock()
		// Remove just this entry from the file's kept list.
		next, err := editConfig(func(raw map[string]json.RawMessage) error {
			kept, err := rawKept(raw)
			if err != nil {
				return err
			}
			rest := []map[string]json.RawMessage{}
			for _, k := range kept {
				if foldKey(rawString(k, "path")) != foldKey(f.Path) {
					rest = append(rest, k)
				}
			}
			raw["kept"] = mustJSON(rest)
			return nil
		})
		if err == nil {
			cfg := e.cfg
			cfg.Kept = next.Kept
			e.cfg = cfg
			e.refreshStatusLocked()
		}
		e.mu.Unlock()
		result(err)
	})
}

func (e *Engine) Reload() error {
	cfg, err := loadConfig()
	if err != nil {
		e.mu.Lock()
		e.cfgErr = err
		e.mu.Unlock()
		return err
	}
	out := cfg.OIDC() && cfg.Configured() && !haveCredentials(cfg) // keyring access: not under the lock
	e.mu.Lock()
	if e.syncing {
		e.mu.Unlock()
		return fmt.Errorf("a sync is running; reload when it finishes")
	}
	reconnect := cfg.SSHHost != e.cfg.SSHHost || cfg.LocalPort != e.cfg.LocalPort || cfg.ServerPort != e.cfg.ServerPort ||
		cfg.DavPath != e.cfg.DavPath || cfg.RcloneRemote != e.cfg.RcloneRemote || cfg.MountPoint != e.cfg.MountPoint ||
		cfg.ServerURL != e.cfg.ServerURL || cfg.AuthMode() != e.cfg.AuthMode() || cfg.Tunnel() != e.cfg.Tunnel()
	wasOut := e.signedOut
	e.cfg, e.cfgErr = cfg, nil
	e.signedOut = out
	if out {
		reconnect = true // signed out: stop the drive and tunnel
		e.message = signedOutMessage
	} else if wasOut {
		// Just signed in: start right away instead of waiting out old retry timers.
		e.message, e.authRetryAt, e.tunnelRetryAt, e.mountRetryAt, e.nextSyncAt = "", time.Time{}, time.Time{}, time.Time{}, time.Time{}
	}
	e.refreshStatusLocked()
	m, t := e.mount, e.tunnel
	e.mu.Unlock()
	if reconnect {
		if m != nil {
			e.mu.Lock()
			e.unmountRequest = true
			e.mu.Unlock()
			_ = e.unmount(m)
		}
		if t != nil && t.Process != nil {
			_ = t.Process.Kill()
		}
	}
	return nil
}

// Shutdown lets a running sync finish its current file, unmounts the drive, closes the tunnel.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	if e.quitting {
		e.mu.Unlock()
		return
	}
	e.quitting = true
	cs := e.currentSync
	m, t, td := e.mount, e.tunnel, e.tunnelDone
	e.mu.Unlock()
	if cs != nil {
		interrupt(cs)
		for i := 0; i < 1200; i++ { // up to 2 minutes
			e.mu.Lock()
			running := e.currentSync != nil
			e.mu.Unlock()
			if !running {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if m != nil {
		_ = e.unmount(m)
	}
	if t != nil && t.Process != nil {
		_ = t.Process.Kill()
		if td != nil {
			<-td
		}
	}
	e.logLine("stopped")
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Status{Message: e.message, Syncing: e.syncing, DriveWanted: e.mountWanted && e.cfg.Mounting()}
	st.UseTunnel, st.SignedOut = e.cfg.Tunnel(), e.cfg.OIDC() && e.signedOut
	st.TunnelUp = !e.cfg.Tunnel() || (e.tunnelOurs() && portOpen(e.cfg.LocalPort))
	st.DriveMounted = e.cfg.Mounting() && isMounted(e.mountTargetLocked())
	st.Paused = e.cfg.Paused
	st.Update = e.updates.State()
	if e.cfgErr != nil {
		st.ConfigError = e.cfgErr.Error()
	}
	var oldest time.Time
	all := true
	var problems []FolderStatus
	for _, f := range e.cfg.Kept {
		s := e.status[f.Path]
		if s == nil {
			continue
		}
		st.Folders = append(st.Folders, *s)
		if needsAttention(s.State) {
			problems = append(problems, *s)
		}
		if s.LastSuccess.IsZero() {
			all = false
		} else if oldest.IsZero() || s.LastSuccess.Before(oldest) {
			oldest = s.LastSuccess
		}
	}
	switch {
	case st.ConfigError != "":
		st.Summary = st.ConfigError
	case !e.cfg.Configured():
		st.Summary = "Not set up: choose Set Up / Sign In…"
	case st.SignedOut:
		st.Summary = signedOutMessage
	case e.syncing:
		st.Summary = activityText(e.activeName, e.activeIndex, e.activeN, e.activeFiles)
	case len(problems) > 0:
		st.Summary = attentionSummary(problems)
	case st.Paused && !oldest.IsZero():
		st.Summary = "Paused · synced " + ago(oldest)
	case st.Paused:
		st.Summary = "Paused"
	case len(e.cfg.Kept) > 0 && all:
		st.Summary = "Up to date · synced " + ago(oldest)
	case !st.TunnelUp:
		st.Summary = "Connecting to server…"
	case len(e.cfg.Kept) == 0:
		st.Summary = "Nothing kept on this computer"
	default:
		st.Summary = "Waiting for first sync"
	}
	return st
}

func needsAttention(state string) bool {
	return state == StFailed || state == StNeedsRepair || state == StBigDeletion
}

// attentionReason says in a few words what is wrong with a folder.
func attentionReason(f FolderStatus) string {
	switch f.State {
	case StNeedsRepair:
		if f.Detail != "" {
			return "needs repair: " + f.Detail
		}
		return "needs repair"
	case StBigDeletion:
		if f.Detail != "" {
			return "large deletion blocked (" + f.Detail + ")"
		}
		return "large deletion blocked"
	case StFailed:
		if f.Detail != "" {
			return "sync failed: " + f.Detail
		}
		return "sync failed"
	}
	return ""
}

// attentionSummary is the menu's first line when folders need the user: which one and why.
func attentionSummary(problems []FolderStatus) string {
	s := "“" + problems[0].Name + "” " + attentionReason(problems[0])
	if n := len(problems) - 1; n == 1 {
		s += " (and 1 more folder)"
	} else if n > 1 {
		s += fmt.Sprintf(" (and %d more folders)", n)
	}
	return s
}

func ago(t time.Time) string { return agoAt(t, time.Now()) }

// agoAt is "just now", "5 min ago", "3 h ago" or "2 days ago" (same wording as the macOS app).
func agoAt(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func init() { log.SetFlags(0) }
