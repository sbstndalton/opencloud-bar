package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type uiRig struct {
	t      *testing.T
	e      *Engine
	ui     *webUI
	srv    *httptest.Server
	port   int
	client *http.Client
	clock  time.Time
}

func newUIRig(t *testing.T) *uiRig {
	testEnv(t)
	e := NewEngine()
	r := &uiRig{t: t, e: e, ui: newWebUI(), clock: time.Now()}
	r.ui.now = func() time.Time { return r.clock }
	r.srv = httptest.NewServer(apiHandler(e, func() {}, "tok", r.ui))
	t.Cleanup(r.srv.Close)
	u, _ := url.Parse(r.srv.URL)
	fmt.Sscanf(u.Port(), "%d", &r.port)
	r.ui.setPort(r.port)
	jar, _ := cookiejar.New(nil)
	r.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return r
}

func (r *uiRig) do(method, path, host, body string, hdr map[string]string) (*http.Response, string) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if host != "" {
		req.Host = host
		// the jar files cookies under the Host header, so a different Host needs the cookie by hand
		u, _ := url.Parse(r.srv.URL)
		for _, c := range r.client.Jar.Cookies(u) {
			req.AddCookie(c)
		}
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (r *uiRig) login() {
	r.t.Helper()
	resp, _ := r.do("GET", "/ui/login?code="+r.ui.NewCode(), "", "", nil)
	if resp.StatusCode != 303 {
		r.t.Fatalf("login: %d", resp.StatusCode)
	}
}

func (r *uiRig) post(path, body string) (*http.Response, string) {
	return r.do("POST", path, "", body, map[string]string{uiHeader: "1", "Content-Type": "application/json"})
}

func TestUICodeSingleUse(t *testing.T) {
	r := newUIRig(t)
	code := r.ui.NewCode()
	resp, _ := r.do("GET", "/ui/login?code="+code, "", "", nil)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/ui/" {
		t.Fatalf("first use: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var sc *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			sc = c
		}
	}
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode || sc.Path != "/" || len(sc.Value) < 32 {
		t.Fatalf("bad cookie: %+v", sc)
	}
	if resp, _ = r.do("GET", "/ui/login?code="+code, "", "", nil); resp.StatusCode != 403 {
		t.Fatalf("second use should be refused, got %d", resp.StatusCode)
	}
	if resp, _ = r.do("GET", "/ui/login", "", "", nil); resp.StatusCode != 403 {
		t.Fatalf("no code should be refused, got %d", resp.StatusCode)
	}
}

func TestUICodeExpiry(t *testing.T) {
	r := newUIRig(t)
	code := r.ui.NewCode()
	r.clock = r.clock.Add(61 * time.Second)
	if resp, _ := r.do("GET", "/ui/login?code="+code, "", "", nil); resp.StatusCode != 403 {
		t.Fatalf("expired code accepted: %d", resp.StatusCode)
	}
	code = r.ui.NewCode()
	r.clock = r.clock.Add(59 * time.Second)
	if resp, _ := r.do("GET", "/ui/login?code="+code, "", "", nil); resp.StatusCode != 303 {
		t.Fatalf("fresh code refused: %d", resp.StatusCode)
	}
}

func TestUICookieRequired(t *testing.T) {
	r := newUIRig(t)
	for _, p := range []string{"/ui/", "/ui/api/state", "/ui/api/log?name=app.log", "/ui/api/account"} {
		if resp, _ := r.do("GET", p, "", "", nil); resp.StatusCode != 401 {
			t.Errorf("%s without cookie: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := r.post("/ui/api/sync", "{}"); resp.StatusCode != 401 {
		t.Errorf("POST without cookie: %d", resp.StatusCode)
	}
	// the control API's bearer token doesn't open the page
	if resp, _ := r.do("GET", "/ui/api/state", "", "", map[string]string{"Authorization": "Bearer tok"}); resp.StatusCode != 401 {
		t.Errorf("bearer on /ui: %d", resp.StatusCode)
	}
	r.login()
	resp, body := r.do("GET", "/ui/api/state", "", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"folders"`) {
		t.Fatalf("with cookie: %d %s", resp.StatusCode, body)
	}
	resp, body = r.do("GET", "/ui/", "", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "OpenCloud Bar") || strings.Contains(body, "{{NONCE}}") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "nonce-") {
		t.Fatalf("page: %d csp=%q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	// a stale cookie is refused
	r.ui.mu.Lock()
	r.ui.sessions = map[string]time.Time{}
	r.ui.mu.Unlock()
	if resp, _ = r.do("GET", "/ui/api/state", "", "", nil); resp.StatusCode != 401 {
		t.Errorf("forgotten session accepted: %d", resp.StatusCode)
	}
}

func TestBearerAPIStillWorks(t *testing.T) {
	r := newUIRig(t)
	if resp, _ := r.do("GET", "/status", "", "", nil); resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	resp, body := r.do("GET", "/status", "", "", map[string]string{"Authorization": "Bearer tok"})
	if resp.StatusCode != 200 || !strings.Contains(body, "summary") {
		t.Errorf("bearer: %d %s", resp.StatusCode, body)
	}
	// a session cookie doesn't open the control API
	r.login()
	if resp, _ = r.do("GET", "/status", "", "", nil); resp.StatusCode != 401 {
		t.Errorf("cookie on /status: %d", resp.StatusCode)
	}
	resp, body = r.do("POST", "/settings-link", "", "", map[string]string{"Authorization": "Bearer tok"})
	if resp.StatusCode != 200 || !strings.Contains(body, "/ui/login?code=") {
		t.Errorf("settings-link: %d %s", resp.StatusCode, body)
	}
}

func TestUIHostCheck(t *testing.T) {
	r := newUIRig(t)
	good := fmt.Sprintf("127.0.0.1:%d", r.port)
	for _, host := range []string{"evil.example:" + fmt.Sprint(r.port), "127.0.0.1", fmt.Sprintf("localhost:%d", r.port+1), fmt.Sprintf("127.0.0.1.evil.test:%d", r.port), "[::1]:" + fmt.Sprint(r.port)} {
		if resp, _ := r.do("GET", "/ui/login?code="+r.ui.NewCode(), host, "", nil); resp.StatusCode != 403 {
			t.Errorf("login with Host %q: %d", host, resp.StatusCode)
		}
	}
	r.login()
	for _, host := range []string{"evil.example:" + fmt.Sprint(r.port), "rebind.test"} {
		if resp, _ := r.do("GET", "/ui/api/state", host, "", nil); resp.StatusCode != 403 {
			t.Errorf("state with Host %q and a valid cookie: %d", host, resp.StatusCode)
		}
	}
	for _, host := range []string{good, fmt.Sprintf("localhost:%d", r.port), fmt.Sprintf("LOCALHOST:%d", r.port)} {
		if resp, b := r.do("GET", "/ui/api/state", host, "", nil); resp.StatusCode != 200 {
			t.Log(b)
			t.Errorf("state with Host %q: %d", host, resp.StatusCode)
		}
	}
}

func TestUIChangesMustComeFromThePage(t *testing.T) {
	r := newUIRig(t)
	r.login()
	origin := "http://" + fmt.Sprintf("127.0.0.1:%d", r.port)
	h := func(m map[string]string) map[string]string { m["Content-Type"] = "application/json"; return m }
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"page", h(map[string]string{uiHeader: "1", "Origin": origin}), 200},
		{"no origin (non-browser client)", h(map[string]string{uiHeader: "1"}), 200},
		{"foreign origin", h(map[string]string{uiHeader: "1", "Origin": "http://evil.example"}), 403},
		{"other port origin", h(map[string]string{uiHeader: "1", "Origin": "http://127.0.0.1:1"}), 403},
		{"null origin", h(map[string]string{uiHeader: "1", "Origin": "null"}), 403},
		{"cross-site fetch metadata", h(map[string]string{uiHeader: "1", "Sec-Fetch-Site": "cross-site"}), 403},
		{"missing custom header", h(map[string]string{"Origin": origin}), 403},
	}
	for _, c := range cases {
		resp, body := r.do("POST", "/ui/api/config", "", "{}", c.hdr)
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d (want %d) %s", c.name, resp.StatusCode, c.want, body)
		}
	}
}

func TestUIConfigPost(t *testing.T) {
	r := newUIRig(t)
	r.login()
	orig := `{
  "sshHost": "", "serverPort": 9200, "localPort": 19201, "rcloneRemote": "opencloud",
  "davPath": "/remote.php/dav/files/sam", "serverURL": "https://cloud.example.com", "auth": "oidc", "useTunnel": false,
  "localRoot": "~/OpenCloud/Personal", "mountPoint": "~/OpenCloud-Drive", "volumeName": "OpenCloud",
  "cacheMaxSize": "20G", "cacheMaxAge": "72h", "syncIntervalSeconds": 300, "minServerAvailableMiB": 8192,
  "futureSetting": {"a": [1,2,3]}, "macOnlyThing": true,
  "kept": [{"path":"Docs","stateName":"docs-1","futureKeyInFolder":"keep me"}],
  "commonFilters": ["- *.tmp"]
}`
	if err := os.WriteFile(configFile(), []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Reload(); err != nil {
		t.Fatal(err)
	}

	// valid change: written, applied to the running engine, unknown keys kept
	resp, body := r.post("/ui/api/config", `{"syncIntervalSeconds":900,"cacheMaxSize":"50G","commonFilters":["- *.iso","","- *.tmp"],
		"keptFilters":{"Docs":["- *.psd"]},"transfers":8}`)
	if resp.StatusCode != 200 {
		t.Fatalf("valid post: %d %s", resp.StatusCode, body)
	}
	data, _ := os.ReadFile(configFile())
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["macOnlyThing"]) != "true" || !strings.Contains(string(raw["futureSetting"]), "3") {
		t.Errorf("unknown top-level keys lost: %s", data)
	}
	if !strings.Contains(string(raw["kept"]), "keep me") || !strings.Contains(string(raw["kept"]), "*.psd") {
		t.Errorf("kept folder keys wrong: %s", raw["kept"])
	}
	if cfg := r.e.config(); cfg.SyncIntervalSeconds != 900 || cfg.CacheMaxSize != "50G" || len(cfg.CommonFilters) != 2 || cfg.Transfers != 8 {
		t.Errorf("engine not reloaded: %+v", cfg)
	}

	// refused changes leave the file untouched
	before, _ := os.ReadFile(configFile())
	bad := map[string]string{
		"interval too small":   `{"syncIntervalSeconds":30}`,
		"tunnel without host":  `{"useTunnel":true,"sshHost":""}`,
		"ssh option injection": `{"sshHost":"-oProxyCommand=calc"}`,
		"bad cache size":       `{"cacheMaxSize":"lots"}`,
		"bad filter":           `{"commonFilters":["*.tmp"]}`,
		"not editable":         `{"serverURL":"https://evil.example"}`,
		"davPath not editable": `{"davPath":"/x"}`,
		"unknown folder":       `{"keptFilters":{"Nope":["- x"]}}`,
		"local root locked":    `{"localRoot":"~/Elsewhere"}`,
		"bad rclone path":      `{"rclonePath":"/definitely/not/here"}`,
		"newline in volume":    `{"volumeName":"a\nb"}`,
		"not json":             `{`,
	}
	for name, b := range bad {
		resp, body := r.post("/ui/api/config", b)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
		if after, _ := os.ReadFile(configFile()); !bytes.Equal(before, after) {
			t.Fatalf("%s changed the file", name)
		}
	}
}

func TestSaveConfigKeepsUnknownKeys(t *testing.T) {
	testEnv(t)
	_ = os.MkdirAll(appDir(), 0o700)
	_ = os.WriteFile(configFile(), []byte(`{"davPath":"/remote.php/dav/files/a","serverURL":"https://x.example","futureKey":42}`), 0o600)
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.Kept = []KeptFolder{{Path: "Docs", StateName: "docs-1"}}
	c.UseTunnel = nil
	if err := saveConfig(c); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(configFile())
	if !strings.Contains(string(data), `"futureKey": 42`) || !strings.Contains(string(data), "docs-1") {
		t.Errorf("saveConfig: %s", data)
	}
}

func TestLogRoute(t *testing.T) {
	r := newUIRig(t)
	r.login()
	_ = os.MkdirAll(logDir(), 0o700)
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	lines = append(lines, "Authorization: Bearer abcdefghijklmnop1234", `{"refresh_token":"zzzzzzzzzzzzzzzz9999"}`)
	_ = os.WriteFile(filepath.Join(logDir(), "app.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	secret := filepath.Join(appDir(), "api-token")
	_ = os.WriteFile(secret, []byte("SECRET-TOKEN"), 0o600)
	_ = os.WriteFile(configFile(), []byte(`{"secret":"SECRET-CONFIG"}`), 0o600)

	resp, body := r.do("GET", "/ui/api/log?name=app.log&lines=5", "", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "line 49") || strings.Contains(body, "line 10") {
		t.Fatalf("tail: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "abcdefghijklmnop1234") || strings.Contains(body, "zzzzzzzzzzzzzzzz9999") {
		t.Errorf("token not hidden: %s", body)
	}
	attacks := []string{
		"../config.json", "..%2Fconfig.json", "..%2f..%2fapi-token", "%2e%2e%2fapi-token", "../api-token", "/etc/passwd",
		`..\config.json`, "app.log/../../api-token", "api-token", "config.json", "", ".log", "..log", "app.log%00.txt", "logs",
	}
	for _, a := range attacks {
		resp, body := r.do("GET", "/ui/api/log?name="+a, "", "", nil)
		if strings.Contains(body, "SECRET") || strings.Contains(body, "passwd") && strings.Contains(body, "root:") {
			t.Errorf("%q leaked: %s", a, body)
		}
		if resp.StatusCode == 200 && strings.Contains(body, `"text":"`) && !strings.Contains(body, `"text":""`) {
			t.Errorf("%q returned text: %s", a, body)
		}
		if resp, _ := r.post("/ui/api/log/clear?name="+a, ""); resp.StatusCode == 200 {
			t.Errorf("clear %q succeeded", a)
		}
	}
	if b, _ := os.ReadFile(secret); string(b) != "SECRET-TOKEN" {
		t.Fatal("secret file was touched")
	}
	if runtime.GOOS != "windows" { // a symlink inside the log folder pointing out is refused
		_ = os.Symlink(secret, filepath.Join(logDir(), "evil.log"))
		if resp, body := r.do("GET", "/ui/api/log?name=evil.log", "", "", nil); strings.Contains(body, "SECRET") || resp.StatusCode == 200 && !strings.Contains(body, `"text":""`) {
			t.Errorf("symlink followed: %d %s", resp.StatusCode, body)
		}
		if resp, _ := r.post("/ui/api/log/clear?name=evil.log", ""); resp.StatusCode == 200 {
			t.Error("cleared through a symlink")
		}
		if b, _ := os.ReadFile(secret); string(b) != "SECRET-TOKEN" {
			t.Fatal("secret file was truncated through a symlink")
		}
	}
	// clearing a real log works
	if resp, _ := r.post("/ui/api/log/clear?name=app.log", ""); resp.StatusCode != 200 {
		t.Errorf("clear: %d", resp.StatusCode)
	}
	if st, _ := os.Stat(filepath.Join(logDir(), "app.log")); st == nil || st.Size() != 0 {
		t.Error("app.log not emptied")
	}
}

func TestReadTailBigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.log")
	var b bytes.Buffer
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&b, "row %d xxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", i)
	}
	_ = os.WriteFile(p, b.Bytes(), 0o600)
	got, err := readTail(p, 3)
	if err != nil || strings.Count(got, "\n") != 2 || !strings.HasPrefix(got, "row 199997 ") {
		t.Fatalf("%v %q", err, got)
	}
	got, _ = readTail(p, 5000)
	if len(got) > logTailBytes {
		t.Fatal("read too much")
	}
}

func TestAttentionFirst(t *testing.T) {
	now := time.Now()
	in := []FolderStatus{
		{Name: "old", State: StOK, LastSuccess: now.Add(-time.Hour)},
		{Name: "bad", State: StFailed},
		{Name: "new", State: StOK, LastSuccess: now},
		{Name: "rep", State: StNeedsRepair},
	}
	var names []string
	for _, f := range attentionFirst(in) {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "bad,rep,new,old" {
		t.Errorf("order %s", got)
	}
	if in[0].Name != "old" {
		t.Error("input was reordered")
	}
}

func TestFetchAccount(t *testing.T) {
	const tok = "TOKEN-VALUE-123456"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ocs/v1.php/cloud/capabilities":
			fmt.Fprint(w, `{"ocs":{"data":{"version":{"string":"4.0.1"},"capabilities":{"files":{"blacklisted_files":[".htaccess"],"undelete":true,"versioning":true}}}}}`)
		case "/graph/v1.0/me":
			if r.Header.Get("Authorization") != "Bearer "+tok {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"displayName":"Sam","mail":"sam@example.com","onPremisesSamAccountName":"sam"}`)
		case "/graph/v1.0/me/drives":
			if r.URL.Query().Get("$filter") != "driveType eq 'personal'" {
				w.WriteHeader(400)
				return
			}
			fmt.Fprint(w, `{"value":[{"quota":{"total":1000,"used":900,"remaining":100,"state":"nearing"}}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	info := accountInfo{BlockedNames: []string{}, Errors: []string{}}
	fetchAccount(t.Context(), srv.URL, tok, &info)
	if len(info.Errors) != 0 || info.DisplayName != "Sam" || info.Email != "sam@example.com" || info.Version != "4.0.1" ||
		info.Quota == nil || info.Quota.State != "nearing" || len(info.BlockedNames) != 1 {
		t.Fatalf("%+v", info)
	}
	if b, _ := json.Marshal(info); strings.Contains(string(b), tok) {
		t.Error("token in the account answer")
	}
	// a wrong token shows an inline error, not a crash
	info = accountInfo{BlockedNames: []string{}, Errors: []string{}}
	fetchAccount(t.Context(), srv.URL, "other", &info)
	if len(info.Errors) == 0 || strings.Contains(strings.Join(info.Errors, " "), "other") {
		t.Errorf("errors: %v", info.Errors)
	}
}

func TestAppWindowArgs(t *testing.T) {
	a := strings.Join(appWindowArgs("http://127.0.0.1:1/ui/login?code=x", "/p"), " ")
	for _, want := range []string{"--app=http://127.0.0.1:1/ui/login?code=x", "--class=opencloud-bar", "--window-size=900,640", "--user-data-dir=/p"} {
		if !strings.Contains(a, want) {
			t.Errorf("missing %s in %s", want, a)
		}
	}
}
