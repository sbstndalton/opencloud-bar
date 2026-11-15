package main

// Browser sign-in for OpenCloud (OpenID Connect, authorization code flow with PKCE).
//
//	login:        discovery -> browser on the server's own login page -> loopback redirect ->
//	              code exchange -> userinfo. Nothing is typed into this app.
//	storage:      the refresh token goes in the OS credential store (file fallback); the
//	              short-lived access token is cached in auth.json (0600) with its expiry.
//	accessToken:  what `opencloud-bar token` prints, and what rclone's bearer_token_command runs.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	oidcClientID   = "OpenCloudDesktop" // OpenCloud's public desktop client: no secret
	oidcScopes     = "openid profile email offline_access"
	keyringService = "OpenCloud Bar"
	refreshMargin  = 60 * time.Second // refresh when less than this is left
	loginTimeout   = 5 * time.Minute
)

// errSignedOut means the stored sign-in is gone or was revoked; the user must sign in again.
var errSignedOut = errors.New("signed out: sign in again")

// Replaced in tests.
var (
	openBrowser = openURL
	httpClient  = &http.Client{Timeout: 20 * time.Second}
)

type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// authState is auth.json: who is signed in and the cached access token. No refresh token here.
type authState struct {
	ServerURL     string    `json:"serverURL"`
	Issuer        string    `json:"issuer"`
	Username      string    `json:"username"`
	TokenEndpoint string    `json:"tokenEndpoint"` // from discovery at sign-in
	Access        string    `json:"accessToken,omitempty"`
	Expiry        time.Time `json:"expiry,omitempty"`
}

func (s authState) account() string { return s.Issuer + "|" + s.Username }

func authFile() string        { return filepath.Join(appDir(), "auth.json") }
func authLockFile() string    { return filepath.Join(appDir(), "auth.lock") }
func refreshFallback() string { return filepath.Join(appDir(), "refresh-token") }

// MARK: server address and discovery

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// normalizeServerURL turns what a person types ("cloud.example.com") into https://cloud.example.com.
// Plain http is only accepted for loopback addresses (development).
func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("enter the server address, for example https://cloud.example.com")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q isn't a valid server address", raw)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return "", errors.New("the server address must start with https://")
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func checkEndpoint(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return fmt.Errorf("the server's %s (%q) isn't a secure URL", name, raw)
	}
	return nil
}

// discover reads <server>/.well-known/openid-configuration. Endpoints are never hard-coded.
func discover(ctx context.Context, serverURL string) (discovery, error) {
	var d discovery
	req, _ := http.NewRequestWithContext(ctx, "GET", serverURL+"/.well-known/openid-configuration", nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return d, fmt.Errorf("can't reach %s: %w", serverURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return d, fmt.Errorf("%s doesn't look like an OpenCloud server (sign-in settings: HTTP %d)", serverURL, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return d, fmt.Errorf("%s returned unreadable sign-in settings: %w", serverURL, err)
	}
	if d.Issuer == "" || d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.UserinfoEndpoint == "" {
		return d, fmt.Errorf("%s sign-in settings are incomplete", serverURL)
	}
	for n, v := range map[string]string{"sign-in page": d.AuthorizationEndpoint, "token endpoint": d.TokenEndpoint, "userinfo endpoint": d.UserinfoEndpoint} {
		if err := checkEndpoint(n, v); err != nil {
			return d, err
		}
	}
	return d, nil
}

// MARK: PKCE and the loopback redirect

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no randomness, no safe sign-in
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkcePair returns a verifier (43 chars, RFC 7636) and its S256 challenge.
func pkcePair() (verifier, challenge string) {
	verifier = randomString(32)
	return verifier, pkceChallenge(verifier)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

const callbackPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>OpenCloud Bar</title><body style="font:16px system-ui,sans-serif;text-align:center;margin-top:20vh">
<h2>%s</h2><p>%s</p>`

// waitForCode serves the redirect on ln until a request with the right state arrives.
// Requests with a wrong or missing state are refused and ignored (anything on this machine can
// reach the port; only the browser we opened knows the state).
func waitForCode(ctx context.Context, ln net.Listener, state string) (string, error) {
	type result struct {
		code string
		err  error
	}
	ch := make(chan result, 1)
	page := func(w http.ResponseWriter, status int, title, body string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(status)
		fmt.Fprintf(w, callbackPage, html.EscapeString(title), html.EscapeString(body))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("state") != state {
			page(w, 400, "Sign-in not recognised", "This link doesn't belong to the sign-in that OpenCloud Bar started. Go back to OpenCloud Bar and try again.")
			return
		}
		if e := q.Get("error"); e != "" {
			page(w, 200, "Sign-in cancelled", "You can close this tab and return to OpenCloud Bar.")
			select {
			case ch <- result{err: fmt.Errorf("the server refused the sign-in: %s %s", e, q.Get("error_description"))}:
			default:
			}
			return
		}
		code := q.Get("code")
		if code == "" {
			page(w, 400, "Sign-in incomplete", "The server didn't send a sign-in code.")
			return
		}
		page(w, 200, "You're signed in", "You can close this tab and return to OpenCloud Bar.")
		select {
		case ch <- result{code: code}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		// Shutdown (not Close) so the browser gets the "you can close this tab" page.
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	select {
	case r := <-ch:
		return r.code, r.err
	case <-ctx.Done():
		return "", errors.New("timed out waiting for the sign-in in the browser")
	}
}

// MARK: token endpoint

type oauthError struct {
	Status int
	Code   string
	Desc   string
}

func (e *oauthError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("server error: %s %s", e.Code, e.Desc)
	}
	return fmt.Sprintf("server error: HTTP %d", e.Status)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func postToken(ctx context.Context, endpoint string, form url.Values) (tokenResponse, error) {
	var tr tokenResponse
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return tr, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode != 200 || tr.Error != "" {
		return tr, &oauthError{Status: resp.StatusCode, Code: tr.Error, Desc: tr.ErrorDesc}
	}
	if tr.AccessToken == "" {
		return tr, errors.New("the server's token response had no access token")
	}
	return tr, nil
}

// expiryOf converts expires_in to a time; the server says about 300 s, and 300 is the guess.
func expiryOf(tr tokenResponse) time.Time {
	n := tr.ExpiresIn
	if n <= 0 {
		n = 300
	}
	return time.Now().Add(time.Duration(n) * time.Second)
}

// MARK: login

type loginResult struct {
	ServerURL string
	Disc      discovery
	Username  string
	Refresh   string
	Access    string
	Expiry    time.Time
}

// login runs the whole browser flow. announce (optional) gets the sign-in URL, for printing
// when the browser can't be opened.
func login(ctx context.Context, rawURL string, announce func(authURL string)) (*loginResult, error) {
	serverURL, err := normalizeServerURL(rawURL)
	if err != nil {
		return nil, err
	}
	d, err := discover(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	// Loopback on a random free port; the server accepts any port for 127.0.0.1.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("can't listen for the sign-in: %w", err)
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	verifier, challenge := pkcePair()
	state := randomString(16)

	au, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		ln.Close()
		return nil, err
	}
	q := au.Query()
	q.Set("response_type", "code")
	q.Set("client_id", oidcClientID)
	q.Set("redirect_uri", redirect)
	q.Set("scope", oidcScopes)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	au.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	type got struct {
		code string
		err  error
	}
	codeCh := make(chan got, 1)
	go func() { c, err := waitForCode(ctx, ln, state); codeCh <- got{c, err} }()
	if announce != nil {
		announce(au.String())
	}
	if err := openBrowser(au.String()); err != nil {
		appLog("couldn't open the browser: %v", err)
		// Not fatal: the URL was announced and the user may open it by hand.
	}
	g := <-codeCh
	if g.err != nil {
		return nil, g.err
	}

	tr, err := postToken(ctx, d.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {g.code},
		"redirect_uri":  {redirect},
		"client_id":     {oidcClientID},
		"code_verifier": {verifier},
	})
	if err != nil {
		return nil, fmt.Errorf("sign-in failed at the token step: %w", err)
	}
	if tr.RefreshToken == "" {
		return nil, errors.New("the server didn't issue a refresh token (offline_access); sign-in can't be kept")
	}
	user, err := fetchUsername(ctx, d.UserinfoEndpoint, tr.AccessToken)
	if err != nil {
		return nil, err
	}
	return &loginResult{ServerURL: serverURL, Disc: d, Username: user,
		Refresh: tr.RefreshToken, Access: tr.AccessToken, Expiry: expiryOf(tr)}, nil
}

func fetchUsername(ctx context.Context, endpoint, access string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't read your account: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("couldn't read your account: HTTP %d", resp.StatusCode)
	}
	var ui struct {
		PreferredUsername string `json:"preferred_username"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ui); err != nil || ui.PreferredUsername == "" {
		return "", errors.New("the server didn't say which user signed in (no preferred_username)")
	}
	return ui.PreferredUsername, nil
}

// signIn is the whole setup: browser sign-in, keep the credentials, record them in config.json.
func signIn(ctx context.Context, rawURL string, announce func(string)) (username string, err error) {
	res, err := login(ctx, rawURL, announce)
	if err != nil {
		return "", err
	}
	if err := storeSignIn(res); err != nil {
		return "", err
	}
	if err := applySetup(res.ServerURL, res.Username); err != nil {
		return "", err
	}
	appLog("signed in as %s on %s", res.Username, res.ServerURL)
	return res.Username, nil
}

// MARK: storage

var authMu sync.Mutex // serialises auth.json and refreshes inside this process

func readAuthState() (authState, error) {
	var st authState
	b, err := os.ReadFile(authFile())
	if errors.Is(err, fs.ErrNotExist) {
		return st, errSignedOut
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil || st.Issuer == "" || st.Username == "" {
		return st, errSignedOut
	}
	return st, nil
}

func writeAuthState(st authState) error {
	if err := os.MkdirAll(appDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := authFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, authFile())
}

// saveRefreshToken uses the OS credential store. Where there isn't one (a headless Linux server
// without Secret Service) or the token doesn't fit (Windows allows 2560 bytes), a 0600 file in the
// app folder is used and the log says so.
func saveRefreshToken(account, token string) error {
	if err := keyring.Set(keyringService, account, token); err == nil {
		_ = os.Remove(refreshFallback())
		return nil
	} else {
		appLog("credential store unavailable (%v); keeping the refresh token in %s", err, refreshFallback())
	}
	if err := os.MkdirAll(appDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(refreshFallback(), []byte(token), 0o600)
}

func loadRefreshToken(account string) (string, bool) {
	if t, err := keyring.Get(keyringService, account); err == nil && t != "" {
		return t, true
	}
	if b, err := os.ReadFile(refreshFallback()); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b)), true
	}
	return "", false
}

func deleteRefreshToken(account string) {
	_ = keyring.Delete(keyringService, account)
	_ = os.Remove(refreshFallback())
}

func storeSignIn(r *loginResult) error {
	authMu.Lock()
	defer authMu.Unlock()
	if old, err := readAuthState(); err == nil && old.account() != r.Disc.Issuer+"|"+r.Username {
		deleteRefreshToken(old.account()) // switching accounts: forget the old one
	}
	st := authState{ServerURL: r.ServerURL, Issuer: r.Disc.Issuer, Username: r.Username,
		TokenEndpoint: r.Disc.TokenEndpoint, Access: r.Access, Expiry: r.Expiry}
	if err := saveRefreshToken(st.account(), r.Refresh); err != nil {
		return fmt.Errorf("couldn't keep the sign-in: %w", err)
	}
	return writeAuthState(st)
}

// haveCredentials says whether a refresh token is stored for this config's server.
func haveCredentials(cfg Config) bool {
	want, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return false
	}
	st, err := readAuthState()
	if err != nil || st.ServerURL != want {
		return false
	}
	_, ok := loadRefreshToken(st.account())
	return ok
}

// logout forgets the sign-in on this computer. (OpenCloud's discovery lists no revocation
// endpoint, so the refresh token simply stops being used.)
func logout() {
	authMu.Lock()
	defer authMu.Unlock()
	if st, err := readAuthState(); err == nil {
		deleteRefreshToken(st.account())
	} else {
		_ = os.Remove(refreshFallback())
	}
	_ = os.Remove(authFile())
	appLog("signed out")
}

// MARK: access token for rclone

// lockFile is a cross-process lock (portable: no flock): a file created exclusively, with
// stale locks from a crashed holder removed after 90 s (longer than a refresh can take).
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(100 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > 90*time.Second {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for another opencloud-bar to refresh the sign-in")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// accessToken returns an access token valid for at least refreshMargin. It uses the cached one
// when fresh, else refreshes (once, even with several rclone processes asking at the same time).
// A revoked or missing sign-in gives errSignedOut.
func accessToken(serverURL string) (string, error) {
	want, err := normalizeServerURL(serverURL)
	if err != nil {
		return "", errSignedOut
	}
	fresh := func(st authState) bool {
		return st.ServerURL == want && st.Access != "" && time.Until(st.Expiry) > refreshMargin
	}
	authMu.Lock()
	defer authMu.Unlock()
	st, err := readAuthState()
	if err != nil {
		return "", err
	}
	if st.ServerURL != want {
		return "", errSignedOut // config points at a different server than the one signed in to
	}
	if fresh(st) {
		return st.Access, nil
	}
	unlock, err := lockFile(authLockFile())
	if err != nil {
		return "", err
	}
	defer unlock()
	// Another process may have refreshed while we waited for the lock.
	if st, err = readAuthState(); err != nil {
		return "", err
	}
	if fresh(st) {
		return st.Access, nil
	}
	refresh, ok := loadRefreshToken(st.account())
	if !ok {
		return "", errSignedOut
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tr, err := postToken(ctx, st.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {oidcClientID},
	})
	if err != nil {
		var oe *oauthError
		if errors.As(err, &oe) && oe.Code == "invalid_grant" {
			appLog("refresh token rejected (invalid_grant); signed out")
			deleteRefreshToken(st.account())
			st.Access, st.Expiry = "", time.Time{}
			_ = writeAuthState(st)
			return "", errSignedOut
		}
		return "", fmt.Errorf("couldn't refresh the sign-in: %w", err)
	}
	// The server may rotate the refresh token; if it doesn't send one, the old one stays valid.
	if tr.RefreshToken != "" && tr.RefreshToken != refresh {
		if err := saveRefreshToken(st.account(), tr.RefreshToken); err != nil {
			return "", err
		}
	}
	st.Access, st.Expiry = tr.AccessToken, expiryOf(tr)
	if err := writeAuthState(st); err != nil {
		return "", err
	}
	return st.Access, nil
}

// bearerTokenCommand is the value for rclone's bearer_token_command (set through
// RCLONE_WEBDAV_BEARER_TOKEN_COMMAND). rclone splits it on spaces and honours double quotes,
// and the program path usually has spaces ("C:\Program Files\...", "/Applications/OpenCloud
// Bar.app/..."), so the path is always quoted. extra are extra arguments after the path.
func bearerTokenCommand(exe string, extra ...string) string {
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	parts := []string{q(exe)}
	for _, a := range extra {
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// cliExecutable is the program rclone should run for tokens. On Windows the tray app is built
// as a GUI program with no console, so `OpenCloud Bar.exe token` couldn't print anything; the
// console `opencloud-bar.exe` installed next to it is used instead.
func cliExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return "opencloud-bar"
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if runtime.GOOS == "windows" {
		cli := filepath.Join(filepath.Dir(exe), "opencloud-bar.exe")
		if !strings.EqualFold(filepath.Base(exe), "opencloud-bar.exe") && exists(cli) {
			return cli
		}
	}
	return exe
}

// tokenCommand is the full bearer_token_command for this install.
func tokenCommand() string { return bearerTokenCommand(cliExecutable(), "token") }

// MARK: command line

// cmdToken prints a valid access token and nothing else (rclone reads stdout).
func cmdToken() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "opencloud-bar:", err)
		os.Exit(1)
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "opencloud-bar:", err)
		os.Exit(1)
	}
	fmt.Println(tok)
}

// cmdSetup: opencloud-bar setup [URL]
func cmdSetup(args []string) {
	var raw string
	if len(args) > 0 {
		raw = args[0]
	} else {
		fmt.Print("OpenCloud server address (for example https://cloud.example.com): ")
		var line string
		_, _ = fmt.Scanln(&line)
		raw = line
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout+time.Minute)
	defer cancel()
	user, err := signIn(ctx, raw, func(u string) {
		fmt.Println("Opening your browser to sign in. If it doesn't open, visit:\n  " + u)
	})
	if err != nil {
		fail(err)
	}
	fmt.Printf("Signed in as %s.\n", user)
	// If the app is already running, make it pick up the new settings; otherwise nothing to do.
	if err := call("POST", "/reload", nil, nil); err != nil {
		fmt.Println("Start the app with `opencloud-bar run` (or from the Start menu) to begin.")
	}
	fmt.Println("Next: choose a folder to keep on this computer with\n  opencloud-bar keep PATH\n(PATH is relative to your OpenCloud drive, for example Documents/Work).")
}

func cmdLogout() {
	logout()
	_ = call("POST", "/reload", nil, nil)
	fmt.Println("Signed out.")
}

func newLoopbackListener() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
