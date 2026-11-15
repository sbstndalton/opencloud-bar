# OpenCloud Bar

A small menu-bar and tray app for macOS, Windows and Linux. It lets you use a self-hosted [OpenCloud](https://opencloud.eu) server the way people used to use Nextcloud's selective sync:

- **Everything on demand.** Your whole OpenCloud space appears as a drive: a Finder volume on the Mac, a drive letter such as `O:` on Windows, a folder on Linux. Files download only when you open them. They're cached (20 GB by default) and evicted again after a few days unused.
- **Some folders kept locally.** Pick the folders you want as real local copies, and they sync both ways every few minutes. Stop keeping one and its local copy is moved out of the way (the Trash on Mac and Linux, a "Removed by OpenCloud Bar" folder on Windows). The folder stays in OpenCloud.

| Platform | App | Status |
|---|---|---|
| macOS 14+ | native SwiftUI menu-bar app (`Sources/`) | in daily use |
| Windows 10/11 | tray app plus a command-line tool (`cross/`, Go) | tested on Windows 11 |
| Linux | tray app, command line, or a headless service (`cross/`, Go) | headless service in daily use; tray untested |

## Download

Get the latest build from [Releases](https://github.com/sbstndalton/opencloud-bar/releases/latest). Each installer includes [rclone](https://rclone.org), so you don't install it separately.

| File | For | What it sets up |
|---|---|---|
| `OpenCloud-Bar-macOS.dmg` | macOS 14+ on Apple silicon | Drag the app to Applications. It's ad-hoc signed, so the first time, right-click it and choose **Open**. macOS's own NFS client provides the drive, so nothing else is needed. |
| `OpenCloud-Bar-Setup-<version>.exe` | Windows 10/11 x64 | Installs the app, rclone and [WinFsp](https://winfsp.dev) (for the drive letter). Adds a Start menu entry, an optional start at sign-in, and the `opencloud-bar` command for terminals. |
| `opencloud-bar_<version>_amd64.deb` / `_arm64.deb` | Debian, Ubuntu | `sudo apt install ./opencloud-bar_*.deb` pulls in FUSE 3 and the SSH client. |
| `opencloud-bar-<version>.x86_64.rpm` / `.aarch64.rpm` | Fedora, openSUSE | `sudo dnf install ./opencloud-bar-*.rpm` |
| `opencloud-bar-linux-amd64.tar.gz` / `arm64` | other Linux | The app plus rclone. Install FUSE 3 yourself. |

What no installer can do for you is connect to your own server. You still need SSH key access to the machine running OpenCloud, and an app token for your account (see Requirements below).

## How it works

[rclone](https://rclone.org) does all the work:

| Piece | rclone command |
|---|---|
| On-demand drive | `rclone nfsmount` on macOS (built-in NFS client, no FUSE or kernel extension), `rclone mount` on Windows and Linux |
| Kept folders | `rclone bisync` (two-way sync with a state file per folder) |

Traffic goes through an SSH tunnel to the server's local OpenCloud port. This skips any reverse proxy or CDN in front of OpenCloud. Cloudflare in particular closes HTTP/2 connections after about 10,000 requests, which breaks the official desktop client on large trees, and it limits uploads to 100 MB. On the Mac, the tunnel runs as a per-user launchd agent that the app creates and removes. That way macOS Local Network privacy doesn't block it, and it survives an app crash.

## macOS

### Requirements

- macOS 14 or later
- `rclone` (`brew install rclone`)
- SSH key access to the machine running OpenCloud, with a host alias in `~/.ssh/config`
- An rclone WebDAV remote for your OpenCloud account, using an app token. Create the token with `opencloud auth-app create` on the server, or in the web UI, then run:

  ```bash
  rclone config create opencloud webdav vendor=owncloud user=YOURUSER pass=YOURTOKEN url=http://127.0.0.1:19201/remote.php/dav/files/YOURUSER --obscure
  ```

### Build and install

```bash
./scripts/build-app.sh
```

```bash
./scripts/install.sh
```

On first launch the app writes `~/Library/Application Support/OpenCloudBar/config.json`. Fill in `sshHost` and `davPath` (`/remote.php/dav/files/YOURUSER`), then choose **Advanced ▸ Reload Settings**.

## Settings (`config.json`)

| Key | Meaning | Default |
|---|---|---|
| `sshHost` | SSH alias of the OpenCloud server | — |
| `serverPort` / `localPort` | OpenCloud port on the server / local end of the tunnel | 9200 / 19201 |
| `rcloneRemote` | name of the rclone remote holding the credentials | `opencloud` |
| `davPath` | WebDAV path of your personal space | — |
| `localRoot` | where kept folders live | `~/OpenCloud/Personal` |
| `mountPoint` | where the drive is mounted | `~/OpenCloud-Drive` |
| `cacheMaxSize` / `cacheMaxAge` | on-demand cache limits | `20G` / `72h` |
| `syncIntervalSeconds` | how often kept folders sync | 300 |
| `minServerAvailableMiB` | skip a sync run when the server has less free memory than this (Linux servers; set 0 to turn off) | 8192 |
| `kept` | kept folders: `path` (relative to the space root), `stateName`, optional `extraFilters` | `[]` |
| `commonFilters` | rclone filter rules for every kept folder | build folders, `node_modules`, `.venv`, temp files |

## Safety notes

- A folder's first sync is a `--resync --resync-mode newer`. It merges both sides, keeps the newer copy of each file, and never deletes anything.
- "Stop keeping" runs a final sync first and only moves the local copy out of the way if that sync succeeds.
- Ejecting the drive in Finder keeps it ejected until you choose **Connect Drive**. If rclone crashes, the app remounts automatically.
- Eject and Quit wait for files written to the drive to finish uploading. The upload cache lives in Application Support, not Caches, so macOS won't purge it.
- If bisync's history goes stale, or the server settings change, the folder is marked **needs repair** rather than resynced automatically. Repair is a merge: the newer copy of each file wins and nothing is deleted.
- If more than half a folder's files would be deleted, bisync refuses. The menu then offers **Allow Large Deletion…**.
- Kept folder paths are limited to about 90 characters. That's a limit of rclone bisync's history file names.

## Linux and Windows

`cross/` is a Go port of the same engine: the same `config.json` format, the same rclone backbone and the same safety rules. It has a tray icon, a command-line interface, and a headless mode for servers.

```bash
cd cross && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o opencloud-bar .
```

```bash
cd cross && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui" -o opencloud-bar.exe .
```

| | Linux | Windows |
|---|---|---|
| On-demand drive | `rclone mount` (FUSE 3) | `rclone mount` to a drive letter such as `O:` (needs [WinFsp](https://winfsp.dev)) |
| Settings file | `~/.config/OpenCloudBar/config.json` | `%AppData%\OpenCloudBar\config.json` |
| Tray | StatusNotifier (most desktops) | notification area |
| Dialogs | `zenity` | built-in Windows dialogs (via PowerShell) |

Start it with `opencloud-bar run`, or `opencloud-bar run --no-tray` on a server. Then control it with `status`, `sync`, `keep PATH [--local DIR]`, `unkeep PATH`, `repair PATH`, `allow-delete PATH`, `connect`, `eject`, `reload` and `quit`. On Windows the installer puts two programs in one folder: `OpenCloud Bar.exe` (the tray app, with no console window) and `opencloud-bar.exe` (the same program built as a console app, so commands print their output in a terminal). Windows decides at build time whether a program gets a console, so one file can't do both cleanly.

Windows builds are made by the **Release** workflow and will be signed through SignPath once that is set up (see [Code signing policy](#code-signing-policy)). Tested on Windows 11: two-way sync, the O: drive, eject, stop keeping and quit. On Windows, Stop Keeping moves the local copy to "Removed by OpenCloud Bar" in your user folder rather than the Recycle Bin.

Extra settings for the port:

| Key | Meaning |
|---|---|
| `mountEnabled` | `false` turns the drive off (servers) |
| `kept[].localPath` | keep a folder at an absolute local path instead of `localRoot/path` |
| `rcloneConfig` | path to an rclone.conf |
| `sshPath` | path to the ssh client |
| `transfers` / `checkers` | rclone parallelism |

`deploy-ms02/` shows a headless server setup: a systemd user service, plus an `rclone` wrapper that runs the official Docker image with a memory cap.

## License

MIT

## Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io), certificate by [SignPath Foundation](https://signpath.org).

Windows binaries are built from this repository by the [Windows workflow](.github/workflows/windows.yml) on GitHub-hosted runners, and each release is approved by hand before it is signed.

- Committers and reviewers: [sbstndalton](https://github.com/sbstndalton)
- Approvers: [sbstndalton](https://github.com/sbstndalton)

## Privacy

This program will not transfer any information to other networked systems unless specifically requested by the user or the person installing or operating it. It only connects to the OpenCloud server and SSH host that you put in its settings file.
