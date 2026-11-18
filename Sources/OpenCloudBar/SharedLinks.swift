import AppKit
import Foundation
import SwiftUI

/// Public links you made: reuse an existing one, and list, copy, re-date and remove them, with
/// OpenCloud's libre-graph API (opencloud-eu/opencloud, services/graph). Same as the Go app
/// (cross/links.go), which has the tests and the source references:
///   GET    /graph/v1beta1/drives/<drive>/items/<item>/permissions   (api_driveitem_permissions.go ListPermissions)
///   GET    /graph/v1beta1/me/drive/sharedByMe                       (sharedbyme.go GetSharedByMe)
///   GET    …/items/<item>/permissions/<perm>                        (GetPermission), fetched fresh before every change
///   PATCH  …/items/<item>/permissions/<perm>  {"expirationDateTime": "…Z" | null}   (null removes the expiry;
///          api_driveitem_permissions_links.go updatePublicLinkPermission)
///   DELETE …/items/<item>/permissions/<perm>                        (204)
/// Safety: removing a link ends it for everyone who has it, and PATCH also updates a share with a
/// person, so ids from the page are only used to pick an entry of the server's own sharedByMe
/// answer; the ids sent come from that answer; the permission is then fetched again by id and must
/// have a "link" facet, or nothing is changed. The token goes only to the configured server
/// (DavHistory.session follows no redirects; every address is checked against the server's host,
/// https only, except loopback for development).
enum SharedLinksAPI {
    struct Link: Identifiable, Equatable {
        var item: String      // the item's id as the server gave it
        var perm: String      // the link's id (a permission id)
        var name: String
        var dir: Bool
        var type: String      // view, edit, upload, createOnly, blocksDownload, internal
        var url: String
        var expires: Date?
        var created: Date?
        var password: Bool
        var id: String { item + "\u{1}" + perm }

        var typeText: String {
            let t: String
            switch type {
            case "view": t = "View"
            case "edit": t = "Edit"
            case "upload": t = "Upload"
            case "createOnly": t = "Upload only"
            case "blocksDownload": t = "View, no download"
            case "internal": t = "Internal"
            default: t = type.isEmpty ? "Link" : type
            }
            return password ? t + ", password" : t
        }
    }

    struct Existing: Equatable {
        var url: String
        var expires: Date?
        var created: Date?
        var password: Bool
    }

    // MARK: - Ids and addresses

    /// An id that is one plain path segment, safe to put in a request address.
    static func validID(_ id: String) -> Bool {
        !id.isEmpty && id.count <= 300 && id != "." && id != ".."
            && !id.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) })
            && !id.contains(where: { "/?#\\% ".contains($0) })
    }

    private static let seg = CharacterSet.urlPathAllowed.subtracting(CharacterSet(charactersIn: "/?#;"))
    private static func esc(_ s: String) -> String { s.addingPercentEncoding(withAllowedCharacters: seg) ?? "" }

    /// /graph/v1beta1/drives/<drive>/items/<item> for an item id "<storage>$<space>!<node>".
    static func itemPath(_ item: String) -> String? {
        guard validID(item), let bang = item.lastIndex(of: "!"), bang > item.startIndex,
              item.index(after: bang) < item.endIndex else { return nil }
        let drive = String(item[..<bang]), node = String(item[item.index(after: bang)...])
        guard validID(drive), validID(node) else { return nil }
        return "/graph/v1beta1/drives/\(esc(drive))/items/\(esc(item))"
    }

    private static func fail(_ what: String, _ code: Int, _ data: Data) -> AuthError {
        let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
        if let m = (obj?["error"] as? [String: Any])?["message"] as? String, !m.isEmpty {
            return .message("\(what): \(clean(m))")
        }
        if code >= 300 && code < 400 { return .message("\(what) (the server sent us somewhere else, which isn't followed).") }
        return .message("\(what) (it answered \(code)).")
    }

    /// A server text made short and printable.
    static func clean(_ s: String) -> String {
        let t = String(String.UnicodeScalarView(s.unicodeScalars.map { CharacterSet.controlCharacters.contains($0) ? " " : $0 }))
        return t.count > 200 ? String(t.prefix(200)) + "…" : t
    }

    /// One request to the configured server. The address is the server's address plus a path
    /// built here, and must stay on the same scheme, host and port.
    static func request(_ method: String, server: String, path: String, token: String, body: Data? = nil) async throws -> (Int, Data) {
        let base = server.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        guard let b = URL(string: base), let url = URL(string: base + path),
              url.host == b.host, url.scheme == b.scheme, url.port == b.port, url.user == nil,
              b.scheme == "https" || (b.scheme == "http" && ["localhost", "127.0.0.1", "::1", "[::1]"].contains(b.host ?? "")) else {
            throw AuthError.message("That isn't an address on your OpenCloud server.")
        }
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.timeoutInterval = 30
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        if let body {
            req.httpBody = body
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        do {
            let (data, resp) = try await DavHistory.session.data(for: req)   // no redirects: the token stays with the server
            return ((resp as? HTTPURLResponse)?.statusCode ?? 0, data)
        } catch {
            throw AuthError.message("The server didn't answer.")
        }
    }

    // MARK: - Parsing

    static func date(_ s: String?) -> Date? {
        guard let s, !s.isEmpty else { return nil }
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f.date(from: s)
    }

    private static func isHTTPS(_ s: String) -> Bool {
        guard let u = URL(string: s) else { return false }
        return u.scheme == "https" && u.host?.isEmpty == false
    }

    /// The links in a sharedByMe answer: permissions with a "link" facet only, never shares with a
    /// person or group; entries whose ids aren't plain segments are left out. nil: not the expected JSON.
    static func parse(_ data: Data) -> [Link]? {
        guard let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any],
              let value = obj["value"] as? [[String: Any]] else { return nil }
        var out: [Link] = []
        for it in value {
            guard let id = it["id"] as? String, itemPath(id) != nil else { continue }
            let name = clean((it["name"] as? String) ?? "")
            let dir = it["folder"] is [String: Any]
            for p in (it["permissions"] as? [[String: Any]]) ?? [] {
                guard let link = p["link"] as? [String: Any], let pid = p["id"] as? String, validID(pid) else { continue }
                out.append(Link(item: id, perm: pid, name: name, dir: dir, type: clean((link["type"] as? String) ?? ""),
                                url: (link["webUrl"] as? String) ?? "", expires: date(p["expirationDateTime"] as? String),
                                created: date(p["createdDateTime"] as? String), password: (p["hasPassword"] as? Bool) ?? false))
            }
        }
        return out.sorted {
            let c = $0.name.localizedCaseInsensitiveCompare($1.name)
            return c != .orderedSame ? c == .orderedAscending : ($0.created ?? .distantPast) > ($1.created ?? .distantPast)
        }
    }

    /// The view link to offer again: not expired, https; one without a password first (it is the
    /// one a plain Copy Public Link would make), then the newest.
    static func pickExisting(_ perms: [[String: Any]], now: Date) -> Existing? {
        var best: Existing?
        for p in perms {
            guard let link = p["link"] as? [String: Any], (link["type"] as? String) == "view",
                  let url = link["webUrl"] as? String, isHTTPS(url) else { continue }
            let exp = date(p["expirationDateTime"] as? String)
            if let exp, exp <= now { continue }
            let c = Existing(url: url, expires: exp, created: date(p["createdDateTime"] as? String), password: (p["hasPassword"] as? Bool) ?? false)
            if let b = best {
                if b.password && !c.password || (b.password == c.password && (c.created ?? .distantPast) > (b.created ?? .distantPast)) { best = c }
            } else {
                best = c
            }
        }
        return best
    }

    // MARK: - Calls

    /// The item's view link that could be reused, if it has one.
    static func existingViewLink(server: String, token: String, item: String, now: Date = Date()) async throws -> Existing? {
        guard let p = itemPath(item) else { throw AuthError.message("The server's item id isn't one this version understands.") }
        let (code, data) = try await request("GET", server: server, path: p + "/permissions", token: token)
        guard code == 200 else { throw fail("OpenCloud didn't list the item's links", code, data) }
        guard let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any],
              let value = obj["value"] as? [[String: Any]] else { throw AuthError.message("The server's answer wasn't what was expected.") }
        return pickExisting(value, now: now)
    }

    /// The links you made.
    static func list(server: String, token: String) async throws -> [Link] {
        let (code, data) = try await request("GET", server: server, path: "/graph/v1beta1/me/drive/sharedByMe", token: token)
        guard code == 200 else { throw fail("OpenCloud didn't list your links", code, data) }
        guard let l = parse(data) else { throw AuthError.message("The server's answer wasn't what was expected.") }
        return l
    }

    /// The permission fetched by id; it must have a "link" facet.
    static func linkPermission(server: String, token: String, item: String, perm: String) async throws -> [String: Any] {
        guard let p = itemPath(item) else { throw AuthError.message("The server's item id isn't one this version understands.") }
        guard validID(perm) else { throw AuthError.message("The server's link id isn't one this version understands.") }
        let (code, data) = try await request("GET", server: server, path: p + "/permissions/" + esc(perm), token: token)
        if code == 404 { throw AuthError.message("That link doesn't exist any more.") }
        guard code == 200 else { throw fail("OpenCloud didn't look up the link", code, data) }
        guard let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else {
            throw AuthError.message("The server's answer wasn't what was expected.")
        }
        guard (obj["id"] as? String) == perm else {
            throw AuthError.message("The server answered about another permission, so nothing was changed.")
        }
        guard obj["link"] is [String: Any] else {
            throw AuthError.message("That isn't a public link (it is a share with a person or group), so it was left alone.")
        }
        return obj
    }

    /// Finds (item, perm) in the server's own list of your links, then fetches the permission
    /// again by id. What it returns is what the server says now.
    static func resolve(server: String, token: String, item: String, perm: String) async throws -> Link {
        guard validID(item), validID(perm) else { throw AuthError.message("That isn't a link this version understands.") }
        let all = try await list(server: server, token: token)
        guard var l = all.first(where: { $0.item == item && $0.perm == perm }) else {
            throw AuthError.message("That link isn't in your shared links any more. Reload the list.")
        }
        let fresh = try await linkPermission(server: server, token: token, item: l.item, perm: l.perm)
        let link = fresh["link"] as? [String: Any] ?? [:]
        l.url = (link["webUrl"] as? String) ?? ""
        l.type = clean((link["type"] as? String) ?? "")
        l.expires = date(fresh["expirationDateTime"] as? String)
        l.password = (fresh["hasPassword"] as? Bool) ?? false
        if let c = date(fresh["createdDateTime"] as? String) { l.created = c }
        return l
    }

    /// The link's address as the server says now (for Copy).
    static func linkURL(server: String, token: String, item: String, perm: String) async throws -> String {
        let l = try await resolve(server: server, token: token, item: item, perm: perm)
        guard isHTTPS(l.url) else { throw AuthError.message("The server's answer had no https link.") }
        return l.url
    }

    /// Sets the link's expiry; nil removes it (an explicit null).
    static func setExpiry(server: String, token: String, item: String, perm: String, to exp: Date?) async throws {
        let l = try await resolve(server: server, token: token, item: item, perm: perm)
        guard let p = itemPath(l.item) else { throw AuthError.message("The server's item id isn't one this version understands.") }
        var v: Any = NSNull()
        if let exp {
            let f = ISO8601DateFormatter()
            f.formatOptions = [.withInternetDateTime]
            v = f.string(from: exp)
        }
        let body = try JSONSerialization.data(withJSONObject: ["expirationDateTime": v])
        let (code, data) = try await request("PATCH", server: server, path: p + "/permissions/" + esc(l.perm), token: token, body: body)
        guard code == 200 || code == 204 else { throw fail("OpenCloud didn't change the link's expiry", code, data) }
    }

    /// Deletes the link. `wantURL` is the address shown when the user confirmed; if the link now
    /// has another address, nothing is deleted.
    static func remove(server: String, token: String, item: String, perm: String, wantURL: String) async throws {
        let l = try await resolve(server: server, token: token, item: item, perm: perm)
        guard !wantURL.isEmpty, l.url == wantURL else {
            throw AuthError.message("That link changed since the list was loaded, so it was not removed. Reload the list.")
        }
        guard let p = itemPath(l.item) else { throw AuthError.message("The server's item id isn't one this version understands.") }
        let (code, data) = try await request("DELETE", server: server, path: p + "/permissions/" + esc(l.perm), token: token)
        guard code == 200 || code == 204 else { throw fail("OpenCloud didn't remove the link", code, data) }
    }

    /// The new expiry for a choice: `days` from now (7 or 30), or a calendar day (valid until the
    /// end of that day on the server; not in the past, within 10 years). Neither: no expiry (nil).
    static func expiry(days: Int?, day: Date?, now: Date = Date(), calendar: Calendar = .current) throws -> Date? {
        if let day {
            var utc = Calendar(identifier: .gregorian)
            utc.timeZone = TimeZone(identifier: "UTC")!
            let c = calendar.dateComponents([.year, .month, .day], from: day)
            let t = calendar.dateComponents([.year, .month, .day], from: now)
            guard let d = utc.date(from: c), let today = utc.date(from: t) else { throw AuthError.message("That isn't a date.") }
            if d < today { throw AuthError.message("That date has passed.") }
            if d > utc.date(byAdding: .year, value: 10, to: today)! { throw AuthError.message("That date is too far away (10 years at most).") }
            return d
        }
        switch days {
        case nil, 0: return nil
        case 7, 30: return now.addingTimeInterval(TimeInterval(days!) * 86400)
        default: throw AuthError.message("The expiry can be none, 7 days, 30 days or a date.")
        }
    }
}

#if DEBUG
extension SharedLinksAPI {
    /// The fixture of `sharedByMeJSON` in cross/v260_test.go and what the Go test expects of it.
    /// Run at startup in DEBUG builds (`--self-check` runs it and quits).
    static func selfCheck() {
        let json = """
        {"value":[
         {"id":"st$sp!aaa","name":"Q1 report.pdf","file":{},"permissions":[
           {"id":"link-view","createdDateTime":"2026-11-01T10:00:00Z","expirationDateTime":"2026-12-01T23:59:59Z","hasPassword":false,"link":{"type":"view","webUrl":"https://cloud.example.org/s/AAA"}},
           {"id":"user-1","grantedToV2":{"user":{"id":"u1","displayName":"Ann"}},"roles":["x"]}]},
         {"id":"st$sp!bbb","name":"Docs","folder":{},"permissions":[
           {"id":"link-edit","createdDateTime":"2026-11-05T10:00:00Z","hasPassword":true,"link":{"type":"edit","webUrl":"https://cloud.example.org/s/BBB"}},
           {"id":"group-1","grantedToV2":{"group":{"id":"g1","displayName":"Team"}}}]},
         {"id":"st$sp!ccc","name":"Only people","file":{},"permissions":[
           {"id":"user-2","grantedToV2":{"user":{"id":"u2"}}}]},
         {"id":"st$sp!d/d","name":"Bad item id","file":{},"permissions":[{"id":"x","link":{"type":"view","webUrl":"https://cloud.example.org/s/X"}}]},
         {"id":"st$sp!eee","name":"Bad perm id","file":{},"permissions":[{"id":"a/b","link":{"type":"view","webUrl":"https://cloud.example.org/s/Y"}}]}
        ]}
        """
        guard let l = parse(Data(json.utf8)) else { preconditionFailure("SharedLinks self-check: not parsed") }
        precondition(l.count == 2, "SharedLinks self-check: links \(l)")
        precondition(l[0].name == "Docs" && l[0].dir && l[0].type == "edit" && l[0].password && l[0].perm == "link-edit"
                     && l[0].item == "st$sp!bbb" && l[0].expires == nil, "SharedLinks self-check: \(l[0])")
        precondition(l[1].name == "Q1 report.pdf" && !l[1].dir && l[1].type == "view" && l[1].perm == "link-view"
                     && l[1].url == "https://cloud.example.org/s/AAA" && l[1].expires != nil, "SharedLinks self-check: \(l[1])")
        precondition(parse(Data("<html>".utf8)) == nil, "SharedLinks self-check: not JSON")
        precondition(parse(Data(#"{"value":[{"id":"st$sp!a","name":"n","permissions":[{"id":"p","link":null}]}]}"#.utf8))?.isEmpty == true,
                     "SharedLinks self-check: null link")
        // Ids: one plain segment.
        for bad in ["", ".", "..", "a/b", "a?b", "a#b", "a\\b", "a%2Fb", "a b", "a\nb"] {
            precondition(!validID(bad), "SharedLinks self-check: \(bad) accepted")
        }
        for ok in ["link-view", "st$sp!aaa", "YWJj=="] { precondition(validID(ok), "SharedLinks self-check: \(ok) refused") }
        for bad in ["nobang", "!x", "x!", "st$sp!a/b", "st$sp!a?b", "st$sp!a#b", "st$sp!a\\b", "../x!y", "st$sp!.."] {
            precondition(itemPath(bad) == nil, "SharedLinks self-check: item \(bad) accepted")
        }
        precondition(itemPath("st$sp!aaa") == "/graph/v1beta1/drives/st$sp/items/st$sp!aaa", "SharedLinks self-check: path \(itemPath("st$sp!aaa") ?? "nil")")
        // The view link to reuse: not the edit, expired, password or http one; the newest plain one.
        let now = date("2026-11-18T12:00:00Z")!
        let perms: [[String: Any]] = [
            ["id": "u", "grantedToV2": ["user": ["id": "u1"]]],
            ["id": "edit", "createdDateTime": "2026-11-09T00:00:00Z", "link": ["type": "edit", "webUrl": "https://cloud.example.org/s/EDIT"]],
            ["id": "expired", "createdDateTime": "2026-11-10T00:00:00Z", "expirationDateTime": "2026-11-17T23:59:59Z", "link": ["type": "view", "webUrl": "https://cloud.example.org/s/OLD"]],
            ["id": "pw", "createdDateTime": "2026-11-12T00:00:00Z", "hasPassword": true, "link": ["type": "view", "webUrl": "https://cloud.example.org/s/PW"]],
            ["id": "plain1", "createdDateTime": "2026-11-02T00:00:00Z", "link": ["type": "view", "webUrl": "https://cloud.example.org/s/ONE"]],
            ["id": "plain2", "createdDateTime": "2026-11-03T00:00:00Z", "expirationDateTime": "2026-12-24T23:59:59Z", "link": ["type": "view", "webUrl": "https://cloud.example.org/s/TWO"]],
            ["id": "http", "createdDateTime": "2026-11-15T00:00:00Z", "link": ["type": "view", "webUrl": "http://cloud.example.org/s/PLAIN"]],
        ]
        let ex = pickExisting(perms, now: now)
        precondition(ex?.url == "https://cloud.example.org/s/TWO" && ex?.password == false, "SharedLinks self-check: existing \(String(describing: ex))")
        precondition(pickExisting([perms[3]], now: now)?.password == true, "SharedLinks self-check: password link")
        precondition(pickExisting([perms[0], perms[1]], now: now) == nil, "SharedLinks self-check: none")
        // Expiry choices.
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(identifier: "UTC")!
        let n = date("2026-11-18T15:30:00Z")!
        func run(_ days: Int?, _ day: Date?) -> (ok: Bool, d: Date?) {
            do { return (true, try expiry(days: days, day: day, now: n, calendar: cal)) } catch { return (false, nil) }
        }
        precondition(run(0, nil).ok && run(0, nil).d == nil, "SharedLinks self-check: no expiry")
        precondition(run(7, nil).d == date("2026-11-25T15:30:00Z"), "SharedLinks self-check: 7 days")
        precondition(run(30, nil).d == date("2026-12-18T15:30:00Z"), "SharedLinks self-check: 30 days")
        precondition(run(nil, date("2027-01-31T00:00:00Z")).d == date("2027-01-31T00:00:00Z"), "SharedLinks self-check: date")
        precondition(run(nil, date("2026-11-18T00:00:00Z")).ok, "SharedLinks self-check: today")
        for bad in [(3, nil as Date?), (-1, nil), (0, date("2026-11-17T00:00:00Z")), (0, date("2040-01-01T00:00:00Z"))] {
            precondition(!run(bad.0, bad.1).ok, "SharedLinks self-check: \(bad) accepted")
        }
    }
}
#endif

// MARK: - The Shared Links page

/// Settings ▸ Shared Links: the public links you made, from OpenCloud. Links only.
struct SharedLinksPage: View {
    @ObservedObject var model: AppModel
    @Local private var links: [SharedLinksAPI.Link] = []
    @Local private var loading = false
    @Local private var error: String?
    @Local private var busy: String?

    var body: some View {
        Form {
            Section {
                Text("The public links you made, from all your files in OpenCloud. Links only: shares with people or groups aren't listed here. Removing a link ends it for everyone who has it, and it can't be undone.")
                    .font(.caption).foregroundStyle(.secondary)
                HStack {
                    Spacer()
                    Button("Reload") { Task { await load() } }.disabled(loading).accessibilityLabel("Reload")
                }
            }
            if let error {
                Section { Text(error).foregroundStyle(.secondary) }
            } else if loading && links.isEmpty {
                Section { ProgressView().controlSize(.small) }
            } else if links.isEmpty {
                Section { Text("You haven't made any public links.").foregroundStyle(.secondary) }
            } else {
                Section(links.count == 1 ? "1 link" : "\(links.count) links") {
                    ForEach(links) { l in row(l) }
                }
            }
        }
        .formStyle(.grouped)
        .task { await load() }
    }

    private func name(_ l: SharedLinksAPI.Link) -> String { l.name + (l.dir ? "/" : "") }

    private func row(_ l: SharedLinksAPI.Link) -> some View {
        let expired = l.expires.map { $0 <= Date() } ?? false
        return HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 3) {
                Text(name(l)).textSelection(.enabled)
                Text([l.typeText,
                      l.expires.map { (expired ? "Expired " : "Expires ") + $0.formatted(date: .abbreviated, time: .omitted) } ?? "Never expires",
                      l.created.map { "Created " + $0.formatted(date: .abbreviated, time: .shortened) }]
                        .compactMap { $0 }.joined(separator: " · "))
                    .font(.caption).foregroundStyle(.secondary)
            }
            .accessibilityElement(children: .combine)
            Spacer()
            Button("Copy") { Task { await model.copySharedLink(l) } }
                .accessibilityLabel("Copy the link to \(name(l))")
            Menu("Change Expiry") {
                Button("No Expiry") { Task { await change(l, days: 0, day: nil) } }
                Button("In 7 Days") { Task { await change(l, days: 7, day: nil) } }
                Button("In 30 Days") { Task { await change(l, days: 30, day: nil) } }
                Button("On a Date…") { if let d = model.askExpiryDate(for: name(l), current: l.expires) { Task { await change(l, days: nil, day: d) } } }
            }
            .fixedSize()
            .accessibilityLabel("Change expiry of the link to \(name(l))")
            Button("Remove Link…", role: .destructive) { Task { await remove(l) } }
                .accessibilityLabel("Remove the link to \(name(l))")
        }
        .disabled(busy != nil)
        .padding(.vertical, 2)
    }

    private func load() async {
        loading = true
        defer { loading = false }
        do {
            links = try await model.listSharedLinks()
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func change(_ l: SharedLinksAPI.Link, days: Int?, day: Date?) async {
        busy = l.id
        defer { busy = nil }
        await model.changeSharedLinkExpiry(l, days: days, day: day)
        await load()
    }

    private func remove(_ l: SharedLinksAPI.Link) async {
        busy = l.id
        defer { busy = nil }
        await model.removeSharedLink(l)
        await load()
    }
}

// MARK: - AppModel: Shared Links, and opening the item itself

extension AppModel {
    /// The server and a token, for the browser sign-in only.
    private func linksAccess() async throws -> (String, String) {
        guard config.isOIDC, !needsSignIn else { throw AuthError.message("Shared links need the browser sign-in. Sign in first.") }
        let server = config.serverURL ?? ""
        return (server, try await Auth.accessToken(serverURL: server))
    }

    func listSharedLinks() async throws -> [SharedLinksAPI.Link] {
        let (server, token) = try await linksAccess()
        return try await SharedLinksAPI.list(server: server, token: token)
    }

    /// Copy: the address as the server says now.
    func copySharedLink(_ l: SharedLinksAPI.Link) async {
        do {
            let (server, token) = try await linksAccess()
            let url = try await SharedLinksAPI.linkURL(server: server, token: token, item: l.item, perm: l.perm)
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(url, forType: .string)
            flash("Link to “\(l.name)” copied")
        } catch {
            alert("Couldn't copy the link", error.localizedDescription)
        }
    }

    func changeSharedLinkExpiry(_ l: SharedLinksAPI.Link, days: Int?, day: Date?) async {
        do {
            let exp = try SharedLinksAPI.expiry(days: days, day: day)
            let (server, token) = try await linksAccess()
            try await SharedLinksAPI.setExpiry(server: server, token: token, item: l.item, perm: l.perm, to: exp)
            Log.line("shared link expiry changed")
            flash("Expiry of the link to “\(l.name)” changed")
        } catch {
            alert("Couldn't change the expiry", error.localizedDescription)
        }
    }

    /// Remove: a confirm dialog with the item and the link's address first; Cancel is the default.
    func removeSharedLink(_ l: SharedLinksAPI.Link) async {
        let shown = l.name + (l.dir ? "/" : "")
        guard confirmRisky("Remove the public link to “\(shown)”?",
                           "\(l.url)\n\nAnyone who has this link loses access to it. This can't be undone. The item itself isn't deleted.",
                           button: "Remove Link") else { return }
        do {
            let (server, token) = try await linksAccess()
            try await SharedLinksAPI.remove(server: server, token: token, item: l.item, perm: l.perm, wantURL: l.url)
            Log.line("shared link removed")
            flash("Link to “\(shown)” removed")
        } catch {
            alert("Couldn't remove the link", error.localizedDescription)
        }
    }

    /// Asks for an expiry date. nil if cancelled.
    func askExpiryDate(for name: String, current: Date?) -> Date? {
        let a = NSAlert()
        a.messageText = "Expire the link to “\(name)” on…"
        a.informativeText = "The link works until the end of that day."
        a.addButton(withTitle: "Set Expiry")
        a.addButton(withTitle: "Cancel")
        let p = NSDatePicker(frame: NSRect(x: 0, y: 0, width: 180, height: 24))
        p.datePickerStyle = .textFieldAndStepper
        p.datePickerElements = .yearMonthDay
        p.minDate = Date()
        p.dateValue = max(current ?? Date().addingTimeInterval(7 * 86400), Date())
        p.setAccessibilityLabel("Expiry date")
        a.accessoryView = p
        NSApp.activate(ignoringOtherApps: true)
        return a.runModal() == .alertFirstButtonReturn ? p.dateValue : nil
    }

    /// Open the item itself in OpenCloud's web interface (<server>/f/<file id>, from oc:fileid by
    /// PROPFIND). `rel` "" is the kept folder itself. Without the browser sign-in there is no token to ask
    /// with, so it opens the web interface as before.
    func openItemInBrowser(_ f: KeptFolder, rel: String) {
        guard let server = config.serverURL, !server.isEmpty else { return }
        guard config.isOIDC, !needsSignIn else {
            if let u = URL(string: server) { NSWorkspace.shared.open(u) }
            return
        }
        Task {
            do {
                guard let dav = SharesAPI.davURL(config, f, rel: rel) else { throw AuthError.message("That path isn't valid.") }
                let token = try await Auth.accessToken(serverURL: server)
                let id = try await SharesAPI.fileID(dav, token: token)
                guard let u = ServerSearch.webURL(server: server, id: id) else { throw AuthError.message("That isn't an item OpenCloud can show.") }
                NSWorkspace.shared.open(u)
            } catch {
                alert("Couldn't open it in OpenCloud", error.localizedDescription)
            }
        }
    }

    /// Activity ▸ Open OpenCloud: the changed file itself.
    func openChangeInBrowser(_ c: Change) {
        guard let f = config.kept.first(where: { $0.key == c.folder }) else { openWeb(); return }
        openItemInBrowser(f, rel: c.path)
    }
}
