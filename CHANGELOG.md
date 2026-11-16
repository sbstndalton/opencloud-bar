# Changelog

All notable changes to OpenCloud Bar. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

The release workflow copies the section for a tag into that tag's GitHub release, so every version heading must stay in the form `## [X.Y.Z] - date`.

## [2.0.0] - 2026-11-16

### What's new in 2.0
Kept folders now sync as things happen, on both sides, and the interval is a safety net. Since 1.4:
- **Live sync, on by default** (1.5, 1.6, 2.0): a change on this computer syncs the folder about 5 seconds after changes stop (FSEvents on the Mac, fsnotify on Windows and Linux); a change in OpenCloud is noticed within about 30 seconds with one small `PROPFIND` per folder (browser sign-in). The app's own writes and uploads don't start syncs; noise (temporary and lock files, `.DS_Store`, excluded folders) doesn't count.
- **Folders sync side by side** (1.7): two at a time by default (1–4), never the same folder twice, folders with a change first. Repair, Stop Keeping and similar actions wait for running syncs instead of refusing.
- **Kinder to small servers** (1.7, 2.0): folders whose syncs take long sync less often on the interval, large folders sync after changes at most every 10 minutes, and a single network blip isn't notified.
- **Recent Changes and Activity** (1.4, 1.7) say what each sync changed and what started it.

### Upgrade note
- **Behaviour change:** live sync is on unless you turned it off. If you never chose the setting, a notification (and an Activity line) says so once: "Changes now sync as they happen. You can turn this off in Settings ▸ Sync." To keep the old behaviour, uncheck **Sync changes right away** in Settings ▸ Sync, or set `"liveSync": false` in `config.json`. An explicit choice made in 1.5–1.7 is kept.

### Changed
- Live sync is on when `liveSync` is absent (new installs and existing settings); only `"liveSync": false` turns it off, and turning it on again removes the key. The setting is named **Sync changes right away** (no longer a preview).
- With live sync covering a folder (watched here and checked in OpenCloud), the interval syncs it at least every 15 minutes (or at the interval if longer): "Checked every 15 min; changes sync right away". Folders it can't cover (watch limit, the rclone sign-in, a missing local copy) keep the interval.
- A folder whose last sync took more than 2 minutes syncs after changes at most every max(10 minutes, 2 × that sync); changes wait and sync then. Its status says "large folder: syncs at most every N min".

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
