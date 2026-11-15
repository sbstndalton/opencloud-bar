import AppKit
import SwiftUI

/// Conflicts. Kept folders sync with --conflict-resolve newer --conflict-loser num: when both
/// sides changed a file, the newer copy keeps the name and the other is renamed. rclone v1.76
/// appends ".conflict<N>" to the whole name ("report.pdf" → "report.pdf.conflict1"). Same rules
/// as the Windows/Linux app (cross/conflicts.go), which has the tests.
struct ConflictItem: Codable, Equatable, Identifiable {
    var file: String       // the losing copy, relative to the kept folder, e.g. "Notes/todo.txt.conflict1"
    var original: String   // the file it lost against, e.g. "Notes/todo.txt"
    var modified: Date
    var size: Int64
    var id: String { file }
}

enum Conflicts {
    static let maxListed = 200
    static let maxWalked = 200_000

    /// The winner's name for a conflict copy's file name, or nil if it isn't one.
    static func originalName(_ base: String) -> String? {
        guard let r = base.range(of: #"^(.+)\.conflict[1-9][0-9]*$"#, options: .regularExpression), r == base.startIndex..<base.endIndex,
              let dot = base.range(of: ".conflict", options: .backwards) else { return nil }
        let name = String(base[..<dot.lowerBound])
        return name.isEmpty ? nil : name
    }

    /// Folder names excluded everywhere ("- **/node_modules/**") and the backup folder: not walked.
    static func skippedDirs(_ filters: [String]) -> Set<String> {
        var out: Set<String> = [Backups.folderName]
        for l in filters.map({ $0.trimmingCharacters(in: .whitespaces) }) where l.hasPrefix("- **/") && l.hasSuffix("/**") {
            let name = String(l.dropFirst(5).dropLast(3))
            if !name.isEmpty, name.rangeOfCharacter(from: CharacterSet(charactersIn: "*?[{/")) == nil { out.insert(name) }
        }
        return out
    }

    /// The conflict copies under `dir`, newest first (capped). Runs off the main thread.
    static func scan(_ dir: URL, filters: [String]) -> [ConflictItem] {
        let skip = skippedDirs(filters)
        let keys: [URLResourceKey] = [.isDirectoryKey, .isRegularFileKey, .contentModificationDateKey, .fileSizeKey]
        guard let e = FileManager.default.enumerator(at: dir, includingPropertiesForKeys: keys, options: [],
                                                     errorHandler: { _, _ in true }) else { return [] }
        let root = dir.standardizedFileURL.path + "/"
        var out: [ConflictItem] = []
        var seen = 0
        for case let u as URL in e {
            seen += 1
            if seen > maxWalked { break }
            let v = try? u.resourceValues(forKeys: Set(keys))
            if v?.isDirectory == true {
                if skip.contains(u.lastPathComponent) { e.skipDescendants() }
                continue
            }
            guard v?.isRegularFile == true, let orig = originalName(u.lastPathComponent) else { continue }
            let path = u.standardizedFileURL.path
            guard path.hasPrefix(root) else { continue }
            let rel = String(path.dropFirst(root.count))
            let dirPart = (rel as NSString).deletingLastPathComponent
            out.append(ConflictItem(file: rel, original: dirPart.isEmpty ? orig : dirPart + "/" + orig,
                                    modified: v?.contentModificationDate ?? .distantPast, size: Int64(v?.fileSize ?? 0)))
        }
        out.sort { $0.modified > $1.modified }
        return Array(out.prefix(maxListed))
    }

    static func load(_ file: URL) -> [ConflictItem] {
        guard let d = try? Data(contentsOf: file) else { return [] }
        let dec = JSONDecoder(); dec.dateDecodingStrategy = .iso8601
        return (try? dec.decode([ConflictItem].self, from: d)) ?? []
    }

    static func save(_ list: [ConflictItem], to file: URL) {
        if list.isEmpty { try? FileManager.default.removeItem(at: file); return }
        let enc = JSONEncoder(); enc.dateEncodingStrategy = .iso8601
        enc.outputFormatting = [.prettyPrinted, .sortedKeys]
        if let d = try? enc.encode(list) { try? d.write(to: file, options: .atomic) }
    }

    /// The loser's and the winner's URLs, if `item` is a conflict copy inside `local`.
    static func target(_ local: URL, _ item: ConflictItem) -> (loser: URL, winner: URL)? {
        let parts = item.file.split(separator: "/", omittingEmptySubsequences: false)
        guard !item.file.hasPrefix("/"), !parts.contains(where: { $0 == ".." || $0 == "." || $0.isEmpty }),
              let orig = originalName(String(parts.last ?? "")) else { return nil }
        let loser = local.appendingPathComponent(item.file)
        return (loser, loser.deletingLastPathComponent().appendingPathComponent(orig))
    }

    /// Keep This Version: the current file goes to the local backups (like a merge's), then the
    /// conflict copy takes its name. The next sync sends it to OpenCloud.
    static func keep(_ item: ConflictItem, local: URL, backupDir: URL) throws {
        guard let (loser, winner) = target(local, item) else { throw AuthError.message("That isn't a conflict copy in this folder.") }
        let fm = FileManager.default
        guard fm.fileExists(atPath: loser.path) else { throw AuthError.message("The conflict copy is gone (synced or removed meanwhile).") }
        if fm.fileExists(atPath: winner.path) {
            let dest = freeURL(backupDir.appendingPathComponent(item.original))
            try fm.createDirectory(at: dest.deletingLastPathComponent(), withIntermediateDirectories: true)
            try fm.moveItem(at: winner, to: dest)
        }
        try fm.moveItem(at: loser, to: winner)
    }
}

extension Conflicts {
    /// `url`, or "name (2).ext", "name (3).ext"… if it exists: a backup never replaces another
    /// (two Keeps of the same file within one second share the stamp folder).
    static func freeURL(_ url: URL) -> URL {
        let fm = FileManager.default
        guard fm.fileExists(atPath: url.path) else { return url }
        let ext = url.pathExtension, stem = url.deletingPathExtension().lastPathComponent, dir = url.deletingLastPathComponent()
        for i in 2... {
            let name = ext.isEmpty ? "\(stem) (\(i))" : "\(stem) (\(i)).\(ext)"
            let q = dir.appendingPathComponent(name)
            if !fm.fileExists(atPath: q.path) { return q }
        }
        return url
    }
}

/// Settings ▸ Conflicts.
struct ConflictsPage: View {
    @ObservedObject var model: AppModel

    private var rows: [(KeptFolder, ConflictItem)] {
        model.config.kept.flatMap { f in (model.conflicts[f.path] ?? []).map { (f, $0) } }
    }

    var body: some View {
        Form {
            Section {
                Text("When a file changed on both sides, the newer copy keeps its name and the other is saved next to it as name.conflict1. Keep This Version makes the conflict copy the file; the current one goes to the backups in the app's state folder. Discard moves the conflict copy to the Trash. The next sync applies either to OpenCloud.")
                    .font(.caption).foregroundStyle(.secondary)
                if model.syncing {
                    Text("A sync is running; the actions wait until it finishes.").font(.caption).foregroundStyle(.secondary)
                }
            }
            if rows.isEmpty {
                Section { Text("No conflicts.").foregroundStyle(.secondary) }
            } else {
                Section(rows.count == 1 ? "1 conflict copy" : "\(rows.count) conflict copies") {
                    ForEach(rows, id: \.1.file) { f, c in
                        VStack(alignment: .leading, spacing: 4) {
                            Text(c.file).font(.body.monospaced()).textSelection(.enabled)
                            Text("In “\(f.displayName)” · modified \(c.modified.formatted(date: .abbreviated, time: .shortened)) · \(ByteCountFormatter.string(fromByteCount: c.size, countStyle: .file))")
                                .font(.caption).foregroundStyle(.secondary)
                            HStack {
                                Button("Reveal in Finder") { model.revealConflict(f, c) }
                                Button("Keep This Version…") { model.keepConflict(f, c) }.disabled(model.syncing)
                                Button("Discard…") { model.discardConflict(f, c) }.disabled(model.syncing)
                            }
                        }
                        .padding(.vertical, 2)
                    }
                }
            }
        }
        .formStyle(.grouped)
    }
}
