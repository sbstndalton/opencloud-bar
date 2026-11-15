import Foundation

/// How a bisync run ended, read from rclone's output. Same rules as classifyBisync in the
/// Go app (cross/recovery.go), which has the tests.
struct BisyncResult: Equatable {
    enum Outcome: Equatable {
        /// quit, sleep or a timeout: --recover picks up on the next run
        case interrupted
        /// the listings say the two sides differ ("out of sync"): a merge fixes it
        case drift
        /// sync history unusable for another reason (listings lost)
        case needsResync
        /// the filters file changed since the last sync: rclone wants a resync
        case filtersChanged
        /// safety abort: too many deletes, or all files changed on one side
        case bigChange
        case signedOut
        /// anything else: retried on the next run
        case failed
    }
    var outcome: Outcome
    /// drift: the first file whose listings differ
    var path: String?
    /// big change: "Path1" (this Mac) or "Path2" (OpenCloud), if rclone said
    var side: String?
    /// interrupted because it ran too long (not quit or sleep)
    var timeout = false

    /// Order matters: after drift, --recover falls back to old listings that disagree the same
    /// way and rclone then says "retryable without --resync", so drift is checked first.
    static func classify(_ out: String, timedOut: Bool) -> BisyncResult {
        if out.contains(Auth.signedOutText) { return .init(outcome: .signedOut) }
        // rclone's own words on SIGINT/SIGTERM; file names are elsewhere on the line.
        if timedOut || out.contains("Signal received: ") || out.contains("Attempting to gracefully shutdown")
            || out.contains("Bisync interrupted") || out.contains("context canceled") {
            return .init(outcome: .interrupted, timeout: timedOut)
        }
        if out.contains("out of sync") || out.contains("not equal in listing") {
            var r = BisyncResult(outcome: .drift)
            for line in out.split(separator: "\n") where line.contains("not equal in listing.") {
                // The path follows the first " - " after Path2's value (paths may contain " - ").
                if let p2 = line.range(of: "Path2: "),
                   let range = line.range(of: " - ", range: p2.upperBound..<line.endIndex) {
                    let p = line[range.upperBound...].trimmingCharacters(in: .whitespaces)
                    if !p.isEmpty { r.path = p; break }
                }
            }
            return r
        }
        if out.contains("Safety abort") || out.contains("too many deletes") || out.contains("all files were changed") {
            var r = BisyncResult(outcome: .bigChange)
            if let range = out.range(of: "Safety abort") {
                let line = out[range.lowerBound...].prefix { $0 != "\n" }
                if line.contains("Path2") { r.side = "Path2" } else if line.contains("Path1") { r.side = "Path1" }
            }
            return r
        }
        // rclone also says "retryable" here (--resilient), but retrying would fail forever.
        if out.contains("filters file has changed") { return .init(outcome: .filtersChanged) }
        if out.contains("retryable without --resync") { return .init(outcome: .failed) }
        if out.contains("Must run --resync") || out.contains("--resync to recover") { return .init(outcome: .needsResync) }
        return .init(outcome: .failed)
    }
}

/// One automatic merge per folder per 24 hours, remembered on disk so a restart doesn't reset it.
enum AutoMerge {
    static let window: TimeInterval = 24 * 3600

    static func allowed(_ marker: URL, now: Date = Date()) -> Bool {
        guard let text = try? String(contentsOf: marker, encoding: .utf8),
              let t = ISO8601DateFormatter().date(from: text.trimmingCharacters(in: .whitespacesAndNewlines))
        else { return true }
        // A time far in the future (clock changed) doesn't block forever.
        return now.timeIntervalSince(t) >= window || t.timeIntervalSince(now) > window
    }

    /// Throws when the record can't be written: then there is no automatic merge, or the
    /// once-a-day limit wouldn't hold.
    static func record(_ marker: URL, now: Date = Date()) throws {
        try ISO8601DateFormatter().string(from: now).write(to: marker, atomically: true, encoding: .utf8)
    }

    /// The needs-repair text when the sides stay out of sync; `note` says what to do.
    static func driftReason(_ path: String?, _ note: String) -> String {
        let what = path.map { "“\(($0 as NSString).lastPathComponent)” keeps changing on both sides" }
            ?? "files keep changing on both sides"
        return "\(what); \(note)"
    }
}

// MARK: - Merge backups and emptied folders

/// Merges (first sync, Repair, automatic merge) keep every copy they overwrite: the older copy
/// goes to a dated folder, on this Mac under the state folder and in OpenCloud under
/// `.opencloud-bar-backups` at the top of the drive. Same layout as the Windows/Linux app.
/// Backups on this Mac are removed after 30 days.
enum Backups {
    static let folderName = ".opencloud-bar-backups"
    static let filter = "- \(folderName)/**"
    static let keep: TimeInterval = 30 * 24 * 3600

    private static func formatter() -> DateFormatter {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = TimeZone(identifier: "UTC")
        f.dateFormat = "yyyyMMdd'T'HHmmss'Z'"
        return f
    }

    static func stamp(_ d: Date = Date()) -> String { formatter().string(from: d) }

    static func localDir(_ stateURL: URL, _ stateName: String, _ stamp: String) -> URL {
        stateURL.appendingPathComponent("backups/\(stateName)/\(stamp)")
    }

    /// On the folder's own remote, outside the folder (rclone requires both).
    static func remoteDir(_ remoteSpec: String, _ stateName: String, _ stamp: String) -> String {
        remoteSpec + "\(folderName)/\(stateName)/\(stamp)"
    }

    static func args(_ stateURL: URL, _ remoteSpec: String, _ stateName: String, _ stamp: String) -> [String] {
        ["--backup-dir1", localDir(stateURL, stateName, stamp).path,
         "--backup-dir2", remoteDir(remoteSpec, stateName, stamp)]
    }

    /// Removes this Mac's backups of a folder older than 30 days (by the date in their name).
    /// True when two filters files differ only in the app's own line (new in 1.0): the user's
    /// excludes are the same, so an upgrade merges once by itself instead of asking for Repair.
    static func onlyAppFilterChanged(_ old: String, _ new: String) -> Bool {
        func strip(_ s: String) -> String {
            s.components(separatedBy: "\n").filter { $0.trimmingCharacters(in: .whitespaces) != filter }.joined(separator: "\n")
        }
        return old != new && strip(old) == strip(new)
    }

    static func prune(_ stateURL: URL, _ stateName: String, now: Date = Date()) {
        let dir = stateURL.appendingPathComponent("backups/\(stateName)")
        let fm = FileManager.default
        let f = formatter()
        for name in (try? fm.contentsOfDirectory(atPath: dir.path)) ?? [] {
            if let t = f.date(from: name), now.timeIntervalSince(t) > keep {
                try? fm.removeItem(at: dir.appendingPathComponent(name))
            }
        }
    }
}

/// True when the folder doesn't exist or holds no files (empty folders and .DS_Store don't
/// count). Stops at the first real file.
func localMissingOrEmpty(_ dir: URL) -> Bool {
    let fm = FileManager.default
    var isDir: ObjCBool = false
    guard fm.fileExists(atPath: dir.path, isDirectory: &isDir), isDir.boolValue else { return true }
    guard let e = fm.enumerator(at: dir, includingPropertiesForKeys: [.isDirectoryKey], options: [],
                                errorHandler: { _, _ in true }) else { return true }
    for case let u as URL in e {
        if u.lastPathComponent == ".DS_Store" { continue }
        if (try? u.resourceValues(forKeys: [.isDirectoryKey]))?.isDirectory == false { return false }
    }
    return true
}

/// "just now", "5 min ago", "3 h ago", "2 days ago": the same words as the Windows/Linux app.
func agoText(_ d: Date, now: Date = Date()) -> String {
    let s = now.timeIntervalSince(d)
    if s < 60 { return "just now" }
    if s < 3600 { return "\(Int(s / 60)) min ago" }
    if s < 48 * 3600 { return "\(Int(s / 3600)) h ago" }
    return "\(Int(s / 86400)) days ago"
}

// MARK: - Updates

/// major.minor.patch with an optional pre-release ("1.2.3-rc.1"). Same rules as the Go app.
struct SemVer: Comparable {
    var n: [Int]
    var pre: String

    init?(_ s: String) {
        var t = s.trimmingCharacters(in: .whitespaces)
        if t.hasPrefix("v") { t.removeFirst() }
        if let plus = t.firstIndex(of: "+") { t = String(t[..<plus]) }
        var pre = ""
        if let dash = t.firstIndex(of: "-") { pre = String(t[t.index(after: dash)...]); t = String(t[..<dash]) }
        let parts = t.split(separator: ".", omittingEmptySubsequences: false)
        guard (2...3).contains(parts.count) else { return nil }
        var n: [Int] = []
        for p in parts {
            guard let v = Int(p), v >= 0, p.allSatisfy(\.isNumber) else { return nil }
            n.append(v)
        }
        while n.count < 3 { n.append(0) }
        self.n = n
        self.pre = pre
    }

    static func < (a: SemVer, b: SemVer) -> Bool {
        if a.n != b.n { return a.n.lexicographicallyPrecedes(b.n) }
        if a.pre == b.pre { return false }
        if a.pre.isEmpty { return false }
        if b.pre.isEmpty { return true }
        return a.pre < b.pre
    }

    /// Development builds (0.0.0, "-dev") are never offered updates.
    static func isDevBuild(_ v: String) -> Bool {
        guard let s = SemVer(v) else { return true }
        return s.pre.contains("dev") || s.n == [0, 0, 0]
    }

    static func updateAvailable(latest: String, running: String) -> Bool {
        guard !isDevBuild(running), let l = SemVer(latest), let r = SemVer(running) else { return false }
        return r < l
    }
}

struct UpdateState: Codable, Equatable {
    var latest: String?
    var url: String?
    var checkedAt: Date?
    var error: String?
    var checking = false

    var available: Bool { latest.map { SemVer.updateAvailable(latest: $0, running: Updates.running) } ?? false }
}

enum Updates {
    static let api = URL(string: "https://api.github.com/repos/sbstndalton/opencloud-bar/releases/latest")!
    static let pagePrefix = "https://github.com/sbstndalton/opencloud-bar/releases/"
    static let interval: TimeInterval = 24 * 3600
    private static let defaultsKey = "updateCheck"

    static var running: String {
        (Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String) ?? "0.0.0-dev"
    }
    static var isDevBuild: Bool { SemVer.isDevBuild(running) }

    static func load() -> UpdateState {
        guard let d = UserDefaults.standard.data(forKey: defaultsKey),
              var s = try? JSONDecoder().decode(UpdateState.self, from: d) else { return UpdateState() }
        s.checking = false
        return s
    }

    static func save(_ s: UpdateState) {
        if let d = try? JSONEncoder().encode(s) { UserDefaults.standard.set(d, forKey: defaultsKey) }
    }

    /// The wait after the nth failed check in a row: 15 min, doubling, at most 6 h. A failed
    /// check isn't the daily check, so without this it would be retried on every tick.
    static func retryDelay(_ failures: Int) -> TimeInterval {
        min(15 * 60 * pow(2, Double(max(0, failures - 1))), 6 * 3600)
    }

    static func due(_ s: UpdateState, now: Date = Date()) -> Bool {
        guard !s.checking, let t = s.checkedAt else { return !s.checking }
        return now.timeIntervalSince(t) >= interval || t > now
    }

    /// Asks GitHub for the newest (non-pre-release) release. No sign-in, nothing about the user.
    static func fetchLatest() async throws -> (tag: String, url: String) {
        var req = URLRequest(url: api)
        req.timeoutInterval = 20
        req.setValue("application/vnd.github+json", forHTTPHeaderField: "Accept")
        req.setValue("OpenCloud-Bar/\(running)", forHTTPHeaderField: "User-Agent")
        let (data, resp) = try await URLSession.shared.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if code == 404 { throw AuthError.message("No releases are published yet") }
        guard code == 200 else { throw AuthError.message("GitHub answered \(code)") }
        guard let obj = try JSONSerialization.jsonObject(with: data) as? [String: Any],
              let tag = obj["tag_name"] as? String, SemVer(tag) != nil else {
            throw AuthError.message("GitHub's answer has no release version")
        }
        var url = (obj["html_url"] as? String) ?? ""
        // Only ever open this project's own release pages.
        if !url.hasPrefix(pagePrefix) { url = pagePrefix + "latest" }
        return (tag, url)
    }
}

// MARK: - Sync activity

/// bisync runs with `--stats 2s --stats-log-level NOTICE`: every 2 seconds rclone prints a
/// progress block. This reads the transferred-file count from it and keeps the blocks out of
/// the output (and so out of the sync log). Same as statsFilter in cross/activity.go (tested there).
final class StatsFilter: @unchecked Sendable {
    private let lock = NSLock()
    private var partial = Data()
    private var inStats = false
    private let out: OutputBuffer
    private let onFiles: @Sendable (Int) -> Void

    init(into out: OutputBuffer, onFiles: @escaping @Sendable (Int) -> Void) {
        self.out = out
        self.onFiles = onFiles
    }

    func feed(_ d: Data) {
        lock.lock(); defer { lock.unlock() }
        partial.append(d)
        while let i = partial.firstIndex(of: UInt8(ascii: "\n")) {
            let line = partial[partial.startIndex...i]
            partial = Data(partial[partial.index(after: i)...])
            handle(Data(line))
        }
    }

    func flush() {
        lock.lock(); defer { lock.unlock() }
        if !partial.isEmpty { handle(partial); partial = Data() }
    }

    private func handle(_ line: Data) {
        let text = String(decoding: line, as: UTF8.self).trimmingCharacters(in: CharacterSet(charactersIn: "\r\n"))
        if inStats {
            if text.trimmingCharacters(in: .whitespaces).isEmpty {
                inStats = false
            } else if text.hasPrefix("Transferred:") {
                // "Transferred:   260 / 300, 87%" (files); the bytes line has units and doesn't parse.
                let rest = text.dropFirst("Transferred:".count).trimmingCharacters(in: .whitespaces)
                let parts = rest.split(separator: " ")
                if parts.count >= 3, parts[1] == "/", let n = Int(parts[0]) { onFiles(n) }
            }
            return
        }
        if text.range(of: #"^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d NOTICE:\s*$"#, options: .regularExpression) != nil {
            inStats = true
            return
        }
        out.append(line)
    }
}

/// The menu's first line while syncing: "Syncing “School” (2 of 3) · 12 files…".
func activityText(name: String?, index: Int, count: Int, files: Int) -> String {
    guard let name else { return "Syncing…" }
    var s = "Syncing “\(name)”"
    if count > 1 { s += " (\(index) of \(count))" }
    if files == 1 { s += " · 1 file" } else if files > 1 { s += " · \(files) files" }
    return s + "…"
}
