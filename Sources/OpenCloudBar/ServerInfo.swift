import Foundation

/// Who is signed in and how much space they use, from the server's Graph API.
struct AccountInfo: Sendable {
    var displayName: String?
    var mail: String?
    var username: String?
    var quotaTotal: Int64?
    var quotaUsed: Int64?
    var quotaRemaining: Int64?
    /// "normal", "nearing", "critical" or "exceeded".
    var quotaState: String?
    var webURL: String?
}

/// What the server says about itself (no sign-in needed).
struct ServerFacts: Sendable {
    var version: String?
    var blockedNames: [String] = []
    var undelete: Bool?
    var versioning: Bool?
}

/// Read-only requests to the OpenCloud server. The access token is only ever sent as an
/// Authorization header; it is never logged, stored or shown.
enum ServerAPI {
    enum Failure: LocalizedError {
        case http(Int)
        case unreadable
        var errorDescription: String? {
            switch self {
            case .http(let c): return "the server answered HTTP \(c)"
            case .unreadable: return "the server's answer wasn't in the expected format"
            }
        }
    }

    private static func getJSON(_ url: URL, bearer: String?) async throws -> [String: Any] {
        var req = URLRequest(url: url)
        req.timeoutInterval = 20
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        if let bearer { req.setValue("Bearer \(bearer)", forHTTPHeaderField: "Authorization") }
        let (data, resp) = try await URLSession.shared.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard code == 200 else { throw Failure.http(code) }
        guard let o = try JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw Failure.unreadable }
        return o
    }

    private static func int64(_ v: Any?) -> Int64? { (v as? NSNumber)?.int64Value }

    static func facts(serverURL: String) async throws -> ServerFacts {
        guard var c = URLComponents(string: serverURL + "/ocs/v1.php/cloud/capabilities") else { throw Failure.unreadable }
        c.queryItems = [URLQueryItem(name: "format", value: "json")]
        guard let url = c.url else { throw Failure.unreadable }
        let root = try await getJSON(url, bearer: nil)
        let data = (root["ocs"] as? [String: Any])?["data"] as? [String: Any]
        var f = ServerFacts()
        f.version = (data?["version"] as? [String: Any])?["string"] as? String
        let files = (data?["capabilities"] as? [String: Any])?["files"] as? [String: Any]
        f.blockedNames = (files?["blacklisted_files"] as? [String]) ?? []
        f.undelete = files?["undelete"] as? Bool
        f.versioning = files?["versioning"] as? Bool
        return f
    }

    static func account(serverURL: String, token: String) async throws -> AccountInfo {
        guard let meURL = URL(string: serverURL + "/graph/v1.0/me"),
              var dc = URLComponents(string: serverURL + "/graph/v1.0/me/drives") else { throw Failure.unreadable }
        dc.queryItems = [URLQueryItem(name: "$filter", value: "driveType eq 'personal'")]
        guard let drivesURL = dc.url else { throw Failure.unreadable }

        var a = AccountInfo()
        let me = try await getJSON(meURL, bearer: token)
        a.displayName = me["displayName"] as? String
        a.mail = me["mail"] as? String
        a.username = me["onPremisesSamAccountName"] as? String
        // The quota is a nice-to-have: show the account even if this request fails.
        if let drives = try? await getJSON(drivesURL, bearer: token),
           let first = (drives["value"] as? [[String: Any]])?.first {
            a.webURL = first["webUrl"] as? String
            if let q = first["quota"] as? [String: Any] {
                a.quotaTotal = int64(q["total"])
                a.quotaUsed = int64(q["used"])
                a.quotaRemaining = int64(q["remaining"])
                a.quotaState = q["state"] as? String
            }
        }
        return a
    }
}

/// Holds what the Account and Sync pages show about the server. Loaded when the window opens
/// (OIDC mode only; legacy configs have no browser sign-in to ask with).
@MainActor
final class ServerInfoStore: ObservableObject {
    @Published private(set) var account: AccountInfo?
    @Published private(set) var facts: ServerFacts?
    @Published private(set) var accountError: String?
    @Published private(set) var factsError: String?
    @Published private(set) var loading = false
    private var loadedOnce = false

    func loadIfNeeded(_ config: Config, signedIn: Bool) {
        if !loadedOnce { refresh(config, signedIn: signedIn) }
    }

    func refresh(_ config: Config, signedIn: Bool) {
        guard !loading else { return }
        loadedOnce = true
        guard config.isOIDC, let serverURL = try? Auth.normalizeServerURL(config.serverURL ?? "") else { return }
        loading = true
        Task { [weak self] in
            var facts: ServerFacts?, factsErr: String?
            do { facts = try await ServerAPI.facts(serverURL: serverURL) }
            catch { factsErr = "Couldn't read the server's details: \(error.localizedDescription)" }

            var account: AccountInfo?, accountErr: String?
            if signedIn {
                do {
                    let token = try await Auth.accessToken(serverURL: serverURL)
                    account = try await ServerAPI.account(serverURL: serverURL, token: token)
                } catch AuthError.signedOut {
                    accountErr = "You're signed out. Sign in again to see your account."
                } catch {
                    accountErr = "Couldn't load your account: \(error.localizedDescription)"
                }
            } else {
                accountErr = "Sign in to see your account."
            }
            guard let self else { return }
            self.facts = facts ?? self.facts
            self.factsError = factsErr
            self.account = account ?? self.account
            self.accountError = accountErr
            self.loading = false
        }
    }
}
