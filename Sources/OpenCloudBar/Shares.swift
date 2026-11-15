import AppKit
import Foundation

/// Share links: a public, read-only link to a file or folder in a kept folder, made with
/// OpenCloud's libre-graph API. PROPFIND the item for oc:fileid ("<storage>$<space>!<node>"),
/// then POST /graph/v1beta1/drives/<storage>$<space>/items/<fileid>/createLink with
/// {"type": "view", "expirationDateTime"?, "password"?}; the answer's link.webUrl is the link.
/// The password is typed for this one link and never stored or logged. Same as the Go app
/// (cross/shares.go), which has the tests.
enum SharesAPI {
    struct Options { var days = 0; var password = "" }

    /// The WebDAV URL (for the server's API, not rclone) of `rel` in kept folder `f`.
    static func davURL(_ c: Config, _ f: KeptFolder, rel: String) -> URL? {
        let base = (c.serverURL ?? "").trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        var u = f.space.map { Config.origin(base) + $0.davPath } ?? base + c.davPath
        let allowed = CharacterSet.urlPathAllowed.subtracting(CharacterSet(charactersIn: "/?#"))
        for part in (f.path + "/" + rel).split(separator: "/") {
            if part == "." || part == ".." || part.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) { return nil }
            u += "/" + (String(part).addingPercentEncoding(withAllowedCharacters: allowed) ?? "")
        }
        return URL(string: u)
    }

    static func fileID(_ url: URL, token: String) async throws -> String {
        var req = URLRequest(url: url)
        req.httpMethod = "PROPFIND"
        req.timeoutInterval = 20
        req.setValue("0", forHTTPHeaderField: "Depth")
        req.setValue("application/xml", forHTTPHeaderField: "Content-Type")
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.httpBody = Data(#"<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:fileid/></d:prop></d:propfind>"#.utf8)
        let (data, resp) = try await URLSession.shared.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if code == 404 { throw AuthError.message("It isn't in OpenCloud yet. Wait for the folder to sync, then try again.") }
        guard code == 207 else { throw AuthError.message("The server answered \(code).") }
        let text = String(decoding: data, as: UTF8.self)
        guard let r = text.range(of: #"<[A-Za-z0-9]*:?fileid>[^<]+</"#, options: .regularExpression) else {
            throw AuthError.message("The server didn't say which file that is.")
        }
        let tag = text[r]
        guard let open = tag.firstIndex(of: ">"), let close = tag.range(of: "</", options: .backwards) else {
            throw AuthError.message("The server didn't say which file that is.")
        }
        return String(tag[tag.index(after: open)..<close.lowerBound]).trimmingCharacters(in: .whitespaces)
    }

    static func createLink(_ dav: URL, serverURL: String, token: String, _ o: Options, now: Date = Date()) async throws -> String {
        let id = try await fileID(dav, token: token)
        guard let bang = id.lastIndex(of: "!"), bang > id.startIndex, !id.contains(where: { "/?#".contains($0) }) else {
            throw AuthError.message("The server's file id isn't one this version understands.")
        }
        let drive = String(id[..<bang])
        var body: [String: Any] = ["type": "view"]
        if o.days > 0 {
            let f = ISO8601DateFormatter()
            f.formatOptions = [.withInternetDateTime]
            body["expirationDateTime"] = f.string(from: now.addingTimeInterval(TimeInterval(o.days) * 86400))
        }
        if !o.password.isEmpty { body["password"] = o.password }
        let seg = CharacterSet.urlPathAllowed.subtracting(CharacterSet(charactersIn: "/?#"))
        let base = serverURL.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        guard let url = URL(string: base + "/graph/v1beta1/drives/" + (drive.addingPercentEncoding(withAllowedCharacters: seg) ?? "")
                            + "/items/" + (id.addingPercentEncoding(withAllowedCharacters: seg) ?? "") + "/createLink") else {
            throw AuthError.message("The server address isn't valid.")
        }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.timeoutInterval = 20
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.httpBody = try JSONSerialization.data(withJSONObject: body)
        let (data, resp) = try await URLSession.shared.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
        guard code == 200 || code == 201 else {
            if let m = (obj?["error"] as? [String: Any])?["message"] as? String, !m.isEmpty {
                throw AuthError.message("OpenCloud didn't make the link: \(String(m.prefix(200)))")
            }
            throw AuthError.message("OpenCloud didn't make the link (it answered \(code)).")
        }
        // A public link must be https (OpenCloud may give it its own public host name).
        guard let link = (obj?["link"] as? [String: Any])?["webUrl"] as? String,
              let u = URL(string: link), u.scheme == "https", u.host?.isEmpty == false else {
            throw AuthError.message("The server's answer had no https link.")
        }
        return link
    }

    /// Asks for the link's expiry and an optional password. nil if cancelled.
    @MainActor
    static func askOptions(for name: String) -> Options? {
        let a = NSAlert()
        a.messageText = "Copy a public link to “\(name)”?"
        a.informativeText = "Anyone with the link can view and download it, without signing in. It stays active until you delete it in OpenCloud (in the browser: the file's sharing panel), unless you pick an expiry. The password, if you set one, isn't stored on this Mac."
        a.addButton(withTitle: "Create and Copy Public Link")
        a.addButton(withTitle: "Cancel")
        let expiry = NSPopUpButton(frame: .zero, pullsDown: false)
        expiry.addItems(withTitles: ["Never expires", "Expires after 1 day", "Expires after 7 days", "Expires after 30 days"])
        expiry.setAccessibilityLabel("Link expires")
        let pw = NSSecureTextField(frame: NSRect(x: 0, y: 0, width: 260, height: 24))
        pw.placeholderString = "Password (optional)"
        pw.setAccessibilityLabel("Password (optional)")
        let stack = NSStackView(views: [expiry, pw])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 8
        stack.frame = NSRect(x: 0, y: 0, width: 260, height: 58)
        a.accessoryView = stack
        a.window.initialFirstResponder = expiry
        NSApp.activate(ignoringOtherApps: true)
        guard a.runModal() == .alertFirstButtonReturn else { return nil }
        return Options(days: [0, 1, 7, 30][max(0, expiry.indexOfSelectedItem)], password: pw.stringValue)
    }
}
