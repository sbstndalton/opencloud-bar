import AppKit
import CoreServices
import Foundation

/// Live sync (preview, setting liveSync, off by default): each kept folder's copy on this Mac is
/// watched with FSEvents (file-level events, about 1 s latency), and a change syncs that folder
/// soon instead of at the next interval. Same rules as the Go app (cross/watch.go), which has
/// the tests:
/// - a folder syncs once it has been quiet for 5 s, or 60 s after the first change while
///   changes keep coming;
/// - runs started by watching are at least max(30 s, the folder's last run) apart;
/// - events while the app itself syncs the folder (and 2 s after) are its own writes;
/// - paused: changes mark the folder, it syncs on resume; never two runs at once (the folder
///   goes to the same queue as a folder kept while a sync runs);
/// - rclone's temporary files, the backups and conflict copies don't count, nor do folders the
///   excludes leave out by name ("- **/node_modules/**", "- /Old/**").
/// Streams are made again after the Mac wakes from sleep.
@MainActor
final class LiveSync {
    static let quiet: TimeInterval = 5, maxWait: TimeInterval = 60, minGap: TimeInterval = 30, tail: TimeInterval = 2

    private struct Folder {
        var pending = false
        var first = Date.distantPast, last = Date.distantPast
        var own = false
        var ownUntil = Date.distantPast
        var lastLive = Date.distantPast
        var lastDur: TimeInterval = 0
        var stream: FolderEventStream?
    }

    private unowned let model: AppModel
    private var folders: [String: Folder] = [:]
    private var triggered = Set<String>()
    private var timer: Timer?
    private var wakeObserver: NSObjectProtocol?

    init(model: AppModel) {
        self.model = model
        wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.restartStreams() }
        }
    }

    /// Starts and stops watches to match the settings (keep, stop keeping, the toggle, a
    /// changed local folder). A folder that isn't on this Mac isn't watched (tried again later).
    func update() {
        let c = model.config
        var want: [String: (KeptFolder, URL)] = [:]
        if c.liveSync == true, model.configError == nil {
            for f in c.kept { want[f.key] = (f, c.localURL(f).standardizedFileURL) }
        }
        for (key, var lf) in folders {
            if let s = lf.stream, want[key]?.1 != s.root { s.stop(); lf.stream = nil; folders[key] = lf }
            if want[key] == nil { folders[key] = nil }
        }
        for (key, (f, root)) in want {
            var lf = folders[key] ?? Folder()
            if lf.stream == nil {
                var isDir: ObjCBool = false
                if FileManager.default.fileExists(atPath: root.path, isDirectory: &isDir), isDir.boolValue {
                    let (any, top) = Self.excludedDirs(f.extraFilters + c.commonFilters)
                    lf.stream = FolderEventStream(root: root, key: key, any: any, top: top) { [weak self] key, rels in
                        self?.events(key, rels)
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

    private func events(_ key: String, _ rels: [String]) {
        guard var lf = folders[key], rels.contains(where: { !Self.ignored($0) }) else { return }
        let now = Date()
        if lf.own || now < lf.ownUntil { return }   // the app's own writes
        if !lf.pending { lf.pending = true; lf.first = now }
        lf.last = now
        folders[key] = lf
        model.setPending(key, true)
    }

    private func tick() {
        guard !model.config.paused, model.configError == nil, !model.needsSignIn else { return }   // still pending: syncs after resume
        let now = Date()
        var due: [String] = []
        for (key, var lf) in folders where lf.pending && !lf.own {
            let settled = now.timeIntervalSince(lf.last) >= Self.quiet || now.timeIntervalSince(lf.first) >= Self.maxWait
            let gapOK = now.timeIntervalSince(lf.lastLive) >= max(Self.minGap, lf.lastDur)
            guard settled, gapOK else { continue }
            lf.pending = false
            folders[key] = lf
            triggered.insert(key)
            due.append(key)
        }
        if !due.isEmpty { model.queueLiveSync(due) }
    }

    /// The app starts syncing a folder: its events are ignored from now until 2 s after it
    /// ends. Returns the run's trigger for Recent Changes.
    func ownStart(_ key: String) -> String {
        var trigger = "timer"
        if triggered.remove(key) != nil {
            trigger = "local change"
            folders[key]?.lastLive = Date()
        }
        folders[key]?.own = true
        folders[key]?.pending = false
        model.setPending(key, false)
        return trigger
    }

    func ownEnd(_ key: String, began: Date) {
        folders[key]?.own = false
        folders[key]?.ownUntil = Date().addingTimeInterval(Self.tail)
        folders[key]?.lastDur = Date().timeIntervalSince(began)
    }

    // MARK: - What doesn't count

    static func ignored(_ rel: String) -> Bool {
        let base = (rel as NSString).lastPathComponent
        return rel == Backups.folderName || rel.hasPrefix(Backups.folderName + "/")
            || base.hasSuffix(".partial") || base.contains(".~tmp~") || Changes.groups(Changes.reConflict, base) != nil
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

/// One folder's FSEvents stream. Events arrive on a background queue and go to the main actor.
final class FolderEventStream {
    let root: URL
    private var stream: FSEventStreamRef?
    private let box: Box

    final class Box {
        let key: String, rootPath: String
        let any: Set<String>, top: Set<String>
        let deliver: @MainActor (String, [String]) -> Void
        init(key: String, rootPath: String, any: Set<String>, top: Set<String>, deliver: @escaping @MainActor (String, [String]) -> Void) {
            self.key = key; self.rootPath = rootPath; self.any = any; self.top = top; self.deliver = deliver
        }

        /// The path relative to the folder, or nil for one in an excluded folder.
        func relative(_ p: String) -> String? {
            guard p == rootPath || p.hasPrefix(rootPath + "/") else { return nil }
            let rel = p == rootPath ? "" : String(p.dropFirst(rootPath.count + 1))
            let parts = rel.split(separator: "/").map(String.init)
            if parts.contains(where: { any.contains($0) }) { return nil }
            if let first = parts.first, top.contains(first) || (parts.count > 1 && top.contains(first + "/" + parts[1])) { return nil }
            return rel
        }
    }

    init(root: URL, key: String, any: Set<String>, top: Set<String>, deliver: @escaping @MainActor (String, [String]) -> Void) {
        self.root = root
        box = Box(key: key, rootPath: root.resolvingSymlinksInPath().path, any: any, top: top, deliver: deliver)
        var ctx = FSEventStreamContext(version: 0, info: Unmanaged.passUnretained(box).toOpaque(),
                                       retain: nil, release: nil, copyDescription: nil)
        let cb: FSEventStreamCallback = { _, info, n, paths, flags, _ in
            guard let info else { return }
            let box = Unmanaged<FolderEventStream.Box>.fromOpaque(info).takeUnretainedValue()
            let list = unsafeBitCast(paths, to: NSArray.self) as? [String] ?? []
            var rels: [String] = []
            for i in 0..<min(n, list.count) {
                let f = Int(flags[i])
                if f & kFSEventStreamEventFlagHistoryDone != 0 { continue }
                if f & (kFSEventStreamEventFlagMustScanSubDirs | kFSEventStreamEventFlagRootChanged) != 0 { rels.append("."); continue }
                if let r = box.relative(list[i]) { rels.append(r) }
            }
            guard !rels.isEmpty else { return }
            let key = box.key, deliver = box.deliver
            DispatchQueue.main.async { MainActor.assumeIsolated { deliver(key, rels) } }
        }
        let flags = FSEventStreamCreateFlags(kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagUseCFTypes
                                             | kFSEventStreamCreateFlagNoDefer | kFSEventStreamCreateFlagWatchRoot)
        guard let s = FSEventStreamCreate(nil, cb, &ctx, [box.rootPath] as CFArray,
                                          FSEventStreamEventId(kFSEventStreamEventIdSinceNow), 1.0, flags) else { return }
        FSEventStreamSetDispatchQueue(s, DispatchQueue(label: "OpenCloudBar.fsevents"))
        if FSEventStreamStart(s) {
            stream = s
        } else {
            FSEventStreamInvalidate(s)
            FSEventStreamRelease(s)
        }
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
