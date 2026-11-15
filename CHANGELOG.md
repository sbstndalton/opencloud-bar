# Changelog

All notable changes to OpenCloud Bar. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

The release workflow copies the section for a tag into that tag's GitHub release, so every version heading must stay in the form `## [X.Y.Z] - date`.

## [1.0.0] - 2026-11-15

The first stable release. It gathers the reliability work and features of 0.4 and 0.5, and adds the items below.

### Added
- **Universal macOS app:** one download runs natively on both Apple silicon and Intel Macs. The app and the bundled rclone both contain both architectures.
- Release notes on GitHub come from this changelog, followed by GitHub's generated notes. A tag with a `-` (for example `v1.1.0-beta.1`) is published as a pre-release; a plain `vX.Y.Z` tag becomes the latest release.

### Changed
- The README was rewritten for 1.0. New sections cover:
  - first launch on each platform (Gatekeeper's **Open Anyway** and SmartScreen's **Run anyway**);
  - uninstalling, and what stays behind;
  - known limits.
- The broken Code signing policy link was fixed.

### Fixed
- Windows and Linux: **About OpenCloud Bar** in the tray now switches an already open settings window to About. Before, it did nothing.

### Highlights since 0.3
- **Sync recovery:** when the two sides drift out of sync, a folder is merged automatically (the newer copy of each file wins, nothing is deleted), at most once per folder a day. Interrupted syncs resume without a repair. The menu says why a folder needs attention.
- **Safety:**
  - The large-deletion override is never offered when the folder is missing or empty in OpenCloud.
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
- When the two sides' listings drift apart ("out of sync"), the folder is merged automatically (newer wins, nothing deleted), at most once per folder in 24 hours. After that, it waits for **Repair**.
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
