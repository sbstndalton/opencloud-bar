import AppKit
import Foundation
import SwiftUI

/// Recent Changes: what each successful sync did, read from rclone bisync's INFO output as it
/// arrives (ChangeCollector, off the main thread; nothing is kept beyond what is recorded), and
/// stored in activity.json in the sync state folder (the last 500). Same rules as the Go app
/// (cross/changes.go), which has the tests and the bisync lines this reads:
/// "- Path1 Queue copy to Path2 - <dest>", "- Path2 Queue delete - <path>" and the like, after
/// "Synching Path1 "<local>" with Path2 "<remote>"". Path1 is this Mac, Path2 OpenCloud.
/// Renames aren't guessed (that could hide a real deletion). A merge (first sync, Repair,
/// automatic merge), and a sync that changed more than 200 files, is one summary line.
struct Change: Codable, Hashable, Identifiable {
    var time: Date
    var folder: String      // KeptFolder.key
    var name: String        // the folder's display name
    var path: String        // relative to the kept folder ("" for a summary)
    /// uploaded, downloaded, deleted here, deleted in OpenCloud, or summary.
    var action: String
    var text: String?
    var id: String { "\(time.timeIntervalSince1970)|\(folder)|\(path)|\(action)|\(text ?? "")" }

    var isDeletion: Bool { action == "deleted here" || action == "deleted in OpenCloud" }
    var label: String { action == "summary" ? (text ?? "") : "\(name): \(path) \(Changes.words(action))" }
}

enum Changes {
    static let max = 500
    static let bulk = 200

    static func words(_ action: String) -> String {
        switch action {
        case "deleted here": return "deleted on this Mac (also removed from OpenCloud)"
        case "deleted in OpenCloud": return "deleted in OpenCloud (also removed here)"
        default: return action
        }
    }

    // Compiled once.
    private static func re(_ p: String) -> NSRegularExpression { try! NSRegularExpression(pattern: p) }
    nonisolated(unsafe) static let reSynching = re(#"Synching Path1 "(.*)" with Path2 "(.*)"\s*$"#)
    nonisolated(unsafe) static let reCopy = re(#"- Path[12]\s+Queue copy to (Path[12])\s+- (.*?)\s*$"#)
    nonisolated(unsafe) static let reDelete = re(#"- (Path[12])\s+Queue delete\s+- (.*?)\s*$"#)
    nonisolated(unsafe) static let reCopied = re(#"INFO  : (.+): Copied \("#)
    nonisolated(unsafe) static let reChecks = re(#"Checks:\s+([0-9]+) / ([0-9]+)"#)
    nonisolated(unsafe) static let reConflict = re(#"\.conflict[0-9]+(\.[^/]*)?$"#)

    static func groups(_ r: NSRegularExpression, _ s: String) -> [String]? {
        guard let m = r.firstMatch(in: s, range: NSRange(s.startIndex..., in: s)) else { return nil }
        return (0..<m.numberOfRanges).map { i in Range(m.range(at: i), in: s).map { String(s[$0]) } ?? "" }
    }

    static func skip(_ p: String) -> Bool {
        p.isEmpty || p == Backups.folderName || p.hasPrefix(Backups.folderName + "/") || groups(reConflict, p) != nil
    }

    /// A whole run's output at once (for checking against the Go app's tests).
    static func parse(_ output: String, kind: String, folder f: KeptFolder, now: Date = Date()) -> [Change] {
        let c = ChangeCollector()
        for l in output.split(separator: "\n") { c.line(String(l)) }
        return c.result(kind: kind, folder: f, now: now)
    }

    static func load(_ file: URL) -> [Change] {
        guard let d = try? Data(contentsOf: file) else { return [] }
        let dec = JSONDecoder()
        dec.dateDecodingStrategy = .iso8601
        guard let list = try? dec.decode([Change].self, from: d) else {
            // Damaged: keep it aside and start a new list.
            let bad = file.appendingPathExtension("bad")
            try? FileManager.default.removeItem(at: bad)
            try? FileManager.default.moveItem(at: file, to: bad)
            return []
        }
        return Array(list.suffix(max))
    }

    static func save(_ list: [Change], to file: URL) {
        let enc = JSONEncoder()
        enc.dateEncodingStrategy = .iso8601
        guard let d = try? enc.encode(Array(list.suffix(max))) else { return }
        try? FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
        try? d.write(to: file, options: .atomic)   // a temporary file, then a rename
    }
}

/// Reads bisync's output line by line and keeps only what Recent Changes records: at most
/// `Changes.bulk` + 1 events, and counts.
final class ChangeCollector: @unchecked Sendable {
    private let lock = NSLock()
    private var root1 = "", root2 = ""
    private var events: [(action: String, path: String)] = []
    private var counts: [String: Int] = [:]
    private var copied = 0, compared = -1

    func line(_ raw: String) {
        var l = raw.trimmingCharacters(in: CharacterSet(charactersIn: "\r\n"))
        if l.contains("\u{1b}") { l = l.replacingOccurrences(of: "\u{1b}\\[[0-9;]*m", with: "", options: .regularExpression) }
        lock.lock(); defer { lock.unlock() }
        if let m = Changes.groups(Changes.reSynching, l) { root1 = m[1]; root2 = m[2]; return }
        if let m = Changes.groups(Changes.reChecks, l) { compared = Int(m[2]) ?? compared; return }
        if let m = Changes.groups(Changes.reCopied, l) { if !Changes.skip(m[1]) { copied += 1 }; return }
        func add(_ side: String, _ p: String, _ action: String) {
            let root = side == "Path2" ? root2 : root1
            guard !root.isEmpty, p.hasPrefix(root) else { return }
            let rel = String(p.dropFirst(root.count))
            guard !Changes.skip(rel) else { return }
            counts[action, default: 0] += 1
            if events.count <= Changes.bulk { events.append((action, rel)) }
        }
        if let m = Changes.groups(Changes.reCopy, l) {
            add(m[1], m[2], m[1] == "Path1" ? "downloaded" : "uploaded")
        } else if let m = Changes.groups(Changes.reDelete, l) {
            add(m[1], m[2], m[1] == "Path1" ? "deleted in OpenCloud" : "deleted here")
        }
    }

    func result(kind: String, folder f: KeptFolder, now: Date) -> [Change] {
        lock.lock(); defer { lock.unlock() }
        if kind != "sync" {
            let what = kind.contains("repair") ? "Repair" : kind.contains("first sync") ? "First sync"
                : kind.contains("excludes") ? "Sync after the excludes changed" : "Merge"
            let text = compared >= 0 ? "\(what) of \(f.displayName): \(compared) files compared, \(copied) updated"
                : "\(what) of \(f.displayName): \(copied) updated"
            return [Change(time: now, folder: f.key, name: f.displayName, path: "", action: "summary", text: text)]
        }
        if counts.values.reduce(0, +) > Changes.bulk {
            let parts = ["uploaded", "downloaded", "deleted here", "deleted in OpenCloud"].compactMap { a in
                counts[a].map { "\($0) \(Changes.words(a))" }
            }
            return [Change(time: now, folder: f.key, name: f.displayName, path: "", action: "summary",
                           text: "Sync of \(f.displayName): " + parts.joined(separator: ", "))]
        }
        return events.map { Change(time: now, folder: f.key, name: f.displayName, path: $0.path, action: $0.action) }
    }
}

/// Settings ▸ Activity: the recent changes, newest first, searchable.
struct ActivityPage: View {
    @ObservedObject var model: AppModel
    @Local private var query = ""

    private var rows: [Change] {
        let all = model.recentChanges.reversed()
        let q = query.trimmingCharacters(in: .whitespaces)
        return q.isEmpty ? Array(all) : all.filter { $0.label.localizedCaseInsensitiveContains(q) }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("What the last syncs changed, newest first (the last 500 changes). A merge, such as a first sync or a Repair, and a sync that changed more than 200 files are one line each.")
                .font(.caption).foregroundStyle(.secondary)
            TextField("Search", text: $query).textFieldStyle(.roundedBorder).accessibilityLabel("Search recent changes")
            List(rows) { c in
                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(c.label).lineLimit(2)
                        Text(c.time.formatted(date: .abbreviated, time: .shortened)).font(.caption).foregroundStyle(.secondary)
                    }
                    .accessibilityElement(children: .combine)
                    Spacer()
                    if model.changeExists(c) {
                        Button("Show in Finder") { model.reveal(c) }.accessibilityLabel("Show \(c.path) in Finder")
                        Button("Open OpenCloud") { model.openWeb() }.accessibilityLabel("Open OpenCloud in the browser")
                    }
                }
            }
            .accessibilityLabel("Recent changes")
            .overlay {
                if rows.isEmpty {
                    Text(model.recentChanges.isEmpty ? "No changes yet. They show up here after the next sync." : "Nothing matches.")
                        .foregroundStyle(.secondary)
                }
            }
        }
        .padding(12)
    }
}
