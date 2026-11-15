import AppKit
import Foundation
import SwiftUI

/// Recent Changes: what each successful sync did, read from rclone bisync's INFO output and
/// kept in activity.json in the sync state folder (the last 500). Same rules as the Go app
/// (cross/changes.go), which has the tests and the bisync lines this reads:
/// "- Path1 Queue copy to Path2 - <dest>", "- Path2 Queue delete - <path>" and the like, after
/// "Synching Path1 "<local>" with Path2 "<remote>"". Path1 is this Mac, Path2 OpenCloud. A
/// delete and a copy of a file with the same name in another folder are one move; a merge
/// (first sync, Repair, automatic merge) is one summary line.
struct Change: Codable, Hashable, Identifiable {
    var time: Date
    var folder: String      // KeptFolder.key
    var name: String        // the folder's display name
    var path: String        // relative to the kept folder ("" for a summary)
    var from: String?
    /// uploaded, downloaded, deleted here, deleted in OpenCloud, renamed here, renamed in
    /// OpenCloud, or summary.
    var action: String
    var text: String?
    var id: String { "\(time.timeIntervalSince1970)|\(folder)|\(path)|\(action)|\(from ?? "")" }

    var isDeletion: Bool { action == "deleted here" || action == "deleted in OpenCloud" }
    var label: String {
        switch action {
        case "summary": return text ?? ""
        case "renamed here", "renamed in OpenCloud": return "\(name): \(from ?? "") → \(path) (\(action))"
        default: return "\(name): \(path) \(action)"
        }
    }
}

enum Changes {
    static let max = 500

    private static func matches(_ re: String, _ s: String) -> [String]? {
        guard let r = try? NSRegularExpression(pattern: re),
              let m = r.firstMatch(in: s, range: NSRange(s.startIndex..., in: s)) else { return nil }
        return (0..<m.numberOfRanges).map { i in
            Range(m.range(at: i), in: s).map { String(s[$0]) } ?? ""
        }
    }

    static func skip(_ p: String) -> Bool {
        p.isEmpty || p == Backups.folderName || p.hasPrefix(Backups.folderName + "/")
            || matches(#"\.conflict[0-9]+(\.[^/]*)?$"#, p) != nil
    }

    static func parse(_ output: String, kind: String, folder f: KeptFolder, now: Date = Date()) -> [Change] {
        let out = output.replacingOccurrences(of: "\u{1b}\\[[0-9;]*m", with: "", options: .regularExpression)
        if kind != "sync" { return [summary(out, kind: kind, folder: f, now: now)] }
        var root1 = "", root2 = ""
        var evs: [(action: String, path: String)] = []
        for raw in out.split(separator: "\n", omittingEmptySubsequences: true) {
            let l = String(raw).trimmingCharacters(in: CharacterSet(charactersIn: "\r"))
            if let m = matches(#"Synching Path1 "(.*)" with Path2 "(.*)"\s*$"#, l) { root1 = m[1]; root2 = m[2]; continue }
            func strip(_ side: String, _ p: String) -> String {
                let root = side == "Path2" ? root2 : root1
                return !root.isEmpty && p.hasPrefix(root) ? String(p.dropFirst(root.count)) : ""
            }
            if let m = matches(#"- Path[12]\s+Queue copy to (Path[12])\s+- (.*?)\s*$"#, l) {
                let p = strip(m[1], m[2])
                if !skip(p) { evs.append((m[1] == "Path1" ? "downloaded" : "uploaded", p)) }
            } else if let m = matches(#"- (Path[12])\s+Queue delete\s+- (.*?)\s*$"#, l) {
                let p = strip(m[1], m[2])
                if !skip(p) { evs.append((m[1] == "Path1" ? "deleted in OpenCloud" : "deleted here", p)) }
            }
        }
        var used = Set<Int>()
        func pair(_ del: String, _ cp: String) -> [Int: Int] {
            var out: [Int: Int] = [:]
            for (i, d) in evs.enumerated() where d.action == del && !used.contains(i) {
                let base = (d.path as NSString).lastPathComponent
                let m = evs.indices.filter { j in
                    evs[j].action == cp && !used.contains(j) && (evs[j].path as NSString).lastPathComponent == base && evs[j].path != d.path
                }
                if m.count == 1 { out[i] = m[0]; used.insert(i); used.insert(m[0]) }
            }
            return out
        }
        let here = pair("deleted here", "uploaded"), remote = pair("deleted in OpenCloud", "downloaded")
        var res: [Change] = []
        for (i, e) in evs.enumerated() {
            if let j = here[i] {
                res.append(Change(time: now, folder: f.key, name: f.displayName, path: evs[j].path, from: e.path, action: "renamed here"))
            } else if let j = remote[i] {
                res.append(Change(time: now, folder: f.key, name: f.displayName, path: evs[j].path, from: e.path, action: "renamed in OpenCloud"))
            } else if !used.contains(i) {
                res.append(Change(time: now, folder: f.key, name: f.displayName, path: e.path, action: e.action))
            }
        }
        return res
    }

    static func summary(_ out: String, kind: String, folder f: KeptFolder, now: Date) -> Change {
        var copied = 0, compared = -1
        for raw in out.split(separator: "\n") {
            let l = String(raw)
            if let m = matches(#"INFO  : (.+): Copied \("#, l), !skip(m[1]) { copied += 1 }
            if let m = matches(#"Checks:\s+([0-9]+) / ([0-9]+)"#, l), let n = Int(m[2]) { compared = n }
        }
        let what = kind.contains("repair") ? "Repair" : kind.contains("first sync") ? "First sync"
            : kind.contains("excludes") ? "Sync after the excludes changed" : "Merge"
        let text = compared >= 0 ? "\(what) of \(f.displayName): \(compared) files compared, \(copied) updated"
            : "\(what) of \(f.displayName): \(copied) updated"
        return Change(time: now, folder: f.key, name: f.displayName, path: "", action: "summary", text: text)
    }

    static func load(_ file: URL) -> [Change] {
        let dec = JSONDecoder()
        dec.dateDecodingStrategy = .iso8601
        guard let d = try? Data(contentsOf: file), let list = try? dec.decode([Change].self, from: d) else { return [] }
        return Array(list.suffix(max))
    }

    static func save(_ list: [Change], to file: URL) {
        let enc = JSONEncoder()
        enc.dateEncodingStrategy = .iso8601
        guard let d = try? enc.encode(Array(list.suffix(max))) else { return }
        try? FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
        try? d.write(to: file, options: .atomic)
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
            Text("What the last syncs changed, newest first (the last 500 changes). A merge, such as a first sync or a Repair, is one line.")
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
                        Button("Open in Browser") { model.openWeb() }.accessibilityLabel("Open OpenCloud in the browser")
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
