# Manual test checklist (Windows 11 and Ubuntu)

Run this on a clean Windows 11 machine and a clean Ubuntu (desktop, with a tray such as the AppIndicator extension) before a release. Use a test server and a test account. Tick each line on both systems; note the version and the date.

## Install
- [ ] **Windows:** run `OpenCloud-Bar-Setup-<version>.exe`. SmartScreen's warning is expected (see the README). WinFsp installs, a Start menu entry appears, `opencloud-bar version` works in a new terminal.
- [ ] **Ubuntu:** `sudo apt install ./opencloud-bar_<version>_amd64.deb` pulls in FUSE 3. **OpenCloud Bar** is in the applications menu.
- [ ] Start the app. The tray icon appears and the set-up opens.

## Sign in
- [ ] Enter the server address. The browser opens the server's sign-in page; sign in there. The app says it is signed in, with no password typed into it.
- [ ] Settings ▸ Account shows your name and the storage bar.

## Keep a folder
- [ ] Keep a folder from your personal space. It appears in Settings ▸ Folders and syncs for the first time.
- [ ] Keep a folder from a project space (Spaces). It lands in its own local folder.

## Live sync, both ways
- [ ] Create a file in the local folder. It shows up in OpenCloud (web) within about 10 seconds.
- [ ] Rename and delete a file locally. The server follows.
- [ ] Upload a file in the web UI. It appears locally within about 30 seconds.
- [ ] Settings ▸ Sync ▸ turn off **Sync changes right away**. Changes now wait for the interval or **Sync Now**. Turn it back on.

## Drive (drive letter or mount), eject
- [ ] **Windows:** Connect. The drive letter (default `O:`) opens in Explorer and lists your files; open a file.
- [ ] **Ubuntu:** Connect. The mount folder opens in Files and lists your files.
- [ ] Copy a file to the drive. It reaches the server.
- [ ] Eject. The letter or mount disappears and nothing is left in the tray as connected.

## Conflict
- [ ] Pause syncing. Edit the same file locally and in the web UI. Resume.
- [ ] The newer copy wins, the other becomes `name.conflict1`, and Settings ▸ Conflicts lists it.
- [ ] **Keep This Version** and **Discard** each work (try one of each).

## File history
- [ ] Edit a synced file twice. Folders ▸ the folder ▸ **File History** lists the versions.
- [ ] Restore an old version. The local file has that content after the next sync.

## Deleted files (trash restore)
- [ ] Delete a synced file. Settings ▸ Deleted Files lists it.
- [ ] **Restore** it. It returns to the server and, after a sync, to the local folder.

## Stop Keeping
- [ ] **Stop Keeping** a folder. It stops syncing; the local files stay, and the server's files stay.
- [ ] Keep it again from the same place. It syncs without duplicating files.

## Start at sign-in
- [ ] Settings ▸ General ▸ turn on **Start at sign-in**. **Windows:** `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` has `OpenCloud Bar`. **Ubuntu:** `~/.config/autostart/opencloud-bar.desktop` exists.
- [ ] Sign out of the computer and back in. The app is running once.
- [ ] Turn it off. The registry value or the file is gone; after the next sign-in the app doesn't start.
- [ ] **Windows:** the installer's start-at-sign-in option and this checkbox are the same setting (install with the option on: the checkbox is on).
- [ ] **Ubuntu, with the service:** `systemctl --user enable opencloud-bar`. The page says "Started by the systemd service" and the checkbox is disabled. `systemctl --user disable opencloud-bar` makes it usable again.

## Uninstall
- [ ] Choose **Sign Out**, then uninstall: **Windows:** Settings ▸ Apps ▸ OpenCloud Bar ▸ Uninstall. **Ubuntu:** `sudo apt remove opencloud-bar`.
- [ ] The app is stopped, the Start menu entry or applications menu entry is gone, and **Windows:** the `Run` value is gone.
- [ ] The kept folders and the settings folder are still there (see the README's "What stays").
