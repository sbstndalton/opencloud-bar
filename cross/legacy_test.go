package main

import (
	"os"
	"strings"
	"testing"
)

// Config shapes in use today must behave exactly as before this feature existed.
func TestLegacyConfigsUnchanged(t *testing.T) {
	cases := map[string]string{
		// owner's Mac: SSH tunnel, mounted drive, named rclone remote with an app token
		"mac": `{"sshHost":"myserver","serverPort":9200,"localPort":19201,"rcloneRemote":"opencloud",
		  "davPath":"/remote.php/dav/files/sebastian","localRoot":"~/OpenCloud/Personal","mountPoint":"~/OpenCloud-Drive",
		  "syncIntervalSeconds":300,"minServerAvailableMiB":8192,"kept":[],"commonFilters":[]}`,
		// headless server: no drive, own rclone and rclone.conf
		"ms02": `{"sshHost":"daserver","serverPort":9200,"localPort":19201,"rcloneRemote":"opencloud",
		  "davPath":"/remote.php/dav/files/sebastian","mountEnabled":false,"rclonePath":"/usr/bin/rclone",
		  "rcloneConfig":"/etc/opencloud/rclone.conf","kept":[{"path":"Docs","stateName":"docs-1"}]}`,
	}
	for name, js := range cases {
		testEnv(t)
		_ = os.MkdirAll(appDir(), 0o700)
		_ = os.WriteFile(configFile(), []byte(js), 0o600)
		c, err := loadConfig()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !c.Configured() || !c.Tunnel() || c.OIDC() || c.AuthMode() != "rclone" || c.BaseURL() != "http://127.0.0.1:19201" {
			t.Errorf("%s: mode wrong: %+v", name, c)
		}
		if got, want := c.RemoteSpec(), "opencloud,url='http://127.0.0.1:19201/remote.php/dav/files/sebastian':"; got != want {
			t.Errorf("%s: remote spec %q, want %q", name, got, want)
		}
		if got, want := c.ServerIdentity(), "myserver|9200|opencloud|/remote.php/dav/files/sebastian"; name == "mac" && got != want {
			t.Errorf("%s: identity %q, want %q", name, got, want)
		}
		e := &Engine{cfg: c}
		for _, kv := range e.env() {
			if strings.HasPrefix(kv, "RCLONE_WEBDAV_BEARER_TOKEN_COMMAND=") && !strings.Contains(strings.Join(os.Environ(), "\n"), kv) {
				t.Errorf("%s: legacy env must not add a token command", name)
			}
		}
	}
}

func TestDefaultConfigNeedsSetup(t *testing.T) {
	testEnv(t)
	c := defaultConfig()
	if c.Configured() || c.Tunnel() || c.OIDC() {
		t.Fatalf("%+v", c)
	}
	// A signed-in OIDC config is configured and has no tunnel.
	c.ServerURL, c.DavPath = "https://c.example.com", "/remote.php/dav/files/a"
	if !c.Configured() || c.Tunnel() || !c.OIDC() || c.ServerIdentity() != "oidc|https://c.example.com|/remote.php/dav/files/a" {
		t.Fatalf("%+v", c)
	}
}
