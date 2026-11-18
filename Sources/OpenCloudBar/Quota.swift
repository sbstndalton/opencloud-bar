import Foundation

/// Storage quota warnings: the personal space's quota from GET /graph/v1.0/me/drives
/// (libre-graph Drive.quota: used, total, remaining, state; total missing or 0 means no limit),
/// checked at most every 30 minutes from the model's tick (a failed check is tried again after
/// 2 minutes). A warning is given once when the
/// storage reaches 90% and once when it is full (100%); again only after the use went below
/// 90%. The last level warned about is kept in the sync state folder (quota-warned); signing
/// out, signing in or changing the server or account forgets it and the shown quota, and the
/// next check runs at once. Same
/// rules as the Go app (cross/quota.go, which has the tests).
struct StorageQuota: Equatable, Sendable {
    var used: Int64
    var total: Int64?          // nil or 0: no limit
    var state: String?

    static let checkEvery: TimeInterval = 30 * 60
    static let retryAfterFailure: TimeInterval = 2 * 60

    /// The warning level: 0, 90 or 100. No limit: 0.
    var level: Int {
        guard let t = total, t > 0, used >= 0 else { return 0 }
        if used >= t { return 100 }
        return Double(used) >= 0.9 * Double(t) ? 90 : 0
    }

    /// Given the level last warned about: the warning to give now (0: none), and the level to
    /// remember. Below 90% re-arms both; no reading at all (nil) keeps the remembered level.
    static func next(last: Int, _ q: StorageQuota?) -> (warn: Int, remember: Int) {
        guard let q else { return (0, last) }
        let lvl = q.level
        if lvl == 0 { return (0, 0) }
        if lvl > last { return (lvl, lvl) }
        return (0, last)
    }

    private static func bytes(_ n: Int64) -> String { ByteCountFormatter.string(fromByteCount: n, countStyle: .binary) }

    /// "1.2 GB of 10 GB used", or "1.2 GB used (no limit)".
    var text: String {
        if let t = total, t > 0 { return "\(Self.bytes(used)) of \(Self.bytes(t)) used" }
        return "\(Self.bytes(used)) used (no limit)"
    }

    /// The menu's line: a warning first when it is at 90% or more.
    var menuText: String {
        switch level {
        case 100: return "Storage full: \(text)"
        case 90: return "Storage almost full: \(text)"
        default: return "Storage: \(text)"
        }
    }

    func note(_ level: Int) -> Note {
        level >= 100
            ? Note(title: "Your OpenCloud storage is full",
                   body: "\(text). New files and changes can't be uploaded until you free up space (empty the trash in OpenCloud too) or get more storage.", page: .account)
            : Note(title: "Your OpenCloud storage is almost full",
                   body: "\(text). When it is full, new files and changes can't be uploaded.", page: .account)
    }

    static func readWarned(_ url: URL) -> Int {
        guard let s = try? String(contentsOf: url, encoding: .utf8),
              let n = Int(s.trimmingCharacters(in: .whitespacesAndNewlines)), n == 90 || n == 100 else { return 0 }
        return n
    }

    static func writeWarned(_ url: URL, _ level: Int) -> Bool {
        let fm = FileManager.default
        if level == 0 {
            if !fm.fileExists(atPath: url.path) { return true }
            return (try? fm.removeItem(at: url)) != nil
        }
        try? fm.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        return (try? Data("\(level)\n".utf8).write(to: url, options: .atomic)) != nil
    }

    /// The personal space's quota (nil: the server gave none).
    static func fetch(_ c: Config) async throws -> StorageQuota? {
        let token = try await Auth.accessToken(serverURL: c.serverURL ?? "")
        let value = try await DavHistory.drives(c, token: token, filter: "driveType eq 'personal'")
        guard let d = value.first(where: { ($0["driveType"] as? String ?? "personal") == "personal" }),
              let q = d["quota"] as? [String: Any], let used = (q["used"] as? NSNumber)?.int64Value else { return nil }
        return StorageQuota(used: used, total: (q["total"] as? NSNumber)?.int64Value, state: q["state"] as? String)
    }
}

extension AppModel {
    /// Whose quota it is: the server and the signed-in account.
    static func quotaIdentity(_ c: Config) -> String {
        let base = (try? Auth.normalizeServerURL(c.serverURL ?? "")) ?? ""
        return base + "|" + (Auth.readState()?.account ?? "")
    }

    var quotaWarnedURL: URL { config.stateURL.appendingPathComponent("quota-warned") }

    /// From tick(): checks the quota when 30 minutes have passed (browser sign-in, signed in).
    func checkQuotaIfDue() {
        guard config.isOIDC, !signedOut, !quotaBusy, Date().timeIntervalSince(quotaCheckedAt) >= StorageQuota.checkEvery else { return }
        // Another account or server than the quota on hand is from: forget that one first.
        let id = Self.quotaIdentity(config)
        if id != quotaIdentity {
            quotaIdentity = id
            resetQuota(stateFolders: [config.stateURL])
        }
        quotaBusy = true
        quotaCheckedAt = Date()
        let c = config
        let gen = quotaGeneration
        Task {
            defer { quotaBusy = false }
            do {
                let q = try await StorageQuota.fetch(c)
                guard gen == quotaGeneration else { return }   // reset meanwhile: an old answer
                applyQuota(q)
            } catch {
                Log.line("storage check: \(error.localizedDescription)")
                // Try again soon rather than after the full 30 minutes.
                if gen == quotaGeneration {
                    quotaCheckedAt = Date().addingTimeInterval(StorageQuota.retryAfterFailure - StorageQuota.checkEvery)
                }
            }
        }
    }

    /// Forgets the shown quota and what was warned about (the quota-warned file in each of the
    /// state folders given), so the next check runs at once. For sign-out, sign-in and another
    /// server or account.
    func resetQuota(stateFolders: [URL]) {
        storageQuota = nil
        quotaCheckedAt = .distantPast
        quotaGeneration += 1
        for u in Set(stateFolders) { try? FileManager.default.removeItem(at: u.appendingPathComponent("quota-warned")) }
    }

    /// Shows the quota in the menu and warns when it crossed a level.
    func applyQuota(_ q: StorageQuota?) {
        storageQuota = q
        let url = quotaWarnedURL
        let last = StorageQuota.readWarned(url)
        let (warn, remember) = StorageQuota.next(last: last, q)
        if remember != last, !StorageQuota.writeWarned(url, remember), warn > 0 {
            Log.line("storage warning not given (it couldn't be remembered)")
            return   // don't risk repeating it every half hour
        }
        guard warn > 0, let q else { return }
        let n = q.note(warn)
        Log.line("storage warning: \(q.text)")
        if config.notifications { notifier.show(n) }
    }
}
