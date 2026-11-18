// Command opencloud-bar keeps chosen OpenCloud folders synced both ways with rclone bisync,
// mounts the rest on demand with rclone mount. It signs in with the browser (OpenID Connect) or,
// for older setups, goes through an SSH tunnel with credentials in rclone.conf.
//
//	opencloud-bar setup [URL]         sign in with the browser (no SSH key or app token needed)
//	opencloud-bar logout              forget the sign-in on this computer
//	opencloud-bar token               print an access token (rclone runs this)
//	opencloud-bar [run [--no-tray]]   start (tray icon if a desktop is available)
//	opencloud-bar status              show what's going on
//	opencloud-bar sync                sync kept folders now
//	opencloud-bar pause | resume      stop or restart syncing kept folders (the drive keeps working)
//	opencloud-bar check-updates       look for a newer release
//	opencloud-bar version             print the version
//	opencloud-bar keep PATH [--local DIR] [--yes]
//	opencloud-bar keep-drive PATH [--yes] [--dialogs]   PATH is inside the OpenCloud drive (the Explorer menu item runs this)
//	opencloud-bar unkeep PATH         final sync, then move the local copy out of the way
//	opencloud-bar repair PATH         merge both sides after "needs repair"
//	opencloud-bar allow-delete PATH   apply a blocked large deletion
//	opencloud-bar connect | eject     the on-demand drive
//	opencloud-bar settings            open the settings page in the browser
//	opencloud-bar reload | quit [--force]
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// parsePauseArgs reads what follows "pause": nothing (until resumed) or --for DURATION (also
// --for=DURATION). It returns 0 for no duration.
func parsePauseArgs(args []string) (time.Duration, error) {
	switch {
	case len(args) == 0:
		return 0, nil
	case len(args) == 2 && args[0] == "--for":
		return pauseFor(args[1])
	case len(args) == 1 && strings.HasPrefix(args[0], "--for="):
		return pauseFor(strings.TrimPrefix(args[0], "--for="))
	}
	return 0, fmt.Errorf("usage: opencloud-bar pause [--for 1h]")
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: opencloud-bar setup [URL] | logout [--force] | token | run [--no-tray] | status | sync [PATH] | pause [--for 1h] | resume |
       keep PATH [--local DIR] [--yes] | keep-drive PATH [--yes] [--dialogs] | unkeep PATH | repair PATH | allow-delete PATH | connect | eject | settings |
       check-updates | version | reload | quit [--force]`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		// Started from a shortcut, Explorer or the Start menu: run the app.
		runDaemon(hasDisplay())
		return
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("opencloud-bar " + version)
	case "check-updates":
		var u UpdateState
		if err := call("POST", "/check-updates", nil, &u); err != nil {
			fail(err)
		}
		printUpdate(u)
	case "pause", "resume":
		q := url.Values{}
		if os.Args[1] == "pause" {
			d, err := parsePauseArgs(os.Args[2:])
			if err != nil {
				fmt.Fprintln(os.Stderr, "opencloud-bar:", err)
				os.Exit(2)
			}
			if d > 0 {
				q.Set("for", d.String())
			}
		}
		if err := call("POST", "/"+os.Args[1], q, nil); err != nil {
			fail(err)
		}
		if os.Args[1] == "pause" {
			if q.Get("for") != "" {
				fmt.Println("syncing paused for " + q.Get("for") + " (the drive keeps working); `opencloud-bar resume` restarts it sooner")
			} else {
				fmt.Println("syncing paused (the drive keeps working); `opencloud-bar resume` restarts it")
			}
		} else {
			fmt.Println("syncing resumed")
		}
	case "quit":
		q := url.Values{}
		if len(os.Args) > 2 && os.Args[2] == "--force" {
			q.Set("force", "1")
		}
		// Exit codes for installers: 0 stopped (or wasn't running), 2 refused because files may
		// still be uploading (the user should quit from the tray, which asks), 1 anything else.
		if err := call("POST", "/quit", q, nil); errors.Is(err, errNotRunning) {
			fmt.Println("opencloud-bar isn't running")
			return
		} else if err != nil {
			fmt.Fprintln(os.Stderr, "opencloud-bar:", err)
			if strings.Contains(err.Error(), "--force") {
				os.Exit(2)
			}
			os.Exit(1)
		}
		// Wait until it has stopped (a running sync saves its state, the drive is ejected), so
		// an installer can replace the files right after.
		for i := 0; i < 200; i++ {
			if call("GET", "/status", nil, &Status{}) != nil {
				break
			}
			time.Sleep(time.Second)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		noTray := fs.Bool("no-tray", false, "run headless (servers)")
		_ = fs.Parse(os.Args[2:])
		runDaemon(!*noTray && hasDisplay())
	case "status":
		var st Status
		if err := call("GET", "/status", nil, &st); err != nil {
			fail(err)
		}
		printStatus(st)
	case "sync", "connect", "eject", "reload":
		q := url.Values{}
		if os.Args[1] == "sync" && len(os.Args) > 2 {
			q.Set("path", os.Args[2]) // one kept folder
		}
		if err := call("POST", "/"+os.Args[1], q, nil); err != nil {
			fail(err)
		}
	case "settings":
		var out struct{ URL string }
		if err := call("POST", "/settings-link", nil, &out); err != nil {
			fail(err)
		}
		if err := openURL(out.URL); err != nil {
			fmt.Println("open this address in your browser within a minute:\n" + out.URL)
		}
	case "settings-window": // internal: started by the tray's Settings…
		cmdSettingsWindow(os.Args[2:])
	case "setup":
		cmdSetup(os.Args[2:])
	case "token":
		cmdToken()
	case "logout":
		cmdLogout()
	case "keep":
		fs := flag.NewFlagSet("keep", flag.ExitOnError)
		local := fs.String("local", "", "absolute local folder (default: localRoot/PATH)")
		yes := fs.Bool("yes", false, "keep it even if it is big or may not fit on this computer")
		path := firstArg(fs, os.Args[2:])
		sc, err := daemonKeep(path, *local, *yes)
		if err != nil {
			fail(err)
		}
		if sc.NeedsConfirm {
			fmt.Fprintln(os.Stderr, "opencloud-bar: "+sc.Message)
			fmt.Fprintln(os.Stderr, "Nothing was kept. Run the command again with --yes to keep it anyway.")
			os.Exit(1)
		}
		fmt.Println("kept; the first sync starts shortly")
	case "keep-drive":
		// The file manager's "Keep on this PC with OpenCloud Bar" (Windows installer), or a
		// script: PATH must be inside the OpenCloud drive. The Explorer verb runs the windowed
		// "OpenCloud Bar.exe" (same main, so it takes subcommands; no console flash) with
		// --dialogs; without it, dialogs are used only where there's no terminal (autoDialogs).
		cfg, _ := loadConfig()
		env := keepDriveEnv{mount: expand(cfg.MountPoint), out: os.Stdout, errw: os.Stderr,
			dialogs: autoDialogs(),
			alert: func(title, text string) {
				if hasDialogs() {
					dialogAlert(title, text)
				}
			},
			confirm: func(title, text, button string) bool { return hasDialogs() && dialogConfirm(title, text, button) },
			keep:    func(rel string, confirm bool) (SizeCheck, error) { return daemonKeep(rel, "", confirm) }}
		os.Exit(env.run(os.Args[2:]))
	case "unkeep":
		path := needPath()
		var n struct{ Unsynced int }
		_ = call("GET", "/unsynced", url.Values{"path": {path}}, &n)
		fmt.Printf("Stop keeping %q here? Changes are synced first, then the local copy is moved to %s.\n", path, trashName())
		if n.Unsynced > 0 {
			fmt.Printf("%d file(s) in it are never synced (build folders, excluded items) and exist only here.\n", n.Unsynced)
		}
		if !askYes() {
			return
		}
		if err := call("POST", "/unkeep", url.Values{"path": {path}}, nil); err != nil {
			fail(err)
		}
		fmt.Println("started; see `opencloud-bar status`")
	case "repair":
		path := needPath()
		fmt.Println("Repair merges both sides: the newer copy of each file wins (the older copy is kept in .opencloud-bar-backups in OpenCloud, or in the backups folder on this computer), and files deleted since the last good sync come back.")
		if askYes() {
			if err := call("POST", "/repair", url.Values{"path": {path}}, nil); err != nil {
				fail(err)
			}
		}
	case "allow-delete":
		path := needPath()
		fmt.Println("More than half the files were deleted on one side. This applies the deletion to the other side.")
		if askYes() {
			if err := call("POST", "/allow-delete", url.Values{"path": {path}}, nil); err != nil {
				fail(err)
			}
		}
	default:
		usage()
	}
}

func firstArg(fs *flag.FlagSet, args []string) string {
	// allow flags after the path
	var path string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if path == "" {
		path = fs.Arg(0)
	}
	if path == "" {
		usage()
	}
	return path
}

func needPath() string {
	if len(os.Args) < 3 {
		usage()
	}
	return os.Args[2]
}

func askYes() bool {
	fmt.Print("Continue? [y/N] ")
	var a string
	_, _ = fmt.Scanln(&a)
	return strings.EqualFold(a, "y") || strings.EqualFold(a, "yes")
}

// daemonKeep asks the running app to keep path (relative to the drive). Without confirm a big
// folder isn't kept: the answer has NeedsConfirm set.
func daemonKeep(path, local string, confirm bool) (SizeCheck, error) {
	q := url.Values{"path": {path}, "local": {local}}
	if confirm {
		q.Set("confirm", "1")
	}
	status, body, err := callRaw("POST", "/keep", q, keepCallTimeout)
	if err != nil {
		return SizeCheck{}, err
	}
	var sc SizeCheck
	switch {
	case status == http.StatusOK:
		err = json.Unmarshal(body, &sc)
		return sc, err
	case status == http.StatusConflict && json.Unmarshal(body, &sc) == nil && sc.NeedsConfirm:
		return sc, nil // 409 with {needsConfirm, size, free, message}: not kept yet
	}
	return SizeCheck{}, errors.New(strings.TrimSpace(string(body)))
}

// keepCallTimeout: the app's size check can take 10 s, and keeping a folder does a little more.
const keepCallTimeout = 45 * time.Second

func fail(err error) { fmt.Fprintln(os.Stderr, "opencloud-bar:", err); os.Exit(1) }

func printStatus(st Status) {
	fmt.Println(st.Summary)
	if st.Message != "" {
		fmt.Println("  " + st.Message)
	}
	if st.Paused && st.PausedUntil != "" {
		if t, err := time.Parse(time.RFC3339, st.PausedUntil); err == nil {
			fmt.Println("  syncing is paused until " + t.Local().Format("Mon 15:04") + " (opencloud-bar resume)")
		}
	} else if st.Paused {
		fmt.Println("  syncing is paused (opencloud-bar resume)")
	}
	if st.Update.Available {
		fmt.Printf("  update available: %s (%s)\n", st.Update.Latest, st.Update.URL)
	}
	if st.UseTunnel {
		fmt.Printf("  tunnel: %v   drive: mounted=%v wanted=%v\n", st.TunnelUp, st.DriveMounted, st.DriveWanted)
	} else {
		fmt.Printf("  drive: mounted=%v wanted=%v\n", st.DriveMounted, st.DriveWanted)
	}
	for _, f := range st.Folders {
		line := fmt.Sprintf("  %-30s %s", f.Name, f.State)
		if f.Detail != "" {
			line += " (" + f.Detail + ")"
		}
		if !f.LastSuccess.IsZero() {
			line += ", synced " + ago(f.LastSuccess)
		}
		fmt.Println(line)
	}
}

func printUpdate(u UpdateState) {
	switch {
	case u.Error != "":
		fmt.Println("couldn't check for updates: " + u.Error)
	case u.Available:
		fmt.Printf("update available: %s (running %s)\n%s\n", u.Latest, u.Running, u.URL)
	case u.DevBuild:
		fmt.Printf("development build %s; latest release is %s\n", u.Running, u.Latest)
	default:
		fmt.Printf("up to date (%s; latest release %s)\n", u.Running, u.Latest)
	}
}

// MARK: local control API (127.0.0.1 only, token-protected)

func tokenFile() string { return filepath.Join(appDir(), "api-token") }

func apiToken(create bool) (string, error) {
	if b, err := os.ReadFile(tokenFile()); err == nil && len(b) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	if !create {
		return "", errors.New("not running (no API token yet)")
	}
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	t := hex.EncodeToString(buf)
	_ = os.MkdirAll(appDir(), 0o700)
	return t, os.WriteFile(tokenFile(), []byte(t), 0o600)
}

var errNotRunning = errors.New("opencloud-bar isn't running (start it with `opencloud-bar run`)")

func call(method, path string, q url.Values, out any) error {
	status, body, err := callRaw(method, path, q, 30*time.Second)
	if err != nil {
		return err
	}
	if status != 200 {
		return errors.New(strings.TrimSpace(string(body)))
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// callRaw sends a request to the running app and returns the status and body.
func callRaw(method, path string, q url.Values, timeout time.Duration) (int, []byte, error) {
	cfg, _ := loadConfig()
	tok, err := apiToken(false)
	if err != nil {
		return 0, nil, errNotRunning
	}
	u := fmt.Sprintf("http://127.0.0.1:%d%s", cfg.APIPort(), path)
	if q != nil {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequest(method, u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, nil, errNotRunning
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// serveAPI starts the control API. It fails if the port is taken, which means another
// copy of opencloud-bar is already running.
func serveAPI(e *Engine, stop func()) error {
	tok, err := apiToken(true)
	if err != nil {
		return fmt.Errorf("api token: %w", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", e.config().APIPort()))
	if err != nil {
		return fmt.Errorf("opencloud-bar is already running (control port %d is in use)", e.config().APIPort())
	}
	settingsUI.setPort(ln.Addr().(*net.TCPAddr).Port)
	go func() { _ = http.Serve(ln, apiHandler(e, stop, tok, settingsUI)) }()
	return nil
}

// apiHandler is the whole local server: the bearer-token control API (CLI and scripts) and the
// cookie-protected settings page under /ui/ (see ui.go). The two never accept each other's credentials.
func apiHandler(e *Engine, stop func(), tok string, ui *webUI) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, err error) {
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(e.Status()) })
	mux.HandleFunc("GET /unsynced", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"Unsynced": e.UnsyncedCount(r.URL.Query().Get("path"))})
	})
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Query().Get("path"); p != "" {
			reply(w, e.SyncFolder(p))
			return
		}
		e.SyncNow()
		reply(w, nil)
	})
	mux.HandleFunc("POST /connect", func(w http.ResponseWriter, r *http.Request) { e.Connect(); reply(w, nil) })
	mux.HandleFunc("POST /eject", func(w http.ResponseWriter, r *http.Request) { reply(w, e.Eject()) })
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) { reply(w, e.Reload()) })
	mux.HandleFunc("POST /keep", func(w http.ResponseWriter, r *http.Request) {
		// Without confirm=1 a folder that is big (or bigger than the free space) isn't kept: the
		// answer is 409 with {"needsConfirm": true, "size": …, "free": …, "message": …}
		// (keepcheck.go), never a 200 that could be taken for "kept".
		q := r.URL.Query()
		sc, err := e.KeepChecked("", q.Get("path"), q.Get("local"), q.Get("confirm") == "1")
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		if sc.NeedsConfirm {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(w).Encode(sc)
	})
	mux.HandleFunc("POST /unkeep", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		_, err := e.Unkeep(p, func(err error) {
			if err != nil {
				e.setMessage(err.Error())
			} else {
				e.setMessage(fmt.Sprintf("%q is now only in OpenCloud", p))
			}
		})
		reply(w, err)
	})
	mux.HandleFunc("POST /repair", func(w http.ResponseWriter, r *http.Request) {
		_, err := e.Repair(r.URL.Query().Get("path"))
		reply(w, err)
	})
	mux.HandleFunc("POST /allow-delete", func(w http.ResponseWriter, r *http.Request) {
		// The command line has no dialog: it allows what is blocked right now.
		p := r.URL.Query().Get("path")
		var err error
		if f, ok := e.find(p); !ok {
			err = fmt.Errorf("%q isn't a kept folder", p)
		} else {
			side, gen, _ := e.bigDeletionDetail(f.Key())
			_, err = e.AllowBigDeletion(p, side, gen)
		}
		reply(w, err)
	})
	mux.HandleFunc("POST /quit", func(w http.ResponseWriter, r *http.Request) {
		// Fail closed: files written to the drive may still be uploading.
		if r.URL.Query().Get("force") == "" {
			if err := e.uploadsBlock(); err != nil {
				reply(w, fmt.Errorf("%v; quit with --force to stop anyway (they upload next time)", err))
				return
			}
		}
		reply(w, nil)
		go stop()
	})
	mux.HandleFunc("GET /pending", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"pending": e.PendingUploads()})
	})
	mux.HandleFunc("POST /open-settings", func(w http.ResponseWriter, r *http.Request) {
		go openSettings(r.URL.Query().Get("section"))
		reply(w, nil)
	})
	mux.HandleFunc("POST /pause", func(w http.ResponseWriter, r *http.Request) {
		if f := r.URL.Query().Get("for"); f != "" {
			d, err := pauseFor(f)
			if err != nil {
				reply(w, err)
				return
			}
			reply(w, e.PauseUntil(time.Now().Add(d)))
			return
		}
		reply(w, e.SetPaused(true))
	})
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, r *http.Request) { reply(w, e.SetPaused(false)) })
	mux.HandleFunc("POST /check-updates", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(e.CheckForUpdates(r.Context()))
	})
	// A one-time link to the settings page, for `opencloud-bar settings`.
	mux.HandleFunc("POST /settings-link", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"url": ui.loginURL(r.URL.Query().Get("section"))})
	})
	want := []byte("Bearer " + tok)
	auth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})
	root := http.NewServeMux()
	root.Handle("/ui/", ui.handler(e))
	root.Handle("/", auth)
	return root
}

func runDaemon(tray bool) {
	e := NewEngine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	stop := func() { cancel(); <-done }
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; cancel() }()
	if err := serveAPI(e, stop); err != nil {
		// Already running (started again from the Start menu, or a notification was clicked):
		// show that copy's settings instead of an error.
		if tray && call("POST", "/open-settings", nil, nil) == nil {
			return
		}
		e.logLine("not started: %v", err)
		if tray {
			dialogAlert("OpenCloud Bar", err.Error())
		}
		fail(err)
	}
	go func() { e.Run(ctx); close(done) }()
	e.logLine("started (tray=%v)", tray)
	if tray {
		runTray(e, ctx, func() { cancel(); <-done })
		return
	}
	<-done
}
