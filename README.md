# OpenCloud Bar

A small macOS menu-bar app for using a self-hosted [OpenCloud](https://opencloud.eu) server the way people used to use Nextcloud's selective sync:

- **Everything on demand.** Your whole OpenCloud space appears as a drive in Finder. Files download only when you open them, are cached (20 GB by default) and are evicted again after a few days unused.
- **Some folders kept on the Mac.** Pick the folders you want as real local copies, and they sync both ways every 5 minutes. Stop keeping one and its local copy goes to the Trash. The folder stays in OpenCloud.

[rclone](https://rclone.org) does all the work:

| Piece | rclone command |
|---|---|
| On-demand drive | `rclone nfsmount` (macOS's built-in NFS client, so no FUSE or kernel extension) |
| Kept folders | `rclone bisync` (two-way sync with a state file per folder) |

Traffic goes through an SSH tunnel to the server's local OpenCloud port. The tunnel runs as a per-user launchd agent that the app creates and removes, so macOS Local Network privacy doesn't block it and it survives an app crash. This skips any reverse proxy or CDN in front of OpenCloud. Cloudflare in particular closes HTTP/2 connections after about 10,000 requests, which breaks the official desktop client on large trees, and it limits uploads to 100 MB.

## Requirements

- macOS 14 or later
- `rclone` (`brew install rclone`)
- SSH key access to the machine running OpenCloud, with a host alias in `~/.ssh/config`
- An rclone WebDAV remote for your OpenCloud account, using an app token. Create the token with `opencloud auth-app create` on the server, or in the web UI, then run:

  ```bash
  rclone config create opencloud webdav vendor=owncloud user=YOURUSER pass=YOURTOKEN url=http://127.0.0.1:19201/remote.php/dav/files/YOURUSER --obscure
  ```

## Build and install

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
- If bisync ever loses track (for example, files changed mid-run), the app schedules another merge-style resync instead of guessing.
- "Stop keeping" runs a final sync first and only moves the local copy to the Trash if that sync succeeds.
- Ejecting the drive in Finder keeps it ejected until you choose **Connect Drive**. If rclone crashes, the app remounts automatically.
- Eject and Quit wait for files written to the drive to finish uploading. The upload cache lives in Application Support, not Caches, so macOS won't purge it.
- If bisync's history goes stale, or the server settings change, the folder is marked **needs repair** rather than resynced automatically. Repair is a merge: the newer copy of each file wins and nothing is deleted.
- If more than half a folder's files would be deleted, bisync refuses. The menu then offers **Allow Large Deletion…**.
- Kept folder paths are limited to about 90 characters. That's a limit of rclone bisync's history file names.

## License

MIT
