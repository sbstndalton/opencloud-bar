# OpenCloud Bar

A small menu-bar and tray app for [OpenCloud](https://opencloud.eu) on macOS, Windows and Linux. You get your whole OpenCloud space as a drive whose files download when you open them, plus real local copies of the folders you choose, synced both ways. [rclone](https://rclone.org) does the transfers and is included.

## Features

- **On-demand drive.** Your whole OpenCloud space appears as a Finder volume on the Mac, a drive letter such as `O:` on Windows, or a folder on Linux. Files download only when you open them. They are cached (20 GB by default) and removed again after a few days unused.
- **Kept folders.** Pick folders to keep as real local copies, synced both ways every few minutes. Stopping keeping one syncs it a last time, then moves the local copy out of the way. The folder stays in OpenCloud.
  - If the two sides drift out of sync, the folder is merged automatically: the newer copy of each file wins, and the older copy is kept in a backup folder (see [Safety](#safety)).
  - Anything that needs you says why in the menu.
- **Browser sign-in.** You sign in on your server's own page, the same way OpenCloud's desktop client does it (OpenID Connect with PKCE). There is no app token, SSH key or rclone set-up.
- **Pause and resume** syncing. The drive keeps working while paused.
- **Notifications** when a folder needs attention, was merged automatically, when you are signed out, and when an update is available. You get one per change, never repeated.
- **Update checks** once a day (they can be turned off), plus **Check for Updates…**. A new version is announced, not installed: the app opens the release page and you download it yourself.
- **Settings in a window:** native on the Mac, a small app window on Windows and Linux. It covers kept folders, account and storage, sync, drive, general settings, logs and About. Live activity shows in the menu, for example "Syncing “School” (2 of 3) · 12 files…".

## Download

Get the latest version from [Releases](https://github.com/sbstndalton/opencloud-bar/releases/latest). Every download includes rclone.

| File | For | Install |
|---|---|---|
| `OpenCloud-Bar-macOS.dmg` | macOS 14 or later, Apple silicon and Intel | Drag **OpenCloud Bar** to Applications. The drive uses macOS's own NFS client, so nothing else is needed. |
| `OpenCloud-Bar-Setup-<version>.exe` | Windows 10/11, x64 | Run it. It installs the app, rclone and [WinFsp](https://winfsp.dev) (for the drive letter). It adds a Start menu entry, an optional start at sign-in, and the `opencloud-bar` command for terminals. |
| `opencloud-bar_<version>_amd64.deb` / `_arm64.deb` | Debian, Ubuntu | `sudo apt install ./opencloud-bar_*.deb` (pulls in FUSE 3) |
| `opencloud-bar-<version>.x86_64.rpm` / `.aarch64.rpm` | Fedora, openSUSE | `sudo dnf install ./opencloud-bar-*.rpm` |
| `opencloud-bar-linux-amd64.tar.gz` / `arm64` | other Linux | Unpack it and run `opencloud-bar`. Install FUSE 3 yourself. |

### The first launch

- **macOS:** the app isn't notarized by Apple, so the first time macOS says it can't check it for malicious software.
  1. Choose **Done**.
  2. Open **System Settings ▸ Privacy & Security**.
  3. Scroll down to the message about OpenCloud Bar and choose **Open Anyway**, then confirm.

  This is needed only once per download.
- **Windows:** the installer isn't code-signed (see [Code signing policy](#code-signing-policy)), so SmartScreen may say "Windows protected your PC". Choose **More info ▸ Run anyway**. Signing the programs inside it doesn't change that warning.
- **Linux:** start **OpenCloud Bar** from your applications menu, or run `opencloud-bar` in a terminal.

## Getting started

1. Open the app. The first time, the set-up opens by itself; later, choose **Set Up / Sign In…** in the menu. On the command line, run `opencloud-bar setup https://cloud.example.com`.
2. Enter your server address, for example `https://cloud.example.com`. Your browser opens your server's sign-in page; sign in there.
3. In the menu, open **Kept Folders ▸ Keep a Folder…** and choose a folder to keep on this computer. Everything else stays on the drive.

**Settings…** opens everything else. **Sign Out** (Settings ▸ Account) forgets the sign-in on this computer.

The app connects straight to your server over HTTPS, so it also works through a proxy such as Cloudflare. Uploads use rclone's `infinitescale` WebDAV vendor, which sends large files in chunks (tus) and so isn't stopped by a proxy's request size limit.

## Using it

### The menu

The menu stays short however many folders you keep. It has:
- the status line;
- the drive items;
- a **Kept Folders (N)** submenu;
- **Sync Now**, **Pause Syncing** and **Settings…**;
- **Check for Updates…**, **About OpenCloud Bar** and **Quit**.

In **Kept Folders**, each folder shows its status or when it last synced. Folders that need attention are listed first and say why. On the Mac they open a submenu with the fix, for example **Repair**; on Windows and Linux, choosing one opens its settings. Choosing any other folder opens it.

### The settings window

On the Mac, **Settings…** (⌘,) opens these pages. Windows and Linux have the same sections in their settings window, plus About.

| Page | What it does |
|---|---|
| **Kept Folders** | A table of your kept folders: name, location on this Mac, status and when each last synced (this survives restarts). Per folder: Open in Finder, Open in Browser, Sync Now, Repair, Allow Large Deletion (when needed) and Stop Keeping. Select a folder to see its paths, its last error and its own exclude rules. |
| **Conflicts** | Conflict copies (`name.conflict1`) in your kept folders, with Reveal, Keep This Version and Discard. See [Conflicts](#conflicts). |
| **Account** | Server address and version, your name, email and username, and a storage bar with used and total space. **Open OpenCloud in Browser**, **Refresh**, and **Sign Out** (or **Sign In…**). It reads these from the server with your sign-in; nothing is shown if you're signed out. |
| **Sync** | How often kept folders sync (1, 5, 15, 30 or 60 minutes), the exclude rules for all folders (with **Restore Defaults**), where kept folders live on this Mac (changeable only while no folder is kept), upload and download limits, and the file names the server refuses. |
| **OpenCloud Drive** | Connect or eject the drive, its mount location and volume name, the cache size limit and age, and live cache use and pending uploads while it is connected. |
| **General** | Open at Login, the version, update checks (**Check Now**) and notifications. With an SSH tunnel: whether to check the server's memory before syncing. Under **Advanced**: the SSH tunnel settings, the rclone path, **Open Settings File** and **Reload Settings**. |
| **Logs** | The app log, the drive and tunnel logs, and each folder's sync log: the last 3000 lines, refreshed every 2 seconds, with errors in red. **Copy**, **Reveal in Finder** and **Clear…**. |

Changing exclude rules (for one folder or for all) asks first. Each affected folder then syncs right away, comparing both sides once (newer copy wins, replaced files kept in the backups, nothing deleted), without a Repair. A filters file changed by hand outside the app still asks for a Repair.

### Command line (Windows and Linux)

`opencloud-bar run` starts the app (`run --no-tray` on a server). Then control it with:
- `status`, `sync`, `pause`, `resume`;
- `keep PATH [--local DIR]`, `unkeep PATH`, `repair PATH`, `allow-delete PATH`;
- `connect`, `eject`, `settings`, `check-updates`, `version`, `reload`, `quit`.

`setup [URL]` signs in through the browser and `logout` forgets the sign-in. `token` prints an access token; rclone runs it, and you rarely need it.

On Windows there are two programs in one folder:
- `OpenCloud Bar.exe` is the tray app, with no console window.
- `opencloud-bar.exe` is the same program as a console app, so commands print their output in a terminal.

`deploy-ms02/` shows a headless server set-up: a systemd user service and an `rclone` wrapper that runs rclone's official Docker image. The memory cap is on that container (3 GB), not on the service: a limit on the service would kill it mid-sync.

## Spaces

Besides your personal files, OpenCloud has **project spaces** and folders **shared with you**. You can keep folders from them too:

- **Mac:** Settings ▸ Kept Folders ▸ **Keep from a Space…**. Choose the space, then the folder (type its path or pick it from the list).
- **Windows, Linux:** in the settings window, Folders ▸ **Keep a folder by its path, or from another space**: **Load Spaces**, choose one, then type or list the folder.
- The app asks the server which spaces you have (`/graph/v1.0/me/drives`: project spaces and accepted shares, not disabled ones). Whether the folder is read-only for you is checked when you keep it: a read-only folder is refused, since a kept folder syncs both ways. Open it in the browser instead.
- A space's folders go to `<local folder>/<space name>/<folder>`, apart from your personal folders. The folder list shows which space each one is in. Characters Windows can't use in a folder name become `-`, and names like `CON` or `NUL` get a `_`, on every system, so the settings file works on any computer.
- The same path can be kept from two spaces with different names (`Docs` from Marketing and `Docs` from Sales go to `Marketing/Docs` and `Sales/Docs`). Two spaces with the same name would share a local folder, so keeping the second one is refused; so is any folder whose local copy would be, contain or sit inside another kept folder's copy.
- They sync like personal folders, with the same sign-in; merge backups go to `.opencloud-bar-backups` at the top of that space (see [Safety](#safety)).
- The drive still shows the personal space only.

## Choose subfolders

You can sync only some subfolders of a kept folder:

- **Mac:** Settings ▸ Kept Folders, select the folder, then **Choose Subfolders…** (also in its Actions menu).
- **Windows, Linux:** in the settings window, Folders, select the folder, then **Choose Subfolders…**.

The list comes from OpenCloud: the folder's subfolders, and with **Show Subfolders** the ones inside those. Uncheck what you don't need here and save.

- Each unchecked subfolder becomes one exclude rule in the folder's excludes, `- /<name>/**` (or `- /<name>/<name>/**`), with `* ? [ ] { } \` in names escaped. Names match exactly, case included (`Photos` and `photos` are two folders).
- These rules go first, as one block at the top of the folder's excludes: rclone uses the first rule that matches, so an unchecked subfolder stays unsynced whatever your own rules say. Your own rules follow, in their order and unchanged. A rule you wrote by hand in exactly that form counts as the checklist's: it shows as unchecked, and checking that subfolder removes it.
- After saving, the folder syncs right away, comparing both sides once: the newer copy of each file wins, and replaced files are kept in the backups (see [Safety](#safety)). No Repair is needed. Changing a folder's excludes, or the common excludes, works the same way.
- Nothing is deleted on either side. An unchecked subfolder stays in OpenCloud, and its copy on this computer stays where it is, no longer synced. Move it to the trash yourself if you don't need it; **Stop Keeping** moves it to the trash with the rest of the folder. Checking it again brings it back with that comparison.
- A folder name with control characters or leading or trailing spaces can't be unchecked here; use the excludes instead.

## Recent changes

Every sync remembers what it changed: files uploaded, downloaded, deleted on this computer (also removed from OpenCloud) and deleted in OpenCloud (also removed here). The last 500 changes are kept, in `activity.json` in the sync state folder (written safely; a damaged file is kept aside as `activity.json.bad`).

- **Mac:** the menu's **Recent Changes** lists the last 10; click one to show it in Finder. Settings ▸ **Activity** lists them all, with a search field, **Show in Finder** and **Open OpenCloud**.
- **Windows, Linux:** the tray's **Recent Changes** lists the last 5; the settings window's **Activity** section lists them all.
- A merge (a first sync, a Repair, the automatic merge) is one line, like "Repair of Notes: 512 files compared, 2 updated", and so is a sync that changed more than 200 files ("Sync of Photos: 5000 uploaded"). Conflict copies and the backup folder aren't listed.
- bisync doesn't report renames or moves: a renamed or moved file shows as a new file and a deleted one.
- Syncs now log at rclone's INFO level (every file copied or deleted). Each folder's sync log starts a new file at 5 MB (the previous one is kept as `.1`), also during a long sync, and the app keeps only the start and end of a sync's output in memory.

## Share links

**Copy Public Link…** makes a public, read-only link to a file or folder in a kept folder and copies it, so you can paste it anywhere. Anyone with the link can view and download it, without signing in. It stays active until you delete it in OpenCloud (in the browser: the file's sharing panel), unless you pick an expiry.

- **Mac:** the menu's **Copy Public Link…** (choose any file or folder in your kept folders), or a kept folder's **Copy Public Link…** in Settings ▸ Kept Folders.
- **Windows, Linux:** select the folder in the settings window, Folders, then **Copy Public Link…** and choose the file or folder.
- Choose when the link expires (never, 1, 7 or 30 days) and, if you like, type a password for it. The password goes to OpenCloud with the request and isn't stored or logged by OpenCloud Bar.
- Only for kept folders that have synced, and only for files that are in OpenCloud already; with the browser sign-in only. If the server requires a password for links, it says so.
- The link is made with OpenCloud's libre-graph API (`createLink`, type `view`); only an https link is accepted. Manage or delete links in OpenCloud's web interface.

## Live sync (preview)

Off by default. Turn on **Sync local changes right away (preview)** (Mac: Settings ▸ Sync; Windows, Linux: the settings window, Sync) and OpenCloud Bar watches the local copy of each kept folder. When you change something in one, the folder shows "Change detected, syncing soon" and syncs on its own, without waiting for the interval:

- A folder syncs once nothing in it has changed for 5 seconds, or 60 seconds after the first change while changes keep coming.
- Syncs started this way are at least 30 seconds apart per folder (longer if the folder's last sync took longer). The regular interval syncs aren't affected, and there is never more than one sync at a time: the folder waits in the same queue as a newly kept folder.
- The app's own writes don't count. Changes seen while the app syncs a folder are kept aside; when the sync is over, the files it downloaded or deleted here are taken out, and if anything else is left (a change you made meanwhile), the folder syncs once more after the 30 seconds.
- Noise doesn't count: rclone's temporary files (`.partial`, `.~tmp~`), the backups folder, conflict copies, `.DS_Store`, `Thumbs.db`, `desktop.ini`, `._*`, `*~`, `*.tmp`, `.sync_*.db`, Office lock files (`~$*`), LibreOffice lock files (`.~lock.*#`) and the simple file names in your excludes (like `- *.bak`). Folders excluded by name (like `- **/node_modules/**` or `- /Old/**`) aren't watched; other excludes still apply to the sync itself.
- While paused, changes only mark the folder; it syncs when you resume.
- Recent Changes shows "(after a local change)" for changes made by such a sync.
- Changes made in OpenCloud still arrive at the next interval.
- Mac: FSEvents watches each folder (about 1 second latency). Watches start again after the Mac wakes from sleep, when a folder's excludes change, and when its folder is moved or replaced. Windows and Linux: each folder in the tree is watched. If the system's watch limit is reached (Linux `fs.inotify.max_user_watches`), or the kept folders have more than 2,000 folders in all on Windows (each watch takes memory there) or 20,000 on Linux, that kept folder goes back to the interval and a notice says so once.
- If watching fails or loses events, or the kept folder itself is moved, removed or replaced, the folder syncs and watching starts over. A local folder that is a symbolic link is watched where it leads.
- A kept folder whose local copy is missing isn't watched; it is watched once it is there again.

## Keeping a folder while a sync runs

**Keep a Folder…** and **Keep from a Space…** work while a sync is running. The folder is added at once and its first sync starts as soon as the running sync finishes. Only the new folder syncs then; the others follow at the next interval. On the Mac, a folder's own excludes or subfolders can be changed while a sync runs, unless that sync still has to do this folder. On Windows and Linux they are saved at once and applied when the running sync finishes; the folder then syncs right after.

## Conflicts

When a file changed on both sides between two syncs, the newer copy keeps its name and the other is saved next to it with `.conflict1` added to the whole name (`report.pdf.conflict1`; a second one is `.conflict2`). Nothing is lost, but nothing tells you either, so OpenCloud Bar looks for these copies after each sync:

- The menu shows "N conflicts", and you get one notification for each new batch.
- **Settings ▸ Conflicts** lists each copy with its folder and when it changed:
  - **Reveal in Finder** (Mac) / **Show in Folder** (Windows, Linux).
  - **Keep This Version** makes the conflict copy the file. The file it replaces goes to the local backups (`backups/` in the sync state folder, kept 30 days).
  - **Discard** moves the conflict copy to the Trash (Windows: the "Removed by OpenCloud Bar" folder in your user folder).
- These are changes on this computer; the next sync applies them to OpenCloud. They wait while a sync is running.

## Bandwidth limit

**Settings ▸ Sync ▸ Upload limit / Download limit** slow syncs and the drive down so they leave room for other things. Use rclone's units: `512K` is 512 KiB/s, `2M` is 2 MiB/s, a plain number is KiB/s. Leave a field empty for no limit. The drive restarts to pick up a change; a running sync uses it from the next sync. In the settings file the keys are `bwLimitUp` and `bwLimitDown`.

## Safety

- A folder's first sync, a Repair and an automatic merge all merge both sides: the newer copy of each file wins. No file is deleted, but the older copy of a file is replaced, so a merge keeps it first:
  - in OpenCloud, in `.opencloud-bar-backups/<stateName>/<date and time>/` at the top of the folder's space (`<stateName>` is the folder's `stateName` in the settings file, its name plus a short code). For personal folders that is the top of your personal space, shown on the drive; for a folder from a project space or a share it is the top of that space, which you reach in the web UI (the drive shows the personal space only). They are never synced and **not removed automatically**: delete old ones when you no longer need them;
  - on this computer, in `backups/<stateName>/<date and time>/` in the sync state folder (`stateDir`), removed after 30 days.
- An automatic merge happens at most once per folder a day; after that, the folder waits for you to choose **Repair**. Until then, each sync interval tries a plain sync, and if that works the folder needs no repair after all.
- A sync stopped by Quit, sleep or a timeout resumes the next time without a repair.
- If more than half a folder's files would be deleted, the sync stops and the folder offers **Allow Large Deletion…**.
  - The dialog says on which side the files were deleted.
  - That override is never offered when the copy on either side is missing or empty: for example the folder is gone from OpenCloud, or the disk with this computer's copy isn't connected. There, applying the "deletion" would empty the other copy. The folder says so instead: reconnect the disk, or choose **Stop Keeping**, which then just stops syncing it here (no final sync, nothing moved to the trash, the folder in OpenCloud stays as it is).
- After upgrading to 1.0, each kept folder does one merge on its own (backed up as above, and not counted as the day's automatic merge), because the app added a line to its sync filters and rclone then insists on starting the sync history over. No Repair is needed.
- **Stop Keeping** runs a final sync first, and only moves the local copy if that sync succeeds. It goes to the Trash (Mac and Linux), or to "Removed by OpenCloud Bar" in your user folder (Windows).
- Eject, Quit and Sign Out wait for files written to the drive to finish uploading. If the app can't tell, they ask first. The Windows installer and uninstaller don't force it either: while files may still be uploading they stop and ask you to try again (or to quit from the tray). Only `opencloud-bar quit --force` / `logout --force` skip the check.
- Ejecting the drive in Finder keeps it ejected until you choose **Connect Drive**. If rclone crashes, the drive is mounted again automatically.
- Kept folder paths are limited to about 90 characters (a limit of rclone bisync's state file names).

## Where your sign-in is stored

- **macOS:** in a private file, `~/Library/Application Support/OpenCloudBar/refresh-token`. Only you can read it (file 0600, folder 0700): the same protection rclone gives its own `rclone.conf`.
  - It isn't in the Keychain on purpose. Each build of the app is ad-hoc signed, so every update looks like a new program to the Keychain. macOS would then ask for permission at launch, and rclone's background token requests would wait on a dialog nobody sees.
  - Versions before 0.5 used the Keychain. 0.5 and later move the token to the file once, then delete the Keychain item. macOS may ask one last time; if nobody answers within a minute, the app offers **Sign In Again…** instead.
- **Windows:** the Windows Credential Manager.
- **Linux:** the Secret Service. Without one (a headless machine), a private file (0600) in the settings folder.

Only the refresh token is kept. The short-lived access token is cached next to the settings in `auth.json` (0600). Your password never reaches the app: you type it on your server's own sign-in page.

**Security trade-off of the file (macOS, and Linux without a Secret Service):** file permissions keep other users out, but not other programs running as you. Any program you run can read the token, as it can read `rclone.conf`, your SSH keys or browser data. The Keychain would ask before another program reads it, but not reliably for an ad-hoc-signed app (see above). If someone may have copied it, choose **Sign Out** (it deletes the file), and sign out of other sessions in OpenCloud's web UI to revoke it on the server. Backups of your home folder (Time Machine, for example) include the file.

## Uninstall

- **macOS:**
  1. Choose **Sign Out** in Settings ▸ Account if you want the sign-in removed.
  2. Quit OpenCloud Bar.
  3. Drag the app from Applications to the Trash.
- **Windows:** use **Settings ▸ Apps ▸ Installed apps ▸ OpenCloud Bar ▸ Uninstall**. It stops the app and removes the stored sign-in. WinFsp is a separate app; uninstall it too if nothing else uses it.
- **Linux:**
  1. Run `opencloud-bar logout` and `opencloud-bar quit`.
  2. If you use the service, run `systemctl --user disable --now opencloud-bar`.
  3. Run `sudo apt remove opencloud-bar`, or `sudo dnf remove opencloud-bar`.

**What stays:** your kept folders (by default `~/OpenCloud/Personal`, untouched), the merge backups in OpenCloud (`.opencloud-bar-backups`), and the settings folder. Delete the settings folder by hand for a clean slate. It holds the settings, the sync state and local merge backups, the logs and the drive cache, and on macOS also the sign-in file if you didn't sign out:
- **macOS:** `~/Library/Application Support/OpenCloudBar` (settings, sync state, backups, drive cache in `vfs-cache`, sign-in) and `~/Library/Logs/OpenCloudBar`. `~/Library/Caches/OpenCloudBar` only holds a lock file and, with an SSH tunnel, its control socket. With an SSH tunnel there is also `~/Library/LaunchAgents/<bundle id>.tunnel.plist`.
- **Windows:** `%AppData%\OpenCloudBar`.
- **Linux:** `~/.config/OpenCloudBar`.

## Known limits

- **Builds aren't notarized (macOS) or code-signed (Windows) yet**, so the first launch needs the steps above. Notarization needs a paid Apple Developer ID. Windows signing through SignPath is being set up. Once it is, it covers the programs inside the installer (`OpenCloud Bar.exe` and `opencloud-bar.exe`); the installer `OpenCloud-Bar-Setup-<version>.exe` itself is still unsigned, so SmartScreen may still warn when you run it.
- **Linux tray:** the tray icon uses StatusNotifier. GNOME shows it only with the AppIndicator extension; KDE, Xfce, Cinnamon and most others show it as is. Without a tray the app still runs (`opencloud-bar run --no-tray`) and the settings page and command line work.
- **Linux dialogs** use `zenity` and notifications use `notify-send` (package `libnotify-bin` or `libnotify`; the packages recommend both). Without zenity, sign in and keep folders from the settings page instead. Quit then only quits when nothing is still uploading; otherwise a notification says so, and `opencloud-bar quit --force` quits anyway.
- One account per computer. The drive shows your personal space only; folders from project spaces and shares can be kept (see [Spaces](#spaces)), but not browsed on the drive. Read-only folders can't be kept. The "Shares" overview itself isn't a space you can keep from; each accepted share is.
- Conflicts are found in the local copies of kept folders only (not on the drive), up to 200 per folder. Folders excluded everywhere (like `- **/node_modules/**`) aren't searched.

## How it works

| Piece | rclone command |
|---|---|
| On-demand drive | `rclone nfsmount` on macOS (built-in NFS client, no FUSE or kernel extension), `rclone mount` on Windows (WinFsp) and Linux (FUSE 3) |
| Kept folders | `rclone bisync`, a two-way sync with a state file per folder |

rclone connects to your server (`serverURL`) over HTTPS. To authenticate, it runs the app's own token command (`opencloud-bar token`, or `OpenCloudBar --token` on the Mac). That command prints a cached access token, refreshing it from the stored refresh token when it is nearly out of date.

The macOS app is native SwiftUI (`Sources/`). Windows and Linux share a Go program (`cross/`) with the same settings file format and the same safety rules.

## Build from source

- **macOS** (Xcode command line tools): `./scripts/build-app.sh` builds `build/OpenCloud Bar.app` (universal; `ARCHS=arm64` for one architecture, `VERSION=1.2.3` to stamp a version). `./scripts/build-dmg.sh` makes the DMG with a universal rclone inside, and `./scripts/install.sh` copies the app to `~/Applications`.
- **Linux:** `cd cross && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=1.2.3" -o opencloud-bar .`
- **Windows:** `cd cross && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui" -o "OpenCloud Bar.exe" .` (and once more without `-H windowsgui` for `opencloud-bar.exe`).
- **Tests:** `cd cross && CGO_ENABLED=0 go test -tags notray ./...`

Releases are built by the [Release workflow](.github/workflows/release.yml) for every `v*` tag, with notes from [CHANGELOG.md](CHANGELOG.md).

## Advanced

### The settings file (`config.json`)

Everything can be changed in the settings window. The file is only a fallback for hand edits:
- **macOS:** `~/Library/Application Support/OpenCloudBar/config.json`. Open it with **Settings ▸ General ▸ Advanced ▸ Open Settings File**, then choose **Reload Settings**.
- **Windows:** `%AppData%\OpenCloudBar\config.json`.
- **Linux:** `~/.config/OpenCloudBar/config.json`.

Keys the app doesn't know about are kept when it saves.

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
| `kept` | kept folders: `path` (relative to the space root), `stateName`, optional `extraFilters`, and `space` (`id`, `name`, `davPath`) for a folder from a project space or share; without `space` it is the personal space | `[]` |
| `commonFilters` | rclone filter rules for every kept folder | build folders, `node_modules`, `.venv`, temp files |
| `paused` | kept folders don't sync | `false` |
| `checkForUpdates` / `notifications` | daily update check / notifications | `true` / `true` |
| `bwLimitUp` / `bwLimitDown` | upload / download limit, like `512K` or `2M` | none |

Extra settings on Windows and Linux:

| Key | Meaning |
|---|---|
| `mountEnabled` | `false` turns the drive off (servers) |
| `kept[].localPath` | keep a folder at an absolute local path instead of `localRoot/path` |
| `rcloneConfig` | path to an rclone.conf |
| `transfers` / `checkers` | rclone parallelism |

### SSH tunnel (legacy setups)

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

Windows binaries are built from this repository by the [Release workflow](.github/workflows/release.yml) on GitHub-hosted runners, and each release is approved by hand before it is signed. Signing covers the two programs (`OpenCloud Bar.exe`, `opencloud-bar.exe`); the installer that contains them is not signed yet.

- Committers and reviewers: [sbstndalton](https://github.com/sbstndalton)
- Approvers: [sbstndalton](https://github.com/sbstndalton)

## Privacy

This program will not transfer any information to other networked systems unless specifically requested by the user or the person installing or operating it. It only connects to the OpenCloud server you enter (and, for legacy setups, the SSH host in its settings file), and, once a day, to GitHub to check for a new release: one anonymous request to `api.github.com/repos/sbstndalton/opencloud-bar/releases/latest` with no sign-in and nothing about you or your files (GitHub sees your IP address and the app version, as with any web request). Turn it off in Settings ▸ General ▸ Check for updates automatically.

## License

MIT. See [LICENSE](LICENSE). rclone is MIT licensed too; its license is included in every download.
