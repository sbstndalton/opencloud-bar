import AppKit
import Foundation
import SwiftUI

/// File history and deleted files, from OpenCloud's WebDAV API (reva ocdav). Same requests
/// and rules as the Go app (cross/versions.go, cross/trash.go), which has the tests and the
/// source references:
///
/// - Versions: PROPFIND <dav>/meta/<fileid>/v (Depth 1; the first response is the "v" folder,
///   then one per version, href …/v/<key>, getlastmodified, getcontentlength), restore with
///   COPY <dav>/meta/<fileid>/v/<key> (204; Destination: the file, as OpenCloud's web UI
///   sends it). reva versions.go, dav.go; web-client listFileVersions.ts, restoreFileVersion.ts.
/// - Trash: PROPFIND <dav>/spaces/trash-bin/<space id> (Depth 1; oc:trashbin-original-filename,
///   -original-location, -delete-timestamp, -delete-datetime, getcontentlength / oc:size,
///   resourcetype), restore with MOVE …/trash-bin/<space id>/<key>, Destination:
///   <dav>/spaces/<space id>/<original location>, Overwrite: F (201; 412 if something is
///   there; 409 if its folder is gone). reva spaces.go, trashbin.go, dav.go.
///
/// The bearer token only goes to the server: this session follows no redirects. Nothing is
/// ever deleted from the trash, and a restore never overwrites.
enum DavHistory {
    struct Version: Identifiable, Hashable {
        var key: String
        var modified: Date?
        var size: Int64
        var id: String { key }
    }

    struct TrashItem: Identifiable, Hashable {
        var spaceID: String
        var spaceName: String
        var key: String
        var name: String
        var path: String         // where it was in the space
        var deleted: Date?
        var size: Int64
        var dir: Bool
        var folderKey: String?   // the kept folder it was in
        var id: String { spaceID + "\u{1}" + key }
    }

    struct TrashSpace {
        var id: String
        var name: String
        var davPath: String      // /dav/spaces/<id>, as the server gave it
        var kept: [KeptFolder]
    }

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
        c.timeoutIntervalForRequest = 30
        return URLSession(configuration: c, delegate: NoRedirect(), delegateQueue: nil)
    }()

    static let segmentAllowed = CharacterSet.urlPathAllowed.subtracting(CharacterSet(charactersIn: "/?#;"))
    static func esc(_ s: String) -> String { s.addingPercentEncoding(withAllowedCharacters: segmentAllowed) ?? "" }

    private static func request(_ method: String, _ url: URL, token: String, headers: [String: String] = [:], body: String? = nil) async throws -> (Data, Int) {
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.timeoutInterval = 30
        for (k, v) in headers { req.setValue(v, forHTTPHeaderField: k) }
        if let body {
            req.setValue("application/xml", forHTTPHeaderField: "Content-Type")
            req.httpBody = Data(body.utf8)
        }
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        do {
            let (data, resp) = try await session.data(for: req)
            return (data, (resp as? HTTPURLResponse)?.statusCode ?? 0)
        } catch {
            throw AuthError.message("The server didn't answer.")
        }
    }

    // MARK: - Paths

    static func plainSegments(_ p: String) -> [String]? {
        let t = p.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        guard !t.isEmpty else { return nil }
        let parts = t.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        for s in parts where s.isEmpty || s == "." || s == ".." || s.contains("\\")
            || s.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) { return nil }
        return parts
    }

    /// The WebDAV root a DAV path lives under: /remote.php/dav/files/sam → /remote.php/dav.
    static func davRoot(_ davPath: String) -> String? {
        var p = davPath
        while p.hasSuffix("/") { p.removeLast() }
        for seg in ["/files/", "/spaces/"] {
            if let r = p.range(of: seg, options: .backwards), r.upperBound < p.endIndex { return String(p[..<r.lowerBound]) }
        }
        return nil
    }

    static func metaURL(_ c: Config, _ f: KeptFolder, fileID: String) -> URL? {
        guard !fileID.isEmpty, !fileID.contains(where: { "/?#\\".contains($0) }) else { return nil }
        let base = (c.serverURL ?? "").trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        let root: String
        if let s = f.space {
            guard let r = davRoot(s.davPath) else { return nil }
            root = Config.origin(base) + r
        } else {
            guard let r = davRoot(c.davPath) else { return nil }
            root = base + r
        }
        return URL(string: root + "/meta/" + esc(fileID) + "/v")
    }

    /// <dav>/spaces/trash-bin/<id> for a space root <dav>/spaces/<id>.
    static func trashURL(origin: String, davPath: String) -> String? {
        var p = davPath
        while p.hasSuffix("/") { p.removeLast() }
        guard let slash = p.lastIndex(of: "/") else { return nil }
        let parent = p[..<slash], id = p[p.index(after: slash)...]
        guard parent.hasSuffix("/spaces"), !id.isEmpty else { return nil }
        return origin + parent + "/trash-bin/" + id
    }

    static func keptFolder(_ kept: [KeptFolder], containing p: String) -> KeptFolder? {
        var best: KeptFolder?
        for f in kept {
            let fp = f.path.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
            if !fp.isEmpty, p == fp || p.hasPrefix(fp + "/") {
                if best == nil || fp.count > best!.path.count { best = f }
            }
        }
        return best
    }

    // MARK: - Multistatus

    /// One <response>: its href and the props of its 200 propstats, by local name.
    struct DavResponse { var href = ""; var props: [String: String] = [:]; var collection = false; var ok = false }

    final class MultistatusParser: NSObject, XMLParserDelegate {
        var out: [DavResponse] = []
        private var cur: DavResponse?
        private var stat: [String: String] = [:]
        private var statCollection = false
        private var statusText = ""
        private var text = ""
        private var inProp = false

        static func parse(_ data: Data) -> [DavResponse]? {
            let p = XMLParser(data: data)
            let d = MultistatusParser()
            p.shouldProcessNamespaces = true
            p.delegate = d
            return p.parse() ? d.out : nil
        }

        func parser(_ parser: XMLParser, didStartElement name: String, namespaceURI: String?, qualifiedName: String?, attributes: [String: String] = [:]) {
            text = ""
            switch name {
            case "response": cur = DavResponse()
            case "propstat": stat = [:]; statCollection = false; statusText = ""
            case "prop": inProp = true
            case "collection" where inProp: statCollection = true
            default: break
            }
        }
        func parser(_ parser: XMLParser, foundCharacters string: String) { text += string }
        func parser(_ parser: XMLParser, didEndElement name: String, namespaceURI: String?, qualifiedName: String?) {
            let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
            switch name {
            case "href" where !inProp: cur?.href = t
            case "prop": inProp = false
            case "status" where !inProp: statusText = t
            case "propstat":
                if statusText.isEmpty || statusText.contains(" 200") {
                    cur?.ok = true
                    for (k, v) in stat where !v.isEmpty { cur?.props[k] = v }
                    if statCollection { cur?.collection = true }
                }
            case "response": if let c = cur { out.append(c) }; cur = nil
            default: if inProp { stat[name] = t }
            }
            text = ""
        }
    }

    static func hrefPath(_ href: String) -> String {
        URLComponents(string: href)?.path ?? ""
    }

    static func httpDate(_ s: String) -> Date? {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = TimeZone(identifier: "UTC")
        for fmt in ["EEE, dd MMM yyyy HH:mm:ss zzz", "EEE, dd MMM yyyy HH:mm:ss Z"] {
            f.dateFormat = fmt
            if let d = f.date(from: s) { return d }
        }
        return nil
    }

    // MARK: - Versions

    struct FileInfo { var id: String; var size: Int64?; var modified: Date? }
    struct NotInOpenCloud: LocalizedError {
        var errorDescription: String? { "It isn't in OpenCloud yet. Wait for the folder to sync, then try again." }
    }

    /// The file's oc:fileid, size and modification time (one PROPFIND, depth 0).
    static func fileInfo(_ url: URL, token: String) async throws -> FileInfo {
        let (data, code) = try await request("PROPFIND", url, token: token, headers: ["Depth": "0"],
            body: #"<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:fileid/><d:getcontentlength/><d:getlastmodified/></d:prop></d:propfind>"#)
        if code == 404 { throw NotInOpenCloud() }
        if code == 401 { throw AuthError.message("Your sign-in ended. Sign in again, then try again.") }
        guard code == 207 else { throw AuthError.message("The server answered \(code).") }
        guard let r = MultistatusParser.parse(data)?.first, let id = r.props["fileid"], !id.isEmpty else {
            throw AuthError.message("The server didn't say which file that is.")
        }
        return FileInfo(id: id, size: r.props["getcontentlength"].flatMap { Int64($0) },
                        modified: r.props["getlastmodified"].flatMap(httpDate))
    }

    static let unsyncedText = "This file has changes that haven't synced yet. Sync first, then restore."

    /// The local copy is the server's current file: same size and (when the server says) a
    /// modification time within 2 seconds. A missing local copy doesn't match. Same as Go.
    nonisolated static func localMatches(_ local: URL, _ srv: FileInfo) -> Bool {
        guard let a = try? FileManager.default.attributesOfItem(atPath: local.path),
              (a[.type] as? FileAttributeType) == .typeRegular else { return false }
        if let size = srv.size, (a[.size] as? NSNumber)?.int64Value != size { return false }
        if let m = srv.modified {
            guard let lm = a[.modificationDate] as? Date, abs(lm.timeIntervalSince(m)) <= 2 else { return false }
        }
        return true
    }

    /// Copies the local file into the backups (never over another backup). Off the main actor.
    nonisolated static func backup(_ local: URL, to dir: URL, rel: String) throws {
        let fm = FileManager.default
        let dest = Conflicts.freeURL(dir.appendingPathComponent(rel))
        try fm.createDirectory(at: dest.deletingLastPathComponent(), withIntermediateDirectories: true,
                               attributes: [.posixPermissions: 0o700])
        try fm.copyItem(at: local, to: dest)
    }

    static func parseVersions(_ data: Data) -> [Version]? {
        guard let rs = MultistatusParser.parse(data) else { return nil }
        var out: [Version] = []
        for r in rs {
            var p = hrefPath(r.href)
            while p.hasSuffix("/") { p.removeLast() }
            guard r.ok, let i = p.range(of: "/v/", options: .backwards) else { continue }
            let key = String(p[i.upperBound...])
            guard let seg = plainSegments(key), seg.count == 1 else { continue }
            out.append(Version(key: key, modified: r.props["getlastmodified"].flatMap(httpDate),
                               size: r.props["getcontentlength"].flatMap { Int64($0) } ?? -1))
        }
        return out.sorted { ($0.modified ?? .distantPast) > ($1.modified ?? .distantPast) }
    }

    static func listVersions(_ meta: URL, token: String) async throws -> [Version] {
        let (data, code) = try await request("PROPFIND", meta, token: token, headers: ["Depth": "1"],
            body: #"<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getlastmodified/><d:getcontentlength/><d:getetag/></d:prop></d:propfind>"#)
        switch code {
        case 207: break
        case 403: throw AuthError.message("You can't see this file's versions.")
        case 404: throw AuthError.message("OpenCloud has no versions of this file.")
        default: throw AuthError.message("The server answered \(code).")
        }
        guard let v = parseVersions(data) else { throw AuthError.message("The server's answer wasn't what was expected.") }
        return v
    }

    static func restoreVersion(_ meta: URL, key: String, file: URL, token: String) async throws {
        guard let seg = plainSegments(key), seg.count == 1, let u = URL(string: meta.absoluteString + "/" + esc(key)) else {
            throw AuthError.message("That isn't a version of this file.")
        }
        let (_, code) = try await request("COPY", u, token: token, headers: ["Destination": file.absoluteString])
        switch code {
        case 200..<300: return
        case 401: throw AuthError.message("Your sign-in ended, so nothing was restored. Sign in again, then try again.")
        case 403: throw AuthError.message("You can't restore versions of this file.")
        case 404: throw AuthError.message("That version isn't there any more.")
        default: throw AuthError.message("OpenCloud didn't restore it (it answered \(code)).")
        }
    }

    // MARK: - Trash

    static let personalProblem = "Personal: the server didn't say where your personal space is"

    /// The path of the personal space's root.webDavUrl, used against the configured server's
    /// origin (its host is ignored); same check as SpacesAPI.parse. nil if unusable. Same as Go.
    static func personalSpacePath(_ webDavURL: String) -> String? {
        guard let u = URLComponents(string: webDavURL) else { return nil }
        var p = u.percentEncodedPath
        while p.hasSuffix("/") { p.removeLast() }
        guard p.hasPrefix("/"), !p.contains("'"), !p.contains(" ") else { return nil }
        return p
    }

    /// The personal space (from /me/drives) and the spaces folders are kept from, and a
    /// problem line when the personal space can't be used.
    static func spaces(_ c: Config, token: String) async throws -> ([TrashSpace], [String]) {
        let base = (c.serverURL ?? "").trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        guard let url = URL(string: base + "/graph/v1.0/me/drives") else { throw AuthError.message("The server address isn't valid.") }
        var req = URLRequest(url: url)
        req.timeoutInterval = 30
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        guard let (data, resp) = try? await session.data(for: req), (resp as? HTTPURLResponse)?.statusCode == 200,
              let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else {
            throw AuthError.message("Couldn't list the spaces.")
        }
        var out: [TrashSpace] = [], problems: [String] = []
        if let d = ((obj["value"] as? [[String: Any]]) ?? []).first(where: { ($0["driveType"] as? String) == "personal" }),
           let id = d["id"] as? String, !id.isEmpty,
           let p = personalSpacePath(((d["root"] as? [String: Any])?["webDavUrl"] as? String) ?? "") {
            out.append(TrashSpace(id: id, name: "Personal", davPath: p, kept: c.kept.filter { $0.space == nil }))
        } else {
            problems.append(personalProblem)
        }
        for f in c.kept {
            guard let s = f.space else { continue }
            if let i = out.firstIndex(where: { $0.id == s.id }) { out[i].kept.append(f); continue }
            var p = s.davPath
            while p.hasSuffix("/") { p.removeLast() }
            out.append(TrashSpace(id: s.id, name: s.name, davPath: p, kept: [f]))
        }
        return (out, problems)
    }

    static func parseTrash(_ data: Data, _ sp: TrashSpace) -> [TrashItem]? {
        guard let rs = MultistatusParser.parse(data) else { return nil }
        var out: [TrashItem] = []
        for r in rs {
            var p = hrefPath(r.href)
            while p.hasSuffix("/") { p.removeLast() }
            guard let i = p.range(of: "/trash-bin/") else { continue }
            let rest = p[i.upperBound...]
            guard let slash = rest.firstIndex(of: "/") else { continue }   // the trash itself
            let key = String(rest[rest.index(after: slash)...])
            guard plainSegments(key) != nil, let seg = plainSegments(r.props["trashbin-original-location"] ?? "") else { continue }
            let loc = seg.joined(separator: "/")
            var name = r.props["trashbin-original-filename"] ?? ""
            if name.isEmpty || name.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) { name = seg.last ?? loc }
            var deleted: Date?
            if let ts = r.props["trashbin-delete-timestamp"].flatMap({ Double($0) }), ts > 0 { deleted = Date(timeIntervalSince1970: ts) }
            else { deleted = r.props["trashbin-delete-datetime"].flatMap(httpDate) }
            let size = (r.props["getcontentlength"] ?? r.props["size"]).flatMap { Int64($0) } ?? -1
            out.append(TrashItem(spaceID: sp.id, spaceName: sp.name, key: key, name: name, path: loc, deleted: deleted,
                                 size: size, dir: r.collection, folderKey: keptFolder(sp.kept, containing: loc)?.key))
        }
        return out
    }

    static func listTrash(origin: String, _ sp: TrashSpace, token: String) async throws -> [TrashItem] {
        guard let s = trashURL(origin: origin, davPath: sp.davPath), let u = URL(string: s) else {
            throw AuthError.message("This server's spaces aren't the kind this version understands.")
        }
        let body = #"<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop>"#
            + "<oc:trashbin-original-filename/><oc:trashbin-original-location/><oc:trashbin-delete-datetime/>"
            + "<oc:trashbin-delete-timestamp/><d:getcontentlength/><oc:size/><d:resourcetype/></d:prop></d:propfind>"
        let (data, code) = try await request("PROPFIND", u, token: token, headers: ["Depth": "1"], body: body)
        switch code {
        case 207: break
        case 403, 404: throw AuthError.message("Its trash isn't available to you.")
        default: throw AuthError.message("The server answered \(code).")
        }
        guard let items = parseTrash(data, sp) else { throw AuthError.message("The server's answer wasn't what was expected.") }
        return items
    }

    enum RestoreError: LocalizedError {
        case exists(String)
        var errorDescription: String? {
            switch self {
            case .exists(let p): return "Something is at “\(p)” in OpenCloud again. Rename or move it first, then restore (nothing was overwritten)."
            }
        }
    }

    /// Restores it to its original place, never over something there.
    static func restore(origin: String, _ sp: TrashSpace, _ it: TrashItem, token: String) async throws {
        guard let loc = plainSegments(it.path), let key = plainSegments(it.key),
              let trash = trashURL(origin: origin, davPath: sp.davPath),
              let dest = URL(string: origin + sp.davPath + "/" + loc.map(esc).joined(separator: "/")),
              let src = URL(string: trash + "/" + key.map(esc).joined(separator: "/")) else {
            throw AuthError.message("That item's place isn't a path this version can restore to.")
        }
        let (_, there) = try await request("PROPFIND", dest, token: token, headers: ["Depth": "0"],
            body: #"<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>"#)
        switch there {
        case 404: break
        case 200, 207: throw RestoreError.exists(it.path)
        default: throw AuthError.message("Couldn't check its original place (the server answered \(there)).")
        }
        let (_, code) = try await request("MOVE", src, token: token, headers: ["Destination": dest.absoluteString, "Overwrite": "F"])
        switch code {
        case 200, 201, 204: return
        case 412: throw RestoreError.exists(it.path)
        case 409: throw AuthError.message("The folder it was in is gone from OpenCloud. Restore that folder first.")
        case 403: throw AuthError.message("You can't restore items in this space.")
        case 404: throw AuthError.message("It isn't in the trash any more.")
        default: throw AuthError.message("OpenCloud didn't restore it (it answered \(code)).")
        }
    }

    /// Asks which version to restore. nil if cancelled.
    @MainActor
    static func askVersion(_ name: String, _ versions: [Version], paused: Bool) -> Version? {
        let a = NSAlert()
        a.messageText = "Earlier versions of “\(name)”"
        a.informativeText = "Choose a version to restore. It becomes the current file in OpenCloud; the copy on this Mac is kept in the backups (in the app's state folder, for 30 days) first. "
            + afterRestoreText(paused: paused)
        a.addButton(withTitle: "Restore")
        a.addButton(withTitle: "Cancel")
        let pick = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 340, height: 26), pullsDown: false)
        for v in versions {
            let when = v.modified?.formatted(date: .abbreviated, time: .shortened) ?? "Unknown date"
            let size = v.size >= 0 ? ByteCountFormatter.string(fromByteCount: v.size, countStyle: .file) : "size unknown"
            pick.addItem(withTitle: "\(when) · \(size)")
        }
        pick.setAccessibilityLabel("Version")
        a.accessoryView = pick
        a.window.initialFirstResponder = pick
        NSApp.activate(ignoringOtherApps: true)
        guard a.runModal() == .alertFirstButtonReturn else { return nil }
        let i = pick.indexOfSelectedItem
        return versions.indices.contains(i) ? versions[i] : nil
    }

    /// What happens after a restore: a sync now, or when syncing resumes.
    static func afterRestoreText(paused: Bool) -> String {
        paused ? "It comes to this computer when you resume syncing." : "The folder then syncs."
    }
}

/// Settings ▸ Deleted Files: OpenCloud's trash of the personal space and the kept spaces.
struct DeletedFilesPage: View {
    @ObservedObject var model: AppModel
    @Local private var showAll = false
    @Local private var items: [DavHistory.TrashItem] = []
    @Local private var problems: [String] = []
    @Local private var hidden = 0
    @Local private var loading = false
    @Local private var error: String?
    @Local private var restoring: String?

    var body: some View {
        Form {
            Section {
                Text("Files and folders deleted in OpenCloud, from your personal space and the spaces you keep folders from, newest first. Restore puts an item back where it was, if that was in a kept folder; the folder then syncs (while syncing is paused, when you resume). If something is at that place again, nothing is restored or overwritten. Deleting for good is done in OpenCloud in the browser.")
                    .font(.caption).foregroundStyle(.secondary)
                HStack {
                    Toggle("Show all (also items that weren't in a kept folder)", isOn: $showAll)
                        .disabled(loading)
                        .accessibilityLabel("Show all deleted items")
                    Spacer()
                    Button("Reload") { Task { await load() } }.disabled(loading).accessibilityLabel("Reload")
                }
                ForEach(Array(problems.enumerated()), id: \.offset) { _, p in
                    Text("Couldn't read the trash of \(p)").font(.caption).foregroundStyle(.orange)
                }
            }
            if let error {
                Section { Text(error).foregroundStyle(.secondary) }
            } else if loading && items.isEmpty {
                Section { ProgressView().controlSize(.small) }
            } else if items.isEmpty {
                Section { Text(hidden > 0 ? "Nothing deleted from your kept folders. \(hidden) other item(s) are in the trash; turn on Show all to see them." : "The trash is empty.").foregroundStyle(.secondary) }
            } else {
                Section(items.count == 1 ? "1 deleted item" : "\(items.count) deleted items") {
                    ForEach(items) { it in
                        HStack(alignment: .top) {
                            VStack(alignment: .leading, spacing: 3) {
                                Text(it.name + (it.dir ? "/" : "")).textSelection(.enabled)
                                Text((it.spaceName == "Personal" ? "" : it.spaceName + ": ") + "/" + it.path)
                                    .font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                                Text([it.deleted.map { "Deleted " + $0.formatted(date: .abbreviated, time: .shortened) },
                                      it.size >= 0 ? ByteCountFormatter.string(fromByteCount: it.size, countStyle: .file) : nil]
                                        .compactMap { $0 }.joined(separator: " · "))
                                    .font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            if it.folderKey != nil {
                                Button("Restore…") { Task { await restore(it) } }
                                    .disabled(restoring != nil || model.needsSignIn)
                                    .accessibilityLabel("Restore \(it.path)…")
                            } else {
                                Text("Not in a kept folder").font(.caption).foregroundStyle(.secondary)
                                    .help("Restore it in OpenCloud in the browser.")
                            }
                        }
                        .padding(.vertical, 2)
                    }
                }
                if hidden > 0 {
                    Section { Text("\(hidden) item(s) that weren't in a kept folder are hidden; turn on Show all to see them.").font(.caption).foregroundStyle(.secondary) }
                }
            }
        }
        .formStyle(.grouped)
        .task { await load() }
        .onChange(of: showAll) { Task { await load() } }
    }

    private func load() async {
        loading = true
        defer { loading = false }
        do {
            let r = try await model.listTrash(all: showAll)
            items = r.items; problems = r.problems; hidden = r.hidden; error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func restore(_ it: DavHistory.TrashItem) async {
        let after = model.config.paused ? "It comes to this computer when you resume syncing." : "The folder then syncs it to this Mac."
        guard model.confirm("Restore “\(it.path)” to where it was?", after, button: "Restore") else { return }
        restoring = it.id
        defer { restoring = nil }
        await model.restoreTrash(it)
        await load()
    }
}

// MARK: - AppModel: File History… and Deleted Files

extension AppModel {
    /// The menu's File History…: choose a file in a kept folder.
    func fileHistory() { history(startingAt: config.localRootURL) }

    /// Kept Folders ▸ File History…: choose a file in this folder.
    func fileHistory(from f: KeptFolder) { history(startingAt: config.localURL(f)) }

    /// The kept folder a local file is in, and its path in there (nil: not in one).
    func keptItem(_ url: URL) -> (KeptFolder, String)? {
        let p = url.standardizedFileURL.resolvingSymlinksInPath().path
        for f in config.kept {
            let root = config.localURL(f).standardizedFileURL.resolvingSymlinksInPath().path
            if Config.foldKey(p).hasPrefix(Config.foldKey(root) + "/") { return (f, String(p.dropFirst(root.count + 1))) }
        }
        return nil
    }

    /// The other Unicode normalization of a path (precomposed ⇄ decomposed), nil if the same.
    nonisolated static func otherNormalization(_ s: String) -> String? {
        let pre = s.precomposedStringWithCanonicalMapping, de = s.decomposedStringWithCanonicalMapping
        let alt = s.unicodeScalars.elementsEqual(pre.unicodeScalars) ? de : pre
        return alt.unicodeScalars.elementsEqual(s.unicodeScalars) ? nil : alt
    }

    private func history(startingAt dir: URL) {
        guard config.isOIDC, !needsSignIn else { alert("Sign in first", "File history needs the browser sign-in."); return }
        let panel = NSOpenPanel()
        panel.canChooseDirectories = false
        panel.canChooseFiles = true
        panel.allowsMultipleSelection = false
        panel.directoryURL = dir
        panel.prompt = "Show History"
        panel.message = "Choose a file in a kept folder to see the earlier versions OpenCloud keeps of it."
        NSApp.activate(ignoringOtherApps: true)
        guard panel.runModal() == .OK, let url = panel.url else { return }
        guard let (f, rel) = keptItem(url), let parts = DavHistory.plainSegments(rel),
              !parts.contains(where: { $0 == Backups.folderName }), !Changes.skip(rel) else {
            alert("That isn't a file in a kept folder", "Choose a file inside one of your kept folders.")
            return
        }
        guard FileManager.default.fileExists(atPath: stateFile(f, ".initialised").path) else {
            alert("“\(f.displayName)” hasn't synced yet", "Its files have a history after its first sync.")
            return
        }
        let local = config.localURL(f).appendingPathComponent(rel)
        let name = (rel as NSString).lastPathComponent
        Task {
            // Find the file and its versions.
            let server = config.serverURL ?? ""
            var dav: URL, info: DavHistory.FileInfo, meta: URL, versions: [DavHistory.Version]
            do {
                guard let u = SharesAPI.davURL(config, f, rel: rel) else { throw AuthError.message("That path isn't valid.") }
                dav = u
                let token = try await Auth.accessToken(serverURL: server)
                do {
                    info = try await DavHistory.fileInfo(dav, token: token)
                } catch is DavHistory.NotInOpenCloud {
                    // The server may keep the name in the other Unicode form than this Mac.
                    guard let alt = Self.otherNormalization(rel), let u2 = SharesAPI.davURL(config, f, rel: alt) else { throw DavHistory.NotInOpenCloud() }
                    info = try await DavHistory.fileInfo(u2, token: token)
                    dav = u2
                }
                guard let m = DavHistory.metaURL(config, f, fileID: info.id) else {
                    throw AuthError.message("This server's WebDAV paths aren't the kind this version understands.")
                }
                meta = m
                versions = try await DavHistory.listVersions(meta, token: token)
            } catch {
                alert("Couldn't show the history of “\(name)”", error.localizedDescription)
                return
            }
            guard !versions.isEmpty else { alert("No earlier versions", "OpenCloud has no earlier versions of “\(name)”."); return }
            guard let v = DavHistory.askVersion(name, versions, paused: config.paused) else { return }
            // Restore it.
            do {
                guard !folderBusy(f), !exclusiveRunning else { throw AuthError.message("“\(f.displayName)” is syncing. Try again when it finishes.") }
                guard DavHistory.localMatches(local, info) else { throw AuthError.message(DavHistory.unsyncedText) }
                // Keep this Mac's copy first, off the main actor; nothing is restored if that fails.
                let dir = Backups.localDir(config.stateURL, f.stateName, Backups.stamp())
                do {
                    try await Task.detached { try DavHistory.backup(local, to: dir, rel: rel) }.value
                } catch {
                    throw AuthError.message("Couldn't keep the current file in the backups, so nothing was restored: \(error.localizedDescription)")
                }
                guard !folderBusy(f), !exclusiveRunning else { throw AuthError.message("“\(f.displayName)” started syncing. Try again when it finishes.") }
                let token = try await Auth.accessToken(serverURL: server)   // again: the picker may have stayed open long
                // And the file again, right before the COPY: changed meanwhile here or by another client?
                let now = try await DavHistory.fileInfo(dav, token: token)
                guard now.id == info.id, DavHistory.localMatches(local, now) else { throw AuthError.message(DavHistory.unsyncedText) }
                try await DavHistory.restoreVersion(meta, key: v.key, file: dav, token: token)
                Log.line("restored an earlier version of \(f.displayName)/\(rel)")
                let paused = config.paused
                syncNow(f)
                alert("Restored “\(name)”", paused ? "The version is the current file in OpenCloud. It comes to this computer when you resume syncing."
                                                    : "The version is the current file in OpenCloud. The folder syncs now, which brings it to this Mac.")
            } catch {
                alert("Couldn't restore “\(name)”", error.localizedDescription)
            }
        }
    }

    /// Deleted items, newest first: without `all`, only those that were in a kept folder (then
    /// capped at 2000, as in Go); the spaces whose trash couldn't be read; how many are hidden.
    func listTrash(all: Bool) async throws -> (items: [DavHistory.TrashItem], problems: [String], hidden: Int) {
        guard config.isOIDC, !needsSignIn else { throw AuthError.message("Deleted files need the browser sign-in.") }
        let server = config.serverURL ?? ""
        let token = try await Auth.accessToken(serverURL: server)
        let origin = Config.origin(server)
        var items: [DavHistory.TrashItem] = [], hidden = 0
        let (spaces, spaceProblems) = try await DavHistory.spaces(config, token: token)
        var problems = spaceProblems
        for sp in spaces {
            do {
                for it in try await DavHistory.listTrash(origin: origin, sp, token: token) {
                    if !all && it.folderKey == nil { hidden += 1 } else { items.append(it) }
                }
            } catch { problems.append("\(sp.name): \(error.localizedDescription)") }
        }
        items.sort { ($0.deleted ?? .distantPast) > ($1.deleted ?? .distantPast) }
        return (Array(items.prefix(2000)), problems, hidden)
    }

    /// Restores one item (looked up on the server again) if it was in a kept folder and nothing
    /// is at its place, in OpenCloud or on this Mac. The folder then syncs.
    func restoreTrash(_ item: DavHistory.TrashItem) async {
        do {
            let server = config.serverURL ?? ""
            let token = try await Auth.accessToken(serverURL: server)
            let origin = Config.origin(server)
            guard let sp = try await DavHistory.spaces(config, token: token).0.first(where: { $0.id == item.spaceID }) else {
                throw AuthError.message("That space isn't one folders are kept from.")
            }
            guard let it = try await DavHistory.listTrash(origin: origin, sp, token: token).first(where: { $0.key == item.key }) else {
                throw AuthError.message("It isn't in the trash any more. Reload the list.")
            }
            guard let f = DavHistory.keptFolder(sp.kept, containing: it.path) else {
                throw AuthError.message("“\(it.path)” wasn't in a kept folder. Restore it in OpenCloud in the browser.")
            }
            let fp = f.path.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
            let rel = it.path == fp ? "" : String(it.path.dropFirst(fp.count + 1))
            if rel.split(separator: "/").contains(where: { String($0) == Backups.folderName }) {
                throw AuthError.message("That item can't be restored from here.")
            }
            // attributesOfItem doesn't follow symbolic links: a broken one counts as there (as Go's Lstat).
            if !rel.isEmpty, (try? FileManager.default.attributesOfItem(atPath: config.localURL(f).appendingPathComponent(rel).path)) != nil {
                throw AuthError.message(config.paused ? "Something is at “\(it.path)” on this Mac. Resume syncing and wait for the folder to sync, then restore."
                                                      : "Something is at “\(it.path)” on this Mac. Wait for the folder to sync, then restore.")
            }
            try await DavHistory.restore(origin: origin, sp, it, token: token)
            Log.line("restored \(it.path) from the trash of \(sp.name)")
            let paused = config.paused
            syncNow(f)
            alert("Restored “\(it.name)”", paused ? "It's back in OpenCloud. It comes to this computer when you resume syncing."
                                                 : "It's back in OpenCloud. The folder syncs now, which brings it to this Mac.")
        } catch {
            alert("Couldn't restore “\(item.name)”", error.localizedDescription)
        }
    }
}
