package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Keeping a folder from the file manager, and the size check before a folder is kept (2.7).
//
//   - driveRel maps a path inside the mounted OpenCloud drive to the folder's path in OpenCloud.
//     Used by the folder dialog (pickAndKeep) and by `opencloud-bar keep-drive PATH`, which the
//     Windows installer's Explorer menu item "Keep on this PC with OpenCloud Bar" runs
//     (packaging/windows/opencloud-bar.iss). On Linux the command is documented only.
//   - Size check: PROPFIND <folder>, Depth 0, <oc:size/> to the configured server (never
//     another host; accountClient follows no redirects). oc:size on a folder is its whole size:
//     opencloud-eu/reva internal/http/services/owncloud/ocdav/propfind/propfind.go ("size":
//     "oc:size is available on files and folders"). A folder bigger than the free space at the
//     local root, or bigger than keepWarnBytes, needs a confirmation. A size that can't be read
//     never blocks. The macOS app does the same (AppModel.swift, KeepSize).

// keepWarnBytes: a folder bigger than this is confirmed before it is kept (5 GB).
const keepWarnBytes int64 = 5 << 30

// keepSizeTimeout caps the size PROPFIND: a slow answer is an unknown size, and never blocks.
const keepSizeTimeout = 10 * time.Second

// driveRel is the path in OpenCloud of picked, a path inside the mounted drive root (the
// mount point: a drive letter like "O:" on Windows, a folder elsewhere). ok is false for a path
// outside the drive and for the drive itself. Comparisons follow the platform's rules (a drive
// letter or a folder's case doesn't matter on Windows). The result uses "/".
func driveRel(root, picked string) (string, bool) {
	root = filepath.Clean(root)
	// A bare drive letter: Clean turns "O:" into "O:." on Windows, which is "the current folder
	// on O:", not the drive's root.
	if v := filepath.VolumeName(root); v != "" && (root == v || root == v+".") {
		root = v + `\`
	} else if len(root) == 2 && root[1] == ':' {
		root += `\`
	}
	rel, err := filepath.Rel(root, filepath.Clean(picked))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// Why a path can't be mapped to a folder in OpenCloud.
var (
	errDriveOutside   = errors.New("outside the drive")
	errDriveMissing   = errors.New("not found in the drive")
	errDriveAmbiguous = errors.New("more than one folder matches")
)

// driveListFn lists the names in a folder of the drive (a test hook).
var driveListFn = func(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

// resolveDriveRel maps picked to its path in OpenCloud like driveRel, and also requires the
// folder to exist. Each segment takes its case from the drive's own listing, walking down from
// the root and matching case-insensitively (on Windows the picked path keeps whatever case the
// caller typed, and OpenCloud's paths are case-sensitive). Two entries that match only
// case-insensitively (possible on a case-sensitive server) are ambiguous: refused.
//
// Names are not otherwise transformed. rclone mount does not apply the Windows "encoding" (the
// fullwidth look-alikes for : * ? " < > | or a trailing dot or space): that is the local
// backend's --local-encoding (https://rclone.org/overview/#restricted-filenames), and the
// mount code (cmd/cmount/fs.go) passes names between WinFsp and the VFS unchanged; the webdav
// backend's encoding is the identity for OpenCloud. So the drive shows the server's names as
// they are, and nothing needs mapping back.
func resolveDriveRel(root, picked string) (string, error) {
	rel, ok := driveRel(root, picked)
	if !ok {
		return "", errDriveOutside
	}
	cur := filepath.Clean(root)
	if runtime.GOOS == "windows" && len(cur) == 2 && cur[1] == ':' {
		cur += `\`
	}
	var out []string
	for _, seg := range strings.Split(rel, "/") {
		names, err := driveListFn(cur)
		if err != nil {
			return "", errDriveMissing
		}
		match := ""
		n := 0
		for _, name := range names {
			if strings.EqualFold(name, seg) {
				match = name
				n++
			}
		}
		switch {
		case n == 0:
			return "", errDriveMissing
		case n > 1:
			return "", errDriveAmbiguous
		}
		out = append(out, match)
		cur = filepath.Join(cur, match)
	}
	return strings.Join(out, "/"), nil
}

// driveRelMessage says why a path isn't kept.
func driveRelMessage(err error, mount, path string) (title, text string) {
	switch err {
	case errDriveMissing:
		return "That folder isn't there", "“" + path + "” doesn't exist in the OpenCloud drive (" + mount + ")."
	case errDriveAmbiguous:
		return "Which folder?", "“" + path + "” matches more than one folder that differs only in upper and lower case. Rename one in OpenCloud, or keep it by its exact path in Settings."
	}
	return "That folder isn't in OpenCloud", "Only a folder inside the OpenCloud drive (" + mount + ") can be kept. “" + path + "” isn't in it."
}

// SizeCheck is the answer of the size check. Size and Free are -1 when unknown.
type SizeCheck struct {
	NeedsConfirm bool   `json:"needsConfirm"`
	Size         int64  `json:"size"`
	Free         int64  `json:"free"`
	Message      string `json:"message,omitempty"`
}

// sizeVerdict says whether keeping a folder of size bytes needs a confirmation, with free the
// bytes free at the local root. Unknown values (negative) never count against the folder.
func sizeVerdict(size, free int64) bool {
	if size < 0 {
		return false
	}
	return (free >= 0 && size > free) || size > keepWarnBytes
}

// sizeWarning is the text asked before a big folder is kept.
func sizeWarning(name string, size, free int64) string {
	s := "“" + name + "” is " + humanSize(size) + " in OpenCloud. "
	if free >= 0 && size > free {
		s += "Only " + humanSize(free) + " is free on this computer, so it may not fit. "
	} else {
		s += "That is a lot to download and keep on this computer. "
	}
	return s + "It will sync both ways. Keep it anyway?"
}

func newSizeCheck(name string, size, free int64) SizeCheck {
	sc := SizeCheck{Size: size, Free: free}
	if sizeVerdict(size, free) {
		sc.NeedsConfirm = true
		sc.Message = sizeWarning(name, size, free)
	}
	return sc
}

// freeAt is the free space on the volume of dir, or of its nearest existing parent (-1 when
// it can't be read).
func freeAt(dir string) int64 {
	dir = filepath.Clean(dir)
	for i := 0; i < 64; i++ {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			n, err := diskFree(dir)
			if err != nil {
				return -1
			}
			return n
		}
		up := filepath.Dir(dir)
		if up == dir {
			return -1
		}
		dir = up
	}
	return -1
}

// Test hooks: the size of a folder in OpenCloud, and the free space of a folder.
var (
	keepSizeFn = davFolderSize
	freeAtFn   = freeAt
)

// davFolderSize reads a folder's oc:size (PROPFIND, depth 0). An error means it isn't known.
func davFolderSize(ctx context.Context, davURL, tok string) (int64, error) {
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:size/></d:prop></d:propfind>`
	resp, err := davDo(ctx, "PROPFIND", davURL, tok, map[string]string{"Depth": "0"}, body)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return -1, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	var ms struct {
		Responses []struct {
			Props []struct {
				Status string `xml:"status"`
				Size   string `xml:"prop>size"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ms); err != nil {
		return -1, errors.New("the server's answer wasn't what was expected")
	}
	for _, r := range ms.Responses {
		for _, p := range r.Props {
			if p.Status != "" && !strings.Contains(p.Status, " 200") {
				continue
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(p.Size), 10, 64); err == nil && n >= 0 {
				return n, nil
			}
		}
		break // depth 0: the folder itself
	}
	return -1, errors.New("the server didn't say how big it is")
}

// checkKeepSize runs the size check for the folder at davURL; localPath is the chosen local
// folder ("" for the default under the local root). Anything that can't be found out leaves
// that value unknown (-1) and never asks for a confirmation.
func checkKeepSize(cfg Config, davURL, name, localPath string) SizeCheck {
	unknown := SizeCheck{Size: -1, Free: -1}
	if !cfg.OIDC() || davURL == "" {
		return unknown // the token is the browser sign-in's
	}
	tok, err := accessToken(cfg.ServerURL)
	if err != nil {
		return unknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), keepSizeTimeout)
	defer cancel()
	size, err := keepSizeFn(ctx, davURL, tok)
	if err != nil {
		appLog("size check of %q: %v", name, err)
		return unknown
	}
	dir := expand(cfg.LocalRoot)
	if localPath != "" {
		dir = expand(localPath)
	}
	return newSizeCheck(name, size, freeAtFn(dir))
}

// KeepChecked is Keep/KeepInSpace with the size check first: unless confirm is set, a folder
// that needs a confirmation isn't kept and the check says why (NeedsConfirm).
func (e *Engine) KeepChecked(spaceID, path, localPath string, confirm bool) (SizeCheck, error) {
	if spaceID == "" {
		path = normalizeKeptPath(path)
		if p := newPathProblem(path); p != "" {
			return SizeCheck{}, errors.New(p)
		}
		if !confirm {
			cfg := e.config()
			u, err := itemDavURL(cfg, KeptFolder{Path: path}, "")
			if err == nil {
				if sc := checkKeepSize(cfg, u, path, localPath); sc.NeedsConfirm {
					return sc, nil
				}
			}
		}
		return SizeCheck{Size: -1, Free: -1}, e.Keep(path, localPath)
	}
	return e.keepInSpaceCk(spaceID, path, localPath, !confirm)
}

// keepWithDialogs keeps rel (a folder's path in OpenCloud) after the size check, asking in a
// dialog when the folder is big.
func keepWithDialogs(e *Engine, rel string) {
	sc, err := e.KeepChecked("", rel, "", false)
	if err != nil {
		alertErr(err)
		return
	}
	if !sc.NeedsConfirm {
		return
	}
	if !dialogConfirm("Keep this folder?", sc.Message, "Keep It Anyway") {
		return
	}
	_, err = e.KeepChecked("", rel, "", true)
	alertErr(err)
}

// ---- keep-drive: the file manager's "Keep on this PC with OpenCloud Bar"

// keepDriveEnv is what `keep-drive` needs, so it can be tested without a daemon or a screen.
type keepDriveEnv struct {
	mount     string // the drive's mount point
	out, errw io.Writer
	dialogs   bool // answer in dialogs (--dialogs, or on other platforms: started without a terminal)
	alert     func(title, text string)
	confirm   func(title, text, button string) bool
	// keep asks the running app to keep rel; with confirm false it may answer NeedsConfirm.
	keep func(rel string, confirm bool) (SizeCheck, error)
}

// say tells the user: in a dialog when there is no terminal, else on stderr.
func (k keepDriveEnv) say(title, text string) {
	fmt.Fprintln(k.errw, "opencloud-bar: "+text)
	if k.dialogs {
		k.alert(title, text)
	}
}

// run is `keep-drive PATH [--yes] [--dialogs]`; the result is the exit code.
func (k keepDriveEnv) run(args []string) int {
	var path string
	yes := false
	for _, a := range args {
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case a == "--dialogs":
			k.dialogs = true
		case path == "" && !strings.HasPrefix(a, "-"):
			path = a
		default:
			fmt.Fprintln(k.errw, "usage: opencloud-bar keep-drive PATH [--yes] [--dialogs]")
			return 2
		}
	}
	if path == "" {
		fmt.Fprintln(k.errw, "usage: opencloud-bar keep-drive PATH [--yes] [--dialogs]")
		return 2
	}
	rel, err := resolveDriveRel(k.mount, path)
	if err != nil {
		title, text := driveRelMessage(err, k.mount, path)
		k.say(title, text)
		return 1
	}
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		k.say("That's a file", "Only folders can be kept. Choose its folder, “"+filepath.Dir(path)+"”, instead.")
		return 1
	}
	sc, err := k.keep(rel, yes)
	if err != nil {
		k.say("OpenCloud Bar", err.Error())
		return 1
	}
	if sc.NeedsConfirm {
		if !k.dialogs {
			fmt.Fprintln(k.errw, "opencloud-bar: "+sc.Message)
			fmt.Fprintln(k.errw, "Nothing was kept. Run the command again with --yes to keep it anyway.")
			return 1
		}
		if !k.confirm("Keep this folder?", sc.Message, "Keep It Anyway") {
			return 1
		}
		if _, err := k.keep(rel, true); err != nil {
			k.say("OpenCloud Bar", err.Error())
			return 1
		}
	}
	fmt.Fprintln(k.out, "kept “"+rel+"”; the first sync starts shortly")
	return 0
}

// stdinIsTerminal: the command was typed in a terminal (not started by the file manager).
func stdinIsTerminal() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// autoDialogs is the fallback when --dialogs isn't given: dialogs when there is no terminal.
// Not on Windows: a console program started by Explorer still has a console as stdin (a
// character device), so the test says "terminal" there; the Explorer verb passes --dialogs.
func autoDialogs() bool {
	return runtime.GOOS != "windows" && hasDialogs() && !stdinIsTerminal()
}
