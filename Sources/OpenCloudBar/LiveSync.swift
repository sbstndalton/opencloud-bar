import AppKit
import CoreServices
import Darwin
import Foundation

/// Live sync (setting liveSync, on by default since 2.0): each kept folder's copy on this Mac is
/// watched with FSEvents (file-level events, about 1 s latency), and a change syncs that folder
/// soon instead of at the next interval. Same rules as the Go app (cross/watch.go), which has
/// the tests:
/// - a folder syncs once it has been quiet for 5 s, or 60 s after the first change while
///   changes keep coming;
/// - runs started by watching are at least max(30 s, the folder's last run) apart;
/// - events from the app's own sync are kept aside, by FSEvents event id (not by when they
///   arrive): those up to the last event id when the run ended. Then the paths the run wrote
///   here (from its parsed output) are taken out; anything left, or too much to tell, is one
///   follow-up sync after the gap;
/// - paused: changes mark the folder, it syncs on resume; never two runs at once (the folder
///   goes to the same queue as a folder kept while a sync runs);
/// - rclone's temporary files, the backups, conflict copies, the excludes' simple names
///   (.DS_Store, ._*, *.tmp…), Office and LibreOffice lock files don't count, nor do folders
///   the excludes leave out by name ("- **/node_modules/**", "- /Old/**").
/// Streams are made again after the Mac wakes from sleep, when the folder's excludes change,
/// and when its root is moved or replaced.
///
/// Changes in OpenCloud (v1.6): every 30 s (± 5 s) each kept folder's root is asked for its
/// ETag (PROPFIND, Depth 0, getetag), which OpenCloud propagates up from any change below. A
/// changed ETag syncs the folder the same way (gap, queue; trigger "server change"). The first
/// check after launch only records it; after each successful sync the ETag is read again as
/// the new baseline (the app's own uploads). Errors back off up to 5 minutes, quietly. No
/// checks while paused, signed out, asleep, or without the browser sign-in; at most 4 at once.
/// The token goes only to the server (no redirects). Same as cross/remote.go, which has the tests.
@MainActor
final class LiveSync {
    nonisolated static let quiet: TimeInterval = 5, maxWait: TimeInterval = 60, minGap: TimeInterval = 30, tail: TimeInterval = 2

    /// The least time between live-triggered runs: a large folder (last run over 2 minutes)
    /// gets max(10 minutes, 2 × that run), so constant changes don't keep the server busy.
    nonisolated static func liveGap(_ lastDur: TimeInterval) -> TimeInterval {
        lastDur > 120 ? max(600, 2 * lastDur) : max(minGap, lastDur)
    }

    /// Live sync covers the folder: on, watched here, and checked in OpenCloud (browser
    /// sign-in). Then the timer is only a safety net (AppModel's timer interval).
    func covers(_ key: String) -> Bool {
        model.config.liveOn && model.config.isOIDC && model.configError == nil && folders[key]?.stream != nil
    }
    nonisolated static let ownCap = 1000, wroteCap = 10_000
    nonisolated static let ignoredNames = [".DS_Store", "Thumbs.db", "desktop.ini", "._*", "*~", "*.tmp",
                               ".sync_*.db", ".sync_*.db-*", "~$*", ".~lock.*#"]

    private struct Folder {
        var pending = false
        var first = Date.distantPast, last = Date.distantPast
        var own = false
        var ownBegan = Date.distantPast
        var ownCheck = false            // the run ended: compare once checkAt passes
        var ownEndId: FSEventStreamEventId = 0
        var checkAt = Date.distantPast
        var ownPaths = Set<String>()
        var ownOver = false
        var ownWrote = Set<String>()
        var ownWroteAll = false
        var local = false, server = false   // what the pending change is
        var lastLive = Date.distantPast
        var lastDur: TimeInterval = 0
        var sig = ""                    // root and filters the stream was made for
        var names: [String] = []
        var stream: FolderEventStream?
    }

    private unowned let model: AppModel
    private var folders: [String: Folder] = [:]
    private var triggered: [String: String] = [:]   // queued by live sync: the run's trigger
    private var timer: Timer?
    private var wakeObserver: NSObjectProtocol?
    private var sleepObserver: NSObjectProtocol?
    private var asleep = false
    private var generation = 0

    init(model: AppModel) {
        self.model = model
        wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.asleep = false; self?.restartStreams() }
        }
        sleepObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.willSleepNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.asleep = true }
        }
    }

    /// Starts and stops watches to match the settings (keep, stop keeping, the toggle, a
    /// changed local folder or excludes). A folder that isn't on this Mac isn't watched (tried
    /// again later).
    func update() {
        let c = model.config
        var want: [String: (URL, [String], String)] = [:]
        if c.liveOn, model.configError == nil {
            for f in c.kept {
                let root = c.localURL(f).standardizedFileURL
                let filters = f.extraFilters + c.commonFilters
                want[f.key] = (root, filters, root.path + "\u{0}" + filters.joined(separator: "\u{0}"))
            }
        }
        triggered = triggered.filter { want[$0.key] != nil }
        remote = remote.filter { want[$0.key] != nil || $0.value.syncing }
        remoteGone = remoteGone.filter { key in !model.config.kept.contains { $0.key == key } }
        for (key, var lf) in folders {
            if let s = lf.stream, want[key]?.2 != lf.sig || !s.stillValid() { s.stop(); lf.stream = nil; folders[key] = lf }
            if want[key] == nil { folders[key] = nil }
        }
        for (key, (root, filters, sig)) in want {
            var lf = folders[key] ?? Folder()
            if lf.stream == nil {
                var isDir: ObjCBool = false
                if FileManager.default.fileExists(atPath: root.path, isDirectory: &isDir), isDir.boolValue {
                    let (any, top) = Self.excludedDirs(filters)
                    generation += 1
                    let gen = generation
                    lf.sig = sig
                    lf.names = Self.ignoredNames + Self.simpleNames(filters)
                    lf.stream = FolderEventStream(root: root, key: key, generation: gen, any: any, top: top) { [weak self] key, gen, items, rootChanged in
                        self?.events(key, gen, items, rootChanged: rootChanged)
                    }
                }
            }
            folders[key] = lf
        }
        if !want.isEmpty, timer == nil {
            var n = 0
            timer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] _ in
                MainActor.assumeIsolated {
                    self?.tick()
                    n += 1
                    if n % 60 == 0 { self?.update() }   // folders that appeared (or came back)
                }
            }
        } else if want.isEmpty {
            timer?.invalidate()
            timer = nil
        }
    }

    private func restartStreams() {
        for (key, var lf) in folders { lf.stream?.stop(); lf.stream = nil; folders[key] = lf }
        update()
    }

    func stopAll() {
        for (key, var lf) in folders { lf.stream?.stop(); lf.stream = nil; folders[key] = lf }
        timer?.invalidate()
        timer = nil
    }

    /// Events from one stream; nil path: events were lost (or the root changed), sync to be sure.
    private func events(_ key: String, _ gen: Int, _ items: [(String?, FSEventStreamEventId)], rootChanged: Bool) {
        guard var lf = folders[key], let s = lf.stream, s.generation == gen else { return }   // a stopped stream's late events
        if rootChanged { s.stop(); lf.stream = nil }
        var real = false, full = false
        for (rel, id) in items {
            if let rel, Self.ignored(rel, lf.names) { continue }
            if lf.own || (lf.ownCheck && id <= lf.ownEndId) {   // maybe the app's own writes
                if let rel, lf.ownPaths.count < Self.ownCap { lf.ownPaths.insert(rel) } else { lf.ownOver = true }
            } else {
                real = true
                if rel == nil { full = true }
            }
        }
        if real {
            let now = Date()
            if !lf.pending { lf.pending = true; lf.first = now }
            lf.local = true
            lf.last = full ? now.addingTimeInterval(-Self.quiet) : now
            model.setPending(key, true)
        }
        folders[key] = lf
        if rootChanged { update() }
    }

    private func tick() {
        let now = Date()
        for (key, var lf) in folders where lf.ownCheck && !lf.own && now >= lf.checkAt {
            lf.ownCheck = false
            let left = lf.ownOver || (lf.ownWroteAll && !lf.ownPaths.isEmpty) || !lf.ownPaths.isSubset(of: lf.ownWrote)
            lf.ownPaths = []; lf.ownOver = false; lf.ownWrote = []; lf.ownWroteAll = false
            if left {   // one follow-up, after the gap from that run's start
                if !lf.pending { lf.pending = true; lf.first = now }
                lf.local = true
                lf.last = now.addingTimeInterval(-Self.quiet)
                if lf.lastLive < lf.ownBegan { lf.lastLive = lf.ownBegan }
                model.setPending(key, true)
            }
            folders[key] = lf
        }
        guard !model.config.paused, model.configError == nil, !model.needsSignIn else { return }   // still pending: syncs after resume
        var due: [String] = []
        for (key, var lf) in folders where lf.pending && !lf.own {
            let settled = now.timeIntervalSince(lf.last) >= Self.quiet || now.timeIntervalSince(lf.first) >= Self.maxWait
            let gapOK = now.timeIntervalSince(lf.lastLive) >= Self.liveGap(lf.lastDur)
            guard settled, gapOK else { continue }
            lf.pending = false
            triggered[key] = lf.server && !lf.local ? "server change" : "local change"
            lf.local = false; lf.server = false
            folders[key] = lf
            due.append(key)
        }
        if !due.isEmpty { model.queueLiveSync(due) }
        remoteTick()
    }

    /// The app starts syncing a folder: its events are kept aside until the run's own writes
    /// are known. Returns the run's trigger for Recent Changes.
    func ownStart(_ key: String) -> String {
        var trigger = "timer"
        if let t = triggered.removeValue(forKey: key) {
            trigger = t
            folders[key]?.lastLive = Date()
        } else if let lf = folders[key], lf.pending, lf.local || lf.server {
            trigger = lf.local ? "local change" : "server change"   // a waiting change: this run takes it
            folders[key]?.lastLive = Date()
        }
        model.dequeue(key)   // this run takes what was queued for it
        if var lf = folders[key] {
            lf.local = false; lf.server = false
            lf.own = true
            lf.ownBegan = Date()
            lf.pending = false
            if !lf.ownCheck { lf.ownPaths = []; lf.ownOver = false; lf.ownWrote = []; lf.ownWroteAll = false }
            lf.ownCheck = false   // a check still waiting: this run takes its events too
            folders[key] = lf
        }
        model.setPending(key, false)
        return trigger
    }

    /// What a run wrote here (and the folders it is in); unknown: it can't be told (a merge).
    func ownWrites(_ key: String, _ paths: [String], unknown: Bool) {
        guard var lf = folders[key] else { return }
        lf.ownWroteAll = lf.ownWroteAll || unknown
        outer: for p in paths {
            var p = p
            while !p.isEmpty, p != ".", !lf.ownWrote.contains(p) {
                if lf.ownWrote.count >= Self.wroteCap { lf.ownWroteAll = true; break outer }
                lf.ownWrote.insert(p)
                guard let i = p.lastIndex(of: "/") else { break }
                p = String(p[..<i])
            }
        }
        folders[key] = lf
    }

    func ownEnd(_ key: String, began: Date) {
        guard var lf = folders[key] else { return }
        lf.own = false
        lf.ownEndId = FSEventsGetCurrentEventId()   // events up to here may be the run's
        lf.ownCheck = true
        lf.checkAt = Date().addingTimeInterval(Self.tail)   // time for them to be delivered
        lf.lastDur = Date().timeIntervalSince(began)
        lf.stream?.flush()
        folders[key] = lf
    }

    // MARK: - Changes in OpenCloud

    static let remoteEvery: TimeInterval = 30, remoteJitter: TimeInterval = 5, remoteMaxBackoff: TimeInterval = 300
    static let remoteParallel = 4

    private struct Remote {
        var etag = ""
        var known = false, loaded = false
        var nextAt = Date.distantPast
        var fails = 0
        var busy = false, pending = false
        var syncing = false   // the app syncs it now (also before live sync knew the folder)
        var gen = 0
    }
    private var remote: [String: Remote] = [:]
    private var remoteGone = Set<String>()   // stopped being kept: no checks, no .server-etag writes
    private var remoteRunning = 0

    private var remoteOn: Bool {
        let c = model.config
        return c.liveOn && model.configError == nil && !model.needsSignIn && !c.paused && c.isOIDC
            && !asleep && !model.quitting
    }

    private struct RemoteJob { let key: String, url: URL, server: String, state: URL, gen: Int, load: Bool, baseline: Bool }

    private func remoteTick() {
        guard remoteOn else { return }
        let now = Date(), c = model.config
        for f in c.kept where remoteRunning < Self.remoteParallel {
            var r = remote[f.key] ?? Remote()
            defer { remote[f.key] = r }
            guard !r.busy, !r.pending, !r.syncing, !remoteGone.contains(f.key), now >= r.nextAt,
                  folders[f.key]?.own != true else { continue }
            guard let url = SharesAPI.davURL(c, f, rel: "") else { r.nextAt = now.addingTimeInterval(Self.remoteMaxBackoff); continue }
            r.busy = true
            remoteRunning += 1
            let job = RemoteJob(key: f.key, url: url, server: c.serverURL ?? "", state: model.stateFile(f, ".server-etag"),
                                gen: r.gen, load: !r.loaded, baseline: false)
            Task { await self.remoteCheck(job) }
        }
    }

    private func remoteCheck(_ j: RemoteJob) async {
        let stored = j.load ? (try? String(contentsOf: j.state, encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "" : ""
        var etag: String?
        if let tok = try? await Auth.accessToken(serverURL: j.server) {
            etag = await RemoteETag.fetch(j.url, token: tok)
        }
        remoteRunning -= 1
        guard var r = remote[j.key] else { return }
        defer { remote[j.key] = r }
        r.busy = false
        let now = Date()
        if j.load, !r.loaded {
            r.loaded = true
            if !stored.isEmpty, !r.known { r.etag = stored; r.known = true }
        }
        guard let etag else {   // network, 401, 5xx, a redirect: back off, quietly
            r.fails += 1
            r.nextAt = now.addingTimeInterval(min(Self.remoteEvery * pow(2, Double(r.fails - 1)), Self.remoteMaxBackoff))
            if j.baseline { r.known = false }
            return
        }
        r.fails = 0
        r.nextAt = now.addingTimeInterval(Self.remoteEvery + Double.random(in: -Self.remoteJitter...Self.remoteJitter))
        guard j.gen == r.gen, !r.syncing else { return }   // a sync started or ended meanwhile
        let changedWhileClosed = j.load && !j.baseline && !stored.isEmpty && etag != stored
        if !changedWhileClosed, j.baseline || j.load || !r.known {   // after a sync, or the first check after launch: record
            if !r.known || r.etag != etag { storeETag(j, etag) }
            r.etag = etag; r.known = true
            return
        }
        guard etag != r.etag || changedWhileClosed else { return }
        r.etag = etag; r.known = true
        storeETag(j, etag)
        var lf = folders[j.key] ?? Folder()
        do {
            if !lf.pending { lf.pending = true; lf.first = now }
            lf.server = true
            lf.last = now.addingTimeInterval(-Self.quiet)
            folders[j.key] = lf
            model.setPending(j.key, true)
        }
    }

    /// Writes the baseline, unless the folder stopped being kept meanwhile.
    private func storeETag(_ j: RemoteJob, _ etag: String) {
        guard remote[j.key] != nil, !remoteGone.contains(j.key), model.config.kept.contains(where: { $0.key == j.key }) else { return }
        try? Data((etag + "\n").utf8).write(to: j.state, options: .atomic)
    }

    /// Stop Keeping: no more checks; the caller removes .server-etag after this.
    func remoteForget(_ key: String) {
        remote[key] = nil
        remoteGone.insert(key)
    }

    func remoteSyncStarted(_ key: String) {
        var r = remote[key] ?? Remote()
        r.gen += 1
        r.syncing = true
        remote[key] = r
    }

    func remoteSyncEnded(_ f: KeptFolder, ok: Bool) {
        guard var r = remote[f.key] ?? (remoteOn ? Remote() : nil) else { return }
        defer { remote[f.key] = r }
        r.gen += 1
        r.syncing = false
        guard ok, remoteOn, !remoteGone.contains(f.key), let url = SharesAPI.davURL(model.config, f, rel: "") else {
            r.known = r.known && ok   // a failed sync: the next check only records
            return
        }
        r.busy = true
        remoteRunning += 1
        let job = RemoteJob(key: f.key, url: url, server: model.config.serverURL ?? "", state: model.stateFile(f, ".server-etag"),
                            gen: r.gen, load: !r.loaded, baseline: true)
        Task { await self.remoteCheck(job) }
    }

    // MARK: - What doesn't count

    nonisolated static func ignored(_ rel: String, _ names: [String] = ignoredNames) -> Bool {
        let base = (rel as NSString).lastPathComponent
        return rel == Backups.folderName || rel.hasPrefix(Backups.folderName + "/")
            || base.hasSuffix(".partial") || base.contains(".~tmp~") || Changes.groups(Changes.reConflict, base) != nil
            || names.contains { fnmatch($0, base, 0) == 0 }
    }

    /// The excludes' simple file names ("- *.bak", "- **/*.log"): a pattern without folders.
    static func simpleNames(_ filters: [String]) -> [String] {
        filters.compactMap { raw in
            let l = raw.trimmingCharacters(in: .whitespaces)
            guard l.hasPrefix("- ") else { return nil }
            var p = String(l.dropFirst(2))
            if p.hasPrefix("**/") { p = String(p.dropFirst(3)) }
            guard !p.isEmpty, !p.contains("**"), p.rangeOfCharacter(from: CharacterSet(charactersIn: "/{}\\")) == nil else { return nil }
            return p
        }
    }

    /// The cheap part of the excludes: whole folders by name at any depth, and from the top.
    static func excludedDirs(_ filters: [String]) -> (Set<String>, Set<String>) {
        var any = Set<String>(), top = Set<String>()
        let glob = CharacterSet(charactersIn: "/*?[]{}\\")
        for raw in filters {
            let l = raw.trimmingCharacters(in: .whitespaces)
            guard l.hasPrefix("- "), l.hasSuffix("/**") else { continue }
            let p = String(l.dropFirst(2).dropLast(3))
            if p.hasPrefix("**/"), String(p.dropFirst(3)).rangeOfCharacter(from: glob) == nil {
                any.insert(String(p.dropFirst(3)))
            } else if p.hasPrefix("/") {
                if let s = SubfolderRules.parse(l) { top.insert(s) }
            } else if p.rangeOfCharacter(from: glob) == nil {
                any.insert(p)
            }
        }
        return (any, top)
    }
}

/// One folder's FSEvents stream. Events arrive on its own queue and go to the main actor,
/// tagged with the stream's generation so a stopped stream's late deliveries are ignored.
final class FolderEventStream {
    let root: URL
    let generation: Int
    private var stream: FSEventStreamRef?
    private let box: Box
    private let queue: DispatchQueue
    private let rootID: (dev: dev_t, ino: ino_t)?

    final class Box: @unchecked Sendable {
        let key: String, rootPath: String, generation: Int
        let any: Set<String>, top: Set<String>
        let deliver: @MainActor (String, Int, [(String?, FSEventStreamEventId)], Bool) -> Void
        init(key: String, rootPath: String, generation: Int, any: Set<String>, top: Set<String>,
             deliver: @escaping @MainActor (String, Int, [(String?, FSEventStreamEventId)], Bool) -> Void) {
            self.key = key; self.rootPath = rootPath; self.generation = generation
            self.any = any; self.top = top; self.deliver = deliver
        }

        /// The path relative to the folder, "" outside it, or nil for one in an excluded folder.
        func relative(_ p: String) -> String? {
            guard p == rootPath || p.hasPrefix(rootPath + "/") else { return nil }
            let rel = p == rootPath ? "" : String(p.dropFirst(rootPath.count + 1))
            let parts = rel.split(separator: "/").map(String.init)
            if parts.contains(where: { any.contains($0) }) { return nil }
            if let first = parts.first, top.contains(first) || (parts.count > 1 && top.contains(first + "/" + parts[1])) { return nil }
            return rel
        }
    }

    init(root: URL, key: String, generation: Int, any: Set<String>, top: Set<String>,
         deliver: @escaping @MainActor (String, Int, [(String?, FSEventStreamEventId)], Bool) -> Void) {
        self.root = root
        self.generation = generation
        let resolved = root.resolvingSymlinksInPath().path
        rootID = Self.identity(resolved)
        box = Box(key: key, rootPath: resolved, generation: generation, any: any, top: top, deliver: deliver)
        queue = DispatchQueue(label: "OpenCloudBar.fsevents.\(generation)")
        var ctx = FSEventStreamContext(version: 0, info: Unmanaged.passUnretained(box).toOpaque(),
                                       retain: nil, release: nil, copyDescription: nil)
        let cb: FSEventStreamCallback = { _, info, n, paths, flags, ids in
            guard let info else { return }
            let box = Unmanaged<FolderEventStream.Box>.fromOpaque(info).takeUnretainedValue()
            let list = unsafeBitCast(paths, to: NSArray.self) as? [String] ?? []
            var items: [(String?, FSEventStreamEventId)] = []
            var rootChanged = false
            for i in 0..<min(n, list.count) {
                let f = Int(flags[i])
                if f & kFSEventStreamEventFlagHistoryDone != 0 { continue }
                if f & kFSEventStreamEventFlagRootChanged != 0 { rootChanged = true; items.append((nil, ids[i])); continue }
                if f & (kFSEventStreamEventFlagMustScanSubDirs | kFSEventStreamEventFlagUserDropped
                        | kFSEventStreamEventFlagKernelDropped) != 0 { items.append((nil, ids[i])); continue }
                if let r = box.relative(list[i]), !r.isEmpty { items.append((r, ids[i])) }
            }
            guard !items.isEmpty else { return }
            let key = box.key, gen = box.generation, deliver = box.deliver, rc = rootChanged
            DispatchQueue.main.async { MainActor.assumeIsolated { deliver(key, gen, items, rc) } }
        }
        let flags = FSEventStreamCreateFlags(kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagUseCFTypes
                                             | kFSEventStreamCreateFlagNoDefer | kFSEventStreamCreateFlagWatchRoot)
        guard let s = FSEventStreamCreate(nil, cb, &ctx, [box.rootPath] as CFArray,
                                          FSEventStreamEventId(kFSEventStreamEventIdSinceNow), 1.0, flags) else { return }
        FSEventStreamSetDispatchQueue(s, queue)
        if FSEventStreamStart(s) {
            stream = s
        } else {
            FSEventStreamInvalidate(s)
            FSEventStreamRelease(s)
        }
    }

    private static func identity(_ path: String) -> (dev: dev_t, ino: ino_t)? {
        var st = stat()
        return stat(path, &st) == 0 ? (st.st_dev, st.st_ino) : nil
    }

    /// The configured folder still leads to the folder being watched.
    func stillValid() -> Bool {
        guard stream != nil, root.resolvingSymlinksInPath().path == box.rootPath,
              let now = Self.identity(box.rootPath), let was = rootID else { return false }
        return now.dev == was.dev && now.ino == was.ino
    }

    /// Delivers what FSEvents holds now (the end of a sync).
    func flush() {
        if let s = stream { FSEventStreamFlushAsync(s) }
    }

    func stop() {
        guard let s = stream else { return }
        stream = nil
        FSEventStreamStop(s)
        FSEventStreamInvalidate(s)
        FSEventStreamRelease(s)
    }

    deinit { stop() }
}

/// The ETag request: no redirects (the token stays with the server), no cookies, no cache.
enum RemoteETag {
    private final class NoRedirect: NSObject, URLSessionTaskDelegate {
        func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                        newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
            completionHandler(nil)
        }
    }
    private static let session: URLSession = {
        let c = URLSessionConfiguration.ephemeral
        c.httpCookieStorage = nil
        c.urlCache = nil
        c.timeoutIntervalForRequest = 20
        return URLSession(configuration: c, delegate: NoRedirect(), delegateQueue: nil)
    }()

    /// The resource's ETag, or nil for any failure.
    static func fetch(_ url: URL, token: String) async -> String? {
        var req = URLRequest(url: url)
        req.httpMethod = "PROPFIND"
        req.timeoutInterval = 20
        req.setValue("0", forHTTPHeaderField: "Depth")
        req.setValue("application/xml", forHTTPHeaderField: "Content-Type")
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.httpBody = Data(#"<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>"#.utf8)
        guard let (data, resp) = try? await session.data(for: req), (resp as? HTTPURLResponse)?.statusCode == 207 else { return nil }
        let text = String(decoding: data.prefix(1 << 20), as: UTF8.self)
        guard let r = text.range(of: #"<([A-Za-z0-9]+:)?getetag>[^<]+</"#, options: .regularExpression) else { return nil }
        let tag = text[r]
        guard let open = tag.firstIndex(of: ">"), let close = tag.range(of: "</", options: .backwards) else { return nil }
        let etag = String(tag[tag.index(after: open)..<close.lowerBound]).trimmingCharacters(in: .whitespaces)
        return etag.isEmpty ? nil : etag
    }
}
