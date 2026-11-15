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
| `opencloud-bar_<version>_amd64.deb` / `_arm64.deb` | Debian, Ubuntu | `sudo apt install ./opencloud-bar_*.deb` pulls in FUSE 3 (the SSH client is only recommended, for the legacy tunnel). |
| `opencloud-bar-<version>.x86_64.rpm` / `.aarch64.rpm` | Fedora, openSUSE | `sudo dnf install ./opencloud-bar-*.rpm` |
| `opencloud-bar-linux-amd64.tar.gz` / `arm64` | other Linux | The app plus rclone. Install FUSE 3 yourself. |

## Getting started

1. Install the app (above).
2. Open it. The first time, the set-up opens by itself; later it is **Set Up / Sign In…** in the menu. On the command line: `opencloud-bar setup https://cloud.example.com`.
3. Enter your server address, for example `https://cloud.example.com`. Your browser opens on OpenCloud's own sign-in page. Sign in there.
4. Choose **Keep a Folder…** to keep a folder on this computer. Everything else is on the drive.

That's all: no SSH key, no app token and no rclone configuration. The app signs in the same way OpenCloud's own desktop client does (OpenID Connect with PKCE). The refresh token is stored in the macOS Keychain, the Windows Credential Manager, or the Linux Secret Service. Without a Secret Service (a headless Linux machine) it is kept in a private file (mode 0600) in the app's settings folder. Choose **Sign Out** to forget it.

It connects straight to your server over HTTPS, so it also works through a proxy such as Cloudflare. Uploads use rclone's `infinitescale` WebDAV vendor, which sends large files in chunks (tus) and so isn't stopped by the proxy's 100 MB request limit.

## How it works

[rclone](https://rclone.org) does all the work:

| Piece | rclone command |
|---|---|
| On-demand drive | `rclone nfsmount` on macOS (built-in NFS client, no FUSE or kernel extension), `rclone mount` on Windows and Linux |
| Kept folders | `rclone bisync` (two-way sync with a state file per folder) |

rclone connects directly to your server (`serverURL`) over HTTPS. To authenticate it runs the app's own token command (`opencloud-bar token`, or `OpenCloudBar --token` on the Mac) as its bearer token command. The command prints a cached access token and refreshes it from the stored refresh token when it is nearly out of date. Existing setups that use an SSH tunnel keep working; see [Legacy: SSH tunnel](#legacy-ssh-tunnel) at the end.

## macOS

### Requirements

- macOS 14 or later
- `rclone`, if the app doesn't bundle one (`brew install rclone`)

### Build and install

```bash
./scripts/build-app.sh
```

```bash
./scripts/install.sh
```

On first launch the menu shows **Set Up / Sign In…**; see [Getting started](#getting-started). Settings live in `~/Library/Application Support/OpenCloudBar/config.json`.

## Settings (`config.json`)

| Key | Meaning | Default |
|---|---|---|
| `serverURL` | your server, for example `https://cloud.example.com` (written by Set Up) | — |
| `auth` | `oidc` (browser sign-in) or `rclone` (legacy: the named rclone remote holds the credentials) | `oidc` if `serverURL` is set, else `rclone` |
| `useTunnel` | legacy: send traffic through the SSH tunnel. Set Up leaves it off | `true` only if `sshHost` is set |
| `webdavVendor` | rclone WebDAV vendor for browser sign-in. `infinitescale` uploads in chunks; `owncloud` uses one request per file | `infinitescale` |
| `davPath` | WebDAV path of your personal space (written by Set Up) | — |
| `localRoot` | where kept folders live | `~/OpenCloud/Personal` |
| `mountPoint` | where the drive is mounted | `~/OpenCloud-Drive` |
| `cacheMaxSize` / `cacheMaxAge` | on-demand cache limits | `20G` / `72h` |
| `syncIntervalSeconds` | how often kept folders sync | 300 |
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
| `transfers` / `checkers` | rclone parallelism |

Setting up on the command line: `opencloud-bar setup [URL]` signs in through the browser and writes `serverURL`, `auth` and `davPath` into `config.json` (other keys are kept; if the app is running it reloads). `opencloud-bar logout` forgets the sign-in, and `opencloud-bar token` prints an access token (rclone runs it; you rarely need it). On Windows the tray app runs the console `opencloud-bar.exe` next to it for the token command.

`deploy-ms02/` shows a headless server setup: a systemd user service, plus an `rclone` wrapper that runs the official Docker image with a memory cap.

## License

MIT

## Legacy: SSH tunnel

Older setups reach the server through an SSH tunnel to its local OpenCloud port and keep the credentials in a named rclone remote. They still work unchanged, and there is nothing to migrate unless you want to: run Set Up and the app switches to browser sign-in. Kept folders then need one **Repair** (a merge) because the server identity changed. New installs don't need any of this.

A config is legacy when it has `sshHost` and no `serverURL`. Settings used only by it:

| Key | Meaning | Default |
|---|---|---|
| `sshHost` | SSH alias of the OpenCloud server | — |
| `serverPort` / `localPort` | OpenCloud port on the server / local end of the tunnel | 9200 / 19201 |
| `rcloneRemote` | name of the rclone remote holding the credentials | `opencloud` |
| `sshPath` | path to the ssh client (Linux and Windows) | `ssh` |
| `minServerAvailableMiB` | skip a sync run when the server has less free memory than this; checked over ssh, so tunnel mode only (0 turns it off) | 8192 |

The rclone remote uses an app token (`opencloud auth-app create` on the server, or in the web UI):

```bash
rclone config create opencloud webdav vendor=owncloud user=YOURUSER pass=YOURTOKEN url=http://127.0.0.1:19201/remote.php/dav/files/YOURUSER --obscure
```

On the Mac the tunnel runs as a per-user launchd agent that the app creates and removes, so macOS Local Network privacy doesn't block it and it survives an app crash. This needs SSH key access to the server and the SSH client (recommended, not required, by the Linux packages).

## Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io), certificate by [SignPath Foundation](https://signpath.org).

Windows binaries are built from this repository by the [Windows workflow](.github/workflows/windows.yml) on GitHub-hosted runners, and each release is approved by hand before it is signed.

- Committers and reviewers: [sbstndalton](https://github.com/sbstndalton)
- Approvers: [sbstndalton](https://github.com/sbstndalton)

## Privacy

This program will not transfer any information to other networked systems unless specifically requested by the user or the person installing or operating it. It only connects to the OpenCloud server you enter (and, for legacy setups, the SSH host in its settings file).
