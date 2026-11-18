import AppKit
import Foundation
import ServiceManagement

/// Status of one kept folder.
struct FolderStatus {
    enum State: Equatable {
        case idle, syncing, ok
        case skipped(String)
        case failed(String)
        /// bisync can't continue safely; the user must choose Repair (a merge of both sides).
        case needsRepair(String)
        /// bisync refused because more than half the files would be deleted. The text says
        /// where ("on this Mac", "in OpenCloud"), or is empty if rclone didn't say.
        case bigDeletion(String)
    }
    var state: State = .idle
    var lastSuccess: Date?
    /// Live sync: a change was seen here, a sync follows soon.
    var pending = false
    /// A first failure that looks like a network blip: shown, not notified (a second in a row is).
    var quiet = false
}

/// Owns the three moving parts: the SSH tunnel, the on-demand drive (rclone serve nfs + mount_nfs)
/// and the two-way sync of kept folders (rclone bisync). All state lives on the main actor,
/// and only one bisync runs at a time.
@MainActor
final class AppModel: ObservableObject {
    @Published private(set) var config: Config
    @Published private(set) var tunnelUp = false
    @Published private(set) var driveMounted = false
    @Published private(set) var driveWanted = true
    @Published private(set) var syncing = false
    @Published private(set) var status: [String: FolderStatus] = [:]   // keyed by KeptFolder.path
    /// How often each folder entered the large-deletion state. Allow Large Deletion approves one
    /// block; a newer block (even on the same side) is a different deletion set.
    private var blockGen: [String: Int] = [:]
    @Published private(set) var lastMessage: String? {
        didSet { if lastMessage != oldValue { lastMessageAt = Date() } }
    }
    private var lastMessageAt = Date()
    /// A short status line from another file (Shared Links).
    func flash(_ m: String) { lastMessage = m }
    private var resumingPause = false
    /// A timed pause that couldn't be ended (the settings couldn't be saved) is tried again
    /// after this, and the user is told once.
    private var resumeRetryAt = Date.distantPast
    private var resumeAlerted = false
    private static let resumeRetryDelay: TimeInterval = 300
    /// A one-off message ("Link copied") leaves the menu after this long.
    private static let messageLifetime: TimeInterval = 120
    /// Settings ▸ Kept Folders: select this folder (its key) when the page shows; the page clears it.
    @Published var selectFolder: String?
    /// Settings ▸ Kept Folders: open the folder picker (Personal and spaces) when the page shows; the page clears it.
    @Published var showKeepPicker = false
    @Published private(set) var configError: String?
    /// OIDC only: no usable sign-in; nothing runs until the user signs in.
    @Published private(set) var signedOut = false
    /// Moving an older version's sign-in out of the Keychain: macOS may be asking for permission.
    @Published private(set) var waitingForKeychain = false
    @Published private(set) var opensAtLogin = SMAppService.mainApp.status == .enabled
    @Published private(set) var updateState = Updates.load()

    /// The drive's remote-control port only answers with this login (random each launch).
    private let rcUser = "ocb-" + UUID().uuidString.prefix(8).lowercased()
    private let rcPass = (0..<2).map { _ in UUID().uuidString.replacingOccurrences(of: "-", with: "") }.joined()

    private var authBusy = false
    private var authRetryAt = Date.distantPast
    private var setupWindow: SetupWindowController?
    private var aboutWindow: AboutWindowController?
    lazy var notifier = Notifier(model: self)
    /// The personal space's quota, last checked (Quota.swift); nil until then or without a limit given.
    @Published var storageQuota: StorageQuota?
    var quotaCheckedAt = Date.distantPast
    var quotaGeneration = 0          // bumped when the quota state is reset
    var quotaIdentity = ""           // server + account the quota on hand is from
    // OpenCloud notifications (ServerNotes.swift).
    var snAt = Date.distantPast      // no poll before this
    var snBusy = false
    var snFails = 0                  // errors in a row
    var snOff = false                // a 404: no polling this session
    var snGeneration = 0             // bumped when the seen ids are reset
    var snIdentity = ""              // server + account the seen ids are from
    var snSeen: [String] = []
    var snSeeded = false
    var snLoaded = false             // snSeen was read from the file
    var quotaBusy = false
    var searchWindow: SearchWindowController?
    private var notePlanner = NotePlanner()
    private var autoMerged: [String] = []
    /// Conflict copies per kept folder (keyed by KeptFolder.path); see Conflicts.swift.
    @Published private(set) var conflicts: [String: [ConflictItem]] = [:]
    /// Conflict copies found since the last notification check: (folder name, count).
    private var newConflicts: [(String, Int, String)] = []
    /// Per folder: the newest scan started. An older scan finishing later doesn't overwrite it.
    private var conflictScanGen: [String: Int] = [:]
    /// A failed update check is retried after a growing wait (it isn't the daily check).
    private var updateRetryAt = Date.distantPast
    private var updateFailures = 0
    /// The folders syncing right now (key → display name), for the menu's first line.
    @Published private(set) var activeNames: [String: String] = [:]
    /// Files the running sync of each folder has transferred so far (key → count).
    @Published private(set) var activeFiles: [String: Int] = [:]
    /// The files each running sync is moving (key → files), read from rclone's progress blocks.
    @Published private(set) var transfers: [String: [Transfer]] = [:]
    /// The drive's upload queue: asked every 5 s while a sync runs, the queue isn't empty or the
    /// Transfers page is open, otherwise every 30 s (see refreshDriveQueue).
    @Published private(set) var driveQueue = DriveQueue()
    private var driveQueueAt = Date.distantPast
    /// The Transfers page is on screen (set by the page): keeps the faster refresh.
    var transfersPageOpen = false
    /// This session's rclone answered vfs/stats but not vfs/queue: don't try vfs/queue again.
    private var queueCallUnsupported = false
    /// The run whose progress blocks may still set `transfers` (per folder). A late block of an
    /// earlier run, still queued on the main actor, finds another id (or none) and is dropped.
    private var transferRun: [String: UUID] = [:]
    /// Live sync (on by default): watches the kept folders (LiveSync.swift).
    private(set) lazy var live = LiveSync(model: self)
    /// What started each folder's running sync ("timer", "local change", "server change"), for Recent Changes.
    private var runTrigger: [String: String] = [:]
    /// Failed runs in a row per folder (a single network blip isn't notified).
    private var failStreak: [String: Int] = [:]
    /// Recent Changes (oldest first), from activity.json in the sync state folder.
    @Published private(set) var recentChanges: [Change] = []
    private var changesURL: URL { config.stateURL.appendingPathComponent("activity.json") }
    /// Folders kept while a sync ran, and live sync's: they start as soon as a lane is free (keys).
    private var queuedKeys = Set<String>()

    // Parallel sync (v1.7; same as cross/sched.go, which has the tests): up to
    // config.parallel folders at once, never the same folder twice; live/new folders first;
    // exclusive actions wait for every run and hold new starts meanwhile.
    private struct WorkItem { let key: String; var manual: Bool; var prio: Bool; var timer: Bool }
    private var work: [WorkItem] = []
    private var running: [String: WorkItem] = [:]
    private var lanes = 0
    private var starting = 0
    private var startingKeys: [String: Int] = [:]
    private var timerOn = false, timerStarting = false
    private var exclQ: [@MainActor () async -> Void] = []
    private var exclOn = false
    /// An exclusive action (Repair, Stop Keeping, conflict actions…) runs.
    var exclusiveRunning: Bool { exclOn }
    private var batchN = 0, batchDone = 0
    /// Each folder's last run (pacing: a large folder syncs less often on the timer).
    private var runTimes: [String: (start: Date, dur: TimeInterval)] = [:]
    /// Running bisyncs (Quit interrupts them).
    private var procs: [ObjectIdentifier: Process] = [:]

    /// Whether a folder's excludes must wait: it syncs now, or waits to.
    func folderBusy(_ f: KeptFolder) -> Bool {
        running[f.key] != nil || work.contains { $0.key == f.key } || startingKeys[f.key] != nil
    }
    private var settingsWindow: SettingsWindowController?
    private var tunnelRestartAt = Date.distantPast
    private var mount: Process?
    private var mountStartedAt = Date.distantPast
    private var mountRestartAt = Date.distantPast
    private var mountBackoff: TimeInterval = 5
    private var unmountRequested = false
    /// The drive's NFS port (rclone serve nfs on 127.0.0.1) and whether we mounted it.
    private var drivePort = 0
    private var driveMountedByUs = false
    private var nextSyncAt = Date()
    private var ticker: Timer?
    private(set) var quitting = false

    /// A single bisync can run this long before it's stopped (seconds).
    private let syncTimeout: TimeInterval = 3 * 3600

    init() {
        let (c, err) = Config.load()
        config = c
        configError = err
        signedOut = c.isOIDC && c.isConfigured && !Auth.haveCredentials(c)
        quotaIdentity = Self.quotaIdentity(c)
        snIdentity = quotaIdentity
        applyEnvironment()
        refreshStatusEntries()
        recentChanges = Changes.load(changesURL)
        startTicker()
        if signedOut && Auth.keychainMigrationPending { migrateKeychain() }
        // First launch (nothing set up yet): open the set-up window right away.
        if configError == nil && !c.isConfigured {
            DispatchQueue.main.async { [weak self] in self?.showSetup() }
        }
    }

    /// In OIDC mode rclone gets its bearer token from this app's own `--token` mode.
    private func applyEnvironment() {
        Shell.setExtraEnvironment(config.isOIDC ? ["RCLONE_WEBDAV_BEARER_TOKEN_COMMAND": Auth.bearerTokenCommand()] : [:])
    }

    /// Keeps the cached access token fresh (rclone only asks for a new one after a 401) and
    /// notices a revoked sign-in.
    private func checkAuth() {
        guard !authBusy, Date() >= authRetryAt else { return }
        authBusy = true
        let url = config.serverURL ?? ""
        Task { [weak self] in
            do { _ = try await Auth.accessToken(serverURL: url) }
            catch AuthError.signedOut { self?.markSignedOut() }
            catch {
                Log.line("token refresh failed: \(error.localizedDescription)")
                self?.authRetryAt = Date().addingTimeInterval(30)
            }
            self?.authBusy = false
        }
    }

    /// One-time move of the refresh token from the Keychain (older versions) to its file.
    /// The read can wait on a macOS permission prompt, so it runs off the main thread; after
    /// 60 s the app stops waiting and offers sign-in instead (a late answer is still used).
    private func migrateKeychain() {
        guard !waitingForKeychain else { return }
        waitingForKeychain = true
        Log.line("moving the sign-in out of the Keychain (macOS may ask for permission)")
        Task.detached(priority: .utility) {
            let moved = Auth.migrateFromKeychain()
            await MainActor.run { [weak self] in self?.keychainMigrationFinished(moved) }
        }
        Task { [weak self] in
            try? await Task.sleep(nanoseconds: 60_000_000_000)
            guard let self, self.waitingForKeychain else { return }
            self.waitingForKeychain = false
            self.lastMessage = Self.signedOutText
            Log.line("no answer from the Keychain after 60 s; showing sign-in")
        }
    }

    private func keychainMigrationFinished(_ moved: Bool) {
        waitingForKeychain = false
        guard moved else {
            if signedOut { lastMessage = Self.signedOutText }
            return
        }
        if signedOut, config.isOIDC, Auth.haveCredentials(config) {
            applyLoadedConfig(message: "Signed in")
        }
    }

    private func markSignedOut() {
        guard !signedOut else { return }
        signedOut = true
        lastMessage = Self.signedOutText
        resetQuota(stateFolders: [config.stateURL])
        Log.line("signed out (sign-in missing or revoked)")
    }

    static let signedOutText = "Signed out: choose Set Up / Sign In…"
    var needsSignIn: Bool { !config.isConfigured || signedOut }

    private func startTicker() {
        guard ticker == nil else { return }
        ticker = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.tick() }
        }
        tick()
    }

    private var lastSuccessURL: URL { config.stateURL.appendingPathComponent("last-success.json") }

    /// "Last synced" times survive restarts: stateDir/last-success.json maps stateName to a date.
    private func loadLastSuccess() -> [String: Date] {
        guard let data = try? Data(contentsOf: lastSuccessURL),
              let raw = try? JSONDecoder().decode([String: String].self, from: data) else { return [:] }
        let f = ISO8601DateFormatter()
        return raw.compactMapValues { f.date(from: $0) }
    }

    private func saveLastSuccess() {
        let f = ISO8601DateFormatter()
        var out: [String: String] = [:]
        for k in config.kept {
            if let d = status[k.key]?.lastSuccess { out[k.stateName] = f.string(from: d) }
        }
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys]
        guard let data = try? enc.encode(out) else { return }
        try? FileManager.default.createDirectory(at: config.stateURL, withIntermediateDirectories: true)
        try? data.write(to: lastSuccessURL, options: .atomic)
    }

    private func refreshStatusEntries() {
        var saved: [String: Date]?
        for f in config.kept where status[f.key] == nil {
            var s = FolderStatus()
            if saved == nil { saved = loadLastSuccess() }
            s.lastSuccess = saved?[f.stateName]
            if let reason = needsRepairReason(f) { s.state = .needsRepair(reason) }
            status[f.key] = s
            conflicts[f.key] = Conflicts.load(stateFile(f, ".conflicts.json"))
        }
        let paths = Set(config.kept.map(\.key))
        for k in status.keys where !paths.contains(k) { status[k] = nil }
        for k in conflicts.keys where !paths.contains(k) { conflicts[k] = nil }
        live.update()
    }

    /// Live sync: show or clear "Change detected, syncing soon".
    func setPending(_ key: String, _ on: Bool) {
        if status[key]?.pending != on { status[key]?.pending = on }
    }

    /// Live sync: a sync of this folder starts, so a queued one for it is done by it.
    func dequeue(_ key: String) {
        queuedKeys.remove(key)
        let before = work.count
        work.removeAll { $0.key == key }
        batchDone += before - work.count
    }

    /// Live sync: these folders changed and settled; sync them, ahead of the timer's folders.
    func queueLiveSync(_ keys: [String]) {
        queuedKeys.formUnion(keys)
        startQueued()
    }

    // MARK: - Supervisor

    private func tick() {
        // A message about one event doesn't stay for hours. The signed-out text is a state, not an event.
        if lastMessage != nil, lastMessage != Self.signedOutText, Date().timeIntervalSince(lastMessageAt) > Self.messageLifetime {
            lastMessage = nil
        }
        // A timed pause ends by itself. A clock set back only keeps it longer; forward, it ends early.
        if config.paused, let end = config.pauseEnd, Date() >= end, configError == nil, !resumingPause, Date() >= resumeRetryAt {
            resumingPause = true
            setPaused(false, automatic: true)
        }
        checkNotifications()
        if !quitting, configError == nil, config.checkForUpdates, Updates.due(updateState), Date() >= updateRetryAt {
            Task { await self.checkForUpdates(manual: false) }
        }
        guard !quitting, config.isConfigured, configError == nil else { return }
        if config.isOIDC {
            if signedOut { return }
            checkAuth()
            checkQuotaIfDue()
            checkServerNotesIfDue()
        }
        driveMounted = Shell.isDriveMount(config.mountURL)
        Task { await refreshDriveQueue() }
        checkFinderEject()
        if config.tunnel {
            let portOpen = Shell.portOpen(config.localPort)
            let ours = FileManager.default.fileExists(atPath: controlPath)
            tunnelUp = portOpen && ours
            if !tunnelUp, Date() >= tunnelRestartAt {
                if portOpen && !ours {
                    // Something else holds the port. Don't send credentials to it.
                    lastMessage = "Port \(config.localPort) is in use by another program; waiting for it to close."
                    tunnelRestartAt = Date().addingTimeInterval(30)
                } else {
                    startTunnel()
                }
            }
        } else {
            tunnelUp = true   // no tunnel: the server is reached directly
        }
        if driveWanted, tunnelUp, mount == nil, Date() >= mountRestartAt { startMount() }
        if !timerOn, !exclOn, exclQ.isEmpty, !config.paused, tunnelUp, Date() >= nextSyncAt, !config.kept.isEmpty { startSyncAll(manual: false) }
    }

    // The tunnel runs as a launchd agent rather than a child of this app: macOS Local Network
    // privacy blocks LAN connections made by an app's children until the user allows it,
    // but not those made by launchd agents. The agent also keeps the tunnel up if the app crashes.
    private var tunnelLabel: String { (Bundle.main.bundleIdentifier ?? "opencloudbar") + ".tunnel" }
    private var tunnelPlist: URL {
        FileManager.default.urls(for: .libraryDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("LaunchAgents/\(tunnelLabel).plist")
    }
    /// SSH control socket: lets the app run server commands over the tunnel's connection.
    /// Kept in Caches because ssh's -o ControlPath can't take a path with spaces.
    var controlPath: String {
        let dir = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("OpenCloudBar")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir.appendingPathComponent("ssh.ctl").path
    }
    private var domain: String { "gui/\(getuid())" }

    private func tunnelArgs() -> [String] {
        ["/usr/bin/ssh", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
         "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
         "-o", "ControlMaster=yes", "-o", "ControlPath=\(controlPath)",
         "-N", "-L", "127.0.0.1:\(config.localPort):127.0.0.1:\(config.serverPort)", config.sshHost]
    }

    private func startTunnel() {
        let plist: [String: Any] = [
            "Label": tunnelLabel,
            "ProgramArguments": tunnelArgs(),
            "KeepAlive": true,
            "ThrottleInterval": 15,
            "StandardOutPath": Log.url("tunnel.log").path,
            "StandardErrorPath": Log.url("tunnel.log").path,
        ]
        do {
            try FileManager.default.createDirectory(at: tunnelPlist.deletingLastPathComponent(), withIntermediateDirectories: true)
            let data = try PropertyListSerialization.data(fromPropertyList: plist, format: .xml, options: 0)
            if (try? Data(contentsOf: tunnelPlist)) != data {
                try data.write(to: tunnelPlist, options: .atomic)
                tunnelLoaded = false
            }
        } catch {
            lastMessage = "Could not write the tunnel agent: \(error.localizedDescription)"
            tunnelRestartAt = Date().addingTimeInterval(60)
            return
        }
        tunnelRestartAt = Date().addingTimeInterval(20)
        let label = tunnelLabel, d = domain, path = tunnelPlist.path, reload = !tunnelLoaded
        tunnelLoaded = true
        Task.detached {
            if reload { _ = await Shell.run("/bin/launchctl", ["bootout", "\(d)/\(label)"], timeout: 15) }
            _ = await Shell.run("/bin/launchctl", ["bootstrap", d, path], timeout: 15)
            _ = await Shell.run("/bin/launchctl", ["kickstart", "\(d)/\(label)"], timeout: 15)
        }
        Log.line("tunnel agent started")
    }

    private func stopTunnel() async {
        _ = await Shell.run("/bin/launchctl", ["bootout", "\(domain)/\(tunnelLabel)"], timeout: 15)
        tunnelLoaded = false
    }

    private var tunnelLoaded = false

    // MARK: - On-demand drive

    // The drive is `rclone serve nfs` on a free 127.0.0.1 port, mounted with mount_nfs under the
    // host name opencloud.localhost (it resolves to 127.0.0.1 by itself): Finder shows that as
    // the server, and `rclone nfsmount` always says "localhost". The volume itself is named
    // after the mount point folder (an NFS mount has no volume label to set; rclone serve nfs
    // only exports "/", so no other export path can carry a name), which is why there is no
    // separate volume name setting on the Mac (2.2). The mount
    // options are the ones rclone nfsmount uses (port, mountport, tcp). Unlike nfsmount, the
    // server doesn't stop when the volume is ejected in Finder: tick() notices and stops it
    // (after any uploads).
    static let driveHost = "opencloud.localhost"

    private func startMount() {
        let fm = FileManager.default
        let mnt = config.mountURL
        // A previous rclone crashed and left a dead mount behind (opencloud.localhost:/, or
        // localhost:/ from rclone nfsmount before 2.0.1): clear it first.
        if Shell.isDriveMount(mnt) {
            forceUnmount()
            mountRestartAt = Date().addingTimeInterval(10)
            return
        }
        if Shell.isMounted(mnt) {
            // Another disk is mounted there (the location may be any folder): never unmount it.
            lastMessage = "Something else is mounted at \(mnt.path); choose another drive location in Settings"
            mountRestartAt = Date().addingTimeInterval(60)
            return
        }
        try? fm.createDirectory(at: mnt, withIntermediateDirectories: true)
        try? fm.createDirectory(at: config.cacheURL, withIntermediateDirectories: true)
        guard let port = Shell.freePort() else {
            lastMessage = "Could not find a free local port for the drive"
            mountRestartAt = Date().addingTimeInterval(mountBackoff)
            return
        }
        drivePort = port
        let args = ["serve", "nfs", config.remoteSpec, "--addr", "127.0.0.1:\(port)",
                    "--vfs-cache-mode", "full", "--cache-dir", config.cacheURL.path,
                    "--vfs-cache-max-size", config.cacheMaxSize, "--vfs-cache-max-age", config.cacheMaxAge,
                    "--dir-cache-time", "30s", "--vfs-read-chunk-size", "16M", "--buffer-size", "16M",
                    "--transfers", "4", "--checkers", "8", "--low-level-retries", "10",
                    "--exclude", ".DS_Store", "--exclude", "._*",
                    "--rc", "--rc-addr", "127.0.0.1:\(config.rcPort)",
                    "--log-level", "NOTICE"] + config.bwLimitArgs
        unmountRequested = false
        driveMountedByUs = false
        mountStartedAt = Date()
        do {
            // The rc login goes in the environment, not the command line.
            let p = try Shell.launch(config.rclone, args, log: Log.url("drive.log"),
                                     extraEnv: ["RCLONE_RC_USER": rcUser, "RCLONE_RC_PASS": rcPass]) { [weak self] code in
                self?.mountExited(code)
            }
            mount = p
            Log.line("drive server started on 127.0.0.1:\(port)")
            Task { await self.mountServed(p, port: port, at: mnt) }
        } catch {
            lastMessage = "Could not start rclone: \(error.localizedDescription)"
            driveWanted = false
        }
    }

    /// Mounts the served drive once its port answers (up to 20 s); else stops the server, and
    /// tick() tries again after the backoff.
    private func mountServed(_ p: Process, port: Int, at mnt: URL) async {
        for _ in 0..<80 where p.isRunning && !Shell.portOpen(port) {
            try? await Task.sleep(nanoseconds: 250_000_000)
        }
        guard p.isRunning, mount === p, !quitting else { return }
        let r = await Shell.run("/sbin/mount_nfs", ["-o", "port=\(port),mountport=\(port),tcp",
                                                    "\(Self.driveHost):/", mnt.path], timeout: 30)
        guard mount === p else { return }
        if r.status == 0, Shell.isMounted(mnt) {
            driveMountedByUs = true
            driveMounted = true
            Log.line("drive mounted at \(mnt.path)")
        } else {
            Log.line("drive mount failed (\(r.status)): \(r.output.trimmingCharacters(in: .whitespacesAndNewlines))")
            p.terminate()   // mountExited retries with the backoff
        }
    }

    /// The volume was ejected in Finder (the server keeps running then): stop the server once
    /// nothing is left to upload, and keep the drive off until Connect.
    private func checkFinderEject() {
        guard let p = mount, p.isRunning, driveMountedByUs, !unmountRequested, !quitting,
              !Shell.isMounted(config.mountURL) else { return }
        driveMountedByUs = false
        unmountRequested = true
        driveWanted = false
        mountBackoff = 5
        Log.line("drive ejected in Finder; stays off until Connect")
        Task {
            for _ in 0..<300 {   // up to 10 minutes for uploads started before the eject
                guard let n = await pendingUploads(), n > 0 else { break }
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
            if p.isRunning { p.terminate() }
        }
    }

    /// Unmounts the drive and stops its server. False if the volume is in use.
    private func unmountDrive() async -> Bool {
        unmountRequested = true
        let r = await Shell.run("/usr/sbin/diskutil", ["unmount", config.mountURL.path], timeout: 30)
        guard r.status == 0 else { unmountRequested = false; return false }
        driveMountedByUs = false
        driveMounted = false
        if let m = mount, m.isRunning { m.terminate() }
        return true
    }

    private func mountExited(_ code: Int32) {
        mount = nil
        driveMountedByUs = false
        let stillMounted = Shell.isDriveMount(config.mountURL)
        driveMounted = stillMounted
        if quitting { return }
        if unmountRequested {
            if stillMounted { forceUnmount() }
            Log.line("drive stopped")
            return
        }
        // crashed or failed to start: clear any dead mount and retry with backoff
        if Date().timeIntervalSince(mountStartedAt) > 300 { mountBackoff = 5 }
        Log.line("drive server exited (\(code)); retrying in \(Int(mountBackoff)) s")
        mountRestartAt = Date().addingTimeInterval(mountBackoff)
        mountBackoff = min(mountBackoff * 2, 300)
        if stillMounted { forceUnmount() }
    }

    private func forceUnmount() {
        let path = config.mountURL.path
        Task.detached {
            let r = await Shell.run("/sbin/umount", [path], timeout: 20)
            if r.status != 0 { _ = await Shell.run("/usr/sbin/diskutil", ["unmount", "force", path], timeout: 30) }
        }
    }

    /// Files written to the drive that rclone hasn't uploaded yet (nil if unknown).
    private func pendingUploads() async -> Int? {
        guard mount?.isRunning == true else { return 0 }
        guard let s = await driveStats() else { return nil }
        return s.uploading + s.queued
    }

    /// Calls the drive's rclone remote control (127.0.0.1 only, with this launch's login).
    private func rc(_ method: String, timeout: TimeInterval) async -> [String: Any]? {
        guard let url = URL(string: "http://127.0.0.1:\(config.rcPort)/\(method)") else { return nil }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.timeoutInterval = timeout
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("Basic " + Data("\(rcUser):\(rcPass)".utf8).base64EncodedString(), forHTTPHeaderField: "Authorization")
        req.httpBody = Data("{}".utf8)
        guard let (data, resp) = try? await URLSession.shared.data(for: req),
              (resp as? HTTPURLResponse)?.statusCode == 200 else { return nil }
        return try? JSONSerialization.jsonObject(with: data) as? [String: Any]
    }

    func connectDrive() {
        driveWanted = true
        mountRestartAt = .distantPast
        tick()
    }

    func ejectDrive() {
        Task {
            if let n = await pendingUploads(), n > 0 {
                alert("Still uploading", "\(n) file(s) written to the drive haven't finished uploading. Try again in a moment.")
                return
            }
            if await unmountDrive() {
                driveWanted = false
                Log.line("drive ejected from the menu")
            } else {
                alert("The drive is in use", "Close any files or Finder windows using the OpenCloud drive, then try again.")
            }
        }
    }

    func openDrive() {
        if driveMounted { NSWorkspace.shared.open(config.mountURL); return }
        connectDrive()
        Task {
            for _ in 0..<40 {
                try? await Task.sleep(nanoseconds: 500_000_000)
                if Shell.isMounted(config.mountURL) { NSWorkspace.shared.open(config.mountURL); return }
            }
            alert("The drive isn't connected yet", "It will appear in Finder as soon as the connection to the server is up.")
        }
    }

    // MARK: - Kept folders (two-way sync)

    func stateFile(_ f: KeptFolder, _ suffix: String) -> URL {
        config.stateURL.appendingPathComponent("\(f.stateName)\(suffix)")
    }
    private func workDir(_ f: KeptFolder) -> URL { config.stateURL.appendingPathComponent("work/\(f.stateName)") }
    private func needsRepairReason(_ f: KeptFolder) -> String? {
        (try? String(contentsOf: stateFile(f, ".needs-repair"), encoding: .utf8))?
            .trimmingCharacters(in: .whitespacesAndNewlines)
    }
    private func markNeedsRepair(_ f: KeptFolder, _ reason: String) {
        try? reason.write(to: stateFile(f, ".needs-repair"), atomically: true, encoding: .utf8)
        status[f.key]?.state = .needsRepair(reason)
        Log.line("\(f.path): needs repair: \(reason)")
    }
    /// A folder whose listings keep drifting. It still waits for Repair, but each sync interval
    /// tries a plain sync (no merge); once that succeeds the folder is fine again.
    private func parkDrift(_ f: KeptFolder, _ reason: String) {
        FileManager.default.createFile(atPath: stateFile(f, ".drift-parked").path, contents: nil)
        markNeedsRepair(f, reason)
    }

    /// The menu's Sync Now: every kept folder, now (not while paused).
    func syncNow() {
        guard !config.paused else { return }
        startSyncAll(manual: true)
    }

    /// Syncs all kept folders (only nil: the timer, Sync Now), or the folders in `only` first
    /// (kept meanwhile, live sync). Each folder's repair state is read from disk, so a folder
    /// that was repaired isn't skipped because of what the app remembered. A manual sync also
    /// tries a blocked large deletion again (it is blocked again if nothing changed). The
    /// server memory check gates each batch.
    private func startSyncAll(manual: Bool, only: Set<String>? = nil) {
        guard config.isConfigured, configError == nil, !signedOut else { return }
        let now = Date()
        let timer = only == nil
        let iv = TimeInterval(max(60, config.syncIntervalSeconds))
        let folders = config.kept.filter { f in
            if let only { return only.contains(f.key) }
            return manual || !pacedOut(f, now: now, interval: iv)
        }
        if timer { nextSyncAt = now.addingTimeInterval(iv) }
        guard !folders.isEmpty else { return }
        if timer { timerOn = true; timerStarting = true }
        starting += 1
        for f in folders { startingKeys[f.key, default: 0] += 1 }
        updateBusy()
        Task { [weak self] in
            guard let self else { return }
            let busy = await self.serverTooBusy()
            self.starting -= 1
            for f in folders {
                self.startingKeys[f.key, default: 1] -= 1
                if self.startingKeys[f.key] ?? 0 <= 0 { self.startingKeys[f.key] = nil }
            }
            if timer { self.timerStarting = false }
            if busy {
                for f in folders {
                    // Don't hide a warning or a running sync behind "busy".
                    switch self.status[f.key]?.state {
                    case .idle?, .ok?, .skipped?, .failed?: self.status[f.key]?.state = .skipped("server busy")
                    default: break
                    }
                }
            } else if !self.quitting {
                for f in folders { self.enqueue(WorkItem(key: f.key, manual: manual, prio: !timer, timer: timer)) }
            }
            self.timerCheck()
            self.dispatch()
            self.updateBusy()
            if self.isIdle { self.runsIdle() }
        }
    }

    /// A folder whose last run took more than half the interval syncs on the timer at most
    /// every min(2 × that run, 30 minutes, 3 × the interval).
    nonisolated static func pacedInterval(_ dur: TimeInterval, _ interval: TimeInterval) -> TimeInterval {
        dur <= interval / 2 ? interval : max(interval, min(2 * dur, 30 * 60, 3 * interval))
    }

    /// How often the timer syncs a folder: with live sync covering it, at least every 15
    /// minutes (the safety net); a large folder less often (pacing).
    nonisolated static func timerInterval(_ interval: TimeInterval, _ dur: TimeInterval, live: Bool) -> TimeInterval {
        max(live ? max(interval, 15 * 60) : interval, pacedInterval(dur, interval))
    }

    private func pacedOut(_ f: KeptFolder, now: Date, interval: TimeInterval) -> Bool {
        guard let rt = runTimes[f.key] else { return false }
        let p = Self.timerInterval(interval, rt.dur, live: live.covers(f.key))
        let slack = min(60, interval / 10)   // the timer ticks every interval: a run started just after a tick
        return p > interval && now.addingTimeInterval(slack) < rt.start.addingTimeInterval(p)
    }

    /// A synced folder's note about how often it syncs (nil for none).
    func scheduleNote(_ f: KeptFolder) -> String? {
        guard let rt = runTimes[f.key] else { return nil }
        let iv = TimeInterval(max(60, config.syncIntervalSeconds))
        func mins(_ t: TimeInterval) -> Int { max(1, Int((t / 60).rounded())) }
        if live.covers(f.key) {   // live sync covers it: the timer is a safety net
            let checked = "Checked every \(mins(Self.timerInterval(iv, rt.dur, live: true))) min; "
            return checked + (rt.dur > 120 ? "changes sync at most every \(mins(LiveSync.liveGap(rt.dur))) min (large folder)"
                                           : "changes sync right away")
        }
        if config.liveOn { return "Synced every \(mins(Self.timerInterval(iv, rt.dur, live: false))) min" }   // fell back
        if Self.pacedInterval(rt.dur, iv) > iv { return "syncs less often: large folder" }
        return nil
    }


    private var isIdle: Bool { lanes == 0 && starting == 0 && !exclOn }
    private func updateBusy() {
        let b = lanes > 0 || exclOn || starting > 0
        if syncing != b { syncing = b }
    }
    private var canStart: Bool {
        !quitting && !exclOn && exclQ.isEmpty && !config.paused && configError == nil && !signedOut
    }

    private func timerCheck() {
        if timerStarting { return }
        if work.contains(where: \.timer) || running.values.contains(where: \.timer) { return }
        timerOn = false
    }

    private func enqueue(_ it: WorkItem) {
        if running[it.key] != nil, !it.prio { return }   // the timer: the run going on now covers it
        if let i = work.firstIndex(where: { $0.key == it.key }) {
            work[i].prio = work[i].prio || it.prio
            work[i].manual = work[i].manual || it.manual
            work[i].timer = work[i].timer || it.timer
            return
        }
        work.append(it)
        batchN += 1
    }

    private func pick() -> WorkItem? {
        guard canStart else {
            if config.paused || quitting || signedOut { work = [] }
            return nil
        }
        for wantPrio in [true, false] {
            if let i = work.firstIndex(where: { $0.prio == wantPrio && running[$0.key] == nil }) {
                return work.remove(at: i)
            }
        }
        return nil
    }

    private func dispatch() {
        guard canStart else { return }
        var avail = Set(work.filter { running[$0.key] == nil }.map(\.key)).count
        while lanes < config.parallel, avail > 0 {
            lanes += 1
            avail -= 1
            Task { [weak self] in await self?.lane() }
        }
        updateBusy()
    }

    private func lane() async {
        while let it = pick() {
            running[it.key] = it
            if let f = config.kept.first(where: { $0.key == it.key }) {
                if let reason = needsRepairReason(f) {
                    if FileManager.default.fileExists(atPath: stateFile(f, ".drift-parked").path) {
                        _ = await syncFolder(f, mode: .driftRetry)
                    } else {
                        status[f.key]?.state = .needsRepair(reason)
                    }
                } else if case .bigDeletion = status[f.key]?.state, !it.manual {
                } else {
                    _ = await syncFolder(f, mode: .normal)
                }
            }
            running[it.key] = nil
            batchDone += 1
            timerCheck()
        }
        lanes -= 1
        timerCheck()
        updateBusy()
        if isIdle { runsIdle() }
    }

    /// The last run ended: a waiting exclusive action runs, else the queued folders start.
    private func runsIdle() {
        guard isIdle else { return }
        batchN = 0; batchDone = 0
        if !exclQ.isEmpty, !quitting {
            let body = exclQ.removeFirst()
            exclOn = true
            updateBusy()
            Task { [weak self] in
                await body()
                guard let self else { return }
                self.exclOn = false
                self.updateBusy()
                self.runsIdle()
            }
            return
        }
        startQueued()
        dispatch()
    }

    /// Starts the queued folders (kept meanwhile, live sync, changed excludes), ahead of the timer's.
    private func startQueued() {
        guard !queuedKeys.isEmpty, canStart else { return }
        let q = queuedKeys
        queuedKeys = []
        startSyncAll(manual: false, only: q)
    }

    /// Returns true if the server is short on memory (or unreachable) and we should not add load.
    private func serverTooBusy() async -> Bool {
        // The memory check runs over ssh, so it only applies with the tunnel.
        guard config.tunnel, config.minServerAvailableMiB > 0 else { return false }
        let r = await Shell.run("/usr/bin/ssh", ["-o", "BatchMode=yes", "-o", "ControlMaster=no",
                                                 "-o", "ControlPath=\(controlPath)", config.sshHost,
                                                 "awk '/MemAvailable/{print int($2/1024)}' /proc/meminfo"], timeout: 20)
        guard r.status == 0, let mib = Int(r.output.trimmingCharacters(in: .whitespacesAndNewlines)) else {
            lastMessage = "Server unreachable; sync skipped"
            return true
        }
        if mib < config.minServerAvailableMiB {
            lastMessage = "Server low on memory (\(mib) MiB); sync skipped"
            Log.line("sync skipped: server has \(mib) MiB available")
            return true
        }
        lastMessage = nil
        return false
    }

    private func waitForTunnel() async -> Bool {
        if !config.tunnel { return true }
        for _ in 0..<20 {
            if FileManager.default.fileExists(atPath: controlPath) && Shell.portOpen(config.localPort) { return true }
            try? await Task.sleep(nanoseconds: 1_000_000_000)
        }
        return false
    }

    /// driftRetry: a folder parked after drift tries a plain sync (no merge).
    enum SyncMode { case normal, repair, allowBigDeletion, driftRetry }

    /// One kept folder's paths for a run.
    private struct FolderRun {
        let f: KeptFolder
        let local: URL, work: URL, filters: URL, serverFile: URL, marker: URL
    }

    /// Runs one bisync for a folder. Returns true on success. Never creates, recreates or
    /// resyncs a folder unless this is its first sync, the user chose Repair, or the two sides'
    /// listings drifted apart (one automatic merge per folder per 24 hours; then the user decides).
    @discardableResult
    private func syncFolder(_ f: KeptFolder, mode: SyncMode) async -> Bool {
        live.remoteSyncStarted(f.key)   // server checks running now don't count
        let ok = await syncFolderRun(f, mode: mode)
        live.remoteSyncEnded(f, ok: ok)   // the ETag after the sync is the new baseline
        return ok
    }

    private func syncFolderRun(_ f: KeptFolder, mode: SyncMode) async -> Bool {
        let began = Date()
        runTrigger[f.key] = live.ownStart(f.key)
        defer {
            live.ownEnd(f.key, began: began); runTrigger[f.key] = nil
            runTimes[f.key] = (began, Date().timeIntervalSince(began))   // pacing
        }
        activeNames[f.key] = f.displayName
        activeFiles[f.key] = 0
        defer {
            activeNames[f.key] = nil
            activeFiles[f.key] = nil
            transfers[f.key] = nil   // the run is over
            transferRun[f.key] = nil
        }
        let fm = FileManager.default
        let parkedReason = mode == .driftRetry ? needsRepairReason(f) : nil
        status[f.key]?.state = .syncing
        let marker = stateFile(f, ".initialised")
        let initialised = fm.fileExists(atPath: marker.path)
        let first = !initialised || mode == .repair

        if mode == .normal, let reason = needsRepairReason(f) {
            status[f.key]?.state = .needsRepair(reason); return false
        }
        if mode == .driftRetry, !initialised || parkedReason == nil {
            try? fm.removeItem(at: stateFile(f, ".drift-parked"))
            status[f.key]?.state = parkedReason.map { .needsRepair($0) } ?? .idle
            return false
        }
        /// A parked folder keeps waiting for Repair whatever stopped this try.
        func stillParked() -> Bool {
            if mode == .driftRetry, let r = parkedReason { status[f.key]?.state = .needsRepair(r) }
            return false
        }
        guard fm.fileExists(atPath: config.localRootURL.path) || !initialised else {
            status[f.key]?.state = .failed("\(config.localRoot) is missing"); return stillParked()
        }
        let local = config.localURL(f)
        if initialised && !fm.fileExists(atPath: local.path) {
            status[f.key]?.state = .failed(Self.missingLocalText(f.displayName)); return stillParked()
        }
        // Sync history is only valid for the server and account it was made with.
        let work = workDir(f)
        let serverFile = work.appendingPathComponent("server.txt")
        if initialised, mode != .repair,
           let saved = try? String(contentsOf: serverFile, encoding: .utf8),
           saved != config.serverIdentity(for: f) {
            markNeedsRepair(f, "server or account settings changed"); return false
        }
        guard await waitForTunnel() else {
            status[f.key]?.state = .failed("no connection to server"); return stillParked()
        }

        if first { try? fm.createDirectory(at: local, withIntermediateDirectories: true) }
        try? fm.createDirectory(at: work, withIntermediateDirectories: true)
        if !fm.fileExists(atPath: serverFile.path) {
            try? config.serverIdentity(for: f).write(to: serverFile, atomically: true, encoding: .utf8)
        }
        let filters = stateFile(f, "").deletingLastPathComponent().appendingPathComponent("filters-\(f.stateName).txt")
        let filterText = (f.extraFilters + config.commonFilters + [Backups.filter]).joined(separator: "\n") + "\n"
        // only rewrite when changed: bisync insists on a resync after a filters change
        if let old = try? String(contentsOf: filters, encoding: .utf8), old != filterText,
           Backups.onlyAppFilterChanged(old, filterText) {
            // Only the app's own line changed (an upgrade): rclone will want a resync, which is
            // then one automatic merge instead of a Repair for the user.
            fm.createFile(atPath: stateFile(f, ".filters-upgraded").path, contents: nil)
        }
        if (try? String(contentsOf: filters, encoding: .utf8)) != filterText {
            try? filterText.write(to: filters, atomically: true, encoding: .utf8)
        }
        let run = FolderRun(f: f, local: local, work: work, filters: filters, serverFile: serverFile, marker: marker)

        if first {
            return await merge(run, kind: mode == .repair ? "merge (repair)" : "merge (first sync)").ok
        }
        var extra: [String] = []
        if mode == .allowBigDeletion {
            // Check both sides again: an emptied copy (an unplugged disk, a server folder that
            // vanished) must never be "applied" to the other side.
            if localMissingOrEmpty(local) {
                status[f.key]?.state = .failed(Self.missingLocalText(f.displayName)); return false
            }
            if await remoteMissingOrEmpty(f) {
                status[f.key]?.state = .failed(Self.missingRemoteText); return false
            }
            extra = ["--force"]
        }
        let r = await bisync(run, kind: "sync", extra: extra)
        if r.status == 0 {
            if mode == .driftRetry { Log.line("\(f.path): syncs again without a merge; no repair needed") }
            succeeded(run); return true
        }
        let res = BisyncResult.classify(r.output, timedOut: r.status == 124)
        if mode == .driftRetry {
            // Still not settled: it keeps waiting for Repair, and the next interval tries again.
            Log.line("\(f.path): still needs repair (plain sync failed)")
            return stillParked()
        }
        if res.outcome == .filtersChanged, fm.fileExists(atPath: stateFile(f, ".filters-upgraded").path) {
            return await filtersUpgradeMerge(run)
        }
        guard res.outcome == .drift else { await failed(run, res, r.output); return false }

        // The two sides' listings disagree. bisync won't continue (and --recover only falls back
        // to listings that disagree the same way); a newer-wins merge brings both sides together.
        let auto = stateFile(f, ".auto-merge")
        guard AutoMerge.allowed(auto) else {
            parkDrift(f, AutoMerge.driftReason(res.path, "already merged automatically today; choose Repair"))
            return false
        }
        // The once-a-day limit only holds if it is on disk: no record, no automatic merge.
        do { try AutoMerge.record(auto) } catch {
            Log.line("\(f.path): couldn't record the automatic merge (\(error.localizedDescription)); not merging")
            markNeedsRepair(f, AutoMerge.driftReason(res.path, "choose Repair"))
            return false
        }
        Log.line("\(f.path): out of sync (\(res.path ?? "listings differ")); merging automatically, newer copy wins")
        let (merged, outcome) = await merge(run, kind: "automatic merge (out of sync)",
                                            driftNote: "the automatic merge didn't settle it; choose Repair")
        guard merged else {
            if outcome == .interrupted { try? fm.removeItem(at: auto) }   // it didn't happen: the next sync may merge
            return false
        }
        lastMessage = "“\(f.displayName)” was out of sync and was merged automatically (the newer copy of each file was kept; older copies are in the backups)"
        autoMerged.append(f.displayName)
        return true
    }

    /// The one merge after the folder's filters changed (an upgrade changed the app's own
    /// filter line, or the user changed the excludes or subfolders): a normal backed-up merge,
    /// tried once, not counted toward the daily automatic-merge limit. An emptied side is never
    /// merged into.
    private func filtersUpgradeMerge(_ run: FolderRun) async -> Bool {
        let f = run.f
        if localMissingOrEmpty(run.local) {
            status[f.key]?.state = .failed(Self.missingLocalText(f.displayName)); return false
        }
        if await remoteMissingOrEmpty(f) {
            status[f.key]?.state = .failed(Self.missingRemoteText); return false
        }
        let marker = stateFile(f, ".filters-upgraded")
        try? FileManager.default.removeItem(at: marker)   // once: if it fails, Repair as usual
        Log.line("\(f.path): the excludes changed; merging once, newer copy wins")
        let (ok, outcome) = await merge(run, kind: "merge (excludes changed)")
        if !ok, outcome == .interrupted { FileManager.default.createFile(atPath: marker.path, contents: nil) }
        return ok
    }

    /// `bisync --resync --resync-mode newer`: for each file the newer copy wins and no file is
    /// deleted. Every copy a merge overwrites is moved to a dated backup folder first (see
    /// Backups). --check-sync stays on, so a merge that didn't bring both sides together fails
    /// now instead of breaking the next sync. Files that change during the merge fail that
    /// check; one more pass usually settles them. Also returns how the last run ended.
    private func merge(_ run: FolderRun, kind: String, driftNote: String = "choose Repair when it's quiet") async
        -> (ok: Bool, outcome: BisyncResult.Outcome?) {
        var kind = kind
        Backups.prune(config.stateURL, run.f.stateName)
        let stamp = Backups.stamp()
        for pass in 1...2 {
            // Each pass gets its own backup folder: a file the second pass replaces again must
            // not overwrite the first pass's backup of it.
            let backups = Backups.args(config.stateURL, config.remote(for: run.f), run.f.stateName, pass == 1 ? stamp : "\(stamp)-\(pass)")
            let r = await bisync(run, kind: kind, extra: ["--resync", "--resync-mode", "newer"] + backups)
            if r.status == 0 { succeeded(run); return (true, nil) }
            let res = BisyncResult.classify(r.output, timedOut: r.status == 124)
            if res.outcome != .drift || pass == 2 || quitting {
                if res.outcome == .drift {
                    parkDrift(run.f, AutoMerge.driftReason(res.path, driftNote))
                } else {
                    await failed(run, res, r.output)
                }
                return (false, res.outcome)
            }
            Log.line("\(run.f.path): files changed during the merge (\(res.path ?? "listings differ")); merging once more")
            kind = "merge (second pass)"
        }
        return (false, nil)
    }

    private func bisync(_ run: FolderRun, kind: String, extra: [String]) async -> Shell.Result {
        // INFO: bisync then says what it copies and deletes, for Recent Changes (Changes.swift),
        // and a merge's every copy can be checked afterwards.
        let level = "INFO"
        let args = ["bisync", run.local.path, config.remote(for: run.f) + run.f.path,
                    "--workdir", run.work.path, "--filters-file", run.filters.path,
                    "--create-empty-src-dirs", "--compare", "size,modtime",
                    "--resilient", "--recover", "--max-lock", "10m",
                    "--conflict-resolve", "newer", "--conflict-loser", "num",
                    "--transfers", "4", "--checkers", "6", "--log-level", level,
                    "--stats", "2s", "--stats-log-level", "NOTICE",   // progress for the menu (StatsFilter)
                    "--stats-file-name-length", "0"] + config.bwLimitArgs + extra   // whole file names in the Transferring: lines
        let log = Log.url("sync-\(run.f.stateName).log")
        Log.append("\n=== \(Date()) \(kind) of \(run.f.path)\n", to: log)
        let changes = ChangeCollector()   // fed off the main thread, line by line
        let r = await runTracked(config.rclone, args, log: log, key: run.f.key, onLine: { changes.line($0) })
        transfers[run.f.key] = nil
        transferRun[run.f.key] = nil   // a block of this run still on its way is dropped
        let (wrote, over) = changes.localWrites()
        live.ownWrites(run.f.key, wrote, unknown: over || kind != "sync")   // live sync: the run's own writes
        if r.status == 0 {
            var list = changes.result(kind: kind, folder: run.f, now: Date())
            if kind == "sync" { for i in list.indices { list[i].trigger = runTrigger[run.f.key] ?? "timer" } }
            addChanges(list)
        }
        return r
    }

    private func addChanges(_ add: [Change]) {
        guard !add.isEmpty else { return }
        recentChanges = Array((recentChanges + add).suffix(Changes.max))
        Changes.save(recentChanges, to: changesURL)
    }

    /// Where a change's file is on this Mac, if its folder is still kept.
    func changeURL(_ c: Change) -> URL? {
        guard !c.path.isEmpty, let f = config.kept.first(where: { $0.key == c.folder }),
              !c.path.split(separator: "/").contains(where: { $0 == ".." || $0 == "." }) else { return nil }
        return config.localURL(f).appendingPathComponent(c.path)
    }

    func changeExists(_ c: Change) -> Bool {
        guard !c.isDeletion, let u = changeURL(c) else { return false }
        return FileManager.default.fileExists(atPath: u.path)
    }

    func reveal(_ c: Change) {
        guard changeExists(c), let u = changeURL(c) else { return }
        NSWorkspace.shared.activateFileViewerSelecting([u])
    }

    func openWeb() {
        guard let s = config.serverURL, let u = URL(string: s) else { return }
        NSWorkspace.shared.open(u)
    }

    // MARK: - Share links

    /// The menu's Copy Public Link…: choose a file or folder in a kept folder.
    func shareFile() { share(startingAt: config.localRootURL) }

    /// Kept Folders ▸ Copy Public Link…: choose in this folder (or the folder itself).
    func shareFrom(_ f: KeptFolder) { share(startingAt: config.localURL(f)) }

    private func share(startingAt dir: URL) {
        guard config.isOIDC, !needsSignIn else { alert("Sign in first", "Share links need the browser sign-in."); return }
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = true
        panel.allowsMultipleSelection = false
        panel.directoryURL = dir
        panel.prompt = "Copy Public Link"
        panel.message = "Choose a file or folder in a kept folder. Anyone with its link can view and download it."
        NSApp.activate(ignoringOtherApps: true)
        guard panel.runModal() == .OK, let url = panel.url?.standardizedFileURL.resolvingSymlinksInPath() else { return }
        // The kept folder it is in, and its path in there.
        var found: (KeptFolder, String)?
        for f in config.kept {
            let root = config.localURL(f).standardizedFileURL.resolvingSymlinksInPath().path
            let p = url.path
            if Config.foldKey(p) == Config.foldKey(root) { found = (f, ""); break }
            if Config.foldKey(p).hasPrefix(Config.foldKey(root) + "/") { found = (f, String(p.dropFirst(root.count + 1))); break }
        }
        guard let (f, rel) = found else {
            alert("That isn't in a kept folder", "Choose a file or folder inside one of your kept folders.")
            return
        }
        guard FileManager.default.fileExists(atPath: stateFile(f, ".initialised").path) else {
            alert("“\(f.displayName)” hasn't synced yet", "Share it after its first sync.")
            return
        }
        if Changes.skip(rel) && !rel.isEmpty { alert("That can't be shared from here", "Choose another file."); return }
        let name = rel.isEmpty ? f.displayName : (rel as NSString).lastPathComponent
        Task {
            do {
                let server = config.serverURL ?? ""
                guard let dav = SharesAPI.davURL(config, f, rel: rel) else { throw AuthError.message("That path isn't valid.") }
                let token = try await Auth.accessToken(serverURL: server)
                let id = try await SharesAPI.fileID(dav, token: token)
                let where_ = "\(f.displayName)\(rel.isEmpty ? "" : "/" + rel)"
                // A view link the item already has is offered again, so public links don't pile up.
                // If the permissions can't be read, the link is made as before.
                var existing: SharedLinksAPI.Existing?
                do { existing = try await SharedLinksAPI.existingViewLink(server: server, token: token, item: id) }
                catch { Log.line("couldn't check \(where_) for an existing link: \(error.localizedDescription)") }
                var link: String?
                if let ex = existing {
                    switch SharesAPI.askExisting(for: name, ex) {
                    case .copyExisting: link = ex.url
                    case .makeNew: break
                    case .cancel: return
                    }
                }
                if link == nil {
                    // If the server insists on a password, the dialog opens again with the choices
                    // kept and the password marked required.
                    var preset: SharesAPI.Options?
                    var needPassword = false
                    while link == nil {
                        guard let o = SharesAPI.askOptions(for: name, preset: preset, passwordRequired: needPassword) else { return }
                        do {
                            link = try await SharesAPI.createLink(id: id, serverURL: server, token: token, o)
                        } catch is SharesAPI.PasswordRequired where !(needPassword && !o.password.isEmpty) {
                            preset = o
                            needPassword = true
                        }
                    }
                    Log.line("share link made for \(where_)")   // never the password
                } else {
                    Log.line("share link for \(where_): an existing view link copied")
                }
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(link ?? "", forType: .string)
                lastMessage = "Link to “\(name)” copied"
                notifier.show(Note(title: "Link copied", body: "A public, read-only link to “\(name)” is on the clipboard.", page: .folders))
            } catch {
                alert("Couldn't make the link", error.localizedDescription)
            }
        }
    }

    private func succeeded(_ run: FolderRun) {
        let fm = FileManager.default
        fm.createFile(atPath: run.marker.path, contents: nil)
        try? fm.removeItem(at: stateFile(run.f, ".needs-repair"))
        try? fm.removeItem(at: stateFile(run.f, ".drift-parked"))
        try? fm.removeItem(at: stateFile(run.f, ".filters-upgraded"))
        try? config.serverIdentity(for: run.f).write(to: run.serverFile, atomically: true, encoding: .utf8)
        status[run.f.key]?.state = .ok
        status[run.f.key]?.quiet = false
        failStreak[run.f.key] = nil
        status[run.f.key]?.lastSuccess = Date()
        saveLastSuccess()
        rescanConflicts(run.f)
    }

    // MARK: - Conflicts

    /// Looks for conflict copies in a folder (off the main thread) and remembers the list.
    func rescanConflicts(_ f: KeptFolder) {
        let local = config.localURL(f)
        let filters = f.extraFilters + config.commonFilters
        let file = stateFile(f, ".conflicts.json")
        let gen = (conflictScanGen[f.key] ?? 0) + 1
        conflictScanGen[f.key] = gen
        Task {
            let list = await Task.detached { Conflicts.scan(local, filters: filters) }.value
            guard self.conflictScanGen[f.key] == gen,
                  self.config.kept.contains(where: { $0.key == f.key }) else { return }
            let old = Set(Conflicts.load(file).map(\.file))
            let fresh = list.filter { !old.contains($0.file) }
            Conflicts.save(list, to: file)
            self.conflicts[f.key] = list
            if let first = fresh.first { self.newConflicts.append((f.displayName, fresh.count, (first.file as NSString).lastPathComponent)) }
        }
    }

    /// Runs a conflict action as the only sync activity (like Stop Keeping): now, or when the
    /// running syncs end (the menu says so). The folder and the copy are looked up again then,
    /// because a sync can have moved or removed the copy meanwhile (moving files under a running
    /// bisync could reach OpenCloud as a deletion), and the folder can have been removed or its
    /// settings changed. `work` gets the folder's current local copy, a fresh backup folder
    /// and the conflict copy. Syncs wait until it is done.
    private func conflictAction(_ f: KeptFolder, _ item: ConflictItem, failure: String,
                                _ work: @escaping @Sendable (_ local: URL, _ backupDir: URL, _ loser: URL) throws -> Void,
                                done: @escaping @MainActor () -> Void) {
        _ = exclusive { [self] in
            do {
                guard let cur = self.config.kept.first(where: { $0.key == f.key }) else {
                    throw AuthError.message("“\(f.displayName)” isn't kept on this Mac any more; nothing was changed.")
                }
                let local = self.config.localURL(cur)
                let backupDir = Backups.localDir(self.config.stateURL, cur.stateName, Backups.stamp())
                // It may have waited for a sync: is the copy still there?
                guard let (loser, _) = Conflicts.target(local, item),
                      FileManager.default.fileExists(atPath: loser.path) else {
                    throw AuthError.message("The conflict copy is gone (synced or removed meanwhile).")
                }
                // Off the main thread: a move to another disk copies the whole file.
                try await Task.detached { try work(local, backupDir, loser) }.value
                done()
            } catch {
                self.alert(failure, error.localizedDescription)
            }
            self.rescanConflicts(f)
        }
    }

    /// Keep This Version: the conflict copy becomes the file; the current file goes to the backups.
    func keepConflict(_ f: KeptFolder, _ c: ConflictItem) {
        guard confirm("Keep this version of “\(c.original)”?",
                      "The conflict copy replaces the current file. The current file is kept in the backups (in the app's state folder, for 30 days). The next sync sends the change to OpenCloud.",
                      button: "Keep This Version") else { return }
        conflictAction(f, c, failure: "Couldn't keep that version", { local, backupDir, _ in
            try Conflicts.keep(c, local: local, backupDir: backupDir)
        }, done: { Log.line("\(f.path): conflict kept: \(c.file)") })
    }

    /// Discard: the conflict copy goes to the Trash.
    func discardConflict(_ f: KeptFolder, _ c: ConflictItem) {
        guard let (_, _) = Conflicts.target(config.localURL(f), c),
              confirm("Discard “\(c.file)”?", "It moves to the Trash. The next sync removes it from OpenCloud too.", button: "Discard") else { return }
        conflictAction(f, c, failure: "Couldn't move it to the Trash", { _, _, loser in
            try FileManager.default.trashItem(at: loser, resultingItemURL: nil)
        }, done: { Log.line("\(f.path): conflict discarded: \(c.file)") })
    }

    func revealConflict(_ f: KeptFolder, _ c: ConflictItem) {
        guard let (loser, _) = Conflicts.target(config.localURL(f), c) else { return }
        NSWorkspace.shared.activateFileViewerSelecting([loser])
    }

    var conflictCount: Int { config.kept.reduce(0) { $0 + (conflicts[$1.key]?.count ?? 0) } }

    nonisolated static func conflictsText(_ n: Int) -> String { n == 1 ? "1 conflict" : "\(n) conflicts" }

    nonisolated static let missingRemoteText = "Folder is missing or empty in OpenCloud"
    /// This Mac's copy vanished (an unplugged disk, a moved folder). The large-deletion override
    /// is never offered then: it would empty the copy in OpenCloud.
    nonisolated static func missingLocalText(_ name: String) -> String {
        "This Mac's copy of “\(name)” is empty or missing. If it is on a disk, reconnect it; to stop syncing it here instead, choose Stop Keeping (the folder in OpenCloud stays as it is)"
    }

    nonisolated static let networkText = "network problem; will retry (see the sync log)"

    /// rclone errors from a network blip (the next run usually works); same as the Go app.
    nonisolated static func transientNetwork(_ out: String) -> Bool {
        let re = #"(?i)can't assign requested address|connection reset|connection refused|i/o timeout|no such host|TLS handshake timeout|network is unreachable|broken pipe|unexpected EOF|context deadline exceeded|temporary failure in name resolution"#
        func blip(_ s: Substring) -> Bool { s.range(of: re, options: .regularExpression) != nil }
        guard blip(Substring(out)) else { return false }
        for l in out.split(separator: "\n") where l.contains("ERROR") && !blip(l) {
            if !(l.contains("Bisync aborted") || l.contains("bisync is EXPERIMENTAL") || l.contains("Failed to bisync") || l.contains("error(s) while")) {
                return false   // other errors too: not just a blip
            }
        }
        return true
    }

    /// Shows why a run failed (drift is handled by the caller).
    private func failed(_ run: FolderRun, _ res: BisyncResult, _ out: String) async {
        let f = run.f
        status[f.key]?.quiet = false
        defer {   // failed runs in a row; a first network blip is quiet
            if case .failed(let why) = status[f.key]?.state {
                failStreak[f.key, default: 0] += 1
                status[f.key]?.quiet = why == Self.networkText && failStreak[f.key] == 1
            }
        }
        switch res.outcome {
        case .signedOut:
            if config.isOIDC {
                // The token command said the sign-in is gone; don't show rclone's raw error.
                markSignedOut()
                status[f.key]?.state = .failed("signed out")
            } else {
                status[f.key]?.state = .failed("rclone couldn't sign in; will retry")
            }
        case .interrupted:
            if res.timeout && !quitting {
                status[f.key]?.state = .failed("timed out; will retry")
            } else {
                status[f.key]?.state = .skipped("interrupted; continues with the next sync")
            }
        case .bigChange:
            // An emptied copy isn't a deletion the user made: never offer to apply it to the other side.
            if res.side != "Path2", localMissingOrEmpty(run.local) {
                status[f.key]?.state = .failed(Self.missingLocalText(f.displayName))
            } else if res.side != "Path1", await remoteMissingOrEmpty(f) {
                status[f.key]?.state = .failed(Self.missingRemoteText)
            } else {
                blockGen[f.key, default: 0] += 1
                status[f.key]?.state = .bigDeletion(res.side == "Path1" ? "on this Mac" : res.side == "Path2" ? "in OpenCloud" : "")
            }
        case .filtersChanged:
            markNeedsRepair(f, "the excludes changed, so the sync history must start over; choose Repair")
        case .needsResync:
            if out.contains("directory not found"), await remoteMissingOrEmpty(f) {
                status[f.key]?.state = .failed(Self.missingRemoteText)
            } else {
                markNeedsRepair(f, "sync history is out of date")
            }
        case .drift:
            parkDrift(f, AutoMerge.driftReason(res.path, "choose Repair when it's quiet"))
        case .failed:
            if out.contains("directory not found"), await remoteMissingOrEmpty(f) {
                status[f.key]?.state = .failed(Self.missingRemoteText)
            } else if Self.transientNetwork(out) {
                status[f.key]?.state = .failed(Self.networkText)
            } else {
                status[f.key]?.state = .failed("rclone failed; will retry (see the sync log)")
            }
        }
    }

    /// True when the folder is gone from OpenCloud or holds nothing. A safety abort then means
    /// the server copy vanished, not that files were deleted on purpose: the large-deletion
    /// override must not be offered, or it would empty this Mac's copy too.
    private func remoteMissingOrEmpty(_ f: KeptFolder) async -> Bool {
        let r = await Shell.run(config.rclone, ["lsf", "--max-depth", "1", "--exclude", ".DS_Store", config.remote(for: f) + f.path], timeout: 60)
        if r.status != 0 { return r.output.contains("directory not found") || r.output.contains("not found") }
        return r.output.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    /// Runs rclone, remembering the process so Quit can stop it cleanly. After syncTimeout it is
    /// interrupted (bisync finishes the current file and saves its state); if it is still
    /// running a minute later it is terminated.
    private func runTracked(_ exe: String, _ args: [String], log: URL, key: String, onLine: (@Sendable (String) -> Void)? = nil) async -> Shell.Result {
        var tracked: Process?
        defer { if let p = tracked { procs[ObjectIdentifier(p)] = nil } }
        let timedOut = Flag()
        let timeout = syncTimeout
        let runID = UUID()
        transferRun[key] = runID
        let result: Shell.Result = await withCheckedContinuation { cont in
            let p = Process()
            p.executableURL = URL(fileURLWithPath: exe)
            p.arguments = args
            p.environment = Shell.environment()
            let pipe = Pipe()
            p.standardOutput = pipe
            p.standardError = pipe
            let buffer = OutputBuffer(headMax: 64 << 10, tailMax: 512 << 10)
            let logStream = LogStream(log)
            let stats = StatsFilter(into: buffer, log: logStream, folder: key, onLine: onLine, onTransfers: { list in
                Task { @MainActor in
                    // Only while the run goes on, and only if it changed (nothing republished otherwise).
                    guard self.transferRun[key] == runID, self.activeNames[key] != nil, (self.transfers[key] ?? []) != list else { return }
                    self.transfers[key] = list.isEmpty ? nil : list
                }
            }) { n in
                Task { @MainActor in
                    if self.activeFiles[key] != nil, self.activeFiles[key] != n { self.activeFiles[key] = n }
                }
            }
            pipe.fileHandleForReading.readabilityHandler = { h in
                let d = h.availableData
                if !d.isEmpty { stats.feed(d) }
            }
            p.terminationHandler = { proc in
                pipe.fileHandleForReading.readabilityHandler = nil
                stats.feed(pipe.fileHandleForReading.readDataToEndOfFile())
                stats.flush()
                logStream.close()
                cont.resume(returning: Shell.Result(status: timedOut.value ? 124 : proc.terminationStatus,
                                                    output: buffer.string))
            }
            do {
                try p.run()
                tracked = p
                procs[ObjectIdentifier(p)] = p
                DispatchQueue.global().asyncAfter(deadline: .now() + timeout) {
                    guard p.isRunning else { return }
                    timedOut.set()
                    p.interrupt()
                    DispatchQueue.global().asyncAfter(deadline: .now() + 60) {
                        guard p.isRunning else { return }
                        p.terminate()
                        // SIGTERM can be ignored too (a stuck network call): then SIGKILL.
                        DispatchQueue.global().asyncAfter(deadline: .now() + 30) {
                            if p.isRunning { kill(p.processIdentifier, SIGKILL) }
                        }
                    }
                }
            } catch {
                cont.resume(returning: Shell.Result(status: -1, output: "\(error)"))
            }
        }
        return result
    }

    /// Runs `body` as the only sync activity: now, or when the running syncs end (no new
    /// folder starts meanwhile). Returns false if another action already waits.
    @discardableResult
    private func exclusive(_ body: @escaping @MainActor () async -> Void) -> Bool {
        guard !exclOn, exclQ.isEmpty else {
            alert("Another action is waiting", "Try again when it finishes.")
            return false
        }
        if !isIdle {
            exclQ.append(body)
            lastMessage = "Starts when the running sync finishes"
            return true
        }
        exclOn = true
        updateBusy()
        Task { [weak self] in
            await body()
            guard let self else { return }
            self.exclOn = false
            self.updateBusy()
            self.runsIdle()
        }
        return true
    }

    // MARK: - Repair and large deletions

    func repair(_ f: KeptFolder) {
        let changed = needsRepairReason(f) == "server or account settings changed"
            ? " This folder last synced with a different server or account: its files on this Mac will be uploaded to the one you're signed in to now. If that's not what you want, use Stop Keeping instead."
            : ""
        guard confirm("Repair “\(f.displayName)”?",
                      "This merges this Mac's copy with OpenCloud. For each file the newer copy wins; the older copy is kept in the backups (\(Backups.folderName) in OpenCloud, and in the app's backups folder on this Mac for 30 days). Files deleted since the last good sync will come back.\(changed)",
                      button: "Repair") else { return }
        _ = exclusive { [weak self] in
            guard let self else { return }
            // It may have waited for a sync: look the folder up again, and repair only if it still needs it.
            guard let cur = self.config.kept.first(where: { $0.key == f.key }) else {
                self.abortQueued("“\(f.displayName)” isn't kept on this Mac any more; nothing was repaired.")
                return
            }
            guard self.needsRepairReason(cur) != nil else {
                self.abortQueued("“\(cur.displayName)” no longer needs a repair.")
                return
            }
            try? FileManager.default.removeItem(at: self.stateFile(cur, ".needs-repair"))
            // A repair the user chose also resets the daily automatic-merge allowance.
            try? FileManager.default.removeItem(at: self.stateFile(cur, ".auto-merge"))
            try? FileManager.default.removeItem(at: self.stateFile(cur, ".drift-parked"))
            await self.syncFolder(cur, mode: .repair)
        }
    }

    /// Only while the folder is blocked for that reason, never when the server copy is missing.
    func allowBigDeletion(_ f: KeptFolder) {
        guard case .bigDeletion(let side) = status[f.key]?.state else { return }
        let gen = blockGen[f.key, default: 0]
        let (where_, other) = side == "on this Mac" ? ("on this Mac", "OpenCloud")
            : side == "in OpenCloud" ? ("in OpenCloud", "this Mac") : ("on one side", "the other side")
        guard confirm("Allow a large deletion in “\(f.displayName)”?",
                      "More than half of this folder's files were deleted \(where_). If that was on purpose, the deletion will be applied to \(other) too. If not, choose Cancel and restore the files first.",
                      button: "Apply Deletion") else { return }
        _ = exclusive { [weak self] in
            guard let self else { return }
            // It may have waited for a sync: the folder must still be blocked for what the user approved.
            guard let cur = self.config.kept.first(where: { $0.key == f.key }),
                  case .bigDeletion(let now)? = self.status[cur.key]?.state, now == side,
                  self.blockGen[cur.key, default: 0] == gen else {
                self.abortQueued(Self.changedWhileWaitingText)
                return
            }
            await self.syncFolder(cur, mode: .allowBigDeletion)
        }
    }

    nonisolated static let missingPersonalText = "That folder doesn't exist in OpenCloud"
    nonisolated static let changedWhileWaitingText = "The folder changed while waiting; check it again before allowing."

    /// A queued action that found things changed: shown in the menu and as an alert, not silent.
    private func abortQueued(_ text: String) {
        lastMessage = text
        alert("Nothing was changed", text)
    }

    // MARK: - Adding and removing kept folders

    /// Lets the user pick a folder in the on-demand drive and keeps it on this Mac from now on.
    /// While config.json couldn't be loaded the model runs on defaults; saving then would replace
    /// the user's settings (kept folders included). Every settings change checks this first.
    var settingsBrokenMessage: String? {
        configError.map { "The settings file has a problem, so nothing is saved until it's fixed or removed (\(Config.fileURL.path)), then Reload Settings.\n\n\($0)" }
    }

    /// Settings ▸ Kept Folders with the folder picker open (Personal, project spaces, shares).
    func showFolderPicker() {
        showKeepPicker = true
        openSettings(.folders)
    }

    func keepFolder() {
        if let m = settingsBrokenMessage { alert("Can't change the settings", m); return }
        guard !needsSignIn else { alert("Sign in first", "Choose Set Up / Sign In… in the menu."); return }
        // The drive isn't up: choose from the server's folder list instead of the drive.
        guard driveMounted else { showFolderPicker(); return }
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.directoryURL = config.mountURL
        panel.prompt = "Keep on This Mac"
        panel.message = "Choose a folder in OpenCloud to keep on this Mac. It will sync both ways."
        NSApp.activate(ignoringOtherApps: true)
        guard panel.runModal() == .OK, let url = panel.url else { return }
        keepFolder(at: url)
    }

    /// Keeps the folder at `url`, which must be inside the mounted OpenCloud drive (the picker's
    /// choice, or Finder's "Keep on This Mac with OpenCloud Bar" service). The path is mapped to
    /// the folder's path in OpenCloud; anything outside the drive is refused with an alert. A
    /// file isn't kept: its folder is offered instead (a sync works on folders only).
    func keepFolder(at url: URL) {
        if let m = settingsBrokenMessage { alert("Can't change the settings", m); return }
        guard !needsSignIn else { alert("Sign in first", "Choose Set Up / Sign In… in the menu."); return }
        guard config.isConfigured else { alert("Set up first", "Choose Set Up / Sign In… in the menu."); return }
        guard driveMounted else {
            alert("The OpenCloud drive isn't connected", "Choose Connect Drive in the OpenCloud Bar menu, then try again. Only folders inside the drive can be kept this way.")
            return
        }
        let root = config.mountURL.path
        let outside = ("That folder isn't in OpenCloud", "Only a folder inside the OpenCloud drive (\(root)) can be kept, and not the drive itself. “\(url.path)” isn't one.")
        guard KeepSize.driveRel(root: root, path: url.path) != nil else { alert(outside.0, outside.1); return }
        var target = url
        if (try? url.resourceValues(forKeys: [.isDirectoryKey]))?.isDirectory == false {
            let parent = url.deletingLastPathComponent()
            guard KeepSize.driveRel(root: root, path: parent.path) != nil else {
                alert("That's a file", "Only folders can be kept, and its folder is the OpenCloud drive itself. Choose a folder inside the drive.")
                return
            }
            guard confirm("That's a file", "Only folders can be kept. Keep its folder, “\(parent.lastPathComponent)”, instead?", button: "Keep Its Folder") else { return }
            target = parent
        }
        // One spelling per folder: no doubled or trailing slashes.
        guard let rel = KeepSize.driveRel(root: root, path: target.path) else { alert(outside.0, outside.1); return }
        if let p = KeptFolder.problem(newPath: rel) { alert("Can't keep that folder", p.prefix(1).uppercased() + p.dropFirst() + "."); return }
        if rel.utf8.count > 90 {
            alert("That path is too long", "rclone's sync history can't handle folder paths longer than about 90 characters. Choose a folder higher up, or rename a folder.")
            return
        }
        let folder = KeptFolder(path: rel, stateName: KeptFolder.makeStateName(for: rel))
        var sizeURL: URL?
        if config.isOIDC { sizeURL = SpacesAPI.davURL(root: (config.serverURL ?? "").trimmingCharacters(in: CharacterSet(charactersIn: "/")) + config.davPath, path: rel) }
        Task {
            guard await sizeConfirmed(folder, davURL: sizeURL) else { return }
            if let e = addKept(folder) { alert("Can't keep that folder", e) }
        }
    }

    /// The size check before keeping (PROPFIND oc:size, KeepSize): asks when the folder is bigger
    /// than the free space at the local folder or than 5 GB. false: the user cancelled. A size
    /// that can't be read (or no browser sign-in) never blocks.
    private func sizeConfirmed(_ folder: KeptFolder, davURL: URL?) async -> Bool {
        guard config.isOIDC, let davURL,
              let token = try? await Auth.accessToken(serverURL: config.serverURL ?? ""),
              let size = await KeepSize.fetch(davURL, token: token) else { return true }
        let free = KeepSize.free(at: config.localRootURL)
        guard KeepSize.needsConfirm(size: size, free: free) else { return true }
        return confirmRisky("Keep this folder?", KeepSize.message(name: folder.space.map { "\(folder.path) in \($0.name)" } ?? folder.path, size: size, free: free), button: "Keep Anyway")
    }

    /// Adds a kept folder (checked against the others in its space and on this Mac), saves the
    /// settings and starts its first sync. Returns why it can't, or nil.
    private func addKept(_ new: KeptFolder) -> String? {
        if let m = settingsBrokenMessage { return m }
        var folder = new
        let key = Config.foldKey(folder.path), sid = folder.space?.id
        if let clash = config.kept.first(where: {
            let k = Config.foldKey($0.path)
            return $0.space?.id == sid && (key == k || key.hasPrefix(k + "/") || k.hasPrefix(key + "/"))
        }) {
            return "“\(folder.path)” overlaps “\(clash.path)”, which is already kept on this Mac."
        }
        while config.kept.contains(where: { $0.stateName == folder.stateName }) { folder.stateName += "x" }
        var c = config
        c.kept.append(folder)
        if let e = c.validationError() { return e.prefix(1).uppercased() + e.dropFirst() + "." }
        do { try c.write() } catch { return "Couldn't save the settings: \(error.localizedDescription)" }
        config = c
        refreshStatusEntries()
        if config.paused {
            lastMessage = "“\(folder.displayName)” downloads when you resume syncing"
        } else {
            if syncing {
                // Keeping doesn't wait for a running sync: a free lane takes it at once.
                queuedKeys.insert(folder.key)
                lastMessage = lanes >= config.parallel || exclOn || !exclQ.isEmpty
                    ? "“\(folder.displayName)” downloads when the running sync finishes" : "Downloading “\(folder.displayName)”…"
                startQueued()
            } else {
                lastMessage = "Downloading “\(folder.displayName)”…"
                syncNow()
            }
        }
        return nil
    }

    /// Keeps a folder from the personal space (`space` nil), a project space or a share.
    /// Read-only folders are refused: a two-way sync can't send changes back. Returns why it
    /// can't, or nil.
    func keepFolder(in space: Space?, path: String) async -> String? {
        if let m = settingsBrokenMessage { return m }
        guard !needsSignIn else { return "Sign in first." }
        let rel = path.split(separator: "/").joined(separator: "/")
        if rel.isEmpty { return "Choose a folder in the space (the whole space can't be kept)." }
        if let p = KeptFolder.problem(newPath: rel) { return p.prefix(1).uppercased() + p.dropFirst() + "." }
        if rel.utf8.count > 90 { return "That path is too long (rclone's limit is about 90 characters)." }
        let server = config.serverURL ?? ""
        guard let space else {
            var sizeURL: URL?
            // The personal space gets the same check as a space (browser sign-in only: the token is
            // the sign-in's). A missing folder is refused here instead of failing at the first sync.
            if config.isOIDC {
                let personal = Space(id: "", name: "OpenCloud", type: "personal", davPath: config.davPath)
                do {
                    let token = try await Auth.accessToken(serverURL: server)
                    // The same base rclone uses: the full server URL (keeping a path prefix) plus davPath.
                    let base = server.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
                    guard let url = SpacesAPI.davURL(root: base + config.davPath, path: rel) else { return "That path isn't valid." }
                    sizeURL = url
                    if SpacesAPI.readOnly(try await SpacesAPI.permissions(url, token: token, missing: Self.missingPersonalText)) {
                        return "“\(rel)” is read-only for you, so it can't be kept here (a kept folder syncs both ways). Open it in the browser instead."
                    }
                } catch {
                    if case AuthError.message(let m) = error, m == Self.missingPersonalText { return m }   // a 404
                    return "Couldn't check “\(rel)”: \(error.localizedDescription)"
                }
            }
            let folder = KeptFolder(path: rel, stateName: KeptFolder.makeStateName(for: rel))
            guard await sizeConfirmed(folder, davURL: sizeURL) else { return nil }   // cancelled
            return addKept(folder)
        }
        var sizeURL: URL?
        do {
            let token = try await Auth.accessToken(serverURL: server)
            guard let url = SpacesAPI.davURL(base: server, space, path: rel) else { return "That path isn't valid." }
            sizeURL = url
            if SpacesAPI.readOnly(try await SpacesAPI.permissions(url, token: token)) {
                return "“\(rel)” in \(space.name) is read-only for you, so it can't be kept here (a kept folder syncs both ways). Open it in the browser instead."
            }
        } catch {
            return "Couldn't check “\(rel)” in \(space.name): \(error.localizedDescription)"
        }
        let folder = KeptFolder(path: rel, stateName: KeptFolder.makeStateName(for: rel, spaceID: space.id), space: space.ref)
        guard await sizeConfirmed(folder, davURL: sizeURL) else { return nil }   // cancelled
        return addKept(folder)
    }

    /// The folders directly in `path` of a space (nil: the personal space), listed with rclone.
    func browse(_ space: Space?, path: String) async -> Result<[String], Error> {
        let f = KeptFolder(path: path.split(separator: "/").joined(separator: "/"), stateName: "", space: space?.ref)
        return await listDirs(in: f, path: f.path)
    }

    /// The text of the confirmation before a folder's excludes change (Change Excludes and
    /// Choose Subfolders): what rclone then needs, and that it deletes nothing.
    static func filtersChangeText(_ name: String) -> String {
        "Nothing is deleted from OpenCloud, and the copy on this Mac stays where it is. \(name) syncs again right after, comparing both sides: the newer copy of each file wins, and replaced files are kept in the backups."
    }

    enum SaveResult { case saved, cancelled, failed(String) }

    /// Choose Subfolders ▸ Save: asks, then applies the checklist's changes to the folder's
    /// excludes like Change Excludes.
    func saveSubfolders(_ f: KeptFolder, changes: [String: Bool]) async -> SaveResult {
        guard let cur = config.kept.first(where: { $0.key == f.key }) else { return .failed("That folder isn't kept any more.") }
        if SubfolderRules.apply(cur.extraFilters, changes) == cur.extraFilters { return .saved }
        let off = changes.contains { !$0.value }
        guard confirm("Change which subfolders of “\(f.displayName)” sync?",
                      (off ? "Unchecked folders stop syncing. " : "") + Self.filtersChangeText("“\(f.displayName)”"),
                      button: "Change Subfolders") else { return .cancelled }
        if let e = await updateConfig({ c in
            if let i = c.kept.firstIndex(where: { $0.key == f.key }) { c.kept[i].extraFilters = SubfolderRules.apply(c.kept[i].extraFilters, changes) }
        }) { return .failed(e) }
        return .saved
    }

    /// The folders directly in `path` (relative to the space root) of f's space, from the server.
    func listDirs(in f: KeptFolder, path: String) async -> Result<[String], Error> {
        if path.contains("'") || path.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) {
            return .failure(AuthError.message("That path isn't valid."))
        }
        let r = await Shell.run(config.rclone, ["lsf", "--dirs-only", "--max-depth", "1", config.remote(for: f) + path], timeout: 60)
        guard r.status == 0 else { return .failure(AuthError.message("Couldn't list that folder.")) }
        let dirs = r.output.split(separator: "\n").map(String.init).filter { $0.hasSuffix("/") }
            .map { String($0.dropLast()) }.filter { !$0.isEmpty && $0 != Backups.folderName }
        return .success(dirs.sorted { Config.foldKey($0) < Config.foldKey($1) })
    }

    /// Final sync, then moves the local copy to the Trash and stops syncing it.
    func stopKeeping(_ f: KeptFolder) {
        if let m = settingsBrokenMessage { alert("Can't change the settings", m); return }
        guard !exclOn, exclQ.isEmpty else { alert("Another action is waiting", "Try again when it finishes."); return }
        Task {
            let local = config.localURL(f)
            // This Mac's copy is gone or empty (an unplugged disk, say): nothing to upload or to
            // move to the Trash, so it just stops being kept. A final sync would only fail.
            let gone = localMissingOrEmpty(local)
            var text: String
            if gone {
                text = "The copy on this Mac is missing or empty, so there is nothing to sync or move to the Trash. OpenCloud Bar just stops keeping it here. The folder stays in OpenCloud and in the OpenCloud drive."
            } else {
                text = "Your changes are synced to OpenCloud first. Then the copy on this Mac is moved to the Trash. The folder stays in OpenCloud and in the OpenCloud drive."
                if let n = await countUnsyncedFiles(f), n > 0 {
                    text += "\n\n\(n) file(s) in it are never synced (for example build folders, node_modules, or folders excluded in Settings). They exist only on this Mac and will go to the Trash with it."
                }
                if !SubfolderRules.excluded(f.extraFilters).isEmpty {
                    text += "\n\nSubfolders you unchecked are in OpenCloud as they were; their old copy on this Mac goes to the Trash with the folder."
                }
            }
            guard confirm("Stop keeping “\(f.displayName)” on this Mac?", text, button: "Stop Keeping") else { return }
            _ = exclusive { [self] in
                let fm = FileManager.default
                if gone, localMissingOrEmpty(local) {
                    Log.line("\(f.path): local copy is missing or empty; stopped keeping it without a final sync")
                } else {
                    guard await self.syncFolder(f, mode: .normal) else {
                        self.alert("Couldn't sync “\(f.displayName)”", "Nothing was removed. Check the sync log and try again.")
                        return
                    }
                    do { try fm.trashItem(at: local, resultingItemURL: nil) } catch {
                        self.alert("Couldn't move it to the Trash", error.localizedDescription)
                        return
                    }
                }
                try? fm.removeItem(at: self.workDir(f))
                self.live.remoteForget(f.key)   // before .server-etag goes: a late check doesn't write it
                for s in [".initialised", ".needs-repair", ".auto-merge", ".drift-parked", ".filters-upgraded", ".conflicts.json", ".server-etag"] { try? fm.removeItem(at: self.stateFile(f, s)) }
                try? fm.removeItem(at: self.config.stateURL.appendingPathComponent("filters-\(f.stateName).txt"))
                var c = self.config
                c.kept.removeAll { $0.key == f.key }
                do { try c.write() } catch {
                    self.alert("Couldn't save the settings", error.localizedDescription)
                    return
                }
                self.config = c
                self.refreshStatusEntries()
                self.saveLastSuccess()
                self.recentChanges.removeAll { $0.folder == f.key }
                Changes.save(self.recentChanges, to: self.changesURL)
                self.lastMessage = "“\(f.displayName)” is now only in OpenCloud"
            }
        }
    }

    /// Number of files in a kept folder that the filters exclude from syncing (nil if unknown).
    private func countUnsyncedFiles(_ f: KeptFolder) async -> Int? {
        let local = config.localURL(f)
        let filters = config.stateURL.appendingPathComponent("filters-\(f.stateName).txt")
        guard FileManager.default.fileExists(atPath: filters.path) else { return nil }
        async let all = Shell.run(config.rclone, ["lsf", "-R", "--files-only", local.path], timeout: 300)
        async let synced = Shell.run(config.rclone, ["lsf", "-R", "--files-only", "--filters-file", filters.path, local.path], timeout: 300)
        let (a, s) = await (all, synced)
        guard a.status == 0, s.status == 0 else { return nil }
        let count = { (t: String) in t.split(separator: "\n").count }
        return max(0, count(a.output) - count(s.output))
    }

    func openLocal(_ f: KeptFolder) {
        NSWorkspace.shared.open(config.localURL(f))
    }

    /// Syncs just this folder now (the Settings window's per-folder Sync Now).
    func syncNow(_ f: KeptFolder) {
        guard !needsSignIn, configError == nil, !config.paused else { return }
        enqueue(WorkItem(key: f.key, manual: true, prio: true, timer: false))   // next free lane, ahead of the timer's
        dispatch()
    }

    /// One file of the drive's upload queue.
    struct DriveUpload: Equatable, Identifiable {
        var name: String
        var size: Int64
        var uploading: Bool
        var id: String { name }
    }

    struct DriveQueue: Equatable {
        var files: [DriveUpload] = []   // the first `listMax`
        var uploading = 0
        var queued = 0
        var countsOnly = false          // this rclone has no `vfs/queue`: only the counts are known
        var total: Int { uploading + queued }
        static let listMax = 100
    }

    /// Reads the drive's upload queue with `vfs/queue` (rclone lists it in rc/list, v1.76 here;
    /// rclone.org/rc/#vfs-queue), or, if that isn't answered, the counts in `vfs/stats` (and then
    /// vfs/queue is not tried again this session). Asked at most every 5 s while a sync runs, the
    /// last answer was not empty or the Transfers page is open, and every 30 s otherwise,
    /// however often it is called (the menu's tick, the page).
    func refreshDriveQueue() async {
        guard mount?.isRunning == true else {
            if driveQueue != DriveQueue() { driveQueue = DriveQueue() }
            return
        }
        // Often only while something moves or the page is open; an idle drive is asked every 30 s.
        let busy = !activeNames.isEmpty || driveQueue.total > 0 || transfersPageOpen
        guard Date().timeIntervalSince(driveQueueAt) >= (busy ? 5 : 30) else { return }
        driveQueueAt = Date()
        var next: DriveQueue?
        if !queueCallUnsupported, let json = await rc("vfs/queue", timeout: 5), let list = json["queue"] as? [[String: Any]] {
            let all = list.map { DriveUpload(name: $0["name"] as? String ?? "", size: ($0["size"] as? NSNumber)?.int64Value ?? 0,
                                             uploading: $0["uploading"] as? Bool ?? false) }
            let n = all.filter(\.uploading).count
            next = DriveQueue(files: Array(all.prefix(DriveQueue.listMax)), uploading: n, queued: all.count - n)
        } else if let s = await driveStats() {
            // rclone answers, so a vfs/queue that failed isn't a blip: it has none. Not asked again.
            queueCallUnsupported = true
            next = DriveQueue(files: [], uploading: s.uploading, queued: s.queued, countsOnly: true)
        }
        let result = mount?.isRunning == true ? (next ?? DriveQueue()) : DriveQueue()
        if driveQueue != result { driveQueue = result }
    }

    /// Everything moving now: the kept folders' files and the drive's uploads (the menu's "Transfers (N)…").
    var transferCount: Int { transfers.values.reduce(0) { $0 + $1.count } + driveQueue.total }

    struct DriveStats {
        var cacheBytes: Int64
        var uploading: Int
        var queued: Int
    }

    /// Live numbers from the drive's rclone (nil when the drive isn't running or doesn't answer).
    func driveStats() async -> DriveStats? {
        guard mount?.isRunning == true,
              let json = await rc("vfs/stats", timeout: 5),
              let disk = json["diskCache"] as? [String: Any] else { return nil }
        func n(_ k: String) -> Int64 { (disk[k] as? NSNumber)?.int64Value ?? 0 }
        return DriveStats(cacheBytes: n("bytesUsed"), uploading: Int(n("uploadsInProgress")), queued: Int(n("uploadsQueued")))
    }

    // MARK: - Notifications and About

    /// Runs every tick: announces what changed since the last check (see NotePlanner).
    private func checkNotifications() {
        // A quiet failure (one network blip) counts as "no news": the last kind stands.
        let folders = config.kept.map { f -> (KeptFolder, FolderStatus.State) in
            let s = status[f.key]
            return (f, s?.quiet == true ? .skipped("") : s?.state ?? .idle)
        }
        var notes = notePlanner.plan(folders: folders, signedOut: signedOut && config.isOIDC, autoMerged: autoMerged)
        autoMerged = []
        let found = newConflicts
        newConflicts = []
        if configError == nil, let n = liveSyncNotice() { notes.append(n) }
        guard config.notifications, configError == nil else { return }
        if !found.isEmpty {
            let n = found.reduce(0) { $0 + $1.1 }
            let title = found.count == 1 ? "\(Self.conflictsText(n)) in “\(found[0].0)”" : "\(Self.conflictsText(n)) in \(found.count) folders"
            let saved = n == 1 ? "as “\(found[0].2)”" : "with .conflict1 (or a higher number) added to its name, like “\(found[0].2)”"
            notes.append(Note(title: title, body: "A file changed on both sides. The newer copy kept the name; the other is saved next to it \(saved). Open Settings ▸ Conflicts to keep one.", page: .conflicts))
        }
        // Only marked as shown when it is: with notifications off, the notice waits.
        if updateState.available, let v = updateState.latest,
           UserDefaults.standard.string(forKey: "notifiedUpdate") != v {
            UserDefaults.standard.set(v, forKey: "notifiedUpdate")   // once per version, across restarts
            notes.append(Note(title: "OpenCloud Bar \(v) is available",
                              body: "You have \(Updates.running). Choose Update Available in the menu to download it.", page: .general))
        }
        for n in notes {
            Log.line("notification: \(n.title)")
            notifier.show(n)
        }
    }

    nonisolated static let liveNoticeText = "Changes now sync as they happen. You can turn this off in Settings ▸ Sync."

    /// Once, for someone who never chose the live sync setting (on by default since 2.0): an
    /// Activity line, and a notification when they are on (the caller shows it). Nothing kept
    /// yet: it waits for the first kept folder. Marked as given only then (the Activity line
    /// counts as given when notifications are off), or when the setting was chosen.
    private func liveSyncNotice() -> Note? {
        let key = "notifiedLiveSync"
        guard !UserDefaults.standard.bool(forKey: key) else { return nil }
        if config.liveSync != nil { UserDefaults.standard.set(true, forKey: key); return nil }   // chosen
        guard config.isConfigured, !config.kept.isEmpty else { return nil }   // later
        addChanges([Change(time: Date(), folder: "", name: "OpenCloud Bar", path: "", action: "summary", text: Self.liveNoticeText)])
        UserDefaults.standard.set(true, forKey: key)
        return Note(title: "Changes now sync right away", body: Self.liveNoticeText, page: .sync)
    }

    func showAbout() {
        if aboutWindow == nil { aboutWindow = AboutWindowController(model: self) }
        aboutWindow?.show()
    }

    // MARK: - Pause and updates

    /// Pauses or resumes syncing of kept folders (saved in the settings). The drive keeps
    /// working; a sync that is already running finishes first.
    /// `until` makes it a timed pause: syncing resumes by itself then (the supervisor's tick).
    /// `automatic` is the supervisor ending a timed pause: if that fails it waits 5 minutes before
    /// trying again and shows one alert, not one per tick.
    func setPaused(_ paused: Bool, until: Date? = nil, automatic: Bool = false) {
        Task {
            defer { resumingPause = false }
            let stamp = paused ? until.map { ISO8601DateFormatter().string(from: $0) } : nil
            if let e = await updateConfig({ $0.paused = paused; $0.pausedUntil = stamp }) {
                if automatic {
                    resumeRetryAt = Date().addingTimeInterval(Self.resumeRetryDelay)
                    guard !resumeAlerted else { return }
                    resumeAlerted = true
                }
                alert(paused ? "Couldn't pause syncing" : "Couldn't resume syncing", e)
                return
            }
            resumeRetryAt = .distantPast
            resumeAlerted = false
            Log.line(paused ? "syncing paused" + (stamp.map { " until \($0)" } ?? "") : "syncing resumed")
            if !paused { nextSyncAt = Date(); tick() }
        }
    }

    /// The next 08:00 local time after `now` (the "until tomorrow" pause).
    nonisolated static func nextMorning(after now: Date, calendar: Calendar = .current) -> Date {
        calendar.nextDate(after: now, matching: DateComponents(hour: 8, minute: 0, second: 0), matchingPolicy: .nextTime) ?? now.addingTimeInterval(86_400)
    }

    /// "14:30" for a time today, "Wed 08:00" for another day: when a timed pause ends.
    nonisolated static func pauseClock(_ end: Date, now: Date = Date(), calendar: Calendar = .current) -> String {
        let f = DateFormatter()
        f.calendar = calendar
        f.setLocalizedDateFormatFromTemplate(calendar.isDate(end, inSameDayAs: now) ? "jjmm" : "EEEjjmm")
        return f.string(from: end)
    }

    /// Asks GitHub for the latest release. The daily check is silent; the menu's
    /// Check for Updates… always says what it found.
    func checkForUpdates(manual: Bool) async {
        guard !updateState.checking else { return }
        updateState.checking = true
        var s = updateState
        do {
            let (tag, url) = try await Updates.fetchLatest()
            s.latest = tag.hasPrefix("v") ? String(tag.dropFirst()) : tag
            s.url = url
            s.error = nil
            s.checkedAt = Date()
            updateFailures = 0
            updateRetryAt = .distantPast
        } catch {
            // Not the daily check: it stays due, after a growing wait (not every tick).
            s.error = error.localizedDescription
            updateFailures += 1
            updateRetryAt = Date().addingTimeInterval(Updates.retryDelay(updateFailures))
        }
        s.checking = false
        updateState = s
        if s.error == nil { Updates.save(s) }
        if s.available { Log.line("update available: \(s.latest ?? "")") }
        guard manual else { return }
        if let e = s.error {
            alert("Couldn't check for updates", e)
        } else if s.available, let v = s.latest {
            if confirm("OpenCloud Bar \(v) is available", "You have \(Updates.running). Open the release page to download it?",
                       button: "Open Release Page") { openRelease() }
        } else if Updates.isDevBuild {
            alert("Development build", "This is a development build (\(Updates.running)), so it isn't compared with releases. The latest release is \(s.latest ?? "unknown").")
        } else {
            alert("OpenCloud Bar is up to date", "You have \(Updates.running), the latest release.")
        }
    }

    /// Opens the release page GitHub reported (only ever this project's own pages).
    func openRelease() {
        var link = updateState.url ?? ""
        if !link.hasPrefix(Updates.pagePrefix) { link = Updates.pagePrefix + "latest" }
        if let u = URL(string: link) { NSWorkspace.shared.open(u) }
    }

    // MARK: - Settings

    func toggleOpenAtLogin() {
        do {
            if SMAppService.mainApp.status == .enabled { try SMAppService.mainApp.unregister() }
            else { try SMAppService.mainApp.register() }
        } catch {
            alert("Couldn't change Open at Login", error.localizedDescription)
        }
        opensAtLogin = SMAppService.mainApp.status == .enabled
    }

    func openLogs() {
        try? FileManager.default.createDirectory(at: Config.logURL, withIntermediateDirectories: true)
        NSWorkspace.shared.open(Config.logURL)
    }

    func editConfig() {
        if !FileManager.default.fileExists(atPath: Config.fileURL.path) { config.save() }
        NSWorkspace.shared.open(Config.fileURL)
    }

    /// Applies one edit from the Settings window: validates, saves config.json (other keys in
    /// the file are kept) and applies it. Returns an error message, or nil on success.
    /// Changes that restart the connection or alter sync filters wait until no sync is running.
    func updateConfig(_ edit: (inout Config) -> Void) async -> String? {
        if let m = settingsBrokenMessage { return m }
        var trial = config
        edit(&trial)
        if trial.connectionKey != config.connectionKey, driveMounted, let n = await pendingUploads(), n > 0 {
            return "\(n) file(s) written to the drive haven't finished uploading. Try again in a moment."
        }
        // Settings may have changed while we waited for rclone.
        var c = config
        edit(&c)
        if let e = c.validationError() { return e }
        if c.localRoot != config.localRoot || c.mountPoint != config.mountPoint, let e = c.folderSettingsError() { return e }
        if config.isConfigured && !c.isConfigured { return "That would leave OpenCloud Bar without a server to connect to." }
        let old = config
        let reconnect = c.connectionKey != old.connectionKey
        // A folder's own excludes may change while another folder syncs: each run uses the
        // folder as it was when the run started, and the change applies from its next sync.
        let changedKeys = Set(c.kept.filter { n in
            guard let o = old.kept.first(where: { $0.key == n.key }) else { return false }
            return o.extraFilters != n.extraFilters
        }.map(\.key))
        let commonChanged = c.commonFilters != old.commonFilters
        if syncing && (reconnect || commonChanged || c.kept.contains { changedKeys.contains($0.key) && folderBusy($0) }) {
            return "A sync is running. Try again when it finishes."
        }
        do { try c.write() } catch { return "Couldn't save the settings: \(error.localizedDescription)" }

        if c.serverURL != old.serverURL {
            quotaIdentity = Self.quotaIdentity(c)
            resetQuota(stateFolders: [old.stateURL, c.stateURL])
        }
        if c.serverNotifications && !old.serverNotifications {
            // Nothing was polled while it was off, so what piled up isn't news: the next fetch
            // only records.
            resetServerNotes(stateFolders: [old.stateURL, c.stateURL])
        }
        config = c
        applyEnvironment()
        refreshStatusEntries()
        // Changed excludes: rclone wants these folders' sync started over. That is one
        // automatic backed-up merge each, right away (filtersUpgradeMerge), not a Repair.
        let refiltered = c.kept.filter { f in commonChanged ? old.kept.contains { $0.key == f.key } : changedKeys.contains(f.key) }
        if !refiltered.isEmpty {
            for f in refiltered where FileManager.default.fileExists(atPath: stateFile(f, ".initialised").path) {
                FileManager.default.createFile(atPath: stateFile(f, ".filters-upgraded").path, contents: nil)
            }
            queuedKeys.formUnion(refiltered.map(\.key))
            startQueued()
        }
        if c.syncIntervalSeconds != old.syncIntervalSeconds {
            nextSyncAt = min(nextSyncAt, Date().addingTimeInterval(TimeInterval(max(60, c.syncIntervalSeconds))))
        }
        if reconnect {
            restartConnection(mountedAt: old.mountURL)
        } else if c.cacheMaxSize != old.cacheMaxSize || c.cacheMaxAge != old.cacheMaxAge
                    || c.bwLimitUp != old.bwLimitUp || c.bwLimitDown != old.bwLimitDown {
            restartDriveForNewSettings()
        }
        return nil
    }

    /// The drive reads its cache and volume settings only at start: remount it (the connection
    /// itself stays up) unless files are still uploading.
    private func restartDriveForNewSettings() {
        guard mount?.isRunning == true else { return }
        Task {
            if let n = await pendingUploads(), n > 0 {
                lastMessage = "Drive settings apply the next time the drive reconnects (\(n) file(s) still uploading)"
                return
            }
            if await unmountDrive() {
                mountRestartAt = .distantPast
                lastMessage = nil
            } else {
                lastMessage = "Drive settings apply the next time the drive reconnects (it is in use)"
            }
        }
    }

    /// Opens the Settings window (one shared window), optionally on a given page.
    func openSettings(_ page: SettingsPage? = nil) {
        if settingsWindow == nil { settingsWindow = SettingsWindowController(model: self) }
        settingsWindow?.show(page: page)
    }

    func reloadConfig() {
        guard !syncing else { alert("A sync is running", "Reload when it finishes."); return }
        applyLoadedConfig(message: "Settings reloaded")
    }

    /// Loads config.json and applies it; restarts the connection if connection settings changed
    /// or the sign-in is gone.
    private func applyLoadedConfig(message: String?) {
        let (c, err) = Config.load()
        configError = err
        guard err == nil else { return }
        let out = c.isOIDC && c.isConfigured && !Auth.haveCredentials(c)
        let wasOut = signedOut
        let reconnect = c.connectionKey != config.connectionKey || out
        let id = Self.quotaIdentity(c)
        if wasOut || out || c.serverURL != config.serverURL || id != quotaIdentity {
            // Signed out or in, another server or account: the old quota isn't this one's.
            quotaIdentity = id
            resetQuota(stateFolders: [config.stateURL, c.stateURL])
        }
        config = c
        signedOut = out
        if !out { waitingForKeychain = false }
        applyEnvironment()
        refreshStatusEntries()
        lastMessage = out ? Self.signedOutText : message
        if wasOut && !out {
            // Just signed in: start right away instead of waiting out old retry timers.
            authRetryAt = .distantPast; tunnelRestartAt = .distantPast; mountRestartAt = .distantPast; nextSyncAt = Date()
        }
        if reconnect { restartConnection() }
        startTicker()
    }

    // MARK: - Set up and sign in

    /// Opens the set-up window (server address + "Sign In with Browser").
    func showSetup() {
        if setupWindow == nil { setupWindow = SetupWindowController(model: self) }
        setupWindow?.show()
    }

    /// Called by the set-up window after a successful sign-in.
    func finishSetup(username: String) {
        setupWindow = nil
        applyLoadedConfig(message: "Signed in as \(username). Choose a folder to keep on this Mac.")
        // Nothing is kept yet and the drive may not be up: open the picker, which doesn't need it.
        if config.kept.isEmpty { showFolderPicker() }
    }

    func signOut() {
        guard confirm("Sign out of OpenCloud?",
                      "The drive is disconnected and syncing stops. Your kept folders stay on this Mac. Sign in again to resume.",
                      button: "Sign Out") else { return }
        Task {
            guard await confirmUploads(action: "Sign Out Anyway", later: "They upload after you sign in again.") else { return }
            await Auth.logout()
            signedOut = true
            lastMessage = Self.signedOutText
            resetQuota(stateFolders: [config.stateURL])
            restartConnection()
        }
    }

    /// Asks before stopping the drive while files may still be uploading. Fails closed: if the
    /// count can't be read, it asks too.
    private func confirmUploads(action: String, later: String) async -> Bool {
        switch await pendingUploads() {
        case .some(0):
            return true
        case .some(let n):
            return confirmRisky("Files are still uploading",
                                "\(n) file(s) written to the OpenCloud drive haven't finished uploading. \(later)", button: action)
        case .none:
            return confirmRisky("Uploads couldn't be checked",
                                "OpenCloud Bar couldn't check whether files written to the OpenCloud drive have finished uploading. Anything not uploaded yet: \(later.prefix(1).lowercased() + later.dropFirst())",
                                button: action)
        }
    }

    /// Stops the drive and tunnel so tick() starts them again with the current settings.
    /// `mountedAt`: where the running drive is mounted, when the settings just moved it.
    private func restartConnection(mountedAt: URL? = nil) {
        let m = mount
        let path = (mountedAt ?? config.mountURL).path
        unmountRequested = true
        Task {
            if let m, m.isRunning {
                _ = await Shell.run("/usr/sbin/diskutil", ["unmount", path], timeout: 30)
                m.terminate()
            }
            await stopTunnel()
            mountRestartAt = .distantPast
            tunnelRestartAt = .distantPast
        }
    }

    // MARK: - Quit

    /// Stops everything cleanly: lets a running sync finish its current file and save state,
    /// ejects the drive once uploads are done, then closes the tunnel.
    func quit() {
        guard !quitting else { return }
        Task {
            guard await confirmUploads(action: "Quit Anyway", later: "They upload the next time OpenCloud Bar runs.") else {
                NSApp.reply(toApplicationShouldTerminate: false)
                return
            }
            quitting = true
            ticker?.invalidate()
            live.stopAll()
            lastMessage = "Finishing sync…"
            work = []; queuedKeys = []; exclQ = []
            let ps = Array(procs.values)
            for p in ps { p.interrupt() }
            for _ in 0..<1200 where ps.contains(where: \.isRunning) {   // up to 2 minutes
                try? await Task.sleep(nanoseconds: 100_000_000)
            }
            for p in ps where p.isRunning {
                p.terminate()
                for _ in 0..<50 where p.isRunning { try? await Task.sleep(nanoseconds: 100_000_000) }
                if p.isRunning { kill(p.processIdentifier, SIGKILL) }
            }
            if let m = mount {
                unmountRequested = true
                _ = await Shell.run("/usr/sbin/diskutil", ["unmount", config.mountURL.path], timeout: 20)
                if m.isRunning { m.terminate() }   // rclone serve nfs doesn't stop by itself on unmount
                for _ in 0..<50 where m.isRunning { try? await Task.sleep(nanoseconds: 100_000_000) }
                if m.isRunning {
                    m.terminate()
                    for _ in 0..<30 where m.isRunning { try? await Task.sleep(nanoseconds: 100_000_000) }
                    if m.isRunning { kill(m.processIdentifier, SIGKILL) }
                }
            }
            await stopTunnel()
            NSApp.reply(toApplicationShouldTerminate: true)
        }
    }

    // MARK: - Dialogs

    func alert(_ title: String, _ text: String) {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        NSApp.activate(ignoringOtherApps: true)
        a.runModal()
    }

    /// Like confirm, but Cancel is the default button (Return doesn't take the risky choice).
    func confirmRisky(_ title: String, _ text: String, button: String) -> Bool {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        a.alertStyle = .warning
        a.addButton(withTitle: "Cancel")
        a.addButton(withTitle: button)
        NSApp.activate(ignoringOtherApps: true)
        return a.runModal() == .alertSecondButtonReturn
    }

    func confirm(_ title: String, _ text: String, button: String) -> Bool {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        a.addButton(withTitle: button)
        a.addButton(withTitle: "Cancel")
        NSApp.activate(ignoringOtherApps: true)
        return a.runModal() == .alertFirstButtonReturn
    }

    // MARK: - Summary for the menu

    func hasProblem(_ f: KeptFolder) -> Bool {
        attentionReason(f) != nil
    }

    /// Why a folder needs the user, in a few words (nil if it doesn't).
    func attentionReason(_ f: KeptFolder) -> String? {
        switch status[f.key]?.state {
        case .needsRepair(let why): return why.isEmpty ? "needs repair" : "needs repair: \(why)"
        case .bigDeletion(let side): return side.isEmpty ? "large deletion blocked" : "large deletion blocked (\(side))"
        case .failed(let why): return why.isEmpty ? "sync failed" : "sync failed: \(why)"
        default: return nil
        }
    }

    /// The folder's line in the Kept Folders menu: the problem, or when it last synced.
    func menuDetail(_ f: KeptFolder) -> String {
        switch status[f.key]?.state {
        case .needsRepair: return "needs repair"
        case .bigDeletion: return "large deletion blocked"
        case .failed(let why): return why == Self.missingRemoteText ? "missing or empty in OpenCloud" : "sync failed"
        default: break
        }
        switch status[f.key]?.state {
        case .syncing: return "syncing…"
        case .skipped(let why): return "skipped (\(why))"
        default: break
        }
        if let n = conflicts[f.key]?.count, n > 0 { return Self.conflictsText(n) }
        if let t = status[f.key]?.lastSuccess { return "synced \(agoText(t))" }
        return "waiting for first sync"
    }

    var summary: String {
        let s = baseSummary
        let n = conflictCount
        return n > 0 && configError == nil && !syncing ? s + " · " + Self.conflictsText(n) : s
    }

    private var baseSummary: String {
        if let e = configError { return e }
        if !config.isConfigured { return "Not set up: choose Set Up / Sign In…" }
        if waitingForKeychain { return "Waiting for Keychain access…" }
        if signedOut { return Self.signedOutText }
        if syncing {
            if activeNames.count > 1 {
                let files = activeFiles.values.reduce(0, +)
                return "Syncing \(activeNames.count) folders" + (files == 1 ? " · 1 file" : files > 1 ? " · \(files) files" : "") + "…"
            }
            let key = activeNames.keys.first
            var idx = batchDone + 1, n = batchN
            if n <= 1 || idx > n { idx = 1; n = 1 }
            return activityText(name: key.flatMap { activeNames[$0] }, index: idx, count: n, files: key.flatMap { activeFiles[$0] } ?? 0)
        }
        let bad = config.kept.filter(hasProblem)
        if let first = bad.first, let why = attentionReason(first) {
            var s = "“\(first.displayName)” \(why)"
            if bad.count == 2 { s += " (and 1 more folder)" } else if bad.count > 2 { s += " (and \(bad.count - 1) more folders)" }
            return s
        }
        let times = config.kept.compactMap { status[$0.key]?.lastSuccess }
        if config.paused {
            let paused = config.pauseEnd.map { "Paused until \(Self.pauseClock($0))" } ?? "Paused"
            return times.min().map { "\(paused) · synced \(agoText($0))" } ?? paused
        }
        if let oldest = times.min(), times.count == config.kept.count {
            return "Up to date · synced \(agoText(oldest))"
        }
        if !tunnelUp { return "Connecting to server…" }
        return config.kept.isEmpty ? "Nothing kept on this Mac" : "Waiting for first sync"
    }

    var symbol: String {
        if configError != nil || !config.isConfigured || signedOut { return "exclamationmark.icloud" }
        if syncing { return "arrow.triangle.2.circlepath.icloud" }
        if config.kept.contains(where: hasProblem) { return "exclamationmark.icloud" }
        if !tunnelUp { return "icloud.slash" }
        if config.paused { return "pause.circle" }
        return "icloud"
    }

    static func relative(_ d: Date) -> String {
        let f = RelativeDateTimeFormatter()
        f.unitsStyle = .short
        return f.localizedString(for: d, relativeTo: Date())
    }
}
