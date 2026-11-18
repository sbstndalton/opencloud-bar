# Changelog

All notable changes to OpenCloud Bar. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

The release workflow copies the section for a tag into that tag's GitHub release, so every version heading must stay in the form `## [X.Y.Z] - date`.

## [2.5.0] - 2026-11-18

### Added
- **Transfers.** A read-only list of what is moving right now. Mac: Settings ▸ **Transfers**, and a **Transfers (N)…** menu item that appears only while something is transferring. Windows and Linux: a **Transfers** section in the settings window, from `GET /ui/api/transfers` (behind the same sign-in as the page's other reads). It shows:
  - each file a kept folder's sync is moving, with its progress, size, speed and time left, read from rclone's progress blocks (the "Transferring:" lines, every 2 seconds). A file whose size isn't known yet shows "Transferring" without a percent. A folder's files are cleared when its run ends;
  - the drive's uploads, from rclone's `vfs/queue` (file names, sizes, uploading or waiting). If it doesn't answer, the `vfs/stats` counts of uploads in progress and waiting are shown, without names. The drive is asked at most every 5 seconds, and the Windows/Linux page asks only while it is open;
  - "Nothing is transferring." when there is nothing.
  rclone doesn't report which way a file goes, so nothing says upload or download and there is no arrow.

### Changed
- Syncs now run rclone with `--stats-file-name-length 0` (next to `--stats 2s`), so the progress blocks carry whole file names instead of names cut to 45 characters. The blocks are still kept out of the sync log.

## [2.4.0] - 2026-11-17

### Added
- **Windows and Linux: Start at sign-in.** A checkbox in Settings ▸ General, reported by the page's API as `openAtLogin` (and set with `POST /ui/api/config`, `{"openAtLogin": true|false}`). It is the operating system's setting, read when the page asks, and is never stored in `config.json`.
  - Windows: the `OpenCloud Bar` value under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, the same name the installer writes. The installer offers "Start OpenCloud Bar when I sign in" on a first install only; an update doesn't show it and leaves the setting as it is, so an update never turns it back on. Uninstalling always removes the value, whether the installer's option or the checkbox set it. The value is the quoted path of `OpenCloud Bar.exe` (when run from `opencloud-bar.exe`, the windowed program next to it).
  - Linux: `$XDG_CONFIG_HOME/autostart/opencloud-bar.desktop` (`~/.config/autostart` by default). If the systemd user service is enabled, the page says "Started by the systemd service" and turning the setting on is refused (nothing is written), so the app can't start twice. Turning it off still works and removes the file, so the checkbox stays usable while the file exists next to the service; with no file it is disabled. The entry runs the path the system reports for the program, not a resolved symlink.
- **Tests on every push and pull request** (`.github/workflows/test.yml`): `go vet` and `go test -race` on Windows, Ubuntu and macOS (`-tags notray` on the last two), and a release build of the Mac app.
- `docs/manual-test-checklist.md`: what to try by hand on Windows 11 and Ubuntu before a release.

### Fixed
- A change to Start at sign-in together with other settings is applied first; if it is refused, nothing else is saved. The remembered state is dropped after the change is written, so the page shows the new state at once.
- Tests on Windows: each test's temporary home now also sets `USERPROFILE` and `LOCALAPPDATA` (Windows ignores `HOME`), and a test that relied on a read-only folder refusing new files is skipped there.

## [2.3.0] - 2026-11-17

### Added
- **Pause for a set time.** Pause Syncing is now a choice: **For 1 Hour**, **Until Tomorrow** (the next 08:00, local time) or **Until I Resume**. Mac: the menu's **Pause Syncing** submenu (also in Settings ▸ Kept Folders). Windows and Linux: the tray's **Pause Syncing** submenu and the command line, `opencloud-bar pause --for 1h` (a length like `30m` or `8h`, from a minute to 30 days; `pause` alone lasts until `resume`). A timed pause is saved as `pausedUntil` (RFC 3339) next to `paused` in `config.json`, the same key in both apps; syncing resumes by itself at that time (checked every few seconds while the app runs, and right at the next start), and the status says "Paused until 14:30". Resuming, or pausing without a time, removes it. A clock set forward only ends the pause early.
- **Windows and Linux: Sync Now for one folder.** A **Sync Now** button in a folder's details, `opencloud-bar sync PATH`, and `POST /ui/api/sync` with an optional `{"path": …}`. The folder goes first, like a newly kept one (also while the interval's run is going on); the others are not synced. The button is disabled while syncing is paused, while that folder syncs or waits, and until the app is set up. A request that can't start says why instead of claiming it started, and `sync PATH` then exits non-zero: syncing is paused, Repair, Allow Large Deletion, Stop Keeping or a conflict action is running or waiting, you are signed out, or the settings file has a problem; an unknown path is a 400. The top **Sync Now** (all folders) is disabled the same way, and also while any sync runs. On the Mac, the menu's **Sync Now** is disabled while paused or signed out, and a folder's **Sync Now** (its Actions menu) also while that folder syncs or waits.
- **Mac: keep a folder without the drive.** The folder picker (Settings ▸ Kept Folders ▸ **Keep from a Space…**) has a **Personal** entry at the top, listing your own files from the server with rclone. With the browser sign-in, keeping a personal folder asks the server first, as keeping a space's folder does: a folder that isn't there is refused with "That folder doesn't exist in OpenCloud", and a read-only one can't be kept. **Keep a Folder…** in the menu opens this picker when the drive isn't connected, instead of asking you to connect it, and the first sign-in opens it too.

### Changed
- **Folder actions wait instead of greying out.** Repair, Allow Large Deletion, Stop Keeping and the conflict actions (Keep This Version, Discard) can be chosen while a sync runs. They wait for the running sync, as Repair already did on the Mac, and say so: Windows and Linux show "Waits for the running sync" (taken from the app's answer, `{"queued": true|false}` from the page's API, not from what the page last saw); the Mac menu shows "Starts when the running sync finishes". Only one action waits at a time; a second one is refused ("Another action is waiting"). When an action runs after waiting, it looks everything up again first:
  - **Allow Large Deletion** runs only if the folder is still blocked for the same thing you were shown (the side the deletion is on: on this computer, or in OpenCloud). Otherwise nothing runs and the app says "The folder changed while waiting; check it again before allowing." (Neither app counts the deleted files, so the side is what is compared.)
  - **Repair** runs only if the folder still needs a repair; otherwise it says the folder no longer needs a repair.
  - **Keep This Version** and **Discard** use the folder's current location and settings; if the folder is no longer kept, or the conflict copy was synced or removed meanwhile, nothing is changed and the app says so. On Windows and Linux a failure after waiting appears in the status message ("Couldn't keep …") and the log; on the Mac it is an alert.
  - Windows and Linux: a Repair or Allow Large Deletion that isn't needed when you choose it is refused at once. The conflict actions now queue like the others instead of being refused during a sync; **Show in Folder** never waits.
- **Windows/Linux:** the settings page's timed pause (`POST /ui/api/pause` with `until`) has the same limits as `pause --for`: a minute to 30 days.
- **Mac:** if ending a timed pause fails (the settings can't be saved), the app waits 5 minutes before trying again and shows one alert, not one per tick.
- **Mac:** a message in the menu (for example "Link copied") disappears after about 2 minutes. The signed-out message stays until you sign in.
- **Mac:** **Show in Settings…** for a folder in the menu now selects that folder in Settings ▸ Kept Folders.

### Fixed
- **Mac:** after the first sign-in the message told you to choose "Keep a Folder on This Mac…", a menu item that doesn't exist. It now opens the folder picker.
- **Windows/Linux:** the Folders page failed to draw its folder list (a variable in the page's script hid the function that keeps focus while the list is redrawn).

## [2.2.0] - 2026-11-17

### Added
- **Search.** Find files and folders in OpenCloud by name, in your personal space, project spaces and shares, like the web interface's search box. Mac: the menu's **Search OpenCloud…** (⌘F) opens a small window. Windows and Linux: the settings window's **Search** section. Up to 50 matches with name, folder and modification date (and how many there are in all when there are more). **Show in Browser** opens the item in OpenCloud's web interface (`<server>/f/<the item's own id>`: a file's folder with the file selected, a folder directly); **Show on This Computer** appears when the item is in a kept folder and is on this computer. Uses a WebDAV `REPORT` with `oc:search-files` on `<dav>/spaces/` and the query `name:"*<term>*"`, as OpenCloud's web interface does (answered by OpenCloud's webdav and search services).
- **Storage warnings.** With the browser sign-in, the personal space's quota is checked every 30 minutes. One notification at 90% ("Your OpenCloud storage is almost full") and one when it is full; not repeated until the use has gone below 90% again. A check that fails is tried again after 2 minutes. The last warning is remembered in the sync state folder (`quota-warned`); signing out, signing in as someone else or changing the server forgets it and the shown storage. No warnings without a limit (total missing or 0). Mac: the menu shows "Storage: X of Y used" ("Storage almost full: …" from 90%). Windows and Linux: a notice at the top of the settings window from 90%, and an Activity line with each warning; the Account page shows X of Y used as before.
- **Mac:** Settings ▸ General explains notification permission. When macOS hasn't been asked yet (common for builds not signed by a registered developer) there is an **Allow Notifications…** button; if macOS still doesn't ask, it says plainly that this build can't turn notifications on. **Open Notification Settings…** (System Settings ▸ Notifications; nothing is changed by the app) is only shown when macOS is blocking them. The permission is read at every notification and asked for only while undecided, so allowing them later works without a relaunch.

### Changed
- **Mac:** the **Volume name** setting is gone. It never did anything since the drive became an NFS mount: macOS names an NFS volume after its mount point folder, and `rclone serve nfs` only exports `/`, so there is no other name to set. The OpenCloud Drive page now says that Finder names the drive after the last folder of its location; choose a location with the name you want to rename it. An existing `volumeName` in `config.json` is kept but ignored on the Mac. Windows keeps the setting (rclone mount's `--volname`); the Linux page no longer shows it, because `--volname` only works on Windows and macOS.

### Fixed
- **Windows/Linux tests:** the tests no longer write sync logs into the real app folder (for example `~/Library/Application Support/OpenCloudBar/logs` when run on a Mac). An engine a test started could write a log after the test had restored its environment; the test run now points the home and config folders at a temporary folder for the whole run, and fails if any test resolves the real app folder.

### Security
- Search and quota requests only go to the configured server (URLs built from its origin and the paths the server gave), with no redirects followed. The browser link is built from the configured server and an id that must be one plain path segment; the page never sends an address. Search results are shown with plain text only.

## [2.1.0] - 2026-11-16

### Added
- **File history.** See the earlier versions OpenCloud keeps of a file in a kept folder (date and size) and restore one. Mac: the menu's **File History…** (an open panel in the local folder; files in kept folders only) and a kept folder's **File History…** in Settings ▸ Kept Folders. Windows and Linux: the settings window, Folders, select a folder, **File History…**, then pick the file from the folder's listing. Before restoring, the copy on this computer is copied to the local backups (`backups/<stateName>/<stamp>/` in the sync state folder); if that fails, nothing is restored. The folder then syncs (while syncing is paused: when you resume). A restore is refused while that folder syncs, and when the local copy differs from the current file in OpenCloud (size, or modification time by more than 2 seconds; checked again right before the restore): "This file has changes that haven't synced yet. Sync first, then restore." Uses OpenCloud's WebDAV versions API: `PROPFIND <dav>/meta/<file id>/v` (`Depth: 1`) and `COPY` of `…/v/<version>`, as OpenCloud's web interface does.
- **Deleted files.** Settings ▸ **Deleted Files** (Mac; also in the menu) and the settings window's **Deleted Files** section (Windows, Linux) list OpenCloud's trash for the personal space and every space a folder is kept from: name, original path, when it was deleted and size, newest first. By default only items that were inside a kept folder; **Show all** lists everything. **Restore** moves an item back to where it was (`MOVE` from `<dav>/spaces/trash-bin/<space id>/<item>`, `Overwrite: F`) and then syncs its kept folder (while paused: when you resume). Nothing is ever overwritten: if something is at the original place again in OpenCloud (checked first, and refused by the server with `Overwrite: F`), the restore is refused; if something is still there on this computer, it says to wait for the folder to sync (while paused: to resume syncing first), then restore. The personal space's trash address uses only the path of its `root.webDavUrl`, on the configured server; if that is missing or not a path, the page shows a problem line instead of an empty trash. Items outside kept folders are restored in the browser. The app never deletes from the trash.

### Security
- The bearer token for these requests only goes to the server: no redirects are followed (Mac: its own session without redirects; Windows, Linux: the existing no-redirect client). Paths are checked to be plain paths inside a kept folder (no `..`, `\`, control characters, the backups folder or conflict copies), and a restore is looked up on the server again rather than taken from the page.

## [2.0.1] - 2026-11-16

### Changed
- **Mac:** the drive shows in Finder as `opencloud.localhost` instead of `localhost`. The app now runs `rclone serve nfs` on a free port on 127.0.0.1 and mounts it with macOS's `mount_nfs` (the options `rclone nfsmount` used: `port`, `mountport`, `tcp`), since `rclone nfsmount` always mounts `localhost:/`. Eject, Quit (waiting for uploads), ejecting in Finder (stays ejected, uploads finish first), crash recovery and restarts for new drive or bandwidth settings work as before. A dead mount left by an older version is cleared at the mount point as before. The README explains why the drive is a network-style (NFS) drive.

## [2.0.0] - 2026-11-16

### What's new in 2.0
Kept folders now sync as things happen, on both sides, and the interval is a safety net. Since 1.4:
- **Live sync, on by default** (1.5, 1.6, 2.0): a change on this computer syncs the folder about 5 seconds after changes stop (FSEvents on the Mac, fsnotify on Windows and Linux); a change in OpenCloud is noticed within about 30 seconds with one small `PROPFIND` per folder (browser sign-in). The app's own writes and uploads don't start syncs; noise (temporary and lock files, `.DS_Store`, excluded folders) doesn't count.
- **Folders sync side by side** (1.7): two at a time by default (1–4), never the same folder twice, folders with a change first. Repair, Stop Keeping and similar actions wait for running syncs instead of refusing.
- **Kinder to small servers** (1.7, 2.0): folders whose syncs take long sync less often on the interval, large folders sync after changes at most every 10 minutes, and a single network blip isn't notified.
- **Recent Changes and Activity** (1.4, 1.7) say what each sync changed and what started it.

### Upgrade note
- **Behaviour change:** live sync is on unless you turned it off. If you never chose the setting, a notification (and an Activity line) says so once: "Changes now sync as they happen. You can turn this off in Settings ▸ Sync." To keep the old behaviour, uncheck **Sync changes right away** in Settings ▸ Sync, or set `"liveSync": false` in `config.json`. If you turned the preview off in 1.5–1.7, 2.0 turns it on again (1.x didn't record that choice: off was the absent key); turn it off in Settings ▸ Sync and it stays off. Turning it on in 1.5–1.7 is kept.
- **Interval floor:** folders live sync covers are synced by the interval every 15 minutes, or at your interval if longer (large folders can be stretched up to 30 minutes or 3 × the interval). Folders it can't cover keep your interval.

### Changed
- Live sync is on when `liveSync` is absent (new installs and existing settings); only `"liveSync": false` turns it off, and turning it on again removes the key. The setting is named **Sync changes right away** (no longer a preview).
- With live sync covering a folder (watched here and checked in OpenCloud), the interval syncs it every 15 minutes (or at the interval if longer; a large folder up to 30 minutes or 3 × the interval): "Checked every 15 min; changes sync right away". Folders it can't cover (watch limit, the rclone sign-in, a missing local copy) keep the interval and say "Synced every N min".
- A covered folder whose last sync took more than 2 minutes syncs after changes at most every max(10 minutes, 2 × that sync); changes wait and sync then. Its status says "Checked every N min; changes sync at most every M min (large folder)".
- The one-time notice waits until a folder is kept, and is marked as given only once it was: as a notification, or, with notifications off, as the Activity line.

## [1.7.1] - 2026-11-16

### Fixed
- macOS: a folder's **Sync Now** (in the menu and in Settings ▸ Kept Folders ▸ Actions) was greyed out whenever any folder was syncing, so a small folder still couldn't sync while a large one ran. It's now only unavailable while that folder itself syncs or waits to. **Repair**, **Allow Large Deletion** and **Stop Keeping** are no longer greyed out during a sync either: as in 1.7.0, they wait for the running syncs and then run.

## [1.7.0] - 2026-11-15

### Added
- **Folders sync side by side.** Up to **Folders synced at once** folders (1–4, default 2; config key `syncParallel`) sync in parallel, never the same folder twice. Folders with a change (live sync), a newly kept folder and a folder's own Sync Now go before the interval's folders, so a small folder with a change doesn't wait behind a long sync of a big one. Repair, allowing a large deletion, Stop Keeping and conflict actions wait for every running sync (nothing new starts meanwhile) instead of refusing. The server memory check gates each batch. The menu says "Syncing 2 folders…".
- **Large folders sync less often on the interval.** A folder whose last sync took more than half the interval is synced by the interval at most every min(2 × that sync, 30 minutes, 3 × the interval); its status says "syncs less often: large folder". Changes and Sync Now still sync it.
- The Activity list shows what started each sync: a change on this computer, a change in OpenCloud, or the interval.
- Windows and Linux: server change checks wait 15 seconds after the computer wakes from sleep (noticed by the clock jumping).

### Fixed
- A change waiting for its sync that an interval sync took was recorded as "timer"; it is now recorded as the change ("local change" or "server change", a local one first).
- Mac: the toggles in Settings (Pause syncing, Sync changes right away, Open at Login, updates, notifications, server memory, SSH tunnel) and the subfolder checkboxes have accessibility labels.
- Windows and Linux: the settings window showed "Sync changes right away" as off even when it was on.

### Changed
- A setting saved while syncs run is applied when the last one ends; until then no new folder starts.

## [1.6.0] - 2026-11-15

### Added
- **Live sync now also notices changes in OpenCloud (preview, same setting).** With **Sync changes right away (preview)** (renamed from "Sync local changes right away"; the config key stays `liveSync`) and the browser sign-in, each kept folder's root (personal and project spaces) is asked for its ETag about every 30 seconds (± 5 s): one WebDAV `PROPFIND`, `Depth: 0`, `getetag` only, at most 4 at once. OpenCloud propagates a change's tree time up to the space root, so the root's ETag changes with anything below it. A changed ETag syncs that folder through the same gap and queue as a local change; Recent Changes says "(after a change in OpenCloud)" (`trigger`: "server change").
- The last ETag per folder is kept in memory and in `<folder>.server-etag` in the sync state folder (removed by Stop Keeping; a check finishing afterwards doesn't write it again). If the ETag differs from the stored one at the first check after launch, the folder syncs (once: a sync that reaches the folder takes the queued one). After each successful sync the ETag is read again as the baseline, so the app's own uploads don't start another sync; no check runs while the folder syncs, and a check that was running when a sync started is ignored.
- Errors (no connection, 401, 5xx, a redirect) back off per folder, doubling up to 5 minutes, without notifications. No checks while paused, signed out, (Mac) asleep, or with the older rclone sign-in. The bearer token goes only to the server: redirects aren't followed.

### Changed
- A single sync that fails because of a network blip (rclone: "can't assign requested address", "connection reset", "i/o timeout", "no such host", "TLS handshake timeout" and similar, with no other errors) shows "network problem; will retry" in the menu but isn't notified; a second failed run of that folder in a row is.

### Not included
- Folders kept with the older rclone sign-in (no bearer token) get no server checks; they sync at the interval.
- A change made in OpenCloud while that folder is syncing goes into the baseline read after the sync: if that sync didn't take it, it arrives with the next interval sync (or the next change).

## [1.5.0] - 2026-11-15

### Added
- **Live sync (preview, off by default).** **Sync local changes right away (preview)** in the Sync settings watches each kept folder's local copy (Mac: FSEvents; Windows and Linux: fsnotify, one watch per folder) and syncs a folder soon after a change: about 5 seconds after changes stop, at most 60 seconds after the first change. Syncs started this way are at least max(30 seconds, the folder's last sync) apart; interval syncs are unchanged and never two syncs run at once (the folder goes through the per-folder queue). While paused, a change only marks the folder; it syncs on resume. The folder shows "Change detected, syncing soon" (after any problem or the paused state).
  - The app's own writes: changes seen during a sync of that folder (Mac: by FSEvents event id) are kept aside, the files the sync downloaded or deleted here are taken out, and anything left (or too many to tell) syncs the folder once more after the gap.
  - Ignored: rclone's temporary files, the backups folder, conflict copies, the default excludes' names (`.DS_Store`, `Thumbs.db`, `desktop.ini`, `._*`, `*~`, `*.tmp`, `.sync_*.db`), Office (`~$*`) and LibreOffice (`.~lock.*#`) lock files, the excludes' simple file names, and folders excluded by name (not watched).
  - Watches follow keeping and unkeeping, the setting, a changed local folder or excludes, a moved or replaced folder (symbolic links resolved) and a missing local folder; the Mac starts them again after waking from sleep. A watch error, lost events, or the folder itself removed syncs the folder and starts watching over.
  - Windows and Linux: events are taken by a goroutine of their own and watches are added without holding the engine's lock (fsnotify on Windows adds watches through the goroutine that delivers events). A watch request that doesn't come back within 5 seconds, or an event loop stuck for a minute, starts the watch over and syncs the folder; no watch request is made after a watch is stopped. Files made in a new folder before its watch was added count as changes. A watch limit (Linux `fs.inotify.max_user_watches`, or more than 2,000 watched folders on Windows, 20,000 on Linux) puts that folder back on the interval with a one-time notice.
- Recent Changes records what started a sync (`trigger`: "timer" or "local change") and shows "(after a local change)".

### Not included
- Changes made in OpenCloud still arrive at the interval.
- Only excludes that leave out whole folders by name (`- **/name/**`, `- name/**`, `- /Path/**`) are applied to watching; other excludes still apply to the sync itself.
- Windows and Linux don't restart watches after sleep (fsnotify watches stay valid). Windows watches each folder separately (fsnotify 1.9's recursive watch is only enabled in its own tests), hence the 2,000-folder cap. The Mac has no watch limit (FSEvents watches a tree with one stream).
- A change you make to a file while the app is downloading that same file is taken for the app's own write; it syncs at the next interval.

## [1.4.0] - 2026-11-15

### Added
- **Recent Changes.** Each sync records what it uploaded, downloaded and deleted on either side, for the last 500 changes (`activity.json` in the sync state folder, written atomically; a damaged file is kept aside as `activity.json.bad`). Mac: a **Recent Changes** submenu with the last 10 (click to show in Finder) and a searchable **Activity** page in Settings. Windows and Linux: a tray submenu with the last 5 and an **Activity** section in the settings window. A merge (first sync, Repair, automatic merge), and a sync that changed more than 200 files, is one summary line; conflict copies and backups aren't listed. Renames aren't guessed: they show as a new file and a deleted one.
- **Public links.** **Copy Public Link…** makes a public, read-only link to a file or folder in a kept folder and copies it (Mac: in the menu and the folder's Actions; Windows and Linux: the folder's detail in the settings window). It stays active until deleted in OpenCloud, unless an expiry (1, 7 or 30 days) is picked; an optional password is never stored. Made with OpenCloud's libre-graph `createLink`; only https links are accepted.

### Changed
- Syncs log at rclone's INFO level, so the sync logs list every file a sync copies or deletes. Each folder's sync log starts a new file at 5 MB, also during a sync, and only the start and end of a sync's output is kept in memory (Recent Changes is read as the output arrives).

### Fixed
- Windows and Linux: a notification about conflicts opened the settings window on its first section instead of Conflicts.

### Not included
- Renamed or moved files show as a new file and a deleted one (bisync doesn't report renames).
- **Open OpenCloud** opens OpenCloud's web interface, not the file itself.
- Public links can't be listed, changed or removed here; use OpenCloud's web interface. The Windows/Linux tray has no Share item (the settings window does).

## [1.3.1] - 2026-11-15

### Fixed
- **Accessibility.** Controls now have names that VoiceOver and other assistive tools can read.
  - Mac: the Kept Folders buttons (Keep a Folder…, Keep from a Space…, Pause/Resume Syncing, the Actions menu) and the sheets' buttons have labels; Choose Subfolders checkboxes are named after the folder, with "synced" or "not synced" as their value; folder rows say the folder's name, space and status, and the status and last-synced cells say what they show; the menu bar icon is "OpenCloud Bar" with the current status as its value; decorative icons are hidden from VoiceOver.
  - Windows and Linux settings page: every field is tied to its label (or named from its placeholder or section); buttons whose text needs context (Show Subfolders, the conflict actions) say what they act on; the sync status line and messages are announced politely; the folder rows can be reached and chosen with the keyboard (Tab, then Enter or Space) and say the folder's status; keyboard focus is always visible and stays put when the page refreshes every few seconds; choosing a section moves focus to its heading.

## [1.3.0] - 2026-11-15

### Added
- **Choose Subfolders…** for a kept folder (Mac: Settings ▸ Kept Folders and the folder's Actions menu; Windows and Linux: the folder's detail on the settings page). A checklist of the folder's subfolders in OpenCloud, two levels deep. An unchecked subfolder becomes the exclude rule `- /<name>/**` (glob characters escaped, names matched exactly). These rules go first in the folder's excludes, so they win; your own rules follow, unchanged and in order.
  - Saving asks first. Nothing is deleted: unchecked subfolders stay in OpenCloud, and their copy on this computer stays there, no longer synced. Stop Keeping says that this copy goes to the trash with the folder.

### Changed
- **No more Repair after changing what syncs.** After Choose Subfolders, a folder's excludes or the common excludes change, the affected folders sync right away with one automatic comparison of both sides (newer copy wins, replaced files kept in the backups, an emptied side never merged into). It doesn't count toward the once-a-day automatic merge.
- **Keep a Folder…** and **Keep from a Space…** no longer wait for a running sync. The folder is saved at once and syncs as soon as the running sync finishes, on its own.
- Mac: a folder's own excludes can be changed while a sync runs, unless that sync still has to do the folder. Windows and Linux: settings saved during a sync are applied before the next run starts, and **Save Excludes** asks first, as on the Mac.

### Not included
- The checklist lists subfolders from OpenCloud only. A subfolder that exists only on this computer isn't listed.
- The copy of an unchecked subfolder on this computer isn't moved to the trash for you.
- Two levels only. Deeper folders need a hand-written exclude rule.

## [1.2.0] - 2026-11-15

### Added
- **Keep folders from project spaces and shares**, not just your personal files. Mac: **Keep from a Space…** in Settings ▸ Kept Folders. Windows and Linux: the settings window's "Keep a folder by its path, or from another space". Choose the space, then type the folder or pick it from a list.
  - The spaces come from the server (`/graph/v1.0/me/drives`: project spaces and accepted shares). A folder that is read-only for you is refused with a clear message (checked with WebDAV `oc:permissions`).
  - A space's folders are stored in `<local folder>/<space name>/…`, and the folder list shows the space. The space's folder name is made safe for Windows on every system (invalid characters become `-`, reserved names like `CON` get `_`). The same path can be kept from spaces with different names; two spaces with the same name share a local folder, so the second is refused.
  - Overlap checks are per space. No two kept folders may share or nest a local folder, whatever their space or `localPath`. Messages name a folder as “path” in <space name>.
  - They use the same sign-in, also when the server lives under a path (`https://host/opencloud`); merge backups go to `.opencloud-bar-backups` at the top of their space, reachable in the web UI.
- **Mac:** while the settings file can't be loaded, every change that would save it (keeping or stopping a folder, settings edits) is refused with the problem shown, and the app never writes over a settings file that doesn't load. Before, saving then could replace your kept folders with the new one. The Windows and Linux app already edited the file in place and refused; its message now says what is wrong.
  - Existing settings and kept folders are unchanged (a folder without `space` is in the personal space).

### Not included
- The drive still shows only the personal space.

## [1.1.0] - 2026-11-15

### Added
- **Conflicts:** when a file changed on both sides between syncs, the newer copy keeps the name and the other is saved next to it as `name.conflict1` (rclone's naming). OpenCloud Bar now finds these copies after every sync and lists them in **Settings ▸ Conflicts** with the folder and the date changed. Each one has:
  - **Reveal in Finder** / **Show in Folder**;
  - **Keep This Version**: the conflict copy replaces the file, and the replaced file goes to the local backups (like a merge's);
  - **Discard**: the conflict copy goes to the Trash (Windows: the "Removed by OpenCloud Bar" folder).
  The next sync applies the change to OpenCloud. An action runs only while no sync does, and syncs wait until it is done. A backup never replaces an earlier one (a second backup of the same file gets "(2)" added). The menu shows "N conflicts", and a notification announces each new batch (if notifications are on).
- **Bandwidth limits:** optional upload and download limits in **Settings ▸ Sync** (for example `512K` or `2M`, either case; empty means no limit). They apply to syncs and to the drive (rclone `--bwlimit UP:DOWN`). Settings keys `bwLimitUp` and `bwLimitDown`. The drive restarts to apply a change; while it is in use it keeps the old limit, says so, and tries again every minute.

### Fixed
- A merge's second pass (when files changed during the first) now keeps its backups in their own folder (`<date and time>-2`), so it can't replace the first pass's backup of the same file.

## [1.0.2] - 2026-11-15

### Fixed
- **A folder could be kept twice under two spellings** (`a//b` next to `a/b`), giving two syncs of the same local folder. Kept-folder paths are now normalised (no doubled, leading or trailing slashes), and new folders with control characters (such as a line break), a name that is only spaces, starts or ends with a space, or ends with a dot are refused. Variants of the backup folder's name (`.opencloud-bar-backups.`, with a trailing space, other case) can't be kept either. A folder's own local path can't contain control characters. Folders kept by an earlier version with such names still load.
- **Windows and Linux settings page:** the local folder must be a full path (or `~/…`), not empty, not the disk root and without `..`. The mount location can't be the disk root, your home folder, a relative path, or the local folder (or inside it, or contain it). The Mac app checks the same.
- Saving settings with nothing to change (`{}` or only empty values) no longer rewrites the settings file. Pause without a `paused` value is refused (400) instead of resuming.
- When two kept folders' local copies clash, the message names the folder being added, not the one already kept.

## [1.0.1] - 2026-11-15

### Fixed
- macOS: after updating from 0.4 or earlier, the one-time Keychain question ("OpenCloud Bar wants to use your confidential information") is asked only once. Before, choosing **Deny** made it come back at every launch. After **Deny**, choose **Sign In Again…**.

## [1.0.0] - 2026-11-15

The first stable release. It gathers the reliability work and features of 0.4 and 0.5, and adds the items below.

### Added
- **Universal macOS app:** one download runs natively on both Apple silicon and Intel Macs. The app and the bundled rclone both contain both architectures.
- Release notes on GitHub come from this changelog, followed by GitHub's generated notes. A tag with a `-` (for example `v1.1.0-beta.1`) is published as a pre-release; a plain `vX.Y.Z` tag becomes the latest release.
- **Merge backups:** a first sync, a Repair and an automatic merge keep the older copy of every file they replace: in OpenCloud under `.opencloud-bar-backups/<folder>/<date and time>/`, and on this computer under `backups/` in the sync state folder (removed after 30 days). The backup folder is never synced and can't be kept.
- A folder that waits for Repair after drifting out of sync tries a plain sync each interval, and needs no repair once that works.
- Windows: a clear message when WinFsp is missing, instead of a drive that silently never appears.
- Linux packages recommend `libnotify-bin` (deb) or `libnotify` (rpm) for notifications.

### Changed
- The README was rewritten for 1.0. New sections cover:
  - first launch on each platform (Gatekeeper's **Open Anyway** and SmartScreen's **Run anyway**);
  - uninstalling, and what stays behind;
  - known limits.
- The broken Code signing policy link was fixed.

### Fixed
- Windows and Linux: **About OpenCloud Bar** in the tray now switches an already open settings window to About. Before, it did nothing.
- **An empty or missing copy on this computer** (an unplugged disk, a moved folder) is no longer offered as a "large deletion" to apply to OpenCloud. The folder says the copy is empty or missing, and to reconnect the disk or choose Stop Keeping. Allow Large Deletion checks both sides again before it runs, and its dialog says on which side the files were deleted.
- The automatic-merge notification said "nothing was deleted", but a merge replaces the older copy of a file. Those copies are now kept (see Merge backups) and the text says where.
- An automatic merge only runs if its once-a-day record could be saved. A merge that was interrupted doesn't count toward the daily limit.
- Windows and Linux: Pause, Keep, Stop Keeping, sign-in and the settings page each change only their own settings, so one no longer undoes another's change, and settings of other versions inside kept folders are kept. A running sync uses its own copy of the settings.
- A failed update check is retried after 15 minutes (doubling, up to 6 hours) instead of counting as the day's check. The update notification isn't used up while notifications are off.
- macOS: the single-instance lock is no longer inherited by rclone and ssh; two notifications at once no longer lose the second while permission is asked; the sign-in is saved under the same lock as token refreshes; a sync that ignores SIGTERM after a timeout or on Quit is killed.
- **Upgrading doesn't send every folder to Repair.** 1.0 adds a line to each folder's sync filters (the backup folder), and rclone then insists on starting the sync history over. Each kept folder now does that as one automatic merge (backed up, newer wins, not counted as the day's automatic merge, never into an empty side) instead of waiting for Repair. A change to your own excludes still asks for Repair, with that reason; before, it failed on every sync.
- **Stop Keeping works when this computer's copy is missing or empty:** it skips the final sync and the trash and just stops keeping the folder (the folder in OpenCloud is untouched). Before, it failed because the sync failed.
- Windows installer and uninstaller: they no longer force the app to quit while files written to the drive may still be uploading. They stop and ask you to try again. `opencloud-bar quit` exits with code 2 in that case, and 0 when the app isn't running.
- Per-folder **Sync Now** (also in the menu's problem submenu) is disabled while syncing is paused.
- Linux without zenity: Quit while files may still be uploading keeps running and says so in a notification, with `opencloud-bar quit --force` to quit anyway.

### Highlights since 0.3
- **Sync recovery:** when the two sides drift out of sync, a folder is merged automatically (the newer copy of each file wins; the older copy is kept in a backup), at most once per folder a day. Interrupted syncs resume without a repair. The menu says why a folder needs attention.
- **Safety:**
  - The large-deletion override is never offered when the folder is missing or empty on either side.
  - Quit and Sign Out ask first while files are still uploading.
  - The drive's control port needs a per-launch password.
  - Only one copy of the macOS app runs at a time.
- **Pause / Resume Syncing**, **Check for Updates**, **notifications**, an **About** window, live sync progress ("Syncing “School” (2 of 3) · 12 files…") and "synced 5 min ago" for every folder.
- **Updates on macOS no longer prompt for Keychain access**, and the app no longer hangs at launch.

## [0.5.0] - 2026-11-15

### Added
- **Notifications:**
  - What they cover: sync failed, needs repair, large deletion blocked, a folder missing or empty in OpenCloud, an automatic merge, signed out, and an available update.
  - One notification per change: never repeated while the state lasts, and several folders share one.
  - Clicking one opens Settings on macOS. Windows uses toast notifications, Linux uses `notify-send`.
  - A setting turns them off.
- **About:** a window on macOS, and a settings page section on Windows and Linux. It shows the version, the rclone version and the project and license links.
- **Live sync activity:** the menu shows "Syncing “X” (k of n) · N files…", and the Kept Folders pages show a spinner on the folder being synced.
- Windows and Linux settings page: sign in by typing the server address. No dialog tool is needed for this.

### Changed
- **macOS:** the sign-in (refresh token) is kept in a private file (0600) instead of the Keychain.
  - Because every build is ad-hoc signed, each update made macOS ask for Keychain access at launch, and the app waited on that dialog.
  - The token is moved out of the Keychain once, in the background. After 60 s without an answer, the app offers **Sign In Again…**.
- **Linux:** the systemd service no longer has `MemoryMax`. The limit killed the service in the middle of syncs and uploads.
- **Windows:** dialogs and notifications get their text through environment variables. Nothing is pasted into PowerShell code.

### Fixed
- **Windows installer:**
  - It stops the running app before an update.
  - It closes apps that still hold the files.
  - Uninstalling stops the app and removes the stored sign-in; settings stay.
  - WinFsp is detected reliably (32-bit registry view, plus a DLL check).
- **Windows:** a drive letter taken by another device now says so, instead of retrying silently.
- **Windows:** starting the app again (or clicking a notification) opens the running copy's settings, instead of showing an error.
- **Linux without zenity:** Set Up and Keep a Folder explain what to install and open the settings page instead of doing nothing.

## [0.4.0] - 2026-11-15

### Added
- **Pause / Resume Syncing:**
  - Available in the menu or tray, in Settings, and with `opencloud-bar pause` / `resume`.
  - The pause survives restarts.
  - The drive keeps working while paused.
- **Check for Updates:**
  - A daily check against GitHub's latest release, which can be turned off.
  - A manual **Check for Updates…**.
  - "Update Available: vX.Y.Z…" opens the release page.
- The menu says why a folder needs attention, and each kept folder shows when it last synced ("synced 5 min ago").
- The version is stamped into every build and shown in Settings, and there is an `opencloud-bar version` command.

### Changed
- When the two sides' listings drift apart ("out of sync"), the folder is merged automatically (newer wins; no file is deleted, but the older copy of a changed file is replaced, which 1.0 keeps in a backup), at most once per folder in 24 hours. After that, it waits for **Repair**.
  - Merges now pass `--resync` explicitly.
  - Merges keep rclone's final check on, so a merge that didn't converge is caught right away.
- Interrupted syncs (quit, sleep, timeout) resume on the next run without a repair. On macOS a timed-out sync is stopped after a further 60 s.
- The drive's rclone remote control requires a random per-launch login.

### Fixed
- Sync Now and Repair read a folder's state from disk, so an outdated "needs repair" no longer blocks them.
- A folder that is missing or empty in OpenCloud shows "Folder is missing or empty in OpenCloud". It is never offered the large-deletion override, which would have emptied the local copy.
- Quit and Sign Out ask first when uploads are pending, or when the upload count can't be read.
- **macOS:** only one copy of the app can run.
- **Windows and Linux:**
  - Keep and Stop Keeping refuse while the settings file has an error.
  - A settings file that can't be read is never overwritten.
  - A folder's own local path can't be the home folder, a drive root, or another kept folder.

## [0.3.5] - 2026-11-15

### Fixed
- macOS DMG: the image is sized explicitly. hdiutil's own estimate sometimes ran out of space on CI.

## [0.3.4] - 2026-11-15

### Added
- macOS settings window:
  - a Kept Folders page;
  - Account, Sync, OpenCloud Drive and General pages;
  - the logs inside the app.

## [0.3.3] - 2026-11-15

### Changed
- App icon: the sync arrows are centred in the cloud.

## [0.3.2] - 2026-11-15

### Added
- App icon: a cloud with sync arrows. It is used for the macOS app, the Windows program and installer, and the Linux desktop entry.

## [0.3.1] - 2026-11-15

### Fixed
- macOS sign-in: the query values of the authorization address are percent-encoded.

## [0.3.0] - 2026-11-15

### Added
- Set-up with browser sign-in (OpenID Connect with PKCE). No SSH key, app token or rclone configuration is needed.

## [0.2.0] - 2026-11-15

### Added
- Installers:
  - Windows setup, with rclone and WinFsp;
  - a macOS DMG, with rclone inside the app;
  - Linux .deb and .rpm packages, with rclone and fuse3.

### Fixed
- Release workflow: the nfpm package variables are filled in properly, and hdiutil is retried on CI.

## [0.1.2] - 2026-11-15

### Added
- Release builds for macOS, Windows and Linux. The README covers all three platforms.

## [0.1.1] - 2026-11-15

### Fixed
- Windows fixes from the first test on a real machine.

## [0.1.0] - 2026-11-15

### Added
- A macOS menu-bar app for OpenCloud:
  - an on-demand drive (`rclone nfsmount`);
  - kept folders, synced both ways with `rclone bisync`.
- A Go version for Windows and Linux, with a tray icon and the same settings file.
- A Windows build workflow with code signing through SignPath, a code signing policy, and a privacy statement.

[1.0.0]: https://github.com/sbstndalton/opencloud-bar/compare/v0.5.0...v1.0.0
[0.5.0]: https://github.com/sbstndalton/opencloud-bar/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/sbstndalton/opencloud-bar/compare/v0.3.5...v0.4.0
[0.3.5]: https://github.com/sbstndalton/opencloud-bar/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/sbstndalton/opencloud-bar/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/sbstndalton/opencloud-bar/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/sbstndalton/opencloud-bar/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/sbstndalton/opencloud-bar/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/sbstndalton/opencloud-bar/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/sbstndalton/opencloud-bar/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/sbstndalton/opencloud-bar/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/sbstndalton/opencloud-bar/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/sbstndalton/opencloud-bar/releases/tag/v0.1.0
