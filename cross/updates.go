package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "0.0.0-dev"

const (
	releasesAPI        = "https://api.github.com/repos/sbstndalton/opencloud-bar/releases/latest"
	releasesPagePrefix = "https://github.com/sbstndalton/opencloud-bar/releases/"
	updateInterval     = 24 * time.Hour
)

// semver is major.minor.patch plus an optional pre-release ("1.2.3-beta.1").
type semver struct {
	n   [3]int
	pre string
}

// parseSemver accepts "1.2.3", "v1.2.3", "1.2" and "1.2.3-rc.1"; build metadata (+...) is ignored.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return semver{}, false
	}
	var v semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		v.n[i] = n
	}
	v.pre = pre
	return v, true
}

// compare returns -1, 0 or 1. A pre-release sorts before its release; pre-releases compare as text.
func (a semver) compare(b semver) int {
	for i := range a.n {
		if a.n[i] != b.n[i] {
			if a.n[i] < b.n[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	case a.pre < b.pre:
		return -1
	}
	return 1
}

// isDevBuild is true for builds without a release version (they are never offered updates).
func isDevBuild(v string) bool {
	s, ok := parseSemver(v)
	return !ok || strings.Contains(s.pre, "dev") || s.n == [3]int{}
}

// updateAvailable reports whether latest is newer than running. Development builds never are.
func updateAvailable(latest, running string) bool {
	if isDevBuild(running) {
		return false
	}
	l, ok1 := parseSemver(latest)
	r, ok2 := parseSemver(running)
	return ok1 && ok2 && l.compare(r) > 0
}

// release is the part of GitHub's "latest release" answer the app uses.
type release struct {
	Tag string `json:"tag_name"`
	URL string `json:"html_url"`
}

// fetchLatestRelease asks GitHub for the newest (non-pre-release) release. No sign-in is used.
func fetchLatestRelease(ctx context.Context, client *http.Client, api string) (release, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", api, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "OpenCloud-Bar/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return release{}, fmt.Errorf("couldn't reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return release{}, errors.New("no releases published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var r release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return release{}, fmt.Errorf("couldn't read GitHub's answer: %w", err)
	}
	if _, ok := parseSemver(r.Tag); !ok {
		return release{}, fmt.Errorf("unexpected release tag %q", r.Tag)
	}
	// Only ever open this project's own release pages.
	if !strings.HasPrefix(r.URL, releasesPagePrefix) {
		r.URL = releasesPagePrefix + "latest"
	}
	return r, nil
}

// UpdateState is what the tray and settings page show.
type UpdateState struct {
	Running   string    `json:"running"`
	DevBuild  bool      `json:"devBuild"`
	Latest    string    `json:"latest,omitempty"`
	URL       string    `json:"url,omitempty"`
	Available bool      `json:"available"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
	Checking  bool      `json:"checking,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// updater checks at most once a day (remembered on disk) plus whenever the user asks.
type updater struct {
	mu    sync.Mutex
	api   string
	file  string // where the last check is remembered
	now   func() time.Time
	state UpdateState
	http  *http.Client
}

func newUpdater() *updater {
	u := &updater{api: releasesAPI, file: filepath.Join(appDir(), "update-check.json"), now: time.Now,
		http: &http.Client{Timeout: 20 * time.Second}}
	u.state.Running, u.state.DevBuild = version, isDevBuild(version)
	if b, err := os.ReadFile(u.file); err == nil {
		var saved UpdateState
		if json.Unmarshal(b, &saved) == nil {
			u.state.Latest, u.state.URL, u.state.CheckedAt = saved.Latest, saved.URL, saved.CheckedAt
			u.state.Available = updateAvailable(saved.Latest, version)
		}
	}
	return u
}

func (u *updater) State() UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

// due reports whether the daily check should run now.
func (u *updater) due() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	return !u.state.Checking && (u.state.CheckedAt.IsZero() || now.Sub(u.state.CheckedAt) >= updateInterval || u.state.CheckedAt.After(now))
}

// Check asks GitHub now and returns the new state.
func (u *updater) Check(ctx context.Context) UpdateState {
	u.mu.Lock()
	if u.state.Checking {
		st := u.state
		u.mu.Unlock()
		return st
	}
	u.state.Checking = true
	u.mu.Unlock()
	r, err := fetchLatestRelease(ctx, u.http, u.api)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.state.Checking = false
	u.state.CheckedAt = u.now()
	if err != nil {
		u.state.Error = err.Error()
	} else {
		u.state.Error = ""
		u.state.Latest, u.state.URL = strings.TrimPrefix(r.Tag, "v"), r.URL
		u.state.Available = updateAvailable(r.Tag, version)
	}
	if b, err := json.MarshalIndent(u.state, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(u.file), 0o700)
		_ = os.WriteFile(u.file, b, 0o600)
	}
	return u.state
}
