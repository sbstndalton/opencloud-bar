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
}

const syncTimeout = 3 * time.Hour

func NewEngine() *Engine {
	cfg, err := loadConfig()
	e := &Engine{cfg: cfg, cfgErr: err, status: map[string]*FolderStatus{},
		mountWanted: cfg.Mounting(), mountBackoff: 5 * time.Second, changed: make(chan struct{}, 1)}
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

// SignOut forgets the sign-in, stops the drive and waits for the next sign-in.
func (e *Engine) SignOut() {
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
}

func (e *Engine) notify() {
	select {
	case e.changed <- struct{}{}:
	default:
	}
}

func (e *Engine) refreshStatusLocked() {
	keep := map[string]bool{}
	for _, f := range e.cfg.Kept {
		keep[f.Path] = true
		if e.status[f.Path] == nil {
			s := &FolderStatus{Path: f.Path, Name: f.DisplayName(), State: StIdle, Local: e.cfg.Local(f)}
			if r := e.needsRepairReason(f); r != "" {
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
	if !e.authReady() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
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
	if !e.syncing && up && len(e.cfg.Kept) > 0 && time.Now().After(e.nextSyncAt) {
		e.startSyncAllLocked()
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

func (e *Engine) env() []string {
	env := os.Environ()
	if e.cfg.RcloneConfig != "" {
		env = append(env, "RCLONE_CONFIG="+expand(e.cfg.RcloneConfig))
	}
	if e.cfg.OIDC() {
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
	if !e.cfg.Tunnel() {
		return true
	}
	for i := 0; i < 20; i++ {
		e.mu.Lock()
		ok := e.tunnelOurs()
		e.mu.Unlock()
		if ok && portOpen(e.cfg.LocalPort) {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

// MARK: drive

func (e *Engine) mountTarget() string { return expand(e.cfg.MountPoint) }

func (e *Engine) startMountLocked() {
	target := e.mountTarget()
	if isMounted(target) {
		go forceUnmount(target)
		e.mountRetryAt = time.Now().Add(10 * time.Second)
		return
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
		"--rc", "--rc-addr", fmt.Sprintf("127.0.0.1:%d", e.cfg.RcPort()), "--rc-no-auth",
		"--log-level", "NOTICE"}
	args = append(args, mountExtraArgs()...)
	cmd := exec.Command(e.cfg.Rclone(), args...)
	cmd.Env = e.env()
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
		_, err = e.run(15*time.Second, nil, e.cfg.Rclone(), "rc", "core/quit", "--url", fmt.Sprintf("http://127.0.0.1:%d/", e.cfg.RcPort()))
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
	out, err := e.run(10*time.Second, nil, e.cfg.Rclone(), "rc", "vfs/stats", "--url", fmt.Sprintf("http://127.0.0.1:%d/", e.cfg.RcPort()))
	if err != nil {
		return -1
	}
	var v struct {
		DiskCache struct {
			UploadsInProgress int `json:"uploadsInProgress"`
			UploadsQueued     int `json:"uploadsQueued"`
		} `json:"diskCache"`
	}
	if json.Unmarshal([]byte(out), &v) != nil {
		return -1
	}
	return v.DiskCache.UploadsInProgress + v.DiskCache.UploadsQueued
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

func (e *Engine) stateFile(f KeptFolder, suffix string) string {
	return filepath.Join(expand(e.cfg.StateDir), f.StateName+suffix)
}
func (e *Engine) workDir(f KeptFolder) string {
	return filepath.Join(expand(e.cfg.StateDir), "work", f.StateName)
}
func (e *Engine) filtersFile(f KeptFolder) string {
	return filepath.Join(expand(e.cfg.StateDir), "filters-"+f.StateName+".txt")
}
func (e *Engine) needsRepairReason(f KeptFolder) string {
	b, err := os.ReadFile(e.stateFile(f, ".needs-repair"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (e *Engine) setState(f KeptFolder, state, detail string) {
	e.mu.Lock()
	if s := e.status[f.Path]; s != nil {
		s.State, s.Detail = state, detail
		if state == StOK {
			s.LastSuccess = time.Now()
		}
	}
	e.mu.Unlock()
	e.notify()
}

func (e *Engine) markNeedsRepair(f KeptFolder, reason string) {
	_ = os.WriteFile(e.stateFile(f, ".needs-repair"), []byte(reason), 0o600)
	e.setState(f, StNeedsRepair, reason)
	e.logLine("%s: needs repair: %s", f.Path, reason)
}

func (e *Engine) SyncNow() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.syncing && e.cfgErr == nil && e.cfg.Configured() && !e.signedOut {
		e.startSyncAllLocked()
	}
}

func (e *Engine) startSyncAllLocked() {
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
		for _, f := range folders {
			e.mu.Lock()
			q := e.quitting
			s := e.status[f.Path]
			e.mu.Unlock()
			if q || s == nil || s.State == StNeedsRepair || s.State == StBigDeletion {
				continue
			}
			e.syncFolder(f, modeNormal)
		}
	}()
}

func (e *Engine) serverTooBusy() string {
	// The memory check runs over ssh, so it only applies with the tunnel.
	if !e.cfg.Tunnel() || e.cfg.MinServerAvailableMiB <= 0 {
		return ""
	}
	out, err := e.run(20*time.Second, nil, e.cfg.SSH(), "-n", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		e.cfg.SSHHost, "grep MemAvailable /proc/meminfo")
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
	if mib < e.cfg.MinServerAvailableMiB {
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
)

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// syncFolder runs one bisync. It never creates, recreates or resyncs a folder unless this is
// its first sync or the user chose repair.
func (e *Engine) syncFolder(f KeptFolder, mode syncMode) bool {
	e.setState(f, StSyncing, "")
	marker := e.stateFile(f, ".initialised")
	initialised := exists(marker)
	first := !initialised || mode == modeRepair
	local := e.cfg.Local(f)

	if mode == modeNormal {
		if r := e.needsRepairReason(f); r != "" {
			e.setState(f, StNeedsRepair, r)
			return false
		}
	}
	if initialised && f.LocalPath == "" && !exists(expand(e.cfg.LocalRoot)) {
		e.setState(f, StFailed, e.cfg.LocalRoot+" is missing")
		return false
	}
	if initialised && !exists(local) {
		e.setState(f, StFailed, "folder is missing on this computer")
		return false
	}
	work := e.workDir(f)
	serverFile := filepath.Join(work, "server.txt")
	if initialised && mode != modeRepair {
		if b, err := os.ReadFile(serverFile); err == nil && string(b) != e.cfg.ServerIdentity() {
			e.markNeedsRepair(f, "server or account settings changed")
			return false
		}
	}
	if !e.waitForTunnel() {
		e.setState(f, StFailed, "no connection to server")
		return false
	}
	if first {
		_ = os.MkdirAll(local, 0o700)
	}
	_ = os.MkdirAll(work, 0o700)
	if !exists(serverFile) {
		_ = os.WriteFile(serverFile, []byte(e.cfg.ServerIdentity()), 0o600)
	}
	filters := e.filtersFile(f)
	text := strings.Join(append(append([]string{}, f.ExtraFilters...), e.cfg.CommonFilters...), "\n") + "\n"
	if b, _ := os.ReadFile(filters); string(b) != text {
		_ = os.WriteFile(filters, []byte(text), 0o600)
	}
	tr, ck := e.cfg.Transfers, e.cfg.Checkers
	if tr <= 0 {
		tr = 4
	}
	if ck <= 0 {
		ck = 6
	}
	args := []string{"bisync", local, e.cfg.RemoteSpec() + f.Path,
		"--workdir", work, "--filters-file", filters,
		"--create-empty-src-dirs", "--compare", "size,modtime",
		"--resilient", "--recover", "--max-lock", "10m",
		"--conflict-resolve", "newer", "--conflict-loser", "num",
		"--transfers", strconv.Itoa(tr), "--checkers", strconv.Itoa(ck), "--log-level", "NOTICE"}
	if first {
		args = append(args, "--resync-mode", "newer", "--check-sync=false")
	}
	if mode == modeAllowBigDeletion {
		args = append(args, "--force")
	}
	lf := e.logFile("sync-" + f.StateName + ".log")
	if lf != nil {
		kind := "sync"
		if first {
			kind = "merge (resync)"
		}
		fmt.Fprintf(lf, "\n=== %s %s of %s\n", time.Now().Format(time.RFC3339), kind, f.Path)
		defer lf.Close()
	}
	out, err := e.run(syncTimeout, lf, e.cfg.Rclone(), args...)
	if err == nil {
		_ = os.WriteFile(marker, nil, 0o600)
		_ = os.Remove(e.stateFile(f, ".needs-repair"))
		_ = os.WriteFile(serverFile, []byte(e.cfg.ServerIdentity()), 0o600)
		e.setState(f, StOK, "")
		return true
	}
	switch {
	case e.cfg.OIDC() && strings.Contains(out, errSignedOut.Error()):
		// The token command said the sign-in is gone; don't show rclone's raw error.
		e.markSignedOut()
		e.setState(f, StFailed, "signed out")
	case strings.Contains(out, "Must run --resync") && !strings.Contains(out, "retryable without --resync"):
		e.markNeedsRepair(f, "sync history is out of date")
	case strings.Contains(out, "Safety abort") || strings.Contains(out, "too many deletes"):
		e.setState(f, StBigDeletion, "")
	case err == context.DeadlineExceeded:
		e.setState(f, StFailed, "timed out")
	default:
		e.setState(f, StFailed, fmt.Sprintf("rclone: %v; will retry", err))
	}
	return false
}

// run executes a command, optionally teeing output to a log, and tracks it so Shutdown can stop it.
func (e *Engine) run(timeout time.Duration, logTo io.Writer, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = e.env()
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
	tracked := name == e.cfg.Rclone() && len(args) > 0 && args[0] == "bisync"
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
		interrupt(cmd)
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
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
		e.syncFolder(f, modeRepair)
	})
}

func (e *Engine) AllowBigDeletion(path string) error {
	f, ok := e.find(path)
	if !ok {
		return fmt.Errorf("%q isn't a kept folder", path)
	}
	return e.exclusive(func() { e.syncFolder(f, modeAllowBigDeletion) })
}

// Keep adds a kept folder. localPath is optional (absolute).
func (e *Engine) Keep(path, localPath string) error {
	path = strings.Trim(filepath.ToSlash(path), "/")
	if p := pathProblem(path); p != "" {
		return fmt.Errorf("%s", p)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.syncing {
		return fmt.Errorf("a sync is running; try again when it finishes")
	}
	for _, g := range e.cfg.Kept {
		if overlaps(path, g.Path) {
			return fmt.Errorf("%q overlaps %q, which is already kept", path, g.Path)
		}
	}
	f := KeptFolder{Path: path, StateName: makeStateName(path), LocalPath: localPath}
	for _, g := range e.cfg.Kept {
		if g.StateName == f.StateName {
			f.StateName += "x"
		}
	}
	next := e.cfg
	next.Kept = append(append([]KeptFolder{}, e.cfg.Kept...), f)
	if err := saveConfig(next); err != nil {
		return err
	}
	e.cfg = next
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
	local := e.cfg.Local(f)
	all, err1 := e.run(5*time.Minute, nil, e.cfg.Rclone(), "lsf", "-R", "--files-only", local)
	synced, err2 := e.run(5*time.Minute, nil, e.cfg.Rclone(), "lsf", "-R", "--files-only", "--filters-file", e.filtersFile(f), local)
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
	return e.exclusive(func() {
		if !e.syncFolder(f, modeNormal) {
			result(fmt.Errorf("couldn't sync %q; nothing was removed", f.DisplayName()))
			return
		}
		if err := moveToTrash(e.cfg.Local(f)); err != nil {
			result(fmt.Errorf("couldn't move it to %s: %w", trashName(), err))
			return
		}
		_ = os.RemoveAll(e.workDir(f))
		for _, s := range []string{".initialised", ".needs-repair"} {
			_ = os.Remove(e.stateFile(f, s))
		}
		_ = os.Remove(e.filtersFile(f))
		e.mu.Lock()
		next := e.cfg
		next.Kept = nil
		for _, g := range e.cfg.Kept {
			if g.Path != f.Path {
				next.Kept = append(next.Kept, g)
			}
		}
		err := saveConfig(next)
		if err == nil {
			e.cfg = next
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
	st.DriveMounted = e.cfg.Mounting() && isMounted(e.mountTarget())
	if e.cfgErr != nil {
		st.ConfigError = e.cfgErr.Error()
	}
	var oldest time.Time
	all := true
	problem := ""
	for _, f := range e.cfg.Kept {
		s := e.status[f.Path]
		if s == nil {
			continue
		}
		st.Folders = append(st.Folders, *s)
		switch s.State {
		case StFailed, StNeedsRepair, StBigDeletion:
			if problem == "" {
				problem = s.Name
			}
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
		st.Summary = "Not set up: choose Set Up / Sign In… (or edit " + configFile() + ")"
	case st.SignedOut:
		st.Summary = signedOutMessage
	case e.syncing:
		st.Summary = "Syncing…"
	case problem != "":
		st.Summary = fmt.Sprintf("%q needs attention", problem)
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

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Minute)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%.0f h ago", d.Hours())
}

func init() { log.SetFlags(0) }
