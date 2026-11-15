import AppKit
import CryptoKit
import Foundation
import Network
import Security

// Browser sign-in for OpenCloud (OpenID Connect, authorization code flow with PKCE).
// Same design and file formats as cross/auth.go:
//   - the refresh token lives in a private file, refresh-token (0600, folder 0700), next to
//     config.json; the short-lived access token is cached in auth.json (0600), with its expiry;
//   - `OpenCloudBar --token` prints a valid access token. rclone runs it as its bearer token command.

enum AuthError: LocalizedError {
    case signedOut
    case message(String)
    var errorDescription: String? {
        switch self {
        case .signedOut: return Auth.signedOutText
        case .message(let m): return m
        }
    }
}

/// auth.json: who is signed in and the cached access token. The refresh token is not in here.
struct AuthState: Codable {
    var serverURL: String
    var issuer: String
    var username: String
    var tokenEndpoint: String
    var accessToken: String?
    var expiry: Date?
    var account: String { "\(issuer)|\(username)" }
}

struct LoginResult {
    var serverURL: String
    var issuer: String
    var tokenEndpoint: String
    var username: String
    var refresh: String
    var access: String
    var expiry: Date
}

enum Auth {
    static let clientID = "OpenCloudDesktop"   // OpenCloud's public desktop client: no secret
    static let scopes = "openid profile email offline_access"
    static let keychainService = "OpenCloud Bar"
    static let refreshMargin: TimeInterval = 60
    static let loginTimeout: TimeInterval = 300
    /// rclone's output contains this when the token command found the sign-in gone.
    static let signedOutText = "signed out: sign in again"

    static var authURL: URL { Config.supportURL.appendingPathComponent("auth.json") }
    static var lockURL: URL { Config.supportURL.appendingPathComponent("auth.lock") }

    // MARK: server address and discovery

    /// "cloud.example.com" -> "https://cloud.example.com". Plain http only for loopback (development).
    static func normalizeServerURL(_ raw: String) throws -> String {
        var s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if s.isEmpty { throw AuthError.message("Enter the server address, for example https://cloud.example.com") }
        if !s.contains("://") { s = "https://" + s }
        guard var c = URLComponents(string: s), let host = c.host, !host.isEmpty else {
            throw AuthError.message("“\(raw)” isn't a valid server address")
        }
        guard secureOrLoopback(c.scheme, host) else {
            throw AuthError.message("The server address must start with https://")
        }
        c.query = nil; c.fragment = nil
        while c.path.hasSuffix("/") { c.path.removeLast() }
        return c.string ?? s
    }

    private static func secureOrLoopback(_ scheme: String?, _ host: String) -> Bool {
        scheme == "https" || (scheme == "http" && ["localhost", "127.0.0.1", "::1", "[::1]"].contains(host.lowercased()))
    }

    struct Discovery: Decodable {
        var issuer: String
        var authorization_endpoint: String
        var token_endpoint: String
        var userinfo_endpoint: String
    }

    /// Reads <server>/.well-known/openid-configuration. Endpoints are never hard-coded.
    static func discover(_ serverURL: String) async throws -> Discovery {
        guard let u = URL(string: serverURL + "/.well-known/openid-configuration") else {
            throw AuthError.message("Invalid server address")
        }
        var req = URLRequest(url: u); req.timeoutInterval = 20
        let (data, resp): (Data, URLResponse)
        do { (data, resp) = try await URLSession.shared.data(for: req) }
        catch { throw AuthError.message("Can't reach \(serverURL): \(error.localizedDescription)") }
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard code == 200 else {
            throw AuthError.message("\(serverURL) doesn't look like an OpenCloud server (sign-in settings: HTTP \(code))")
        }
        guard let d = try? JSONDecoder().decode(Discovery.self, from: data),
              !d.issuer.isEmpty, !d.authorization_endpoint.isEmpty, !d.token_endpoint.isEmpty, !d.userinfo_endpoint.isEmpty else {
            throw AuthError.message("\(serverURL) returned incomplete sign-in settings")
        }
        for e in [d.authorization_endpoint, d.token_endpoint, d.userinfo_endpoint] {
            guard let c = URLComponents(string: e), let h = c.host, secureOrLoopback(c.scheme, h) else {
                throw AuthError.message("The server's sign-in settings contain an insecure URL")
            }
        }
        return d
    }

    // MARK: PKCE

    static func randomString(_ n: Int) -> String {
        var b = [UInt8](repeating: 0, count: n)
        guard SecRandomCopyBytes(kSecRandomDefault, n, &b) == errSecSuccess else {
            fatalError("no randomness, no safe sign-in")
        }
        return base64url(Data(b))
    }

    static func base64url(_ d: Data) -> String {
        d.base64EncodedString().replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
    }

    static func pkceChallenge(_ verifier: String) -> String {
        base64url(Data(SHA256.hash(data: Data(verifier.utf8))))
    }

    // MARK: token endpoint

    private struct TokenResponse: Decodable {
        var access_token: String?
        var refresh_token: String?
        var expires_in: Int?
        var error: String?
        var error_description: String?
    }

    private struct OAuthError: Error { var code: String; var status: Int }

    private static func postToken(_ endpoint: String, _ form: [String: String]) async throws -> (access: String, refresh: String?, expiry: Date) {
        guard let u = URL(string: endpoint) else { throw AuthError.message("Invalid token endpoint") }
        var req = URLRequest(url: u)
        req.httpMethod = "POST"
        req.timeoutInterval = 20
        req.setValue("application/x-www-form-urlencoded", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        var allowed = CharacterSet.alphanumerics; allowed.insert(charactersIn: "-._~")
        req.httpBody = form.map { "\($0.key)=\($0.value.addingPercentEncoding(withAllowedCharacters: allowed) ?? "")" }
            .joined(separator: "&").data(using: .utf8)
        let (data, resp) = try await URLSession.shared.data(for: req)
        let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
        let tr = try? JSONDecoder().decode(TokenResponse.self, from: data)
        if status != 200 || tr?.error != nil {
            throw OAuthError(code: tr?.error ?? "http_\(status)", status: status)
        }
        guard let access = tr?.access_token, !access.isEmpty else {
            throw AuthError.message("The server's token response had no access token")
        }
        // The server says about 300 s; use that guess if it says nothing.
        let secs = (tr?.expires_in ?? 0) > 0 ? tr!.expires_in! : 300
        return (access, tr?.refresh_token, Date().addingTimeInterval(TimeInterval(secs)))
    }

    // MARK: login

    /// Runs the whole browser flow: discovery, browser, loopback redirect, code exchange, userinfo.
    static func login(serverURL raw: String) async throws -> LoginResult {
        let serverURL = try normalizeServerURL(raw)
        let d = try await discover(serverURL)
        let verifier = randomString(32), state = randomString(16)

        let server = try CallbackServer(state: state)
        let port = try await server.start()
        defer { server.stop() }
        let redirect = "http://127.0.0.1:\(port)/"

        guard var au = URLComponents(string: d.authorization_endpoint) else { throw AuthError.message("Invalid sign-in page") }
        // Encode values strictly (":" and "/" too). URLComponents leaves them as is, and a
        // query like "redirect_uri=http://..." is rejected (403) by common reverse-proxy
        // exploit filters, such as Nginx Proxy Manager's "Block Common Exploits".
        var strict = CharacterSet.alphanumerics
        strict.insert(charactersIn: "-._~")
        func enc(_ s: String) -> String { s.addingPercentEncoding(withAllowedCharacters: strict) ?? s }
        au.percentEncodedQueryItems = (au.percentEncodedQueryItems ?? []) + [
            ("response_type", "code"), ("client_id", clientID), ("redirect_uri", redirect),
            ("scope", scopes), ("state", state), ("code_challenge", pkceChallenge(verifier)),
            ("code_challenge_method", "S256"),
        ].map { URLQueryItem(name: $0.0, value: enc($0.1)) }
        guard let url = au.url else { throw AuthError.message("Invalid sign-in page") }
        let opened = await MainActor.run { NSWorkspace.shared.open(url) }
        if !opened { Log.line("couldn't open the browser for sign-in") }

        let code = try await withTaskCancellationHandler {
            try await server.waitForCode(timeout: loginTimeout)
        } onCancel: { server.cancelWaiting() }

        let tokens: (access: String, refresh: String?, expiry: Date)
        do {
            tokens = try await postToken(d.token_endpoint, [
                "grant_type": "authorization_code", "code": code, "redirect_uri": redirect,
                "client_id": clientID, "code_verifier": verifier,
            ])
        } catch let e as OAuthError {
            throw AuthError.message("Sign-in failed at the token step (\(e.code))")
        }
        guard let refresh = tokens.refresh, !refresh.isEmpty else {
            throw AuthError.message("The server didn't issue a refresh token (offline_access); sign-in can't be kept")
        }
        let user = try await fetchUsername(d.userinfo_endpoint, access: tokens.access)
        return LoginResult(serverURL: serverURL, issuer: d.issuer, tokenEndpoint: d.token_endpoint, username: user,
                           refresh: refresh, access: tokens.access, expiry: tokens.expiry)
    }

    private static func fetchUsername(_ endpoint: String, access: String) async throws -> String {
        guard let u = URL(string: endpoint) else { throw AuthError.message("Invalid userinfo endpoint") }
        var req = URLRequest(url: u); req.timeoutInterval = 20
        req.setValue("Bearer \(access)", forHTTPHeaderField: "Authorization")
        let (data, resp) = try await URLSession.shared.data(for: req)
        guard (resp as? HTTPURLResponse)?.statusCode == 200 else { throw AuthError.message("Couldn't read your account from the server") }
        guard let o = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let name = o["preferred_username"] as? String, !name.isEmpty else {
            throw AuthError.message("The server didn't say which user signed in (no preferred_username)")
        }
        return name
    }

    /// The whole setup: browser sign-in, keep the credentials, record them in config.json.
    static func signIn(serverURL: String) async throws -> String {
        let r = try await login(serverURL: serverURL)
        try store(r)
        try Config.applySetup(serverURL: r.serverURL, username: r.username)
        Log.line("signed in as \(r.username) on \(r.serverURL)")
        return r.username
    }

    // MARK: storage

    // Where the refresh token is kept, and why not in the Keychain: every build of this app is
    // ad-hoc signed, so each update has a new code identity. A Keychain item made by an older
    // build then makes macOS ask "OpenCloud Bar wants to use your confidential information…",
    // and a Keychain read waits for that answer: the app hung at launch after updates, and the
    // `--token` helper (run by rclone, with nobody to answer) could hang too. A file readable
    // only by you is the same protection rclone.conf and the Windows/Linux app's fallback use.
    // Older versions stored the token in the Keychain; AppModel moves it to the file once
    // (off the main thread, giving up after 60 s), then deletes the Keychain item.
    static var refreshURL: URL { Config.supportURL.appendingPathComponent("refresh-token") }

    static func readRefresh() -> String? {
        guard let t = try? String(contentsOf: refreshURL, encoding: .utf8)
            .trimmingCharacters(in: .whitespacesAndNewlines), !t.isEmpty else { return nil }
        return t
    }

    static func writeRefresh(_ token: String) throws {
        let fm = FileManager.default
        try fm.createDirectory(at: Config.supportURL, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try? fm.setAttributes([.posixPermissions: 0o700], ofItemAtPath: Config.supportURL.path)
        let tmp = refreshURL.appendingPathExtension("tmp")
        guard fm.createFile(atPath: tmp.path, contents: Data(token.utf8), attributes: [.posixPermissions: 0o600]) else {
            throw AuthError.message("Couldn't keep the sign-in")
        }
        if fm.fileExists(atPath: refreshURL.path) {
            _ = try fm.replaceItemAt(refreshURL, withItemAt: tmp)
        } else {
            try fm.moveItem(at: tmp, to: refreshURL)
        }
    }

    /// An older version may have left the refresh token in the Keychain: signed in (auth.json)
    /// but no token file yet.
    static var keychainMigrationPending: Bool { readState() != nil && readRefresh() == nil }

    /// The one-time move from the Keychain to the file. Blocks while macOS asks for permission,
    /// so it must run off the main thread. Returns true if the token was moved.
    static func migrateFromKeychain() -> Bool {
        guard let s = readState(), readRefresh() == nil, let t = Keychain.get(s.account) else { return false }
        // The user may have signed in again while macOS was asking: keep that newer sign-in.
        guard readRefresh() == nil, readState()?.account == s.account else { Keychain.delete(s.account); return false }
        do { try writeRefresh(t) } catch { return false }
        Keychain.delete(s.account)
        Log.line("sign-in moved from the Keychain to a private file")
        return true
    }

    static func readState() -> AuthState? {
        guard let data = try? Data(contentsOf: authURL) else { return nil }
        let dec = JSONDecoder(); dec.dateDecodingStrategy = .iso8601
        guard let s = try? dec.decode(AuthState.self, from: data), !s.issuer.isEmpty, !s.username.isEmpty else { return nil }
        return s
    }

    static func writeState(_ s: AuthState) throws {
        try FileManager.default.createDirectory(at: Config.supportURL, withIntermediateDirectories: true)
        let enc = JSONEncoder(); enc.dateEncodingStrategy = .iso8601
        enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        let tmp = authURL.appendingPathExtension("tmp")
        FileManager.default.createFile(atPath: tmp.path, contents: try enc.encode(s), attributes: [.posixPermissions: 0o600])
        _ = try FileManager.default.replaceItemAt(authURL, withItemAt: tmp)
    }

    private static func store(_ r: LoginResult) throws {
        let s = AuthState(serverURL: r.serverURL, issuer: r.issuer, username: r.username, tokenEndpoint: r.tokenEndpoint,
                          accessToken: r.access, expiry: r.expiry)
        try writeRefresh(r.refresh)   // replaces any earlier account's token
        try writeState(s)
    }

    /// True if a refresh token is stored for this config's server.
    static func haveCredentials(_ c: Config) -> Bool {
        guard let want = try? normalizeServerURL(c.serverURL ?? ""), let s = readState(), s.serverURL == want else { return false }
        return readRefresh() != nil   // a file read: never a Keychain prompt
    }

    /// Forgets the sign-in on this Mac. (The discovery document lists no revocation endpoint.)
    static func logout() {
        if let s = readState(), readRefresh() == nil {
            // Not moved to the file yet: the token may still be in the Keychain. Deleting can
            // also wait for a permission prompt, so it runs in the background.
            let account = s.account
            DispatchQueue.global().async { Keychain.delete(account) }
        }
        try? FileManager.default.removeItem(at: refreshURL)
        try? FileManager.default.removeItem(at: authURL)
        Log.line("signed out")
    }

    // MARK: access token for rclone

    /// Cross-process lock (also used by `--token` processes): a file created exclusively, with
    /// stale locks removed after 90 s (longer than a refresh can take).
    private static func lock() async throws {
        try FileManager.default.createDirectory(at: Config.supportURL, withIntermediateDirectories: true)
        let deadline = Date().addingTimeInterval(45)
        while true {
            let fd = open(lockURL.path, O_CREAT | O_EXCL | O_WRONLY, 0o600)
            if fd >= 0 { close(fd); return }
            if errno != EEXIST { throw AuthError.message("Couldn't lock the sign-in files") }
            if let m = (try? FileManager.default.attributesOfItem(atPath: lockURL.path))?[.modificationDate] as? Date,
               Date().timeIntervalSince(m) > 90 {
                try? FileManager.default.removeItem(at: lockURL); continue
            }
            if Date() > deadline { throw AuthError.message("Timed out waiting for another refresh of the sign-in") }
            try await Task.sleep(nanoseconds: 100_000_000)
        }
    }
    private static func unlock() { try? FileManager.default.removeItem(at: lockURL) }

    /// An access token valid for at least a minute: the cached one, else refreshed (once, even
    /// when several rclone processes ask together). A missing or revoked sign-in throws `.signedOut`.
    static func accessToken(serverURL raw: String) async throws -> String {
        guard let want = try? normalizeServerURL(raw) else { throw AuthError.signedOut }
        func fresh(_ s: AuthState) -> Bool {
            s.serverURL == want && !(s.accessToken ?? "").isEmpty && (s.expiry?.timeIntervalSinceNow ?? 0) > refreshMargin
        }
        guard let first = readState(), first.serverURL == want else { throw AuthError.signedOut }
        if fresh(first), let t = first.accessToken { return t }

        try await lock()
        defer { unlock() }
        // Someone else may have refreshed while we waited.
        guard var st = readState(), st.serverURL == want else { throw AuthError.signedOut }
        if fresh(st), let t = st.accessToken { return t }
        guard let refresh = readRefresh() else { throw AuthError.signedOut }   // never the Keychain: see refreshURL
        do {
            let r = try await postToken(st.tokenEndpoint, ["grant_type": "refresh_token", "refresh_token": refresh, "client_id": clientID])
            // The server may rotate the refresh token; if it sends none, the old one stays valid.
            if let nr = r.refresh, !nr.isEmpty, nr != refresh { try writeRefresh(nr) }
            st.accessToken = r.access; st.expiry = r.expiry
            try writeState(st)
            return r.access
        } catch let e as OAuthError where e.code == "invalid_grant" {
            Log.line("refresh token rejected (invalid_grant); signed out")
            try? FileManager.default.removeItem(at: refreshURL)
            st.accessToken = nil; st.expiry = nil
            try? writeState(st)
            throw AuthError.signedOut
        } catch let e as AuthError {
            throw e
        } catch {
            throw AuthError.message("Couldn't refresh the sign-in: \(error.localizedDescription)")
        }
    }

    /// rclone's bearer_token_command: this program with --token. It splits on spaces and honours
    /// double quotes, and the app path contains spaces ("OpenCloud Bar.app"), so it is quoted.
    static func bearerTokenCommand(exe: String? = nil) -> String {
        let path = exe ?? Bundle.main.executablePath ?? CommandLine.arguments[0]
        return "\"" + path.replacingOccurrences(of: "\"", with: "\"\"") + "\" --token"
    }

    /// `OpenCloudBar --token`: print a token (or an error on stderr, exit 1) and exit before any UI.
    static func runTokenMode() -> Never {
        Task {
            do {
                let (cfg, err) = Config.load()
                if let err { throw AuthError.message(err) }
                let t = try await accessToken(serverURL: cfg.serverURL ?? "")
                FileHandle.standardOutput.write(Data((t + "\n").utf8))
                exit(0)
            } catch {
                FileHandle.standardError.write(Data("opencloud-bar: \(error.localizedDescription)\n".utf8))
                exit(1)
            }
        }
        dispatchMain()
    }
}

// MARK: Keychain (only to move an older version's sign-in to the file, then delete it)

enum Keychain {
    private static func query(_ account: String) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword,
         kSecAttrService as String: Auth.keychainService, kSecAttrAccount as String: account]
    }

    static func get(_ account: String) -> String? {
        var q = query(account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let d = out as? Data else { return nil }
        return String(data: d, encoding: .utf8)
    }

    static func delete(_ account: String) { SecItemDelete(query(account) as CFDictionary) }
}

// MARK: loopback redirect

/// Listens on 127.0.0.1 (random free port) for the browser's redirect. Requests with a wrong or
/// missing state are refused and ignored: anything on this Mac can reach the port, but only the
/// browser we opened knows the state.
final class CallbackServer: @unchecked Sendable {
    private let listener: NWListener
    private let queue = DispatchQueue(label: "opencloudbar.callback")
    private let state: String
    private var waiter: CheckedContinuation<String, Error>?
    private var pending: Result<String, Error>?

    init(state: String) throws {
        self.state = state
        let p = NWParameters.tcp
        p.requiredLocalEndpoint = NWEndpoint.hostPort(host: .ipv4(.loopback), port: .any)
        listener = try NWListener(using: p)
    }

    func start() async throws -> UInt16 {
        try await withCheckedThrowingContinuation { cont in
            let once = Flag()
            listener.stateUpdateHandler = { [listener] s in
                switch s {
                case .ready:
                    if !once.value, let port = listener.port { once.set(); cont.resume(returning: port.rawValue) }
                case .failed(let e):
                    if !once.value { once.set(); cont.resume(throwing: AuthError.message("Can't listen for the sign-in: \(e)")) }
                default: break
                }
            }
            listener.newConnectionHandler = { [weak self] c in self?.handle(c) }
            listener.start(queue: queue)
        }
    }

    func stop() { listener.cancel() }

    func cancelWaiting() { deliver(.failure(CancellationError())) }

    func waitForCode(timeout: TimeInterval) async throws -> String {
        try await withCheckedThrowingContinuation { cont in
            queue.async {
                if let r = self.pending { cont.resume(with: r); return }
                self.waiter = cont
            }
            queue.asyncAfter(deadline: .now() + timeout) {
                self.deliver(.failure(AuthError.message("Timed out waiting for the sign-in in the browser")))
            }
        }
    }

    /// First result wins. Runs on any thread; hops to the queue.
    private func deliver(_ r: Result<String, Error>) {
        queue.async {
            guard self.pending == nil else { return }
            self.pending = r
            if let w = self.waiter { self.waiter = nil; w.resume(with: r) }
        }
    }

    private func handle(_ c: NWConnection) {
        c.start(queue: queue)
        c.receive(minimumIncompleteLength: 1, maximumLength: 16384) { [weak self] data, _, _, _ in
            guard let self, let data, let text = String(data: data, encoding: .utf8) else { c.cancel(); return }
            let parts = (text.components(separatedBy: "\r\n").first ?? "").split(separator: " ")
            guard parts.count >= 2, let u = URLComponents(string: "http://127.0.0.1" + parts[1]) else {
                self.respond(c, 400, "Sign-in incomplete", "Bad request."); return
            }
            guard u.path == "/" else { self.respond(c, 404, "Not found", ""); return }
            let q = Dictionary((u.queryItems ?? []).map { ($0.name, $0.value ?? "") }, uniquingKeysWith: { a, _ in a })
            guard q["state"] == self.state else {
                self.respond(c, 400, "Sign-in not recognised",
                             "This link doesn't belong to the sign-in that OpenCloud Bar started. Go back to OpenCloud Bar and try again.")
                return
            }
            if let e = q["error"] {
                self.respond(c, 200, "Sign-in cancelled", "You can close this tab and return to OpenCloud Bar.")
                self.deliver(.failure(AuthError.message("The server refused the sign-in: \(e) \(q["error_description"] ?? "")")))
                return
            }
            guard let code = q["code"], !code.isEmpty else {
                self.respond(c, 400, "Sign-in incomplete", "The server didn't send a sign-in code."); return
            }
            self.respond(c, 200, "You're signed in", "You can close this tab and return to OpenCloud Bar.")
            self.deliver(.success(code))
        }
    }

    private func respond(_ c: NWConnection, _ status: Int, _ title: String, _ body: String) {
        func esc(_ s: String) -> String {
            s.replacingOccurrences(of: "&", with: "&amp;").replacingOccurrences(of: "<", with: "&lt;")
                .replacingOccurrences(of: ">", with: "&gt;")
        }
        let html = "<!doctype html><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\"><title>OpenCloud Bar</title>"
            + "<body style=\"font:16px system-ui,sans-serif;text-align:center;margin-top:20vh\"><h2>\(esc(title))</h2><p>\(esc(body))</p>"
        let head = "HTTP/1.1 \(status) \(status == 200 ? "OK" : "Error")\r\nContent-Type: text/html; charset=utf-8\r\n"
            + "Content-Length: \(html.utf8.count)\r\nCache-Control: no-store\r\nReferrer-Policy: no-referrer\r\nConnection: close\r\n\r\n"
        c.send(content: Data((head + html).utf8), completion: .contentProcessed { _ in c.cancel() })
    }
}
