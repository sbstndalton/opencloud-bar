import AppKit
import Foundation
import SwiftUI

/// Search OpenCloud…: file and folder names across everything the user can see, the way the
/// web UI's search box does it. Same request and rules as the Go app (cross/search.go, which
/// has the tests and the source references):
///
///   REPORT <dav>/spaces/ with an oc:search-files body whose oc:pattern is name:"*<term>*"
///   (quotes doubled) and oc:limit 50 → 207, href <dav>/spaces/<space ref>/<path>, oc:fileid,
///   oc:file-parent, oc:name, getlastmodified, resourcetype, getcontentlength / oc:size;
///   Content-Range: rows 0-N/<total>.
///
/// OpenCloud's webdav service answers it (services/webdav/pkg/service/v0/search.go; reva's own
/// ocdav answers 501). <dav>/spaces/ is the personal space's root.webDavUrl path without its
/// last segment, against the configured server's origin. "Show in Browser" opens
/// <server>/f/<the folder's id> (the web UI's private-link route). The bearer token only goes
/// to the server: DavHistory.session follows no redirects.
enum ServerSearch {
    static let limit = 50

    struct Result: Identifiable, Hashable {
        var name: String
        var folder: String          // "/Docs", "Marketing: /Plans"
        var modified: Date?
        var dir: Bool
        var size: Int64
        var webID: String?          // what Show in Browser opens (its folder's id)
        var local: URL?             // its copy on this Mac, when it is in a kept folder and there
        var id: String
    }

    struct Space { var id: String; var name: String; var kept: [KeptFolder] }

    static func cleanTerm(_ t: String) -> String? {
        let term = t.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !term.isEmpty, term.count <= 200,
              !term.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else { return nil }
        return term
    }

    static func query(_ term: String) -> String { #"name:"*"# + term.replacingOccurrences(of: "\"", with: "\"\"") + #"*""# }

    static func xmlEscape(_ s: String) -> String {
        s.replacingOccurrences(of: "&", with: "&amp;").replacingOccurrences(of: "<", with: "&lt;")
            .replacingOccurrences(of: ">", with: "&gt;").replacingOccurrences(of: "\"", with: "&quot;")
    }

    static func body(_ term: String) -> String {
        #"<?xml version="1.0"?><oc:search-files xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop>"#
            + "<oc:fileid/><oc:file-parent/><oc:name/><d:getlastmodified/><d:resourcetype/><d:getcontentlength/><oc:size/>"
            + "</d:prop><oc:search><oc:pattern>" + xmlEscape(query(term)) + "</oc:pattern><oc:limit>\(limit)</oc:limit></oc:search></oc:search-files>"
    }

    /// <dav>/spaces/ for the personal space root <dav>/spaces/<id>.
    static func spacesRoot(origin: String, personalDavPath: String) -> String? {
        var p = personalDavPath
        while p.hasSuffix("/") { p.removeLast() }
        guard let slash = p.lastIndex(of: "/") else { return nil }
        let parent = p[..<slash], id = p[p.index(after: slash)...]
        guard parent.hasSuffix("/spaces"), !id.isEmpty else { return nil }
        return origin + parent + "/"
    }

    /// A match's space ref is the drive id, or (shares) the id without its provider part.
    static func spaceMatches(_ id: String, _ ref: String) -> Bool { !id.isEmpty && (id == ref || id.hasSuffix("$" + ref)) }

    static func validWebID(_ id: String) -> Bool {
        !id.isEmpty && id.count < 300 && !id.contains(where: { "/?#\\ ".contains($0) })
            && !id.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) })
    }

    /// <server>/f/<id>: the web UI's link for a match's folder.
    static func webURL(server: String, id: String) -> URL? {
        guard validWebID(id) else { return nil }
        let base = server.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        return URL(string: base + "/f/" + DavHistory.esc(id))
    }

    static func total(_ contentRange: String?) -> Int? {
        guard let s = contentRange, let slash = s.lastIndex(of: "/") else { return nil }
        return Int(s[s.index(after: slash)...].trimmingCharacters(in: .whitespaces))
    }

    /// The matches; repeats, hrefs outside /dav/spaces/ and unsafe paths left out.
    static func parse(_ data: Data, spaces: [Space], local: (KeptFolder, String) -> URL?) -> [Result]? {
        guard let rs = DavHistory.MultistatusParser.parse(data) else { return nil }
        var out: [Result] = [], seen = Set<String>()
        for r in rs where r.ok {
            var p = DavHistory.hrefPath(r.href)
            while p.hasSuffix("/") { p.removeLast() }
            guard let i = p.range(of: "/dav/spaces/") else { continue }
            let rest = p[i.upperBound...]
            let ref = String(rest.split(separator: "/", maxSplits: 1, omittingEmptySubsequences: false).first ?? "")
            guard !ref.isEmpty else { continue }
            let inSpace = rest.count > ref.count ? String(rest.dropFirst(ref.count + 1)) : ""
            var clean = ""
            if !inSpace.isEmpty {
                guard let seg = DavHistory.plainSegments(inSpace) else { continue }
                clean = seg.joined(separator: "/")
            }
            let fileID = r.props["fileid"] ?? ""
            if !fileID.isEmpty {
                if seen.contains(fileID) { continue }
                seen.insert(fileID)
            }
            var name = r.props["name"] ?? ""
            if name.isEmpty || name.contains("/") || name.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) {
                name = clean.isEmpty ? ref : String(clean.split(separator: "/").last ?? "")
            }
            let parent = r.props["file-parent"] ?? ""
            let webID = validWebID(parent) ? parent : (validWebID(fileID) ? fileID : nil)
            let dirPath = "/" + clean.split(separator: "/").dropLast().joined(separator: "/")
            let sp = spaces.first { spaceMatches($0.id, ref) }
            let folder = (sp.map { $0.name != "Personal" && !$0.name.isEmpty } ?? false) ? sp!.name + ": " + dirPath : dirPath
            var localURL: URL?
            if let sp, !clean.isEmpty, let f = DavHistory.keptFolder(sp.kept, containing: clean) {
                let fp = f.path.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
                let rel = clean == fp ? "" : String(clean.dropFirst(fp.count + 1))
                if !rel.isEmpty, !rel.split(separator: "/").contains(where: { String($0) == Backups.folderName }) {
                    localURL = local(f, rel)
                }
            }
            let size = (r.props["getcontentlength"] ?? r.props["size"]).flatMap { Int64($0) } ?? -1
            out.append(Result(name: name, folder: folder, modified: r.props["getlastmodified"].flatMap(DavHistory.httpDate),
                              dir: r.collection, size: size, webID: webID, local: localURL,
                              id: fileID.isEmpty ? p : fileID))
        }
        return out
    }

    /// Searches names in OpenCloud (all spaces): the matches (at most `limit`) and how many
    /// there are on the server.
    static func search(_ c: Config, term raw: String) async throws -> ([Result], Int) {
        guard let term = cleanTerm(raw) else { throw AuthError.message("Type part of a name to search for.") }
        let server = c.serverURL ?? ""
        let token = try await Auth.accessToken(serverURL: server)
        let value = try await DavHistory.drives(c, token: token)
        var spaces: [Space] = [], personal: String?
        for d in value {
            guard let id = d["id"] as? String, !id.isEmpty else { continue }
            let type = d["driveType"] as? String
            if type == "virtual" { continue }
            var sp = Space(id: id, name: (d["name"] as? String) ?? "", kept: c.kept.filter { $0.space?.id == id })
            if type == "personal", personal == nil {
                sp.name = "Personal"
                sp.kept += c.kept.filter { $0.space == nil }
                personal = DavHistory.personalSpacePath(((d["root"] as? [String: Any])?["webDavUrl"] as? String) ?? "")
            }
            spaces.append(sp)
        }
        guard let personal, let root = spacesRoot(origin: Config.origin(server), personalDavPath: personal), let url = URL(string: root) else {
            throw AuthError.message("The server didn't say where your personal space is.")
        }
        let (data, resp) = try await DavHistory.requestWithHeaders("REPORT", url, token: token, body: body(term))
        switch resp?.statusCode ?? 0 {
        case 207: break
        case 401: throw AuthError.message("Your sign-in ended. Sign in again, then try again.")
        case 400: throw AuthError.message("OpenCloud couldn't search for that. Try other words.")
        case 404, 405, 501: throw AuthError.message("This OpenCloud server doesn't offer search.")
        case let code: throw AuthError.message("OpenCloud didn't search (it answered \(code)).")
        }
        let fm = FileManager.default
        guard var results = parse(data, spaces: spaces, local: { f, rel in
            let u = c.localURL(f).appendingPathComponent(rel)
            // attributesOfItem doesn't follow symbolic links (as Go's Lstat).
            return (try? fm.attributesOfItem(atPath: u.path)) != nil ? u : nil
        }) else { throw AuthError.message("The server's answer wasn't what was expected.") }
        results = Array(results.prefix(limit))
        return (results, max(results.count, total(resp?.value(forHTTPHeaderField: "Content-Range")) ?? 0))
    }
}

/// The small Search OpenCloud window: one field and the matches.
@MainActor
final class SearchWindowController: NSObject, NSWindowDelegate {
    private var window: NSWindow?
    private unowned let model: AppModel

    init(model: AppModel) { self.model = model }

    func show() {
        if window == nil {
            let host = NSHostingController(rootView: SearchView(model: model))
            host.sizingOptions = []
            let w = NSWindow(contentViewController: host)
            w.title = "Search OpenCloud"
            w.styleMask = [.titled, .closable, .resizable]
            w.isReleasedWhenClosed = false
            w.contentMinSize = NSSize(width: 480, height: 320)
            w.setContentSize(NSSize(width: 620, height: 460))
            w.delegate = self
            w.center()
            window = w
        }
        NSApp.activate(ignoringOtherApps: true)
        window?.makeKeyAndOrderFront(nil)
    }

    func windowWillClose(_ notification: Notification) {
        DispatchQueue.main.async { [weak self] in self?.window = nil }
    }
}

struct SearchView: View {
    @ObservedObject var model: AppModel
    @Local private var term = ""
    @Local private var results: [ServerSearch.Result]?
    @Local private var total = 0
    @Local private var searching = false
    @Local private var error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                TextField("Part of a name", text: $term)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit { Task { await run() } }
                    .accessibilityLabel("Search OpenCloud")
                Button("Search") { Task { await run() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(searching || term.trimmingCharacters(in: .whitespaces).isEmpty || model.needsSignIn)
                    .accessibilityLabel("Search")
            }
            Text("Finds files and folders in OpenCloud by name, in your personal space, project spaces and shares (up to 50 matches).")
                .font(.caption).foregroundStyle(.secondary)
            if model.needsSignIn || !model.config.isOIDC {
                Text("Search needs the browser sign-in.").foregroundStyle(.secondary)
            }
            if searching {
                ProgressView().controlSize(.small)
            } else if let error {
                Text(error).foregroundStyle(.secondary)
            } else if let results {
                if results.isEmpty {
                    Text("Nothing in OpenCloud has a name like that.").foregroundStyle(.secondary)
                } else {
                    List(results) { r in row(r) }
                    if total > results.count {
                        Text("Showing \(results.count) of \(total) matches. Type more of the name to narrow it down.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            Spacer(minLength: 0)
        }
        .padding(16)
    }

    private func row(_ r: ServerSearch.Result) -> some View {
        HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 2) {
                Text(r.name + (r.dir ? "/" : "")).textSelection(.enabled)
                Text(r.folder).font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                if let m = r.modified {
                    Text("Modified " + m.formatted(date: .abbreviated, time: .shortened)).font(.caption).foregroundStyle(.secondary)
                }
            }
            Spacer()
            if let id = r.webID {
                Button("Show in Browser") { model.openSearchResultInBrowser(id) }
                    .accessibilityLabel("Show \(r.name) in the browser")
            }
            if let u = r.local {
                Button("Show on This Computer") { NSWorkspace.shared.activateFileViewerSelecting([u]) }
                    .accessibilityLabel("Show \(r.name) on this computer")
            }
        }
        .padding(.vertical, 2)
    }

    private func run() async {
        guard !searching else { return }
        searching = true
        defer { searching = false }
        do {
            guard model.config.isOIDC, !model.needsSignIn else { throw AuthError.message("Search needs the browser sign-in.") }
            let (rs, n) = try await ServerSearch.search(model.config, term: term)
            results = rs; total = n; error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}

extension AppModel {
    /// The menu's Search OpenCloud….
    func showSearch() {
        if searchWindow == nil { searchWindow = SearchWindowController(model: self) }
        searchWindow?.show()
    }

    /// Show in Browser: the match's folder in OpenCloud's web UI (the configured server only).
    func openSearchResultInBrowser(_ id: String) {
        guard let u = ServerSearch.webURL(server: config.serverURL ?? "", id: id) else { return }
        NSWorkspace.shared.open(u)
    }
}
