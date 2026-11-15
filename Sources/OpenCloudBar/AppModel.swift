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
}

/// Owns the three moving parts: the SSH tunnel, the on-demand drive (rclone nfsmount)
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
    @Published private(set) var lastMessage: String?
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
    private lazy var notifier = Notifier(model: self)
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
    /// What the running sync is doing, for the menu's first line.
    @Published private(set) var activeFolder: String?
    @Published private(set) var activeFiles = 0
    private var activeIndex = 0, activeCount = 0
    private var settingsWindow: SettingsWindowController?
    private var tunnelRestartAt = Date.distantPast
    private var mount: Process?
    private var mountStartedAt = Date.distantPast
    private var mountRestartAt = Date.distantPast
    private var mountBackoff: TimeInterval = 5
    private var unmountRequested = false
    private var syncTask: Task<Void, Never>?
    private var currentSync: Process?
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
        applyEnvironment()
        refreshStatusEntries()
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
    }

    // MARK: - Supervisor

    private func tick() {
        checkNotifications()
        if !quitting, configError == nil, config.checkForUpdates, Updates.due(updateState), Date() >= updateRetryAt {
            Task { await self.checkForUpdates(manual: false) }
        }
        guard !quitting, config.isConfigured, configError == nil else { return }
        if config.isOIDC {
            if signedOut { return }
            checkAuth()
        }
        driveMounted = Shell.isMounted(config.mountURL)
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
        if !syncing, !config.paused, tunnelUp, Date() >= nextSyncAt, !config.kept.isEmpty { startSyncAll(manual: false) }
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

    private func startMount() {
        let fm = FileManager.default
        let mnt = config.mountURL
        // A previous rclone crashed and left a dead mount behind: clear it first.
        if Shell.isMounted(mnt) {
            forceUnmount()
            mountRestartAt = Date().addingTimeInterval(10)
            return
        }
        try? fm.createDirectory(at: mnt, withIntermediateDirectories: true)
        try? fm.createDirectory(at: config.cacheURL, withIntermediateDirectories: true)
        let args = ["nfsmount", config.remoteSpec, mnt.path,
                    "--volname", config.volumeName,
                    "--vfs-cache-mode", "full", "--cache-dir", config.cacheURL.path,
                    "--vfs-cache-max-size", config.cacheMaxSize, "--vfs-cache-max-age", config.cacheMaxAge,
                    "--dir-cache-time", "30s", "--vfs-read-chunk-size", "16M", "--buffer-size", "16M",
                    "--transfers", "4", "--checkers", "8", "--low-level-retries", "10",
                    "--exclude", ".DS_Store", "--exclude", "._*",
                    "--rc", "--rc-addr", "127.0.0.1:\(config.rcPort)",
                    "--log-level", "NOTICE"] + config.bwLimitArgs
        unmountRequested = false
        mountStartedAt = Date()
        do {
            // The rc login goes in the environment, not the command line.
            mount = try Shell.launch(config.rclone, args, log: Log.url("drive.log"),
                                     extraEnv: ["RCLONE_RC_USER": rcUser, "RCLONE_RC_PASS": rcPass]) { [weak self] code in
                self?.mountExited(code)
            }
            Log.line("drive mount started")
        } catch {
            lastMessage = "Could not start rclone: \(error.localizedDescription)"
            driveWanted = false
        }
    }

    private func mountExited(_ code: Int32) {
        mount = nil
        let stillMounted = Shell.isMounted(config.mountURL)
        driveMounted = stillMounted
        if quitting { return }
        if unmountRequested {
            Log.line("drive ejected from the menu")
            return
        }
        if !stillMounted && code == 0 {
            // rclone exits cleanly when the volume is ejected in Finder.
            driveWanted = false
            mountBackoff = 5
            Log.line("drive ejected in Finder; stays off until Connect")
            return
        }
        // crashed or failed to start: clear any dead mount and retry with backoff
        if Date().timeIntervalSince(mountStartedAt) > 300 { mountBackoff = 5 }
        Log.line("drive mount exited (\(code)); retrying in \(Int(mountBackoff)) s")
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
            let path = config.mountURL.path
            unmountRequested = true
            let r = await Shell.run("/usr/sbin/diskutil", ["unmount", path], timeout: 30)
            if r.status == 0 {
                driveWanted = false
            } else {
                unmountRequested = false
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

    private func stateFile(_ f: KeptFolder, _ suffix: String) -> URL {
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

    /// Syncs all kept folders. Each folder's repair state is read from disk, so a folder that
    /// was repaired isn't skipped because of what the app remembered. A manual sync also tries
    /// a blocked large deletion again (it is blocked again if nothing changed).
    private func startSyncAll(manual: Bool) {
        guard !syncing, config.isConfigured, configError == nil, !signedOut else { return }
        nextSyncAt = Date().addingTimeInterval(TimeInterval(max(60, config.syncIntervalSeconds)))
        syncing = true
        let folders = config.kept
        syncTask = Task { [weak self] in
            guard let self else { return }
            if await self.serverTooBusy() {
                for f in folders { self.status[f.key]?.state = .skipped("server busy") }
            } else {
                for (i, f) in folders.enumerated() where !Task.isCancelled && !self.quitting {
                    self.activeIndex = i + 1
                    self.activeCount = folders.count
                    // skip folders removed while this run was waiting
                    guard self.config.kept.contains(where: { $0.key == f.key }) else { continue }
                    if let reason = self.needsRepairReason(f) {
                        if FileManager.default.fileExists(atPath: self.stateFile(f, ".drift-parked").path) {
                            _ = await self.syncFolder(f, mode: .driftRetry)
                        } else {
                            self.status[f.key]?.state = .needsRepair(reason)
                        }
                        continue
                    }
                    if case .bigDeletion = self.status[f.key]?.state, !manual { continue }
                    _ = await self.syncFolder(f, mode: .normal)
                }
            }
            self.activeIndex = 0
            self.activeCount = 0
            self.syncing = false
        }
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
        activeFolder = f.displayName
        activeFiles = 0
        let single = activeCount == 0   // Repair, Stop Keeping…: one folder on its own
        if single { activeIndex = 1; activeCount = 1 }
        defer {
            activeFolder = nil
            activeFiles = 0
            if single { activeIndex = 0; activeCount = 0 }
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

    /// The one merge after an upgrade changed only the app's own filter line: a normal
    /// backed-up merge, tried once, not counted toward the daily automatic-merge limit. An
    /// emptied side is never merged into.
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
        Log.line("\(f.path): the app's filters changed with the upgrade; merging once, newer copy wins")
        let (ok, outcome) = await merge(run, kind: "merge (app filters updated)")
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
        // Merges are rare: log every copy so a merge can be checked afterwards.
        let level = kind == "sync" ? "NOTICE" : "INFO"
        let args = ["bisync", run.local.path, config.remote(for: run.f) + run.f.path,
                    "--workdir", run.work.path, "--filters-file", run.filters.path,
                    "--create-empty-src-dirs", "--compare", "size,modtime",
                    "--resilient", "--recover", "--max-lock", "10m",
                    "--conflict-resolve", "newer", "--conflict-loser", "num",
                    "--transfers", "4", "--checkers", "6", "--log-level", level,
                    "--stats", "2s", "--stats-log-level", "NOTICE"] + config.bwLimitArgs + extra   // progress for the menu (StatsFilter)
        let log = Log.url("sync-\(run.f.stateName).log")
        Log.append("\n=== \(Date()) \(kind) of \(run.f.path)\n", to: log)
        return await runTracked(config.rclone, args, log: log)
    }

    private func succeeded(_ run: FolderRun) {
        let fm = FileManager.default
        fm.createFile(atPath: run.marker.path, contents: nil)
        try? fm.removeItem(at: stateFile(run.f, ".needs-repair"))
        try? fm.removeItem(at: stateFile(run.f, ".drift-parked"))
        try? fm.removeItem(at: stateFile(run.f, ".filters-upgraded"))
        try? config.serverIdentity(for: run.f).write(to: run.serverFile, atomically: true, encoding: .utf8)
        status[run.f.key]?.state = .ok
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

    private func conflictActionAllowed() -> Bool {
        guard !syncing else { alert("A sync is running", "Try again when it finishes."); return false }
        return true
    }

    /// Runs a conflict action as the only sync activity (like Stop Keeping), checked again after
    /// the confirmation: the dialog's run loop can start a scheduled sync, and moving files
    /// under a running bisync could reach OpenCloud as a deletion. Syncs wait until it is done.
    private func conflictAction(_ f: KeptFolder, failure: String,
                                _ work: @escaping @Sendable () throws -> Void, done: @escaping @MainActor () -> Void) {
        _ = exclusive { [self] in
            do {
                // Off the main thread: a move to another disk copies the whole file.
                try await Task.detached { try work() }.value
                done()
            } catch {
                self.alert(failure, error.localizedDescription)
            }
            self.rescanConflicts(f)
        }
    }

    /// Keep This Version: the conflict copy becomes the file; the current file goes to the backups.
    func keepConflict(_ f: KeptFolder, _ c: ConflictItem) {
        guard conflictActionAllowed(),
              confirm("Keep this version of “\(c.original)”?",
                      "The conflict copy replaces the current file. The current file is kept in the backups (in the app's state folder, for 30 days). The next sync sends the change to OpenCloud.",
                      button: "Keep This Version") else { return }
        let local = config.localURL(f)
        let backupDir = Backups.localDir(config.stateURL, f.stateName, Backups.stamp())
        conflictAction(f, failure: "Couldn't keep that version", {
            try Conflicts.keep(c, local: local, backupDir: backupDir)
        }, done: { Log.line("\(f.path): conflict kept: \(c.file)") })
    }

    /// Discard: the conflict copy goes to the Trash.
    func discardConflict(_ f: KeptFolder, _ c: ConflictItem) {
        guard conflictActionAllowed(),
              let (loser, _) = Conflicts.target(config.localURL(f), c),
              confirm("Discard “\(c.file)”?", "It moves to the Trash. The next sync removes it from OpenCloud too.", button: "Discard") else { return }
        conflictAction(f, failure: "Couldn't move it to the Trash", {
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

    /// Shows why a run failed (drift is handled by the caller).
    private func failed(_ run: FolderRun, _ res: BisyncResult, _ out: String) async {
        let f = run.f
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
    private func runTracked(_ exe: String, _ args: [String], log: URL) async -> Shell.Result {
        let timedOut = Flag()
        let timeout = syncTimeout
        let result: Shell.Result = await withCheckedContinuation { cont in
            let p = Process()
            p.executableURL = URL(fileURLWithPath: exe)
            p.arguments = args
            p.environment = Shell.environment()
            let pipe = Pipe()
            p.standardOutput = pipe
            p.standardError = pipe
            let buffer = OutputBuffer()
            let stats = StatsFilter(into: buffer) { n in
                Task { @MainActor in
                    if self.activeFiles != n { self.activeFiles = n }
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
                Log.append(buffer.string, to: log)
                cont.resume(returning: Shell.Result(status: timedOut.value ? 124 : proc.terminationStatus,
                                                    output: buffer.string))
            }
            do {
                try p.run()
                currentSync = p
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
        currentSync = nil
        return result
    }

    /// Runs `body` as the only sync activity. Returns false if a sync is already running.
    private func exclusive(_ body: @escaping @MainActor () async -> Void) -> Bool {
        guard !syncing else {
            alert("A sync is running", "Try again when it finishes.")
            return false
        }
        syncing = true
        syncTask = Task {
            await body()
            syncing = false
        }
        return true
    }

    // MARK: - Repair and large deletions

    func repair(_ f: KeptFolder) {
        guard confirm("Repair “\(f.displayName)”?",
                      "This merges this Mac's copy with OpenCloud. For each file the newer copy wins; the older copy is kept in the backups (\(Backups.folderName) in OpenCloud, and in the app's backups folder on this Mac for 30 days). Files deleted since the last good sync will come back.",
                      button: "Repair") else { return }
        _ = exclusive { [weak self] in
            guard let self else { return }
            try? FileManager.default.removeItem(at: self.stateFile(f, ".needs-repair"))
            // A repair the user chose also resets the daily automatic-merge allowance.
            try? FileManager.default.removeItem(at: self.stateFile(f, ".auto-merge"))
            try? FileManager.default.removeItem(at: self.stateFile(f, ".drift-parked"))
            await self.syncFolder(f, mode: .repair)
        }
    }

    /// Only while the folder is blocked for that reason, never when the server copy is missing.
    func allowBigDeletion(_ f: KeptFolder) {
        guard case .bigDeletion(let side) = status[f.key]?.state else { return }
        let (where_, other) = side == "on this Mac" ? ("on this Mac", "OpenCloud")
            : side == "in OpenCloud" ? ("in OpenCloud", "this Mac") : ("on one side", "the other side")
        guard confirm("Allow a large deletion in “\(f.displayName)”?",
                      "More than half of this folder's files were deleted \(where_). If that was on purpose, the deletion will be applied to \(other) too. If not, choose Cancel and restore the files first.",
                      button: "Apply Deletion") else { return }
        _ = exclusive { [weak self] in
            await self?.syncFolder(f, mode: .allowBigDeletion)
        }
    }

    // MARK: - Adding and removing kept folders

    /// Lets the user pick a folder in the on-demand drive and keeps it on this Mac from now on.
    /// While config.json couldn't be loaded the model runs on defaults; saving then would replace
    /// the user's settings (kept folders included). Every settings change checks this first.
    var settingsBrokenMessage: String? {
        configError.map { "The settings file has a problem, so nothing is saved until it's fixed or removed (\(Config.fileURL.path)), then Reload Settings.\n\n\($0)" }
    }

    func keepFolder() {
        if let m = settingsBrokenMessage { alert("Can't change the settings", m); return }
        guard !needsSignIn else { alert("Sign in first", "Choose Set Up / Sign In… in the menu."); return }
        guard driveMounted else {
            alert("Connect the OpenCloud drive first", "Folders are chosen from the drive. Choose Connect Drive, then try again.")
            return
        }
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.directoryURL = config.mountURL
        panel.prompt = "Keep on This Mac"
        panel.message = "Choose a folder in OpenCloud to keep on this Mac. It will sync both ways."
        NSApp.activate(ignoringOtherApps: true)
        guard panel.runModal() == .OK, let url = panel.url else { return }

        let root = config.mountURL.standardizedFileURL.path + "/"
        let picked = url.standardizedFileURL.path
        guard picked.hasPrefix(root) else {
            alert("That folder isn't in OpenCloud", "Choose a folder inside the OpenCloud drive.")
            return
        }
        // One spelling per folder: no doubled or trailing slashes.
        let rel = picked.dropFirst(root.count).split(separator: "/").joined(separator: "/")
        if let p = KeptFolder.problem(newPath: rel) { alert("Can't keep that folder", p.prefix(1).uppercased() + p.dropFirst() + "."); return }
        if rel.utf8.count > 90 {
            alert("That path is too long", "rclone's sync history can't handle folder paths longer than about 90 characters. Choose a folder higher up, or rename a folder.")
            return
        }
        if let e = addKept(KeptFolder(path: rel, stateName: KeptFolder.makeStateName(for: rel))) {
            alert("Can't keep that folder", e)
        }
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
            lastMessage = "Downloading “\(folder.displayName)”…"
            if !syncing { syncNow() }
        }
        return nil
    }

    /// Keeps a folder from a project space or a share. Read-only folders are refused: a two-way
    /// sync can't send changes back. Returns why it can't, or nil.
    func keepFolder(in space: Space, path: String) async -> String? {
        if let m = settingsBrokenMessage { return m }
        guard !needsSignIn else { return "Sign in first." }
        guard !syncing else { return "A sync is running. Try again when it finishes." }
        let rel = path.split(separator: "/").joined(separator: "/")
        if rel.isEmpty { return "Choose a folder in the space (the whole space can't be kept)." }
        if let p = KeptFolder.problem(newPath: rel) { return p.prefix(1).uppercased() + p.dropFirst() + "." }
        if rel.utf8.count > 90 { return "That path is too long (rclone's limit is about 90 characters)." }
        let server = config.serverURL ?? ""
        do {
            let token = try await Auth.accessToken(serverURL: server)
            guard let url = SpacesAPI.davURL(base: server, space, path: rel) else { return "That path isn't valid." }
            if SpacesAPI.readOnly(try await SpacesAPI.permissions(url, token: token)) {
                return "“\(rel)” in \(space.name) is read-only for you, so it can't be kept here (a kept folder syncs both ways). Open it in the browser instead."
            }
        } catch {
            return "Couldn't check “\(rel)” in \(space.name): \(error.localizedDescription)"
        }
        guard !syncing else { return "A sync is running. Try again when it finishes." }
        return addKept(KeptFolder(path: rel, stateName: KeptFolder.makeStateName(for: rel, spaceID: space.id), space: space.ref))
    }

    /// The folders directly in `path` of a space, listed with rclone.
    func browse(_ space: Space, path: String) async -> Result<[String], Error> {
        let f = KeptFolder(path: path.split(separator: "/").joined(separator: "/"), stateName: "", space: space.ref)
        if f.path.contains("'") { return .failure(AuthError.message("That path isn't valid.")) }
        let r = await Shell.run(config.rclone, ["lsf", "--dirs-only", "--max-depth", "1", config.remote(for: f) + f.path], timeout: 60)
        guard r.status == 0 else { return .failure(AuthError.message("Couldn't list that folder.")) }
        let dirs = r.output.split(separator: "\n").map(String.init).filter { $0.hasSuffix("/") }
            .map { String($0.dropLast()) }.filter { !$0.isEmpty && $0 != Backups.folderName }
        return .success(dirs.sorted { Config.foldKey($0) < Config.foldKey($1) })
    }

    /// Final sync, then moves the local copy to the Trash and stops syncing it.
    func stopKeeping(_ f: KeptFolder) {
        if let m = settingsBrokenMessage { alert("Can't change the settings", m); return }
        guard !syncing else { alert("A sync is running", "Try again when it finishes."); return }
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
                for s in [".initialised", ".needs-repair", ".auto-merge", ".drift-parked", ".filters-upgraded", ".conflicts.json"] { try? fm.removeItem(at: self.stateFile(f, s)) }
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
        _ = exclusive { [weak self] in
            await self?.syncFolder(f, mode: .normal)
        }
    }

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
        let folders = config.kept.map { ($0, status[$0.key]?.state ?? .idle) }
        var notes = notePlanner.plan(folders: folders, signedOut: signedOut && config.isOIDC, autoMerged: autoMerged)
        autoMerged = []
        let found = newConflicts
        newConflicts = []
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

    func showAbout() {
        if aboutWindow == nil { aboutWindow = AboutWindowController(model: self) }
        aboutWindow?.show()
    }

    // MARK: - Pause and updates

    /// Pauses or resumes syncing of kept folders (saved in the settings). The drive keeps
    /// working; a sync that is already running finishes first.
    func setPaused(_ paused: Bool) {
        Task {
            if let e = await updateConfig({ $0.paused = paused }) {
                alert(paused ? "Couldn't pause syncing" : "Couldn't resume syncing", e)
                return
            }
            Log.line(paused ? "syncing paused" : "syncing resumed")
            if !paused { nextSyncAt = Date(); tick() }
        }
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
        let filtersChanged = c.commonFilters != old.commonFilters || c.kept.map(\.extraFilters) != old.kept.map(\.extraFilters)
        if (reconnect || filtersChanged) && syncing { return "A sync is running. Try again when it finishes." }
        do { try c.write() } catch { return "Couldn't save the settings: \(error.localizedDescription)" }

        config = c
        applyEnvironment()
        refreshStatusEntries()
        if c.syncIntervalSeconds != old.syncIntervalSeconds {
            nextSyncAt = min(nextSyncAt, Date().addingTimeInterval(TimeInterval(max(60, c.syncIntervalSeconds))))
        }
        if reconnect {
            restartConnection()
        } else if c.cacheMaxSize != old.cacheMaxSize || c.cacheMaxAge != old.cacheMaxAge || c.volumeName != old.volumeName
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
            unmountRequested = true
            let r = await Shell.run("/usr/sbin/diskutil", ["unmount", config.mountURL.path], timeout: 30)
            if r.status == 0 {
                mountRestartAt = .distantPast
                lastMessage = nil
            } else {
                unmountRequested = false
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
        applyLoadedConfig(message: "Signed in as \(username). Choose Keep a Folder on This Mac… to start.")
    }

    func signOut() {
        guard confirm("Sign out of OpenCloud?",
                      "The drive is disconnected and syncing stops. Your kept folders stay on this Mac. Sign in again to resume.",
                      button: "Sign Out") else { return }
        Task {
            guard await confirmUploads(action: "Sign Out Anyway", later: "They upload after you sign in again.") else { return }
            Auth.logout()
            signedOut = true
            lastMessage = Self.signedOutText
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
    private func restartConnection() {
        let m = mount
        unmountRequested = true
        Task {
            if let m, m.isRunning {
                _ = await Shell.run("/usr/sbin/diskutil", ["unmount", m.arguments?[2] ?? ""], timeout: 30)
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
            lastMessage = "Finishing sync…"
            syncTask?.cancel()
            currentSync?.interrupt()
            for _ in 0..<1200 where currentSync?.isRunning == true {   // up to 2 minutes
                try? await Task.sleep(nanoseconds: 100_000_000)
            }
            if let p = currentSync, p.isRunning {
                p.terminate()
                for _ in 0..<50 where p.isRunning { try? await Task.sleep(nanoseconds: 100_000_000) }
                if p.isRunning { kill(p.processIdentifier, SIGKILL) }
            }
            if let m = mount {
                _ = await Shell.run("/usr/sbin/diskutil", ["unmount", config.mountURL.path], timeout: 20)
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
        if syncing { return activityText(name: activeFolder, index: activeIndex, count: activeCount, files: activeFiles) }
        let bad = config.kept.filter(hasProblem)
        if let first = bad.first, let why = attentionReason(first) {
            var s = "“\(first.displayName)” \(why)"
            if bad.count == 2 { s += " (and 1 more folder)" } else if bad.count > 2 { s += " (and \(bad.count - 1) more folders)" }
            return s
        }
        let times = config.kept.compactMap { status[$0.key]?.lastSuccess }
        if config.paused {
            return times.min().map { "Paused · synced \(agoText($0))" } ?? "Paused"
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
