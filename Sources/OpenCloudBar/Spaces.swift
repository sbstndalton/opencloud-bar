import Foundation
import SwiftUI

/// Spaces: OpenCloud keeps project spaces and accepted shares as drives next to the personal
/// one. GET /graph/v1.0/me/drives lists them (libre-graph): id, name, driveType ("personal",
/// "project", "virtual" — the Shares overview — or "mountpoint", one accepted share) and
/// root.webDavUrl. Kept folders from a space sync with the same on-the-fly WebDAV remote as the
/// personal space, pointed at that URL's path. Same rules as the Go app (cross/spaces.go),
/// which has the tests.
struct Space: Identifiable, Hashable, Sendable {
    var id: String
    var name: String
    var type: String      // "project" or "mountpoint" (shared with you)
    var davPath: String
    var ref: SpaceRef { SpaceRef(id: id, name: name, davPath: davPath) }
    var label: String { type == "mountpoint" ? "\(name) (shared with you)" : name }
}

enum SpacesAPI {
    /// Project spaces and accepted shares that aren't disabled, on this server, sorted by name.
    static func parse(_ obj: [String: Any], base: String) -> [Space] {
        let host = URL(string: base)?.host?.lowercased()
        var out: [Space] = []
        for d in (obj["value"] as? [[String: Any]]) ?? [] {
            let type = d["driveType"] as? String ?? ""
            guard type == "project" || type == "mountpoint", let id = d["id"] as? String, !id.isEmpty else { continue }
            let root = d["root"] as? [String: Any] ?? [:]
            if let del = root["deleted"], !(del is NSNull) { continue }
            guard let s = root["webDavUrl"] as? String, let u = URLComponents(string: s) else { continue }
            if let h = u.host?.lowercased(), let host, h != host { continue }
            var p = u.percentEncodedPath
            while p.hasSuffix("/") { p.removeLast() }
            guard p.hasPrefix("/"), !p.contains("'"), !p.contains(" ") else { continue }
            let name = ((d["name"] as? String) ?? "").trimmingCharacters(in: .whitespaces)
            out.append(Space(id: id, name: name.isEmpty ? id : name, type: type, davPath: p))
        }
        return out.sorted { Config.foldKey($0.name) < Config.foldKey($1.name) }
    }

    static func list(serverURL: String) async throws -> [Space] {
        let token = try await Auth.accessToken(serverURL: serverURL)
        guard let url = URL(string: serverURL + "/graph/v1.0/me/drives") else { throw AuthError.message("The server address isn't valid") }
        var req = URLRequest(url: url)
        req.timeoutInterval = 20
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        let (data, resp) = try await URLSession.shared.data(for: req)
        guard (resp as? HTTPURLResponse)?.statusCode == 200,
              let obj = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw AuthError.message("Couldn't list the spaces (the server answered \((resp as? HTTPURLResponse)?.statusCode ?? 0))")
        }
        return parse(obj, base: serverURL)
    }

    /// The WebDAV URL of a folder in a space, for the server's API (not rclone).
    static func davURL(base: String, _ s: Space, path: String) -> URL? {
        var u = Config.origin(base) + s.davPath   // davPath already holds the server's path prefix
        for part in path.split(separator: "/") {
            u += "/" + (String(part).addingPercentEncoding(withAllowedCharacters: .urlPathAllowed.subtracting(CharacterSet(charactersIn: "/"))) ?? "")
        }
        return URL(string: u + "/")
    }

    /// A folder's oc:permissions (PROPFIND, depth 0), like "RDNVCK"; nil if the server didn't say.
    static func permissions(_ url: URL, token: String) async throws -> String? {
        var req = URLRequest(url: url)
        req.httpMethod = "PROPFIND"
        req.timeoutInterval = 20
        req.setValue("0", forHTTPHeaderField: "Depth")
        req.setValue("application/xml", forHTTPHeaderField: "Content-Type")
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.httpBody = Data(#"<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:permissions/></d:prop></d:propfind>"#.utf8)
        let (data, resp) = try await URLSession.shared.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if code == 404 { throw AuthError.message("That folder doesn't exist in the space.") }
        guard code == 207 else { throw AuthError.message("The server answered \(code).") }
        let text = String(decoding: data, as: UTF8.self)
        guard let r = text.range(of: #"<[A-Za-z0-9]*:?permissions>([^<]*)</"#, options: .regularExpression) else { return nil }
        let tag = text[r]
        guard let open = tag.firstIndex(of: ">"), let close = tag.range(of: "</", options: .backwards) else { return nil }
        let p = String(tag[tag.index(after: open)..<close.lowerBound]).trimmingCharacters(in: .whitespaces)
        return p.isEmpty ? nil : p
    }

    /// No C/K (create) and no W (write): files can't be added or changed there.
    static func readOnly(_ perms: String?) -> Bool {
        guard let p = perms else { return false }
        return !p.contains(where: { "CKW".contains($0) })
    }
}

/// Kept Folders ▸ Keep from a Space…: choose a space, then a folder in it.
struct KeepFromSpaceSheet: View {
    @ObservedObject var model: AppModel
    var close: () -> Void
    @Local private var spaces: [Space]?
    @Local private var spaceID = ""
    @Local private var path = ""
    @Local private var folders: [String]?
    @Local private var error: String?
    @Local private var busy = false
    @Local private var loadFailed = false

    private var space: Space? { spaces?.first { $0.id == spaceID } }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Keep a Folder from a Space").font(.headline)
            Text("Project spaces and folders shared with you. Kept folders from a space go to \(model.config.localRoot)/<space name>/…. Read-only folders can't be kept, and the OpenCloud drive shows the personal space only.")
                .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if loadFailed {
                Button("Retry") { Task { await load() } }
            } else if let spaces {
                if spaces.isEmpty {
                    Text("No project spaces or shares are available to you.").foregroundStyle(.secondary)
                } else {
                    Picker("Space", selection: Binding(get: { spaceID }, set: { spaceID = $0; path = ""; folders = nil; browse() })) {
                        ForEach(spaces) { Text($0.label).tag($0.id) }
                    }
                    HStack {
                        TextField("Folder in the space", text: $path, prompt: Text("Projects/2026"))
                        Button("Up") { path = (path as NSString).deletingLastPathComponent; browse() }.disabled(path.isEmpty)
                        Button("List") { browse() }
                    }
                    if let folders {
                        List(folders, id: \.self) { d in
                            Button(d + "/") { path = path.isEmpty ? d : path + "/" + d; browse() }.buttonStyle(.link)
                        }
                        .frame(minHeight: 120)
                        .overlay { if folders.isEmpty { Text("No folders here.").foregroundStyle(.secondary) } }
                    }
                }
            } else {
                ProgressView("Loading spaces…")
            }
            InlineError(text: error)
            HStack {
                Spacer()
                Button("Cancel") { close() }.keyboardShortcut(.cancelAction)
                Button("Keep on This Mac") {
                    guard let space else { return }
                    busy = true
                    Task {
                        error = await model.keepFolder(in: space, path: path)
                        busy = false
                        if error == nil { close() }
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(space == nil || path.trimmingCharacters(in: .whitespaces).isEmpty || busy || model.syncing)
            }
        }
        .padding(20)
        .frame(width: 480)
        .task { await load() }
    }

    private func load() async {
        loadFailed = false
        spaces = nil
        error = nil
        do {
            let s = try await SpacesAPI.list(serverURL: model.config.serverURL ?? "")
            spaces = s
            if spaceID.isEmpty, let first = s.first { spaceID = first.id; browse() }
        } catch {
            loadFailed = true
            self.error = error.localizedDescription
        }
    }

    private func browse() {
        guard let space else { return }
        Task {
            switch await model.browse(space, path: path) {
            case .success(let d): folders = d; error = nil
            case .failure(let e): folders = nil; error = e.localizedDescription
            }
        }
    }
}
