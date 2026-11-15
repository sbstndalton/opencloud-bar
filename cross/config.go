package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// KeptFolder is a folder kept on this machine and synced both ways with the server.
type KeptFolder struct {
	Path         string   `json:"path"`      // relative to the space root
	StateName    string   `json:"stateName"` // unique; names the bisync state
	ExtraFilters []string `json:"extraFilters,omitempty"`
	LocalPath    string   `json:"localPath,omitempty"` // optional: absolute local folder instead of localRoot/path
}

func (f KeptFolder) DisplayName() string { return filepath.Base(f.Path) }

// Config uses the same JSON format as the macOS app; unknown keys are ignored and
// missing keys keep their defaults.
type Config struct {
	SSHHost               string       `json:"sshHost"`
	ServerPort            int          `json:"serverPort"`
	LocalPort             int          `json:"localPort"`
	RcloneRemote          string       `json:"rcloneRemote"`
	DavPath               string       `json:"davPath"`
	RclonePath            string       `json:"rclonePath,omitempty"`   // empty: the rclone bundled with the app, else rclone on PATH
	RcloneConfig          string       `json:"rcloneConfig,omitempty"` // optional rclone.conf path
	SSHPath               string       `json:"sshPath,omitempty"`
	LocalRoot             string       `json:"localRoot"`
	MountPoint            string       `json:"mountPoint"` // folder, or a drive letter like "O:" on Windows
	MountEnabled          *bool        `json:"mountEnabled,omitempty"`
	VolumeName            string       `json:"volumeName"`
	CacheMaxSize          string       `json:"cacheMaxSize"`
	CacheMaxAge           string       `json:"cacheMaxAge"`
	SyncIntervalSeconds   int          `json:"syncIntervalSeconds"`
	MinServerAvailableMiB int          `json:"minServerAvailableMiB"`
	StateDir              string       `json:"stateDir"`
	Transfers             int          `json:"transfers,omitempty"`
	Checkers              int          `json:"checkers,omitempty"`
	Kept                  []KeptFolder `json:"kept"`
	CommonFilters         []string     `json:"commonFilters"`
}

func defaultConfig() Config {
	mnt := "~/OpenCloud-Drive"
	if runtime.GOOS == "windows" {
		mnt = "O:"
	}
	return Config{
		ServerPort: 9200, LocalPort: 19201, RcloneRemote: "opencloud",
		DavPath:   "/remote.php/dav/files/USER",
		LocalRoot: "~/OpenCloud/Personal", MountPoint: mnt, VolumeName: "OpenCloud",
		CacheMaxSize: "20G", CacheMaxAge: "72h", SyncIntervalSeconds: 300,
		MinServerAvailableMiB: 8192, StateDir: filepath.Join(appDir(), "state"),
		Transfers: 4, Checkers: 6,
		CommonFilters: []string{
			"- *.tmp", "- .*.bundle.tmp", "- *~", "- .DS_Store", "- ._*", "- Thumbs.db",
			"- .sync_*.db", "- .sync_*.db-*", "- .OpenCloudSync.log", "- .owncloudsync.log",
			"- .nextcloudsync.log", "- .sync-exclude.lst",
			"- **/target/**", "- **/node_modules/**", "- **/.venv/**", "- **/__pycache__/**",
			"- **/tmp_objdir-*/**",
		},
	}
}

func appDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(home(), ".config")
	}
	return filepath.Join(d, "OpenCloudBar")
}
func configFile() string { return filepath.Join(appDir(), "config.json") }
func logDir() string     { return filepath.Join(appDir(), "logs") }

func home() string { h, _ := os.UserHomeDir(); return h }

// Rclone returns the rclone to run: rclonePath if set, else a copy installed with the app
// (next to the executable, or /usr/lib/opencloud-bar on Linux packages), else rclone on PATH.
func (c Config) Rclone() string {
	if c.RclonePath != "" {
		return expand(c.RclonePath)
	}
	name := "rclone"
	if runtime.GOOS == "windows" {
		name = "rclone.exe"
	}
	var dirs []string
	if runtime.GOOS == "linux" {
		// Linux packages put it here; checked first so an older distro rclone in /usr/bin isn't picked.
		dirs = append(dirs, "/usr/lib/opencloud-bar")
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return name
}

func expand(p string) string {
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home(), p[2:])
	}
	return p
}

func (c Config) Configured() bool { return c.SSHHost != "" && !strings.Contains(c.DavPath, "USER") }
func (c Config) Mounting() bool   { return c.MountEnabled == nil || *c.MountEnabled }
func (c Config) RcPort() int      { return c.LocalPort + 100 }
func (c Config) APIPort() int     { return c.LocalPort + 101 }
func (c Config) RemoteSpec() string {
	return fmt.Sprintf("%s,url='http://127.0.0.1:%d%s':", c.RcloneRemote, c.LocalPort, c.DavPath)
}
func (c Config) ServerIdentity() string {
	return fmt.Sprintf("%s|%d|%s|%s", c.SSHHost, c.ServerPort, c.RcloneRemote, c.DavPath)
}
func (c Config) Local(f KeptFolder) string {
	if f.LocalPath != "" {
		return expand(f.LocalPath)
	}
	return filepath.Join(expand(c.LocalRoot), filepath.FromSlash(f.Path))
}
func (c Config) SSH() string {
	if c.SSHPath != "" {
		return c.SSHPath
	}
	return "ssh"
}

// foldKey compares paths the way case-insensitive disks do.
func foldKey(p string) string { return cases.Fold().String(norm.NFC.String(p)) }

var slugRe = regexp.MustCompile(`[^A-Za-z0-9]+`)

func makeStateName(path string) string {
	slug := strings.ToLower(strings.Trim(slugRe.ReplaceAllString(path, "-"), "-"))
	if len(slug) > 40 {
		slug = slug[:40]
	}
	sum := sha256.Sum256([]byte(foldKey(path)))
	return slug + "-" + hex.EncodeToString(sum[:4])
}

func pathProblem(p string) string {
	switch {
	case p == "":
		return "the whole drive can't be kept; choose a folder inside it"
	case strings.HasPrefix(p, "/"):
		return "path must be relative to the drive"
	case strings.Contains(p, "'"):
		return "path can't contain a single quote"
	case len(p) > 90:
		return "path is longer than about 90 characters (an rclone bisync limit)"
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." {
			return "path can't contain . or .."
		}
	}
	return ""
}

func overlaps(a, b string) bool {
	ka, kb := foldKey(a), foldKey(b)
	return ka == kb || strings.HasPrefix(ka, kb+"/") || strings.HasPrefix(kb, ka+"/")
}

func (c Config) Validate() error {
	states := map[string]bool{}
	for i, f := range c.Kept {
		if p := pathProblem(f.Path); p != "" {
			return fmt.Errorf("kept folder %q: %s", f.Path, p)
		}
		if f.StateName == "" || states[f.StateName] {
			return fmt.Errorf("kept folder %q needs a unique stateName", f.Path)
		}
		states[f.StateName] = true
		for _, g := range c.Kept[:i] {
			if overlaps(f.Path, g.Path) {
				return fmt.Errorf("kept folders %q and %q overlap", f.Path, g.Path)
			}
		}
	}
	if strings.Contains(c.DavPath, "'") {
		return errors.New("davPath can't contain a single quote")
	}
	return nil
}

// loadConfig never overwrites a file it couldn't read.
func loadConfig() (Config, error) {
	c := defaultConfig()
	data, err := os.ReadFile(configFile())
	if errors.Is(err, os.ErrNotExist) {
		return c, saveConfig(c)
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return defaultConfig(), fmt.Errorf("couldn't read %s: %w", configFile(), err)
	}
	if err := c.Validate(); err != nil {
		return defaultConfig(), fmt.Errorf("settings problem: %w", err)
	}
	return c, nil
}

func saveConfig(c Config) error {
	if err := os.MkdirAll(appDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := configFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configFile())
}
