package main

// The settings page for Windows and Linux. The tray can't show windows, so the control API also
// serves one embedded HTML page under /ui/, opened in the default browser.
//
// How it is protected (the page can change settings and read logs, so a web page you visit must
// never be able to drive it):
//   - "Settings…" in the tray makes a random one-time code (valid 60 s, single use) and opens
//     /ui/login?code=… . Redeeming it sets an HttpOnly, SameSite=Strict session cookie (random,
//     kept in memory) and redirects to /ui/.
//   - Every /ui/ request must carry a Host of 127.0.0.1:<port> or localhost:<port> (DNS rebinding).
//   - Every request that changes something must come from the page itself: Origin (if sent) must
//     be our own origin, Sec-Fetch-Site (if sent) must be same-origin, and a custom header is required.
//   - The routes under /ui/ only accept the cookie. The bearer-token routes (CLI) are unchanged and
//     don't accept the cookie.
//   - Access and refresh tokens never reach the page; log text is scrubbed of anything that looks like one.

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML string

// A copy of packaging/icon/icon-256.png (go:embed can't reach outside this folder).
//
//go:embed assets/icon-256.png
var iconPNG []byte

const (
	sessionCookie = "ocb_session"
	loginCodeTTL  = 60 * time.Second
	sessionTTL    = 12 * time.Hour
	uiHeader      = "X-OpenCloud-Bar" // must be present on every changing request
)

type webUI struct {
	mu       sync.Mutex
	port     int
	now      func() time.Time
	codes    map[string]time.Time // code -> expiry
	sessions map[string]time.Time // session -> expiry
	gotoPage string               // a section the open page should switch to (tray ▸ About…)
}

// requestSection asks an already open settings page to show section (it checks every 2 s).
func (u *webUI) requestSection(s string) {
	if validSection(s) {
		u.mu.Lock()
		u.gotoPage = s
		u.mu.Unlock()
	}
}

// takeSection returns and clears the requested section.
func (u *webUI) takeSection() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.gotoPage
	u.gotoPage = ""
	return s
}

// settingsUI is the one instance the running app uses.
var settingsUI = newWebUI()

func newWebUI() *webUI {
	return &webUI{now: time.Now, codes: map[string]time.Time{}, sessions: map[string]time.Time{}}
}

func (u *webUI) setPort(p int) { u.mu.Lock(); u.port = p; u.mu.Unlock() }
func (u *webUI) getPort() int  { u.mu.Lock(); defer u.mu.Unlock(); return u.port }

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (u *webUI) pruneLocked() {
	now := u.now()
	for k, exp := range u.codes {
		if !now.Before(exp) {
			delete(u.codes, k)
		}
	}
	for k, exp := range u.sessions {
		if !now.Before(exp) {
			delete(u.sessions, k)
		}
	}
}

// NewCode makes a one-time login code, valid for 60 seconds.
func (u *webUI) NewCode() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pruneLocked()
	if len(u.codes) >= 8 { // someone is spamming the menu item; forget the oldest
		var oldest string
		for k, exp := range u.codes {
			if oldest == "" || exp.Before(u.codes[oldest]) {
				oldest = k
			}
		}
		delete(u.codes, oldest)
	}
	c := randomToken()
	u.codes[c] = u.now().Add(loginCodeTTL)
	return c
}

// redeem exchanges a code for a session. A code works once, whether or not it was still valid.
func (u *webUI) redeem(code string) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pruneLocked()
	exp, ok := u.codes[code]
	delete(u.codes, code)
	if !ok || code == "" || !u.now().Before(exp) {
		return "", false
	}
	if len(u.sessions) >= 16 {
		for k := range u.sessions { // drop an arbitrary old one; the page just signs in again
			delete(u.sessions, k)
			break
		}
	}
	s := randomToken()
	u.sessions[s] = u.now().Add(sessionTTL)
	return s, true
}

func (u *webUI) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	exp, ok := u.sessions[c.Value]
	if !ok {
		return false
	}
	if !u.now().Before(exp) {
		delete(u.sessions, c.Value)
		return false
	}
	return true
}

// hostOK allows only the loopback names with our own port, so a hostile DNS name that points at
// 127.0.0.1 can't talk to us.
func (u *webUI) hostOK(r *http.Request) bool {
	p := u.getPort()
	return r.Host == fmt.Sprintf("127.0.0.1:%d", p) || strings.EqualFold(r.Host, fmt.Sprintf("localhost:%d", p))
}

// sameOrigin is false for anything a browser marks as cross-site.
func (u *webUI) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
		return false
	}
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		return false
	}
	return true
}

// loginURL is what the tray opens. section is "" or a page section such as "folders".
func (u *webUI) loginURL(section string) string {
	q := url.Values{"code": {u.NewCode()}}
	if validSection(section) {
		q.Set("to", section)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/ui/login?%s", u.getPort(), q.Encode())
}

func validSection(s string) bool {
	switch s {
	case "folders", "search", "conflicts", "trash", "activity", "account", "sync", "drive", "general", "logs", "about":
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func jsonOK(w http.ResponseWriter, err error) {
	if err != nil {
		jsonErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, 200, map[string]any{})
}

// readJSON decodes a small request body strictly.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		jsonErr(w, http.StatusBadRequest, fmt.Errorf("bad request: %v", err))
		return false
	}
	return true
}

// handler serves everything under /ui/.
func (u *webUI) handler(e *Engine) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/{$}", u.servePage)
	mux.HandleFunc("GET /ui/login", u.serveLogin)
	mux.HandleFunc("GET /ui/icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(iconPNG)
	})

	mux.HandleFunc("GET /ui/api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, e.uiState()) })
	mux.HandleFunc("GET /ui/api/account", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, e.uiAccount(r.Context())) })
	mux.HandleFunc("GET /ui/api/drive", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, e.driveStats()) })
	mux.HandleFunc("GET /ui/api/unsynced", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]int{"unsynced": e.UnsyncedCount(r.URL.Query().Get("path"))})
	})
	mux.HandleFunc("GET /ui/api/goto", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"section": u.takeSection()})
	})
	mux.HandleFunc("GET /ui/api/about", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, e.about()) })
	mux.HandleFunc("GET /ui/api/logs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, e.logSources()) })
	mux.HandleFunc("GET /ui/api/log", func(w http.ResponseWriter, r *http.Request) { e.serveLog(w, r) })

	mux.HandleFunc("POST /ui/api/log/clear", func(w http.ResponseWriter, r *http.Request) { e.clearLog(w, r) })
	mux.HandleFunc("POST /ui/api/config", func(w http.ResponseWriter, r *http.Request) { e.serveConfigPost(w, r) })
	mux.HandleFunc("POST /ui/api/sync", func(w http.ResponseWriter, r *http.Request) { e.SyncNow(); jsonOK(w, nil) })
	mux.HandleFunc("POST /ui/api/connect", func(w http.ResponseWriter, r *http.Request) { e.Connect(); jsonOK(w, nil) })
	mux.HandleFunc("POST /ui/api/eject", func(w http.ResponseWriter, r *http.Request) { jsonOK(w, e.Eject()) })
	mux.HandleFunc("POST /ui/api/reload", func(w http.ResponseWriter, r *http.Request) { jsonOK(w, e.Reload()) })
	mux.HandleFunc("POST /ui/api/setup", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			ServerURL string `json:"serverURL"` // typed on the page: no dialog needed
		}
		if !readJSON(w, r, &b) {
			return
		}
		if b.ServerURL != "" {
			if _, err := normalizeServerURL(b.ServerURL); err != nil {
				jsonErr(w, http.StatusBadRequest, err)
				return
			}
			go signInWith(e, b.ServerURL)
		} else {
			go setUpAndSignIn(e)
		}
		jsonOK(w, nil)
	})
	mux.HandleFunc("POST /ui/api/signout", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Force bool `json:"force"`
		}
		if !readJSON(w, r, &b) {
			return
		}
		if err := e.SignOut(b.Force); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "uploads": true})
			return
		}
		jsonOK(w, nil)
	})
	mux.HandleFunc("POST /ui/api/pause", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Paused *bool `json:"paused"`
		}
		if !readJSON(w, r, &b) {
			return
		}
		if b.Paused == nil {
			jsonErr(w, http.StatusBadRequest, errors.New(`bad request: "paused" (true or false) is missing`))
			return
		}
		jsonOK(w, e.SetPaused(*b.Paused))
	})
	mux.HandleFunc("GET /ui/api/subfolders", e.serveSubfolders)
	mux.HandleFunc("GET /ui/api/changes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"changes": e.changesForPage()})
	})
	mux.HandleFunc("GET /ui/api/items", func(w http.ResponseWriter, r *http.Request) {
		items, err := e.ListItems(r.URL.Query().Get("folder"), r.URL.Query().Get("sub"))
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("POST /ui/api/share", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Folder, Path, Password string
			Days                   int
		}
		if !readJSON(w, r, &b) {
			return
		}
		link, err := e.ShareLink(b.Folder, b.Path, ShareOptions{Days: b.Days, Password: b.Password})
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, 200, map[string]any{"url": link})
	})
	mux.HandleFunc("GET /ui/api/versions", func(w http.ResponseWriter, r *http.Request) {
		vs, err := e.FileVersions(r.URL.Query().Get("folder"), r.URL.Query().Get("path"))
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, 200, map[string]any{"versions": vs})
	})
	mux.HandleFunc("POST /ui/api/restore-version", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Folder, Path, Key string }
		if !readJSON(w, r, &b) {
			return
		}
		jsonOK(w, e.RestoreVersion(b.Folder, b.Path, b.Key))
	})
	mux.HandleFunc("GET /ui/api/search", func(w http.ResponseWriter, r *http.Request) {
		res, err := e.Search(r.URL.Query().Get("q"))
		if err != nil {
			jsonErr(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, 200, res)
	})
	mux.HandleFunc("GET /ui/api/trash", func(w http.ResponseWriter, r *http.Request) {
		l, err := e.ListTrash(r.URL.Query().Get("all") == "1")
		if err != nil {
			jsonErr(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, 200, l)
	})
	mux.HandleFunc("POST /ui/api/restore-trash", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Space, Key string }
		if !readJSON(w, r, &b) {
			return
		}
		jsonOK(w, e.RestoreTrash(b.Space, b.Key))
	})
	mux.HandleFunc("GET /ui/api/spaces", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		sp, err := listSpaces(ctx, e.config())
		if err != nil {
			jsonErr(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, 200, map[string]any{"spaces": append([]Space{}, sp...)})
	})
	mux.HandleFunc("POST /ui/api/browse", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Space, Path string }
		if !readJSON(w, r, &b) {
			return
		}
		dirs, err := e.BrowseSpace(b.Space, b.Path)
		if err != nil {
			jsonErr(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, 200, map[string]any{"folders": append([]string{}, dirs...)})
	})
	mux.HandleFunc("POST /ui/api/conflict", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Folder, File, Action string }
		if readJSON(w, r, &b) {
			jsonOK(w, e.ConflictAction(b.Folder, b.File, b.Action))
		}
	})
	mux.HandleFunc("POST /ui/api/check-updates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, e.CheckForUpdates(r.Context()))
	})
	mux.HandleFunc("POST /ui/api/keep", func(w http.ResponseWriter, r *http.Request) { e.serveKeep(w, r) })
	mux.HandleFunc("POST /ui/api/unkeep", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Path string }
		if !readJSON(w, r, &b) {
			return
		}
		jsonOK(w, e.Unkeep(b.Path, func(err error) {
			if err != nil {
				e.setMessage(err.Error())
			} else {
				e.setMessage(fmt.Sprintf("%q is now only in OpenCloud", b.Path))
			}
			e.notify()
		}))
	})
	mux.HandleFunc("POST /ui/api/repair", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Path string }
		if readJSON(w, r, &b) {
			jsonOK(w, e.Repair(b.Path))
		}
	})
	mux.HandleFunc("POST /ui/api/allow-delete", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Path string }
		if readJSON(w, r, &b) {
			jsonOK(w, e.AllowBigDeletion(b.Path))
		}
	})
	mux.HandleFunc("POST /ui/api/open", func(w http.ResponseWriter, r *http.Request) { e.serveOpen(w, r) })

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if !u.hostOK(r) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		// Only requests that change something must come from the page itself. Plain navigations
		// (the one-time login link, opening /ui/) may arrive from anywhere: the login code and the
		// SameSite=Strict session cookie protect them, and the browser won't send that cookie
		// with a cross-site read.
		changes := r.Method != http.MethodGet && r.Method != http.MethodHead
		if changes && !u.sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if changes && r.Header.Get(uiHeader) == "" {
			http.Error(w, "missing "+uiHeader+" header", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/ui/login" && !u.validSession(r) {
			if strings.HasPrefix(r.URL.Path, "/ui/api/") {
				jsonErr(w, http.StatusUnauthorized, fmt.Errorf("not signed in to the settings page; choose Settings… in the OpenCloud Bar menu"))
			} else {
				http.Error(w, "Not signed in. Choose Settings… in the OpenCloud Bar menu to open this page.", http.StatusUnauthorized)
			}
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (u *webUI) serveLogin(w http.ResponseWriter, r *http.Request) {
	sess, ok := u.redeem(r.URL.Query().Get("code"))
	if !ok {
		http.Error(w, "This sign-in link has expired or was already used. Choose Settings… in the OpenCloud Bar menu to open the page again.", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL / time.Second),
	})
	dest := "/ui/"
	if s := r.URL.Query().Get("to"); validSection(s) {
		dest += "#" + s
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (u *webUI) servePage(w http.ResponseWriter, r *http.Request) {
	nb := make([]byte, 16)
	_, _ = rand.Read(nb)
	nonce := base64.RawStdEncoding.EncodeToString(nb)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+
		"'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	_, _ = w.Write([]byte(strings.ReplaceAll(uiHTML, "{{NONCE}}", nonce)))
}

// MARK: state

// attentionFirst orders folders for menus: ones that need the user first, then the most
// recently synced.
func attentionFirst(in []FolderStatus) []FolderStatus {
	out := append([]FolderStatus(nil), in...)
	rank := func(f FolderStatus) int {
		switch f.State {
		case StNeedsRepair, StBigDeletion, StFailed:
			return 0
		}
		return 1
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i].LastSuccess.After(out[j].LastSuccess)
	})
	return out
}

// config returns a copy of the running settings.
func (e *Engine) config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

type uiFolder struct {
	FolderStatus
	Remote       string   `json:"remote"`
	ExtraFilters []string `json:"extraFilters"`
}

func (e *Engine) uiState() map[string]any {
	st := e.Status()
	cfg := e.config()
	folders := make([]uiFolder, 0, len(st.Folders))
	for _, f := range st.Folders {
		uf := uiFolder{FolderStatus: f, Remote: f.Path, ExtraFilters: []string{}}
		for _, k := range cfg.Kept {
			if k.Key() == f.Path {
				uf.Remote = k.Path
				uf.ExtraFilters = append(uf.ExtraFilters, k.ExtraFilters...)
			}
		}
		folders = append(folders, uf)
	}
	def := defaultConfig()
	mountEnabled := cfg.Mounting()
	return map[string]any{
		"status":     st,
		"folders":    folders,
		"configFile": configFile(),
		"logDir":     logDir(),
		"apiPort":    settingsUI.getPort(),
		"version":    version,
		"config": map[string]any{
			"syncIntervalSeconds":   cfg.SyncIntervalSeconds,
			"commonFilters":         nonNil(cfg.CommonFilters),
			"localRoot":             cfg.LocalRoot,
			"mountPoint":            cfg.MountPoint,
			"mountEnabled":          mountEnabled,
			"volumeName":            cfg.VolumeName,
			"cacheMaxSize":          cfg.CacheMaxSize,
			"cacheMaxAge":           cfg.CacheMaxAge,
			"minServerAvailableMiB": cfg.MinServerAvailableMiB,
			"transfers":             orDefault(cfg.Transfers, def.Transfers),
			"checkers":              orDefault(cfg.Checkers, def.Checkers),
			"useTunnel":             cfg.Tunnel(),
			"sshHost":               cfg.SSHHost,
			"serverPort":            cfg.ServerPort,
			"localPort":             cfg.LocalPort,
			"rclonePath":            cfg.RclonePath,
			"serverURL":             cfg.ServerURL,
			"oidc":                  cfg.OIDC(),
			"configured":            cfg.Configured(),
			"paused":                cfg.Paused,
			"checkForUpdates":       cfg.UpdateChecks(),
			"notifications":         cfg.NotificationsOn(),
			"bwLimitUp":             cfg.BwLimitUp,
			"bwLimitDown":           cfg.BwLimitDown,
			"liveSync":              cfg.LiveOn(),
			"syncParallel":          cfg.Parallel(),
		},
		"defaults": map[string]any{
			"syncIntervalSeconds": def.SyncIntervalSeconds,
			"commonFilters":       def.CommonFilters,
			"mountPoint":          def.MountPoint,
			"volumeName":          def.VolumeName,
			"cacheMaxSize":        def.CacheMaxSize,
			"cacheMaxAge":         def.CacheMaxAge,
			"transfers":           def.Transfers,
			"checkers":            def.Checkers,
		},
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orDefault(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}

// MARK: actions

func (e *Engine) serveKeep(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Path  string `json:"path"`  // empty: choose with the folder dialog
		Space string `json:"space"` // a space's id; empty for the personal space
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Space != "" {
		if strings.TrimSpace(b.Path) == "" {
			jsonErr(w, http.StatusBadRequest, errors.New("choose a folder in the space (the whole space can't be kept)"))
			return
		}
		jsonOK(w, e.KeepInSpace(b.Space, b.Path, ""))
		return
	}
	if b.Path != "" {
		jsonOK(w, e.Keep(b.Path, ""))
		return
	}
	st := e.Status()
	switch {
	case !e.config().Configured() || st.SignedOut:
		jsonErr(w, http.StatusConflict, fmt.Errorf("sign in first"))
	case !st.DriveMounted:
		jsonErr(w, http.StatusConflict, fmt.Errorf("folders are chosen from the drive: connect the drive first (Drive section), or type the folder's path instead"))
	default:
		go pickAndKeep(e) // a native dialog on the desktop; the page sees the result in the folder list
		jsonOK(w, nil)
	}
}

// serveOpen opens something on this computer. The page never sends a path or address: only
// what kind of thing, resolved here from the settings.
func (e *Engine) serveOpen(w http.ResponseWriter, r *http.Request) {
	var b struct{ Kind, Path, Folder string }
	if !readJSON(w, r, &b) {
		return
	}
	cfg := e.config()
	switch b.Kind {
	case "item": // a file or folder in a kept folder (Recent Changes): show it in its folder
		p, err := e.itemLocal(b.Folder, b.Path)
		if err != nil {
			jsonErr(w, http.StatusNotFound, err)
			return
		}
		if strings.Trim(b.Path, "/") == "" {
			openPath(p) // the kept folder itself, never its parent
		} else {
			openPath(filepath.Dir(p))
		}
	case "folder":
		f, ok := e.find(b.Path)
		if !ok {
			jsonErr(w, http.StatusNotFound, fmt.Errorf("%q isn't a kept folder", b.Path))
			return
		}
		local := cfg.Local(f)
		if !exists(local) {
			jsonErr(w, http.StatusNotFound, fmt.Errorf("the folder isn't on this computer yet"))
			return
		}
		openPath(local)
	case "drive":
		openPath(expand(cfg.MountPoint))
	case "web":
		base, err := normalizeServerURL(cfg.ServerURL)
		if err != nil {
			jsonErr(w, http.StatusConflict, fmt.Errorf("no server address is set up yet"))
			return
		}
		if err := openURL(base); err != nil {
			jsonErr(w, http.StatusConflict, err)
			return
		}
	case "search-web": // a search match in OpenCloud in the browser (its own id only)
		u, err := searchWebURL(cfg, b.Path)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err)
			return
		}
		if err := openURL(u); err != nil {
			jsonErr(w, http.StatusConflict, err)
			return
		}
	case "config":
		openPath(configFile())
	case "logs":
		openPath(logDir())
	case "project", "license", "rclone-license":
		links := map[string]string{"project": projectURL, "license": projectURL + "/blob/main/LICENSE",
			"rclone-license": "https://github.com/rclone/rclone/blob/master/COPYING"}
		if err := openURL(links[b.Kind]); err != nil {
			jsonErr(w, http.StatusConflict, err)
			return
		}
	case "release":
		// Only this project's release page, as GitHub reported it (checked in fetchLatestRelease).
		u := e.Status().Update.URL
		if !strings.HasPrefix(u, releasesPagePrefix) { // also checked when saved; never open anything else
			u = releasesPagePrefix + "latest"
		}
		if err := openURL(u); err != nil {
			jsonErr(w, http.StatusConflict, err)
			return
		}
	default:
		jsonErr(w, http.StatusBadRequest, fmt.Errorf("unknown kind %q", b.Kind))
		return
	}
	jsonOK(w, nil)
}

// driveStats reads the mounted drive's cache usage from rclone's remote control port.
func (e *Engine) driveStats() map[string]any {
	e.mu.Lock()
	running := e.mount != nil
	e.mu.Unlock()
	if !running {
		return map[string]any{"mounted": false}
	}
	out, err := e.rc("vfs/stats", 10*time.Second)
	if err != nil {
		return map[string]any{"mounted": true, "error": "rclone didn't answer"}
	}
	var v struct {
		DiskCache struct {
			BytesUsed         int64 `json:"bytesUsed"`
			UploadsInProgress int   `json:"uploadsInProgress"`
			UploadsQueued     int   `json:"uploadsQueued"`
		} `json:"diskCache"`
	}
	if json.Unmarshal(out, &v) != nil {
		return map[string]any{"mounted": true, "error": "couldn't read rclone's answer"}
	}
	return map[string]any{"mounted": true, "cacheBytes": v.DiskCache.BytesUsed,
		"uploadsInProgress": v.DiskCache.UploadsInProgress, "uploadsQueued": v.DiskCache.UploadsQueued}
}
