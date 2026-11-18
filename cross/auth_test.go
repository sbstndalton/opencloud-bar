package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// fakeOIDC is a stand-in OpenCloud/Konnect: discovery, authorize, token and userinfo.
type fakeOIDC struct {
	srv *httptest.Server
	mu  sync.Mutex

	challenges   map[string]string // code -> PKCE challenge
	lastAuth     url.Values        // query of the last authorize request
	exchanges    int
	refreshCalls int
	refreshToken string // the one currently valid
	rotate       bool   // send a new refresh token on refresh
	revoked      bool   // refresh answers invalid_grant
	seq          int
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	f := &fakeOIDC{challenges: map[string]string{}, refreshToken: "refresh-0"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.srv.URL,
			"authorization_endpoint": f.srv.URL + "/signin/v1/identifier/_/authorize",
			"token_endpoint":         f.srv.URL + "/konnect/v1/token",
			"userinfo_endpoint":      f.srv.URL + "/konnect/v1/userinfo",
		})
	})
	mux.HandleFunc("/signin/v1/identifier/_/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.lastAuth = q
		f.seq++
		code := fmt.Sprintf("code-%d", f.seq)
		f.challenges[code] = q.Get("code_challenge")
		f.mu.Unlock()
		// The "user" signs in; the server sends the browser back to the loopback redirect.
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), 302)
	})
	mux.HandleFunc("/konnect/v1/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		fail := func(code string) {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
		}
		if r.Form.Get("client_id") != oidcClientID {
			fail("invalid_client")
			return
		}
		f.seq++
		resp := map[string]any{"access_token": fmt.Sprintf("access-%d", f.seq), "expires_in": 300, "token_type": "Bearer"}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			ch, ok := f.challenges[r.Form.Get("code")]
			delete(f.challenges, r.Form.Get("code")) // single use
			if !ok || ch != pkceChallenge(r.Form.Get("code_verifier")) {
				fail("invalid_grant")
				return
			}
			f.exchanges++
			resp["refresh_token"] = f.refreshToken
		case "refresh_token":
			f.refreshCalls++
			if f.revoked || r.Form.Get("refresh_token") != f.refreshToken {
				fail("invalid_grant")
				return
			}
			if f.rotate {
				f.refreshToken = fmt.Sprintf("refresh-%d", f.seq)
				resp["refresh_token"] = f.refreshToken
			}
		default:
			fail("unsupported_grant_type")
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/konnect/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-") {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"preferred_username": "alice"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// testEnv isolates the app folder and keyring, and makes the "browser" a plain HTTP client that
// follows the sign-in redirect back to the app's loopback listener.
func testEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// The same five TestMain sets: os.UserHomeDir reads USERPROFILE on Windows (not HOME) and
	// os.UserConfigDir reads APPDATA, so setting only HOME would leave a Windows test sharing
	// the run's folder with every other test.
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("APPDATA", filepath.Join(dir, "cfg"))
	t.Setenv("LOCALAPPDATA", filepath.Join(dir, "local"))
	keyring.MockInit()
	old := openBrowser
	openBrowser = func(u string) error {
		go func() {
			resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(u)
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() { openBrowser = old })
}

func signedInFake(t *testing.T) *fakeOIDC {
	t.Helper()
	testEnv(t)
	f := newFakeOIDC(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := signIn(ctx, f.srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

// expireCachedToken makes the cached access token look about to expire.
func expireCachedToken(t *testing.T, left time.Duration) {
	t.Helper()
	st, err := readAuthState()
	if err != nil {
		t.Fatal(err)
	}
	st.Expiry = time.Now().Add(left)
	if err := writeAuthState(st); err != nil {
		t.Fatal(err)
	}
}

func TestPKCE(t *testing.T) {
	// RFC 7636 appendix B
	if got := pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("S256 challenge wrong: %s", got)
	}
	v, c := pkcePair()
	if len(v) < 43 || len(v) > 128 || c != pkceChallenge(v) || strings.ContainsAny(v+c, "+/=") {
		t.Fatalf("bad pair: %q %q", v, c)
	}
	v2, _ := pkcePair()
	if v == v2 {
		t.Fatal("verifier repeats")
	}
}

func TestNormalizeServerURL(t *testing.T) {
	for in, want := range map[string]string{
		"cloud.example.com":           "https://cloud.example.com",
		" https://cloud.example.com/": "https://cloud.example.com",
		"https://x.org/sub/?a=b#c":    "https://x.org/sub",
		"http://127.0.0.1:9200":       "http://127.0.0.1:9200",
	} {
		if got, err := normalizeServerURL(in); err != nil || got != want {
			t.Errorf("%q -> %q, %v (want %q)", in, got, err, want)
		}
	}
	for _, in := range []string{"", "http://cloud.example.com", "ftp://x", "https://"} {
		if _, err := normalizeServerURL(in); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func TestSignInEndToEnd(t *testing.T) {
	f := signedInFake(t)

	// What the browser was sent to.
	q := f.lastAuth
	if q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" ||
		q.Get("client_id") != "OpenCloudDesktop" || q.Get("scope") != "openid profile email offline_access" ||
		!strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") || q.Get("state") == "" {
		t.Fatalf("unexpected authorize request: %v", q)
	}
	if f.exchanges != 1 {
		t.Fatalf("code exchanges = %d", f.exchanges)
	}

	// Config written for the new flow.
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerURL != f.srv.URL || !cfg.OIDC() || cfg.Tunnel() || cfg.DavPath != "/remote.php/dav/files/alice" || !cfg.Configured() {
		t.Fatalf("config after setup: %+v", cfg)
	}
	if !strings.HasPrefix(cfg.RemoteSpec(), ":webdav,vendor=infinitescale,url='"+f.srv.URL+"/remote.php/dav/files/alice'") {
		t.Fatalf("remote spec: %s", cfg.RemoteSpec())
	}
	// Credentials: refresh token in the keyring, not in auth.json.
	st, err := readAuthState()
	if err != nil || st.Username != "alice" || st.Issuer != f.srv.URL {
		t.Fatalf("auth state: %+v %v", st, err)
	}
	if rt, err := keyring.Get(keyringService, st.account()); err != nil || rt != "refresh-0" {
		t.Fatalf("keyring: %q %v", rt, err)
	}
	raw, _ := os.ReadFile(authFile())
	if strings.Contains(string(raw), "refresh-0") {
		t.Fatal("refresh token leaked into auth.json")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(authFile()); fi.Mode().Perm() != 0o600 {
			t.Fatalf("auth.json mode %v", fi.Mode())
		}
	}
	if !haveCredentials(cfg) {
		t.Fatal("haveCredentials false after sign-in")
	}
}

func TestWebdavVendorOverride(t *testing.T) {
	c := Config{ServerURL: "https://c.example.com", DavPath: "/d"}
	if c.Vendor() != "infinitescale" {
		t.Fatal(c.Vendor())
	}
	c.WebdavVendor = "owncloud"
	if got := c.RemoteSpec(); got != ":webdav,vendor=owncloud,url='https://c.example.com/d':" {
		t.Fatal(got)
	}
}

func TestSetupPreservesConfig(t *testing.T) {
	testEnv(t)
	// An existing config with an unknown key and an SSH host: useTunnel is left alone.
	_ = os.MkdirAll(appDir(), 0o700)
	orig := `{"sshHost":"nas","davPath":"/remote.php/dav/files/USER","futureKey":{"a":1},"mountPoint":"~/Drive","kept":[]}`
	_ = os.WriteFile(configFile(), []byte(orig), 0o600)
	if err := applySetup("https://cloud.example.com", "bob smith"); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	b, _ := os.ReadFile(configFile())
	_ = json.Unmarshal(b, &m)
	if m["futureKey"] == nil || m["mountPoint"] != "~/Drive" || m["sshHost"] != "nas" ||
		m["serverURL"] != "https://cloud.example.com" || m["auth"] != "oidc" ||
		m["davPath"] != "/remote.php/dav/files/bob%20smith" {
		t.Fatalf("config: %s", b)
	}
	if _, set := m["useTunnel"]; set {
		t.Fatal("useTunnel must be left alone when sshHost is set")
	}
	cfg, err := loadConfig()
	if err != nil || !cfg.Tunnel() || cfg.BaseURL() != "http://127.0.0.1:19201" {
		t.Fatalf("%+v %v", cfg, err)
	}
	if davPathFor("o'neil") != "/remote.php/dav/files/o%27neil" {
		t.Fatal(davPathFor("o'neil"))
	}
}

func TestWrongStateRejected(t *testing.T) {
	testEnv(t)
	f := newFakeOIDC(t)
	var statusWrong, statusMissing int
	openBrowser = func(u string) error {
		go func() {
			au, _ := url.Parse(u)
			redirect := au.Query().Get("redirect_uri")
			// An attacker (or stray page) hits the loopback port with a code but the wrong state.
			c := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			if r, err := c.Get(redirect + "?code=evil&state=wrong"); err == nil {
				statusWrong = r.StatusCode
				r.Body.Close()
			}
			if r, err := c.Get(redirect + "?code=evil"); err == nil {
				statusMissing = r.StatusCode
				r.Body.Close()
			}
			// Then the real browser flow continues.
			if r, err := c.Get(u); err == nil {
				if loc := r.Header.Get("Location"); loc != "" {
					if r2, err := http.Get(loc); err == nil {
						r2.Body.Close()
					}
				}
				r.Body.Close()
			}
		}()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := login(ctx, f.srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if statusWrong != 400 || statusMissing != 400 {
		t.Fatalf("wrong-state status %d, missing-state status %d", statusWrong, statusMissing)
	}
	if res.Username != "alice" || f.exchanges != 1 {
		t.Fatalf("%+v exchanges=%d", res, f.exchanges)
	}
}

func TestCallbackTimesOutOnWrongState(t *testing.T) {
	ln, err := newLoopbackListener()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		if r, err := http.Get("http://" + ln.Addr().String() + "/?code=x&state=nope"); err == nil {
			r.Body.Close()
		}
	}()
	if code, err := waitForCode(ctx, ln, "right"); err == nil || code != "" {
		t.Fatalf("accepted a wrong state: %q %v", code, err)
	}
}

func TestProviderError(t *testing.T) {
	ln, _ := newLoopbackListener()
	go func() {
		time.Sleep(50 * time.Millisecond)
		if r, err := http.Get("http://" + ln.Addr().String() + "/?error=access_denied&state=s1"); err == nil {
			r.Body.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := waitForCode(ctx, ln, "s1"); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("got %v", err)
	}
}

func TestAccessTokenCachedWhenFresh(t *testing.T) {
	f := signedInFake(t)
	a1, err := accessToken(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := accessToken(f.srv.URL)
	if a1 != a2 || f.refreshCalls != 0 {
		t.Fatalf("%q %q refreshes=%d", a1, a2, f.refreshCalls)
	}
	// A trailing slash or missing scheme in the config still means the same server.
	if a3, err := accessToken(f.srv.URL + "/"); err != nil || a3 != a1 {
		t.Fatalf("%q %v", a3, err)
	}
}

func TestAccessTokenRefreshesNearExpiry(t *testing.T) {
	f := signedInFake(t)
	old, _ := accessToken(f.srv.URL)
	expireCachedToken(t, 30*time.Second) // less than the 60 s margin
	fresh, err := accessToken(f.srv.URL)
	if err != nil || fresh == old || f.refreshCalls != 1 {
		t.Fatalf("%q -> %q, refreshes=%d, %v", old, fresh, f.refreshCalls, err)
	}
	// Now cached again.
	if again, _ := accessToken(f.srv.URL); again != fresh || f.refreshCalls != 1 {
		t.Fatalf("not cached: %q refreshes=%d", again, f.refreshCalls)
	}
}

func TestRefreshKeepsOldRefreshTokenWhenNoneReturned(t *testing.T) {
	f := signedInFake(t)
	st, _ := readAuthState()
	expireCachedToken(t, 0)
	if _, err := accessToken(f.srv.URL); err != nil {
		t.Fatal(err)
	}
	if rt, _ := keyring.Get(keyringService, st.account()); rt != "refresh-0" {
		t.Fatalf("refresh token changed to %q", rt)
	}
	// A second refresh still works with the kept token.
	expireCachedToken(t, 0)
	if _, err := accessToken(f.srv.URL); err != nil || f.refreshCalls != 2 {
		t.Fatalf("%v refreshes=%d", err, f.refreshCalls)
	}
}

func TestRefreshStoresRotatedRefreshToken(t *testing.T) {
	f := signedInFake(t)
	f.rotate = true
	st, _ := readAuthState()
	expireCachedToken(t, 0)
	if _, err := accessToken(f.srv.URL); err != nil {
		t.Fatal(err)
	}
	rt, _ := keyring.Get(keyringService, st.account())
	if rt == "refresh-0" || rt != f.refreshToken {
		t.Fatalf("rotated token not stored: %q (server has %q)", rt, f.refreshToken)
	}
	expireCachedToken(t, 0)
	if _, err := accessToken(f.srv.URL); err != nil {
		t.Fatalf("second refresh with the rotated token: %v", err)
	}
}

func TestInvalidGrantMeansSignInAgain(t *testing.T) {
	f := signedInFake(t)
	f.revoked = true
	expireCachedToken(t, 0)
	_, err := accessToken(f.srv.URL)
	if !errors.Is(err, errSignedOut) || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("got %v", err)
	}
	cfg, _ := loadConfig()
	if haveCredentials(cfg) {
		t.Fatal("credentials should be gone after invalid_grant")
	}
	if _, err := accessToken(f.srv.URL); !errors.Is(err, errSignedOut) {
		t.Fatalf("second call: %v", err)
	}
}

func TestSignedOutCases(t *testing.T) {
	testEnv(t)
	if _, err := accessToken("https://cloud.example.com"); !errors.Is(err, errSignedOut) {
		t.Fatalf("never signed in: %v", err)
	}
	f := signedInFake(t)
	if _, err := accessToken("https://other.example.com"); !errors.Is(err, errSignedOut) {
		t.Fatalf("different server: %v", err)
	}
	logout()
	if _, err := accessToken(f.srv.URL); !errors.Is(err, errSignedOut) {
		t.Fatalf("after logout: %v", err)
	}
	cfg, _ := loadConfig()
	if haveCredentials(cfg) {
		t.Fatal("credentials survive logout")
	}
}

func TestConcurrentRefreshRefreshesOnce(t *testing.T) {
	f := signedInFake(t)
	expireCachedToken(t, 0)
	var wg sync.WaitGroup
	toks := make([]string, 8)
	for i := range toks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			toks[i], _ = accessToken(f.srv.URL)
		}()
	}
	wg.Wait()
	for _, tk := range toks {
		if tk == "" || tk != toks[0] {
			t.Fatalf("tokens differ: %v", toks)
		}
	}
	if f.refreshCalls != 1 {
		t.Fatalf("refreshes = %d", f.refreshCalls)
	}
}

func TestLockFileExcludesOthers(t *testing.T) {
	testEnv(t)
	p := authLockFile()
	unlock, err := lockFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan time.Time, 1)
	go func() {
		u2, err := lockFile(p)
		if err == nil {
			got <- time.Now()
			u2()
		}
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("second lock acquired while the first was held")
	default:
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("second lock never acquired")
	}
}

func TestRefreshTokenFileFallback(t *testing.T) {
	testEnv(t)
	keyring.MockInitWithError(errors.New("no secret service"))
	if err := saveRefreshToken("issuer|u", "tok-1"); err != nil {
		t.Fatal(err)
	}
	if tok, ok := loadRefreshToken("issuer|u"); !ok || tok != "tok-1" {
		t.Fatalf("%q %v", tok, ok)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(refreshFallback()); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("fallback file: %v %v", fi, err)
		}
	}
	deleteRefreshToken("issuer|u")
	if _, ok := loadRefreshToken("issuer|u"); ok {
		t.Fatal("still there after delete")
	}
}

func TestBearerTokenCommandQuoting(t *testing.T) {
	for exe, want := range map[string]string{
		`C:\Program Files\OpenCloud Bar\opencloud-bar.exe`:            `"C:\Program Files\OpenCloud Bar\opencloud-bar.exe" token`,
		`/Applications/OpenCloud Bar.app/Contents/MacOS/OpenCloudBar`: `"/Applications/OpenCloud Bar.app/Contents/MacOS/OpenCloudBar" token`,
		`/usr/bin/opencloud-bar`:                                      `"/usr/bin/opencloud-bar" token`,
		`/odd"name/x`:                                                 `"/odd""name/x" token`,
	} {
		if got := bearerTokenCommand(exe, "token"); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

// With a real rclone and a program in a folder whose name has a space, rclone must run the
// quoted command (it splits on spaces and honours double quotes).
func TestRcloneRunsQuotedCommandWithSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	rc, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not installed")
	}
	dir := filepath.Join(t.TempDir(), "Program Files", "OpenCloud Bar")
	_ = os.MkdirAll(dir, 0o755)
	out := filepath.Join(t.TempDir(), "args.txt")
	script := filepath.Join(dir, "opencloud-bar")
	_ = os.WriteFile(script, []byte("#!/bin/sh\necho \"$#:$1\" > '"+out+"'\necho tok\n"), 0o755)
	cmd := exec.Command(rc, "lsd", ":webdav,vendor=owncloud,url='http://127.0.0.1:9/x':", "--retries", "1", "--low-level-retries", "1")
	cmd.Env = append(os.Environ(), "RCLONE_WEBDAV_BEARER_TOKEN_COMMAND="+bearerTokenCommand(script, "token"))
	_, _ = cmd.CombinedOutput() // the connection itself fails; only the token command matters
	b, err := os.ReadFile(out)
	if err != nil || strings.TrimSpace(string(b)) != "1:token" {
		t.Fatalf("rclone didn't run the token command correctly: %q %v", b, err)
	}
}

func TestEngineEnvCarriesTokenCommand(t *testing.T) {
	testEnv(t)
	oidc := &Engine{cfg: Config{ServerURL: "https://c.example.com", DavPath: "/remote.php/dav/files/a"}}
	found := false
	for _, kv := range envFor(oidc.cfg) {
		if strings.HasPrefix(kv, "RCLONE_WEBDAV_BEARER_TOKEN_COMMAND=") && strings.HasSuffix(kv, ` token`) && strings.Contains(kv, `="`) {
			found = true
		}
	}
	if !found {
		t.Fatal("OIDC engine env lacks the bearer token command")
	}
	legacy := &Engine{cfg: Config{SSHHost: "h", DavPath: "/remote.php/dav/files/a", RcloneRemote: "opencloud"}}
	for _, kv := range envFor(legacy.cfg) {
		if strings.HasPrefix(kv, "RCLONE_WEBDAV_BEARER_TOKEN_COMMAND=") && !strings.Contains(strings.Join(os.Environ(), "\n"), kv) {
			t.Fatal("legacy engine env must not set a token command")
		}
	}
}
