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
//	opencloud-bar keep PATH [--local DIR]
//	opencloud-bar unkeep PATH         final sync, then move the local copy out of the way
//	opencloud-bar repair PATH         merge both sides after "needs repair"
//	opencloud-bar allow-delete PATH   apply a blocked large deletion
//	opencloud-bar connect | eject     the on-demand drive
//	opencloud-bar settings            open the settings page in the browser
//	opencloud-bar reload | quit
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

func usage() {
	fmt.Fprintln(os.Stderr, `usage: opencloud-bar setup [URL] | logout | token | run [--no-tray] | status | sync | keep PATH [--local DIR] |
       unkeep PATH | repair PATH | allow-delete PATH | connect | eject | settings | reload | quit`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		// Started from a shortcut, Explorer or the Start menu: run the app.
		runDaemon(hasDisplay())
		return
	}
	switch os.Args[1] {
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
	case "sync", "connect", "eject", "reload", "quit":
		if err := call("POST", "/"+os.Args[1], nil, nil); err != nil {
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
		path := firstArg(fs, os.Args[2:])
		q := url.Values{"path": {path}, "local": {*local}}
		if err := call("POST", "/keep", q, nil); err != nil {
			fail(err)
		}
		fmt.Println("kept; the first sync starts shortly")
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
		fmt.Println("Repair merges both sides: the newer copy of each file wins, nothing is deleted, and files deleted since the last good sync come back.")
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

func fail(err error) { fmt.Fprintln(os.Stderr, "opencloud-bar:", err); os.Exit(1) }

func printStatus(st Status) {
	fmt.Println(st.Summary)
	if st.Message != "" {
		fmt.Println("  " + st.Message)
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

func call(method, path string, q url.Values, out any) error {
	cfg, _ := loadConfig()
	tok, err := apiToken(false)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("http://127.0.0.1:%d%s", cfg.APIPort(), path)
	if q != nil {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequest(method, u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("opencloud-bar isn't running (start it with `opencloud-bar run`)")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return errors.New(strings.TrimSpace(string(body)))
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// serveAPI starts the control API. It fails if the port is taken, which means another
// copy of opencloud-bar is already running.
func serveAPI(e *Engine, stop func()) error {
	tok, err := apiToken(true)
	if err != nil {
		return fmt.Errorf("api token: %w", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", e.cfg.APIPort()))
	if err != nil {
		return fmt.Errorf("opencloud-bar is already running (control port %d is in use)", e.cfg.APIPort())
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
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) { e.SyncNow(); reply(w, nil) })
	mux.HandleFunc("POST /connect", func(w http.ResponseWriter, r *http.Request) { e.Connect(); reply(w, nil) })
	mux.HandleFunc("POST /eject", func(w http.ResponseWriter, r *http.Request) { reply(w, e.Eject()) })
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) { reply(w, e.Reload()) })
	mux.HandleFunc("POST /keep", func(w http.ResponseWriter, r *http.Request) {
		reply(w, e.Keep(r.URL.Query().Get("path"), r.URL.Query().Get("local")))
	})
	mux.HandleFunc("POST /unkeep", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		reply(w, e.Unkeep(p, func(err error) {
			if err != nil {
				e.setMessage(err.Error())
			} else {
				e.setMessage(fmt.Sprintf("%q is now only in OpenCloud", p))
			}
		}))
	})
	mux.HandleFunc("POST /repair", func(w http.ResponseWriter, r *http.Request) { reply(w, e.Repair(r.URL.Query().Get("path"))) })
	mux.HandleFunc("POST /allow-delete", func(w http.ResponseWriter, r *http.Request) {
		reply(w, e.AllowBigDeletion(r.URL.Query().Get("path")))
	})
	mux.HandleFunc("POST /quit", func(w http.ResponseWriter, r *http.Request) { reply(w, nil); go stop() })
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
