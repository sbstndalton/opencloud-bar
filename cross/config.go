package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// KeptFolder is a folder kept on this machine and synced both ways with the server.
type KeptFolder struct {
	Path         string   `json:"path"`      // relative to the space root
	StateName    string   `json:"stateName"` // unique; names the bisync state
	ExtraFilters []string `json:"extraFilters,omitempty"`
	LocalPath    string   `json:"localPath,omitempty"` // optional: absolute local folder instead of localRoot/path
	// Space is the OpenCloud space the folder is in (a project space or a share); nil is the
	// personal space, as in every config before 1.2. Path is then relative to that space's root.
	Space *SpaceRef `json:"space,omitempty"`
}

// SpaceRef names a space other than the personal one. DavPath is the WebDAV path of its root
// as the server reported it (root.webDavUrl), e.g. "/dav/spaces/<id>"; it is used like davPath.
type SpaceRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	DavPath string `json:"davPath"`
}

func (f KeptFolder) DisplayName() string { return filepath.Base(f.Path) }

// Key identifies a kept folder: its path in the personal space (unchanged from earlier
// versions), else "<space id>:<path>", so the same path can be kept from two spaces.
func (f KeptFolder) Key() string {
	if f.Space == nil {
		return f.Path
	}
	return f.Space.ID + ":" + f.Path
}

// Label names a kept folder in messages: “path”, or “path” in <space name>.
func (f KeptFolder) Label() string {
	if f.Space == nil {
		return "“" + f.Path + "”"
	}
	return "“" + f.Path + "” in " + f.Space.Name
}

func (f KeptFolder) spaceID() string {
	if f.Space == nil {
		return ""
	}
	return f.Space.ID
}

// spaceDirName is a space's folder name under localRoot: its name, made safe for a folder on
// every system the same way (so a settings file moves between computers): characters Windows
// can't store become "-", trailing dots and spaces go, and a reserved Windows device name
// (CON, NUL, COM1, LPT1.txt, …) gets "_" appended.
func spaceDirName(s *SpaceRef) string {
	name := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\\:<>"|?*`, r) || unicode.IsControl(r) {
			return '-'
		}
		return r
	}, strings.TrimSpace(s.Name))
	name = strings.TrimRight(name, " .")
	if reservedWindowsName(name) {
		name += "_"
	}
	if name == "" || name == "." || name == ".." {
		name = "Space " + strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return r
			}
			return -1
		}, s.ID)
		if len(name) > 14 {
			name = name[:14]
		}
	}
	return name
}

// reservedWindowsName: CON, PRN, AUX, NUL, COM1–9, LPT1–9, any case, with or without an
// extension ("nul.txt" is the device too).
func reservedWindowsName(name string) bool {
	base := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9'
}

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

	// Browser sign-in (see auth.go). All three are optional; a config without them is a
	// "legacy" config (SSH tunnel + named rclone remote) and behaves exactly as before.
	ServerURL string `json:"serverURL,omitempty"` // https://cloud.example.com, no trailing slash
	Auth      string `json:"auth,omitempty"`      // "oidc" or "rclone"; default oidc if serverURL is set
	UseTunnel *bool  `json:"useTunnel,omitempty"` // default: true if sshHost is set (legacy only)

	// WebdavVendor is rclone's webdav vendor for the OIDC remote. Default "infinitescale": tus
	// chunked uploads, which pass proxies like Cloudflare (owncloud's single PUT gets 413 over 100 MB).
	// Ignored for legacy remotes (named rclone remote), which keep their own settings.
	WebdavVendor string `json:"webdavVendor,omitempty"`

	// Paused stops kept-folder syncing (the drive keeps working). Same key as the macOS app.
	Paused bool `json:"paused,omitempty"`
	// CheckForUpdates: look for a new release once a day (default on).
	CheckForUpdates *bool `json:"checkForUpdates,omitempty"`
	// Notifications: desktop notifications for problems, automatic merges and updates (default on).
	Notifications *bool `json:"notifications,omitempty"`
	// LiveSync (preview): watch kept folders and sync a folder soon after a local change.
	LiveSync bool `json:"liveSync,omitempty"`
	// BwLimitUp / BwLimitDown: upload / download limits for syncs and the drive, in rclone's
	// units ("512K" = 512 KiB/s, "2M" = 2 MiB/s, a bare number is KiB/s). Empty: no limit.
	BwLimitUp   string `json:"bwLimitUp,omitempty"`
	BwLimitDown string `json:"bwLimitDown,omitempty"`
}

// Units as rclone takes them, either case ("512k" is "512K").
var bwRe = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?)([KMGkmg])?$`)

// normBwLimit is how a limit is stored: trimmed, upper-case unit.
func normBwLimit(v string) string { return strings.ToUpper(strings.TrimSpace(v)) }

// bwLimitProblem checks one limit ("" is no limit).
func bwLimitProblem(name, v string) error {
	if v == "" {
		return nil
	}
	m := bwRe.FindStringSubmatch(v)
	if m == nil {
		return fmt.Errorf("%s looks like 512K or 2M (K = KiB/s, M = MiB/s), or empty for no limit", name)
	}
	if n, _ := strconv.ParseFloat(m[1], 64); n <= 0 {
		return fmt.Errorf("%s must be more than 0, or empty for no limit", name)
	}
	return nil
}

// bwLimitArgs is rclone's --bwlimit UP:DOWN ("off" for the side without a limit), or nothing.
func bwLimitArgs(up, down string) []string {
	if up == "" && down == "" {
		return nil
	}
	or := func(s string) string {
		if s == "" {
			return "off"
		}
		return s
	}
	return []string{"--bwlimit", or(up) + ":" + or(down)}
}

func (c Config) UpdateChecks() bool    { return c.CheckForUpdates == nil || *c.CheckForUpdates }
func (c Config) NotificationsOn() bool { return c.Notifications == nil || *c.Notifications }

func (c Config) Vendor() string {
	if c.WebdavVendor != "" {
		return c.WebdavVendor
	}
	return "infinitescale"
}

const (
	authOIDC   = "oidc"
	authRclone = "rclone"
)

// AuthMode is "oidc" (browser sign-in) or "rclone" (the named rclone remote holds the credentials).
func (c Config) AuthMode() string {
	if c.Auth != "" {
		return c.Auth
	}
	if c.ServerURL != "" {
		return authOIDC
	}
	return authRclone
}
func (c Config) OIDC() bool { return c.AuthMode() == authOIDC }

// Tunnel reports whether traffic goes through the SSH tunnel to 127.0.0.1:localPort.
func (c Config) Tunnel() bool {
	if c.UseTunnel != nil {
		return *c.UseTunnel
	}
	return c.SSHHost != ""
}

// BaseURL is where rclone connects, before davPath.
func (c Config) BaseURL() string {
	if c.Tunnel() {
		return fmt.Sprintf("http://127.0.0.1:%d", c.LocalPort)
	}
	return strings.TrimRight(c.ServerURL, "/")
}

// urlOrigin is scheme://host[:port] of u, without any path.
func urlOrigin(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Scheme == "" || p.Host == "" {
		return strings.TrimRight(u, "/")
	}
	return p.Scheme + "://" + p.Host
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

// Configured says the settings are complete. Whether an OIDC config is also signed in is
// the engine's business (it needs the keyring), not checked here.
func (c Config) Configured() bool {
	if strings.Contains(c.DavPath, "USER") {
		return false
	}
	if c.OIDC() {
		return c.ServerURL != ""
	}
	return c.SSHHost != "" || (c.ServerURL != "" && !c.Tunnel())
}
func (c Config) Mounting() bool { return c.MountEnabled == nil || *c.MountEnabled }
func (c Config) RcPort() int    { return c.LocalPort + 100 }
func (c Config) APIPort() int   { return c.LocalPort + 101 }

// RemoteSpec is the rclone remote for the space root. Legacy: the named remote from rclone.conf.
// OIDC: an on-the-fly webdav remote; the bearer token comes from RCLONE_WEBDAV_BEARER_TOKEN_COMMAND
// (see Engine.env), so no rclone.conf is needed.
func (c Config) RemoteSpec() string {
	if c.OIDC() {
		return fmt.Sprintf(":webdav,vendor=%s,url='%s%s':", c.Vendor(), c.BaseURL(), c.DavPath)
	}
	return fmt.Sprintf("%s,url='%s%s':", c.RcloneRemote, c.BaseURL(), c.DavPath)
}

// ServerIdentity changes when the server or account does; kept folders then need a repair.
// The legacy format is unchanged so existing sync state stays valid.
func (c Config) ServerIdentity() string {
	if c.OIDC() {
		return fmt.Sprintf("oidc|%s|%s", c.ServerURL, c.DavPath)
	}
	return fmt.Sprintf("%s|%d|%s|%s", c.SSHHost, c.ServerPort, c.RcloneRemote, c.DavPath)
}
func (c Config) Local(f KeptFolder) string {
	if f.LocalPath != "" {
		return expand(f.LocalPath)
	}
	if f.Space != nil {
		// <localRoot>/<space name>/<path>: apart from the personal folders' copies.
		return filepath.Join(expand(c.LocalRoot), spaceDirName(f.Space), filepath.FromSlash(f.Path))
	}
	return filepath.Join(expand(c.LocalRoot), filepath.FromSlash(f.Path))
}

// RemoteFor is the rclone remote of the folder's space (the personal one: RemoteSpec). Spaces
// use the same on-the-fly WebDAV remote with that space's path, so the sign-in applies.
func (c Config) RemoteFor(f KeptFolder) string {
	if f.Space == nil {
		return c.RemoteSpec()
	}
	// davPath comes from the space's webDavUrl, which already holds any path prefix the server
	// lives under (https://host/opencloud/dav/spaces/…), so it goes after the origin only.
	return fmt.Sprintf(":webdav,vendor=%s,url='%s%s':", c.Vendor(), urlOrigin(c.BaseURL()), f.Space.DavPath)
}

// ServerIdentityFor is what a folder's sync history was made with (personal: ServerIdentity).
func (c Config) ServerIdentityFor(f KeptFolder) string {
	if f.Space == nil {
		return c.ServerIdentity()
	}
	return c.ServerIdentity() + "|space|" + f.Space.DavPath
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

func makeStateName(path string) string { return makeStateNameIn("", path) }

// makeStateNameIn: a space's folders hash the space id too, so the same path in two spaces
// gets two names. The personal space's names are unchanged.
func makeStateNameIn(spaceID, path string) string {
	slug := strings.ToLower(strings.Trim(slugRe.ReplaceAllString(path, "-"), "-"))
	if len(slug) > 40 {
		slug = slug[:40]
	}
	key := foldKey(path)
	if spaceID != "" {
		key = spaceID + "\x00" + key
	}
	sum := sha256.Sum256([]byte(key))
	return slug + "-" + hex.EncodeToString(sum[:4])
}

// spaceRefProblem checks a kept folder's space (hand edits included).
func (c Config) spaceRefProblem(f KeptFolder) error {
	s := f.Space
	if s == nil {
		return nil
	}
	switch {
	case !c.OIDC():
		return fmt.Errorf("kept folder %q: spaces need the browser sign-in (serverURL)", f.Path)
	case s.ID == "" || hasControl(s.ID) || hasControl(s.Name):
		return fmt.Errorf("kept folder %q: its space needs an id (and no control characters)", f.Path)
	case !strings.HasPrefix(s.DavPath, "/") || strings.ContainsAny(s.DavPath, "' ") || hasControl(s.DavPath):
		return fmt.Errorf("kept folder %q: its space's davPath must start with / (no quotes or spaces)", f.Path)
	}
	return nil
}

// normalizeKeptPath is how a kept folder's path is stored: forward slashes, no leading,
// trailing or doubled slashes ("a//b/" is "a/b"), so one folder can't be kept twice under two
// spellings. pathProblem then checks what is left.
func normalizeKeptPath(p string) string {
	var parts []string
	for _, s := range strings.Split(filepath.ToSlash(p), "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "/")
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// isBackupFolder: the backup folder's name as the server and the disks see it. Trailing spaces
// and dots are dropped by Windows and some servers, so ".opencloud-bar-backups." is the same.
func isBackupFolder(seg string) bool {
	return foldKey(strings.TrimRight(strings.TrimSpace(seg), " .")) == foldKey(backupFolderName)
}

func pathProblem(p string) string {
	if hasControl(p) {
		return "path can't contain control characters (such as a line break)"
	}
	if first, _, _ := strings.Cut(p, "/"); p != "" && isBackupFolder(first) {
		return backupFolderName + " holds the app's backup copies; it can't be kept"
	}
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
		switch {
		case part == ".." || part == ".":
			return "path can't contain . or .."
		case part == "":
			return "path can't contain an empty folder name (two slashes in a row, or a slash at the end)"
		}
	}
	return ""
}

// newPathProblem is pathProblem plus rules for folder names that only apply when a folder is
// added (a settings file with such a folder kept from an earlier version still loads): no name
// that is only spaces, starts or ends with one, or ends with a dot (Windows can't store those,
// and some servers drop them, so two spellings would be one folder).
func newPathProblem(p string) string {
	if pp := pathProblem(p); pp != "" {
		return pp
	}
	for _, part := range strings.Split(p, "/") {
		switch {
		case strings.TrimSpace(part) == "":
			return "a folder name can't be only spaces"
		case strings.TrimSpace(part) != part:
			return fmt.Sprintf("folder name %q starts or ends with a space", part)
		case strings.HasSuffix(part, "."):
			return fmt.Sprintf("folder name %q ends with a dot (Windows can't store that)", part)
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
		if err := c.spaceRefProblem(f); err != nil {
			return err
		}
		for _, g := range c.Kept[:i] {
			if f.spaceID() == g.spaceID() && overlaps(f.Path, g.Path) {
				return fmt.Errorf("kept folders %q and %q overlap", f.Path, g.Path)
			}
		}
		if err := c.localPathProblem(f, i); err != nil {
			return err
		}
		// No two kept folders, whatever their space or localPath, may share (or nest) a local
		// folder: two sync histories on one folder would undo each other's work.
		for _, g := range c.Kept[:i] {
			a, b := filepath.Clean(c.Local(f)), filepath.Clean(c.Local(g))
			if samePath(a, b) || within(a, b) || within(b, a) {
				return fmt.Errorf("kept folder %s would share its folder on this computer (%s) with %s", f.Label(), a, g.Label())
			}
		}
	}
	if strings.Contains(c.DavPath, "'") {
		return errors.New("davPath can't contain a single quote")
	}
	if strings.ContainsAny(c.WebdavVendor, "',: ") {
		return errors.New("webdavVendor isn't a valid rclone vendor name")
	}
	if strings.Contains(c.ServerURL, "'") {
		return errors.New("serverURL can't contain a single quote")
	}
	if c.Auth != "" && c.Auth != authOIDC && c.Auth != authRclone {
		return fmt.Errorf(`auth must be "oidc" or "rclone", not %q`, c.Auth)
	}
	if c.Tunnel() && c.SSHHost == "" {
		return errors.New("useTunnel needs sshHost")
	}
	if err := bwLimitProblem("bwLimitUp", c.BwLimitUp); err != nil {
		return err
	}
	if err := bwLimitProblem("bwLimitDown", c.BwLimitDown); err != nil {
		return err
	}
	return nil
}

// localPathProblem checks a kept folder's own local folder (localPath). Syncing makes the local
// folder look like the server folder, deletions included, so it must never be the home folder,
// a drive root, the folder that holds all kept folders, or another kept folder's local copy,
// nor contain any of them.
func (c Config) localPathProblem(f KeptFolder, index int) error {
	if f.LocalPath == "" {
		return nil
	}
	if hasControl(f.LocalPath) {
		return fmt.Errorf("kept folder %q: localPath can't contain control characters", f.Path)
	}
	p := filepath.Clean(expand(f.LocalPath))
	if !filepath.IsAbs(p) {
		return fmt.Errorf("kept folder %q: localPath must be a full path", f.Path)
	}
	if p == filepath.Dir(p) || (runtime.GOOS == "windows" && len(p) <= 3) {
		return fmt.Errorf("kept folder %q: localPath can't be a drive root", f.Path)
	}
	type guard struct {
		what, path string
		other      int // the other kept folder's index, or -1
	}
	guarded := []guard{{"your home folder", home(), -1}, {"the local folder (localRoot)", expand(c.LocalRoot), -1}}
	for j, g := range c.Kept {
		if j != index {
			guarded = append(guarded, guard{fmt.Sprintf("the local copy of %q", g.Path), c.Local(g), j})
		}
	}
	for _, g := range guarded {
		if g.path == "" {
			continue
		}
		gp := filepath.Clean(g.path)
		clash := samePath(p, gp) || within(gp, p) || (g.other >= 0 && within(p, gp))
		if !clash {
			continue
		}
		// A clash between two kept folders is reported for the one added later (the one the
		// user is adding now), naming the one that was there first.
		if g.other > index {
			return fmt.Errorf("kept folder %q: its local copy (%s) can't be, contain or be inside the local copy of %q",
				c.Kept[g.other].Path, c.Local(c.Kept[g.other]), f.Path)
		}
		if samePath(p, gp) || within(gp, p) {
			return fmt.Errorf("kept folder %q: localPath can't be or contain %s", f.Path, g.what)
		}
		return fmt.Errorf("kept folder %q: localPath is inside %s", f.Path, g.what)
	}
	return nil
}

// folderSettingProblem checks the folder that holds all kept folders (localRoot) or the drive's
// mount point: a full path (or ~/…), not the disk root, no "..", no control characters.
func folderSettingProblem(name, v string) error {
	t := strings.TrimSpace(v)
	switch {
	case t == "":
		return fmt.Errorf("%s can't be empty", name)
	case t != v:
		return fmt.Errorf("%s can't start or end with a space", name)
	case hasControl(v):
		return fmt.Errorf("%s can't contain control characters", name)
	}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return fmt.Errorf("%s can't contain ..", name)
		}
	}
	p := expand(v)
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%s must be a full path (or start with ~/)", name)
	}
	if c := filepath.Clean(p); c == filepath.Dir(c) {
		return fmt.Errorf("%s can't be the root of a disk", name)
	}
	return nil
}

// samePath and within compare paths the way the disk does (case-insensitive on Windows and macOS).
func samePath(a, b string) bool {
	if runtime.GOOS == "linux" {
		return a == b
	}
	return foldKey(a) == foldKey(b)
}

// within reports whether p is strictly inside dir (both cleaned, absolute).
func within(p, dir string) bool {
	if dir == filepath.Dir(dir) { // a root: everything else is inside it
		return p != dir
	}
	prefix := strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator)
	if runtime.GOOS == "linux" {
		return strings.HasPrefix(p, prefix)
	}
	return strings.HasPrefix(foldKey(p), foldKey(prefix))
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
	c.BwLimitUp, c.BwLimitDown = normBwLimit(c.BwLimitUp), normBwLimit(c.BwLimitDown) // a hand edit may say "512k"
	if err := c.Validate(); err != nil {
		return defaultConfig(), fmt.Errorf("settings problem: %w", err)
	}
	return c, nil
}

// saveConfig writes c to config.json. Keys in the existing file that this version doesn't know
// about are kept, so the Go app and the macOS app (and hand edits) don't wipe each other's settings.
func saveConfig(c Config) error {
	configMu.Lock()
	defer configMu.Unlock()
	fresh, err := json.Marshal(c)
	if err != nil {
		return err
	}
	var data []byte
	old, rerr := os.ReadFile(configFile())
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return fmt.Errorf("couldn't read %s: %w", configFile(), rerr)
	}
	if rerr == nil {
		raw := map[string]json.RawMessage{}
		if err := json.Unmarshal(old, &raw); err != nil {
			// Never replace a file someone is editing (or that got damaged) with our copy.
			return fmt.Errorf("the settings file can't be read (%v); fix it, then reload the settings", err)
		}
		for k := range knownConfigKeys() {
			delete(raw, k)
		}
		var mine map[string]json.RawMessage
		_ = json.Unmarshal(fresh, &mine)
		for k, v := range mine {
			raw[k] = v
		}
		if data, err = json.MarshalIndent(raw, "", "  "); err != nil {
			return err
		}
	}
	if data == nil {
		if data, err = json.MarshalIndent(c, "", "  "); err != nil {
			return err
		}
	}
	return writeConfigBytes(data)
}

// configMu serialises every change to config.json (settings page, Pause, Keep, Stop Keeping,
// sign-in), so one writer never overwrites another's change with an older copy.
var configMu sync.Mutex

// readConfigRaw reads config.json as generic JSON (the defaults if there is none yet).
// The caller holds configMu.
func readConfigRaw() (map[string]json.RawMessage, error) {
	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(configFile())
	switch {
	case errors.Is(err, os.ErrNotExist):
		d, _ := json.Marshal(defaultConfig())
		_ = json.Unmarshal(d, &raw)
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("the settings file can't be read (%v); fix it, then reload the settings", err)
		}
	}
	return raw, nil
}

// editConfig changes only the keys edit touches: it reads config.json fresh, applies edit and
// writes it back, all under configMu. What another writer (or a hand edit) changed meanwhile,
// and keys this version doesn't know, inside kept folders too, survive. It returns the new
// settings as a whole, already checked.
func editConfig(edit func(raw map[string]json.RawMessage) error) (Config, error) {
	configMu.Lock()
	defer configMu.Unlock()
	raw, err := readConfigRaw()
	if err != nil {
		return Config{}, err
	}
	if err := edit(raw); err != nil {
		return Config{}, err
	}
	next := defaultConfig()
	if err := json.Unmarshal(mustJSON(raw), &next); err != nil {
		return Config{}, fmt.Errorf("those settings don't fit the settings file: %w", err)
	}
	if err := next.Validate(); err != nil {
		return Config{}, err
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return Config{}, err
	}
	return next, writeConfigBytes(out)
}

// rawKept is config.json's kept list with every key of every entry.
func rawKept(raw map[string]json.RawMessage) ([]map[string]json.RawMessage, error) {
	var kept []map[string]json.RawMessage
	if b, ok := raw["kept"]; ok && string(b) != "null" {
		if err := json.Unmarshal(b, &kept); err != nil {
			return nil, fmt.Errorf("couldn't read the kept folders: %w", err)
		}
	}
	return kept, nil
}

// rawSpaceID and rawKey read a raw kept entry's space id and KeptFolder.Key().
func rawSpaceID(m map[string]json.RawMessage) string {
	var s struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(m["space"], &s)
	return s.ID
}

func rawKey(m map[string]json.RawMessage) string {
	if id := rawSpaceID(m); id != "" {
		return id + ":" + rawString(m, "path")
	}
	return rawString(m, "path")
}

func rawString(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}

// knownConfigKeys lists the JSON keys of the Config struct.
func knownConfigKeys() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

// writeConfigBytes replaces config.json atomically.
func writeConfigBytes(data []byte) error {
	if err := os.MkdirAll(appDir(), 0o700); err != nil {
		return err
	}
	tmp := configFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configFile())
}

// davPathFor is the WebDAV path of a user's files.
func davPathFor(username string) string {
	// PathEscape leaves ' alone, but davPath is embedded in a single-quoted rclone value.
	return "/remote.php/dav/files/" + strings.ReplaceAll(url.PathEscape(username), "'", "%27")
}

// applySetup records a finished sign-in in config.json. It edits the file as generic JSON so keys
// this version doesn't know about (and everything else the user set) are preserved. Without
// an sshHost the tunnel is off; with one, useTunnel is kept as the user had it.
func applySetup(serverURL, username string) error {
	configMu.Lock()
	defer configMu.Unlock()
	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(configFile())
	switch {
	case errors.Is(err, os.ErrNotExist):
		d, _ := json.Marshal(defaultConfig())
		_ = json.Unmarshal(d, &raw)
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("couldn't read %s: %w", configFile(), err)
		}
	}
	set := func(k string, v any) { b, _ := json.Marshal(v); raw[k] = b }
	set("serverURL", serverURL)
	set("auth", authOIDC)
	set("davPath", davPathFor(username))
	var ssh string
	_ = json.Unmarshal(raw["sshHost"], &ssh)
	if ssh == "" {
		set("useTunnel", false)
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(appDir(), 0o700); err != nil {
		return err
	}
	tmp := configFile() + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configFile())
}
