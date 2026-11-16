package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"
)

// Server change checks (live sync, preview, with liveSync; v1.6): every remoteEvery (± jitter)
// each kept folder's root in OpenCloud is asked for its ETag (WebDAV PROPFIND, Depth 0,
// getetag only). OpenCloud's decomposedfs propagates a change up to the space root (the sync
// propagator sets each parent's tree mtime, which a folder's ETag comes from), so the root's
// ETag changes when anything below it changes. A changed ETag syncs that folder through the
// same path as a local change: the minimum gap, the per-folder queue, never two runs at once.
//
//   - Baseline: the last ETag per folder, in memory and in the state folder (<state>.server-etag).
//     The first check after launch syncs the folder if the ETag differs from the stored one
//     (the queue takes it once with the launch sync), else only records it. After each successful sync the ETag is read again and becomes
//     the baseline, so the app's own uploads don't start another sync. A check that was
//     running when a sync started is ignored.
//   - Errors (network, 401, 5xx, a redirect): that folder backs off, doubling up to
//     remoteMaxBackoff, without notifications. Signed out is handled where it always is.
//   - No checks while paused, signed out, quitting, without the browser sign-in, or with a
//     configuration error; at most remoteParallel at once.
//   - The bearer token goes only to the server: accountClient follows no redirects.
//   - A change in OpenCloud while the folder syncs goes into the baseline read after the
//     sync; if that sync didn't take it, it comes with the next timer sync (or change).

var (
	remoteEvery      = 30 * time.Second
	remoteJitter     = 5 * time.Second
	remoteMaxBackoff = 5 * time.Minute
	remoteParallel   = 4
	remoteTimeout    = 20 * time.Second
	remoteWakeGap    = 15 * time.Second // a tick this late: the computer slept
	remoteWakeWait   = 15 * time.Second
)

type remoteFolder struct {
	syncing bool   // the engine runs a sync of it now
	etag    string // the baseline
	known   bool   // the baseline was read (or loaded)
	loaded  bool   // the state file was read
	nextAt  time.Time
	fails   int
	busy    bool // a check (or baseline read) runs
	gen     int  // bumped when a sync starts and ends: older checks are ignored
	pending bool // a baseline read waits for the end of the sync
}

// davETag reads a resource's ETag (PROPFIND, depth 0). The status is the server's answer.
func davETag(ctx context.Context, davURL, tok string) (string, int, error) {
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", davURL, strings.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := accountClient.Do(req) // no redirects: the token stays with the server
	if err != nil {
		return "", 0, errors.New("the server didn't answer")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return "", resp.StatusCode, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	var ms struct {
		Responses []struct {
			Props []struct {
				ETag string `xml:"prop>getetag"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ms); err != nil {
		return "", 207, errors.New("the server's answer wasn't what was expected")
	}
	for _, r := range ms.Responses {
		for _, p := range r.Props {
			if t := strings.TrimSpace(p.ETag); t != "" {
				return t, 207, nil
			}
		}
	}
	return "", 207, errors.New("the server didn't give an ETag")
}

func remoteNext(now time.Time) time.Time {
	j := time.Duration(0)
	if remoteJitter > 0 {
		j = time.Duration(rand.Int64N(int64(2*remoteJitter))) - remoteJitter
	}
	return now.Add(remoteEvery + j)
}

func remoteBackoff(fails int) time.Duration {
	d := remoteEvery
	for i := 1; i < fails && d < remoteMaxBackoff; i++ {
		d *= 2
	}
	return min(d, remoteMaxBackoff)
}

// remoteOnLocked: server checks may run now. The caller holds e.mu.
func (e *Engine) remoteOnLocked() bool {
	return e.cfg.LiveSync && e.cfgErr == nil && !e.quitting && !e.signedOut && !e.cfg.Paused &&
		e.cfg.OIDC() && e.cfg.Configured()
}

type remoteJob struct {
	key, url, server, state string
	gen                     int
	load, baseline          bool
	token                   func(string) (string, error)
	etag                    func(context.Context, string, string) (string, int, error)
}

// remoteTick starts the checks that are due (from liveLoop, every liveTickTime).
func (e *Engine) remoteTick() {
	e.mu.Lock()
	l := e.live()
	if !e.remoteOnLocked() || time.Now().Round(0).Before(l.remoteResumeAt) {
		e.mu.Unlock()
		return
	}
	now := l.now()
	keep := map[string]bool{}
	var jobs []remoteJob
	for _, f := range e.cfg.Kept {
		key := f.Key()
		keep[key] = true
		rf := l.remote[key]
		if rf == nil {
			rf = &remoteFolder{}
			l.remote[key] = rf
		}
		if l.remoteRun >= remoteParallel || rf.busy || rf.pending || rf.syncing || l.remoteGone[key] || now.Before(rf.nextAt) {
			continue
		}
		if lf := l.folders[key]; (lf != nil && lf.own) || e.waitingLocked(key) || e.isRunningLocked(key) {
			continue // a sync of it is running or waiting: it reads the baseline after
		}
		u, err := itemDavURL(e.cfg, f, "")
		if err != nil {
			rf.nextAt = now.Add(remoteMaxBackoff)
			continue
		}
		rf.busy = true
		l.remoteRun++
		jobs = append(jobs, remoteJob{key: key, url: u, server: e.cfg.ServerURL, state: e.cfg.statePath(f, ".server-etag"),
			gen: rf.gen, load: !rf.loaded, token: l.remoteToken, etag: l.remoteETag})
	}
	for key := range l.remote {
		if !keep[key] {
			delete(l.remote, key)
		}
	}
	for key := range l.remoteGone {
		if !keep[key] {
			delete(l.remoteGone, key) // kept again later: checked again
		}
	}
	e.mu.Unlock()
	for _, j := range jobs {
		go e.remoteCheck(j)
	}
}

// remoteCheck asks the server for one folder's ETag (no lock held).
func (e *Engine) remoteCheck(j remoteJob) {
	stored := ""
	if j.load {
		if b, err := os.ReadFile(j.state); err == nil {
			stored = strings.TrimSpace(string(b))
		}
	}
	etag, status, err := "", 0, error(nil)
	tok, terr := j.token(j.server)
	if terr != nil {
		err = terr
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
		etag, status, err = j.etag(ctx, j.url, tok)
		cancel()
	}
	save := e.remoteResult(j, stored, etag, status, err)
	if save == "" {
		return
	}
	l := e.live()
	l.fileMu.Lock() // with remoteForget: no write after Stop Keeping removed the file
	defer l.fileMu.Unlock()
	_, kept := e.find(j.key) // takes e.mu itself
	e.mu.Lock()
	still := kept && l.remote[j.key] != nil && !l.remoteGone[j.key]
	e.mu.Unlock()
	if still {
		_ = os.WriteFile(j.state, []byte(save+"\n"), 0o600)
	}
}

// remoteForget: the folder stops being kept; its checks stop and its stored ETag goes.
func (e *Engine) remoteForget(f KeptFolder) {
	l := e.live()
	e.mu.Lock()
	delete(l.remote, f.Key())
	l.remoteGone[f.Key()] = true
	state := e.cfg.statePath(f, ".server-etag")
	e.mu.Unlock()
	l.fileMu.Lock()
	_ = os.Remove(state)
	l.fileMu.Unlock()
}

// remoteResult applies a check's answer; it returns an ETag to store ("" for none).
func (e *Engine) remoteResult(j remoteJob, stored, etag string, status int, err error) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.live()
	l.remoteRun--
	rf := l.remote[j.key]
	if rf == nil {
		return ""
	}
	rf.busy = false
	now := l.now()
	if j.load && !rf.loaded {
		rf.loaded = true
		if stored != "" && !rf.known {
			rf.etag, rf.known = stored, true
		}
	}
	if err != nil {
		rf.fails++
		rf.nextAt = now.Add(remoteBackoff(rf.fails))
		if j.baseline {
			rf.known = false // the next check only records
		}
		return ""
	}
	rf.fails = 0
	rf.nextAt = remoteNext(now)
	if j.gen != rf.gen || rf.syncing {
		return "" // a sync started or ended meanwhile: its baseline read decides
	}
	if j.load && !j.baseline && stored != "" && etag != stored {
		// Changed while the app was closed: sync (the queue takes it once with the launch sync).
		rf.etag, rf.known = etag, true
		e.serverChangeLocked(j.key)
		return etag
	}
	if j.baseline || j.load || !rf.known { // after a sync, the first check after launch: record
		changed := !rf.known || rf.etag != etag
		rf.etag, rf.known = etag, true
		if changed {
			return etag
		}
		return ""
	}
	if etag == rf.etag {
		return ""
	}
	rf.etag = etag
	e.serverChangeLocked(j.key)
	return etag
}

// serverChangeLocked: the folder changed in OpenCloud; it syncs as soon as the gap allows.
func (e *Engine) serverChangeLocked(key string) {
	l := e.live()
	lf := l.folder(key)
	now := l.now()
	if !lf.pending {
		lf.pending, lf.first = true, now
	}
	lf.server = true
	lf.last = now.Add(-liveQuiet)
	if s := e.status[key]; s != nil {
		s.Pending = true
	}
}

// remoteSyncStartedLocked: a sync of the folder starts; running checks are ignored.
func (e *Engine) remoteSyncStartedLocked(key string) {
	l := e.live()
	rf := l.remote[key]
	if rf == nil {
		rf = &remoteFolder{}
		l.remote[key] = rf
	}
	rf.gen++
	rf.syncing = true
}

// remoteSyncEndedLocked: after a successful sync the folder's ETag is read again and becomes
// the baseline (the app's own uploads changed it). It returns the job to run without the lock.
func (e *Engine) remoteSyncEndedLocked(f KeptFolder, ok bool) *remoteJob {
	l := e.live()
	key := f.Key()
	rf := l.remote[key]
	if rf == nil {
		if !e.remoteOnLocked() {
			return nil
		}
		rf = &remoteFolder{}
		l.remote[key] = rf
	}
	rf.gen++
	rf.syncing = false
	if !ok || !e.remoteOnLocked() || l.remoteGone[key] {
		// A failed sync may have uploaded part: the next check only records (the folder is
		// tried again at the interval), so a failing sync doesn't restart every 30 s.
		rf.known = rf.known && ok
		return nil
	}
	u, err := itemDavURL(e.cfg, f, "")
	if err != nil {
		return nil
	}
	rf.pending = true
	return &remoteJob{key: key, url: u, server: e.cfg.ServerURL, state: e.cfg.statePath(f, ".server-etag"),
		gen: rf.gen, baseline: true, load: !rf.loaded, token: l.remoteToken, etag: l.remoteETag}
}

// remoteBaseline reads the baseline after a sync (no lock held).
func (e *Engine) remoteBaseline(j remoteJob) {
	e.mu.Lock()
	l := e.live()
	rf := l.remote[j.key]
	if rf != nil {
		rf.pending = false
		rf.busy = true
	}
	l.remoteRun++
	e.mu.Unlock()
	e.remoteCheck(j)
}
