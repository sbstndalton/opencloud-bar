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
        /// bisync refused because more than half the files would be deleted.
        case bigDeletion
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
    @Published private(set) var opensAtLogin = SMAppService.mainApp.status == .enabled

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
        refreshStatusEntries()
        startTicker()
    }

    private func startTicker() {
        guard ticker == nil else { return }
        ticker = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.tick() }
        }
        tick()
    }

    private func refreshStatusEntries() {
        for f in config.kept where status[f.path] == nil {
            var s = FolderStatus()
            if let reason = needsRepairReason(f) { s.state = .needsRepair(reason) }
            status[f.path] = s
        }
        let paths = Set(config.kept.map(\.path))
        for k in status.keys where !paths.contains(k) { status[k] = nil }
    }

    // MARK: - Supervisor

    private func tick() {
        guard !quitting, config.isConfigured, configError == nil else { return }
        let portOpen = Shell.portOpen(config.localPort)
        let ours = FileManager.default.fileExists(atPath: controlPath)
        tunnelUp = portOpen && ours
        driveMounted = Shell.isMounted(config.mountURL)

        if !tunnelUp, Date() >= tunnelRestartAt {
            if portOpen && !ours {
                // Something else holds the port. Don't send credentials to it.
                lastMessage = "Port \(config.localPort) is in use by another program; waiting for it to close."
                tunnelRestartAt = Date().addingTimeInterval(30)
            } else {
                startTunnel()
            }
        }
        if driveWanted, tunnelUp, mount == nil, Date() >= mountRestartAt { startMount() }
        if !syncing, tunnelUp, Date() >= nextSyncAt, !config.kept.isEmpty { syncNow() }
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
                    "--rc", "--rc-addr", "127.0.0.1:\(config.rcPort)", "--rc-no-auth",
                    "--log-level", "NOTICE"]
        unmountRequested = false
        mountStartedAt = Date()
        do {
            mount = try Shell.launch(config.rclone, args, log: Log.url("drive.log")) { [weak self] code in
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
        let r = await Shell.run(config.rclone, ["rc", "vfs/stats", "--url", "http://127.0.0.1:\(config.rcPort)/"], timeout: 10)
        guard r.status == 0, let data = r.output.data(using: .utf8),
              let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let disk = json["diskCache"] as? [String: Any] else { return nil }
        return ((disk["uploadsInProgress"] as? Int) ?? 0) + ((disk["uploadsQueued"] as? Int) ?? 0)
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
        status[f.path]?.state = .needsRepair(reason)
        Log.line("\(f.path): needs repair: \(reason)")
    }

    func syncNow() {
        guard !syncing, config.isConfigured, configError == nil else { return }
        nextSyncAt = Date().addingTimeInterval(TimeInterval(max(60, config.syncIntervalSeconds)))
        syncing = true
        let folders = config.kept
        syncTask = Task { [weak self] in
            guard let self else { return }
            if await self.serverTooBusy() {
                for f in folders { self.status[f.path]?.state = .skipped("server busy") }
            } else {
                for f in folders where !Task.isCancelled && !self.quitting {
                    // skip folders removed while this run was waiting
                    guard self.config.kept.contains(where: { $0.path == f.path }) else { continue }
                    if case .needsRepair = self.status[f.path]?.state { continue }
                    if case .bigDeletion = self.status[f.path]?.state { continue }
                    _ = await self.syncFolder(f, mode: .normal)
                }
            }
            self.syncing = false
        }
    }

    /// Returns true if the server is short on memory (or unreachable) and we should not add load.
    private func serverTooBusy() async -> Bool {
        guard config.minServerAvailableMiB > 0 else { return false }
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
        for _ in 0..<20 {
            if FileManager.default.fileExists(atPath: controlPath) && Shell.portOpen(config.localPort) { return true }
            try? await Task.sleep(nanoseconds: 1_000_000_000)
        }
        return false
    }

    enum SyncMode { case normal, repair, allowBigDeletion }

    /// Runs one bisync for a folder. Returns true on success. Never creates, recreates or
    /// resyncs a folder unless this is its first sync or the user chose Repair.
    @discardableResult
    private func syncFolder(_ f: KeptFolder, mode: SyncMode) async -> Bool {
        let fm = FileManager.default
        status[f.path]?.state = .syncing
        let marker = stateFile(f, ".initialised")
        let initialised = fm.fileExists(atPath: marker.path)
        let first = !initialised || mode == .repair

        if mode == .normal, let reason = needsRepairReason(f) {
            status[f.path]?.state = .needsRepair(reason); return false
        }
        guard fm.fileExists(atPath: config.localRootURL.path) || !initialised else {
            status[f.path]?.state = .failed("\(config.localRoot) is missing"); return false
        }
        let local = config.localRootURL.appendingPathComponent(f.path)
        if initialised && !fm.fileExists(atPath: local.path) {
            status[f.path]?.state = .failed("folder is missing on this Mac"); return false
        }
        // Sync history is only valid for the server and account it was made with.
        let work = workDir(f)
        let serverFile = work.appendingPathComponent("server.txt")
        if initialised, mode != .repair,
           let saved = try? String(contentsOf: serverFile, encoding: .utf8),
           saved != config.serverIdentity {
            markNeedsRepair(f, "server or account settings changed"); return false
        }
        guard await waitForTunnel() else {
            status[f.path]?.state = .failed("no connection to server"); return false
        }

        if first { try? fm.createDirectory(at: local, withIntermediateDirectories: true) }
        try? fm.createDirectory(at: work, withIntermediateDirectories: true)
        if !fm.fileExists(atPath: serverFile.path) {
            try? config.serverIdentity.write(to: serverFile, atomically: true, encoding: .utf8)
        }
        let filters = stateFile(f, "").deletingLastPathComponent().appendingPathComponent("filters-\(f.stateName).txt")
        let filterText = (f.extraFilters + config.commonFilters).joined(separator: "\n") + "\n"
        // only rewrite when changed: bisync insists on a resync after a filters change
        if (try? String(contentsOf: filters, encoding: .utf8)) != filterText {
            try? filterText.write(to: filters, atomically: true, encoding: .utf8)
        }

        var args = ["bisync", local.path, config.remoteSpec + f.path,
                    "--workdir", work.path, "--filters-file", filters.path,
                    "--create-empty-src-dirs", "--compare", "size,modtime",
                    "--resilient", "--recover", "--max-lock", "10m",
                    "--conflict-resolve", "newer", "--conflict-loser", "num",
                    "--transfers", "4", "--checkers", "6", "--log-level", "NOTICE"]
        if first { args += ["--resync-mode", "newer", "--check-sync=false"] }
        if mode == .allowBigDeletion { args += ["--force"] }

        let log = Log.url("sync-\(f.stateName).log")
        Log.append("\n=== \(Date()) \(first ? "merge (resync)" : "sync") of \(f.path)\n", to: log)
        let r = await runTracked(config.rclone, args, log: log)

        if r.status == 0 {
            fm.createFile(atPath: marker.path, contents: nil)
            try? fm.removeItem(at: stateFile(f, ".needs-repair"))
            try? config.serverIdentity.write(to: serverFile, atomically: true, encoding: .utf8)
            status[f.path]?.state = .ok
            status[f.path]?.lastSuccess = Date()
            return true
        }
        if r.output.contains("Must run --resync") && !r.output.contains("retryable without --resync") {
            markNeedsRepair(f, "sync history is out of date")
        } else if r.output.contains("Safety abort") || r.output.contains("too many deletes") {
            status[f.path]?.state = .bigDeletion
        } else {
            let reason = r.status == 124 ? "timed out" : "rclone exit \(r.status); will retry"
            status[f.path]?.state = .failed(reason)
        }
        return false
    }

    /// Runs rclone, remembering the process so Quit can stop it cleanly; stops it after syncTimeout.
    private func runTracked(_ exe: String, _ args: [String], log: URL) async -> Shell.Result {
        let timedOut = Flag()
        let result: Shell.Result = await withCheckedContinuation { cont in
            let p = Process()
            p.executableURL = URL(fileURLWithPath: exe)
            p.arguments = args
            p.environment = Shell.environment()
            let pipe = Pipe()
            p.standardOutput = pipe
            p.standardError = pipe
            let buffer = OutputBuffer()
            pipe.fileHandleForReading.readabilityHandler = { h in
                let d = h.availableData
                if !d.isEmpty { buffer.append(d) }
            }
            p.terminationHandler = { proc in
                pipe.fileHandleForReading.readabilityHandler = nil
                buffer.append(pipe.fileHandleForReading.readDataToEndOfFile())
                Log.append(buffer.string, to: log)
                cont.resume(returning: Shell.Result(status: timedOut.value ? 124 : proc.terminationStatus,
                                                    output: buffer.string))
            }
            do {
                try p.run()
                currentSync = p
                DispatchQueue.global().asyncAfter(deadline: .now() + syncTimeout) {
                    if p.isRunning { timedOut.set(); p.interrupt() }
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
                      "This merges this Mac's copy with OpenCloud. For each file the newer copy wins, and nothing is deleted. Files deleted since the last good sync will come back, and an edit made on the other side may be replaced by a newer one.",
                      button: "Repair") else { return }
        _ = exclusive { [weak self] in
            guard let self else { return }
            try? FileManager.default.removeItem(at: self.stateFile(f, ".needs-repair"))
            await self.syncFolder(f, mode: .repair)
        }
    }

    func allowBigDeletion(_ f: KeptFolder) {
        guard confirm("Allow a large deletion in “\(f.displayName)”?",
                      "More than half of this folder's files were deleted on one side. If that was on purpose, the deletion will be applied to the other side too. If not, choose Cancel and restore the files first.",
                      button: "Apply Deletion") else { return }
        _ = exclusive { [weak self] in
            await self?.syncFolder(f, mode: .allowBigDeletion)
        }
    }

    // MARK: - Adding and removing kept folders

    /// Lets the user pick a folder in the on-demand drive and keeps it on this Mac from now on.
    func keepFolder() {
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
        let rel = String(picked.dropFirst(root.count))
        if let p = KeptFolder.problem(with: rel) { alert("Can't keep that folder", p.prefix(1).uppercased() + p.dropFirst() + "."); return }
        if rel.utf8.count > 90 {
            alert("That path is too long", "rclone's sync history can't handle folder paths longer than about 90 characters. Choose a folder higher up, or rename a folder.")
            return
        }
        let key = Config.foldKey(rel)
        if let clash = config.kept.first(where: {
            let k = Config.foldKey($0.path)
            return key == k || key.hasPrefix(k + "/") || k.hasPrefix(key + "/")
        }) {
            alert("Already covered", "“\(rel)” overlaps “\(clash.path)”, which is already kept on this Mac.")
            return
        }
        var folder = KeptFolder(path: rel, stateName: KeptFolder.makeStateName(for: rel))
        while config.kept.contains(where: { $0.stateName == folder.stateName }) { folder.stateName += "x" }
        config.kept.append(folder)
        config.save()
        refreshStatusEntries()
        lastMessage = "Downloading “\(folder.displayName)”…"
        if !syncing { syncNow() }
    }

    /// Final sync, then moves the local copy to the Trash and stops syncing it.
    func stopKeeping(_ f: KeptFolder) {
        guard !syncing else { alert("A sync is running", "Try again when it finishes."); return }
        Task {
            let unsynced = await countUnsyncedFiles(f)
            var text = "Your changes are synced to OpenCloud first. Then the copy on this Mac is moved to the Trash. The folder stays in OpenCloud and in the OpenCloud drive."
            if let n = unsynced, n > 0 {
                text += "\n\n\(n) file(s) in it are never synced (for example build folders, node_modules, or folders excluded in Settings). They exist only on this Mac and will go to the Trash with it."
            }
            guard confirm("Stop keeping “\(f.displayName)” on this Mac?", text, button: "Stop Keeping") else { return }
            _ = exclusive { [weak self] in
                guard let self else { return }
                guard await self.syncFolder(f, mode: .normal) else {
                    self.alert("Couldn't sync “\(f.displayName)”", "Nothing was removed. Check the sync log and try again.")
                    return
                }
                let fm = FileManager.default
                let local = self.config.localRootURL.appendingPathComponent(f.path)
                do { try fm.trashItem(at: local, resultingItemURL: nil) } catch {
                    self.alert("Couldn't move it to the Trash", error.localizedDescription)
                    return
                }
                try? fm.removeItem(at: self.workDir(f))
                for s in [".initialised", ".needs-repair"] { try? fm.removeItem(at: self.stateFile(f, s)) }
                try? fm.removeItem(at: self.config.stateURL.appendingPathComponent("filters-\(f.stateName).txt"))
                self.config.kept.removeAll { $0.path == f.path }
                self.config.save()
                self.refreshStatusEntries()
                self.lastMessage = "“\(f.displayName)” is now only in OpenCloud"
            }
        }
    }

    /// Number of files in a kept folder that the filters exclude from syncing (nil if unknown).
    private func countUnsyncedFiles(_ f: KeptFolder) async -> Int? {
        let local = config.localRootURL.appendingPathComponent(f.path)
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
        NSWorkspace.shared.open(config.localRootURL.appendingPathComponent(f.path))
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

    func reloadConfig() {
        guard !syncing else { alert("A sync is running", "Reload when it finishes."); return }
        let (c, err) = Config.load()
        configError = err
        guard err == nil else { return }
        let reconnect = c.connectionKey != config.connectionKey
        config = c
        refreshStatusEntries()
        lastMessage = "Settings reloaded"
        if reconnect { restartConnection() }
        startTicker()
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
            if let n = await pendingUploads(), n > 0,
               !confirm("Files are still uploading",
                        "\(n) file(s) written to the OpenCloud drive haven't finished uploading. They will upload the next time OpenCloud Bar runs. Quit anyway?",
                        button: "Quit Anyway") {
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
            currentSync?.terminate()
            if let m = mount {
                _ = await Shell.run("/usr/sbin/diskutil", ["unmount", config.mountURL.path], timeout: 20)
                for _ in 0..<50 where m.isRunning { try? await Task.sleep(nanoseconds: 100_000_000) }
                if m.isRunning { m.terminate() }
            }
            await stopTunnel()
            NSApp.reply(toApplicationShouldTerminate: true)
        }
    }

    // MARK: - Dialogs

    private func alert(_ title: String, _ text: String) {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        NSApp.activate(ignoringOtherApps: true)
        a.runModal()
    }

    private func confirm(_ title: String, _ text: String, button: String) -> Bool {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        a.addButton(withTitle: button)
        a.addButton(withTitle: "Cancel")
        NSApp.activate(ignoringOtherApps: true)
        return a.runModal() == .alertFirstButtonReturn
    }

    // MARK: - Summary for the menu

    private func hasProblem(_ f: KeptFolder) -> Bool {
        switch status[f.path]?.state {
        case .failed, .needsRepair, .bigDeletion: return true
        default: return false
        }
    }

    var summary: String {
        if let e = configError { return e }
        if !config.isConfigured { return "Not set up: open Advanced ▸ Edit Settings File" }
        if syncing { return "Syncing…" }
        if let bad = config.kept.first(where: hasProblem) { return "“\(bad.displayName)” needs attention" }
        let times = config.kept.compactMap { status[$0.path]?.lastSuccess }
        if let oldest = times.min(), times.count == config.kept.count {
            return "Up to date · synced \(Self.relative(oldest))"
        }
        if !tunnelUp { return "Connecting to server…" }
        return config.kept.isEmpty ? "Nothing kept on this Mac" : "Waiting for first sync"
    }

    var symbol: String {
        if configError != nil || !config.isConfigured { return "exclamationmark.icloud" }
        if syncing { return "arrow.triangle.2.circlepath.icloud" }
        if config.kept.contains(where: hasProblem) { return "exclamationmark.icloud" }
        if !tunnelUp { return "icloud.slash" }
        return "icloud"
    }

    static func relative(_ d: Date) -> String {
        let f = RelativeDateTimeFormatter()
        f.unitsStyle = .short
        return f.localizedString(for: d, relativeTo: Date())
    }
}
