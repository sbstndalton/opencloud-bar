import CryptoKit
import Foundation

/// A folder kept on this Mac and synced both ways with the server.
struct KeptFolder: Codable, Hashable, Identifiable {
    /// Path relative to the drive root, e.g. "Documents/School".
    var path: String
    /// Name of the bisync state (work dir, marker, filters, log). Must be unique.
    var stateName: String
    /// Extra rclone filter lines for this folder only, e.g. "- /Old backups/**".
    var extraFilters: [String] = []

    var id: String { path }
    var displayName: String { (path as NSString).lastPathComponent }

    init(path: String, stateName: String, extraFilters: [String] = []) {
        self.path = path; self.stateName = stateName; self.extraFilters = extraFilters
    }

    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: CodingKeys.self)
        path = try c.decode(String.self, forKey: .path)
        stateName = try c.decode(String.self, forKey: .stateName)
        extraFilters = try c.decodeIfPresent([String].self, forKey: .extraFilters) ?? []
    }

    /// Unique, filesystem-safe state name: readable slug plus a hash of the case-folded path.
    static func makeStateName(for path: String) -> String {
        let slug = path.replacingOccurrences(of: "[^A-Za-z0-9]+", with: "-", options: .regularExpression)
            .trimmingCharacters(in: CharacterSet(charactersIn: "-")).lowercased()
        let key = Config.foldKey(path)
        let hash = SHA256.hash(data: Data(key.utf8)).prefix(4).map { String(format: "%02x", $0) }.joined()
        return "\(slug.prefix(40))-\(hash)"
    }

    /// Why this path can't be a kept folder, or nil if it's fine.
    static func problem(with path: String) -> String? {
        if path.isEmpty { return "the whole drive can't be kept; choose a folder inside it" }
        if path.hasPrefix("/") { return "path must be relative to the drive" }
        if path.split(separator: "/").contains(where: { $0 == ".." || $0 == "." }) { return "path can't contain . or .." }
        if path.contains("'") { return "path can't contain a single quote" }
        return nil
    }
}

/// Everything the app needs to know. Stored as JSON at
/// ~/Library/Application Support/OpenCloudBar/config.json. Missing keys take their defaults.
struct Config: Codable {
    var sshHost = ""
    var serverPort = 9200
    var localPort = 19201
    var rcloneRemote = "opencloud"
    var davPath = "/remote.php/dav/files/USER"
    /// Empty: the rclone bundled in the app, else Homebrew's.
    var rclonePath = ""
    var localRoot = "~/OpenCloud/Personal"
    var mountPoint = "~/OpenCloud-Drive"
    var volumeName = "OpenCloud"
    var cacheMaxSize = "20G"
    var cacheMaxAge = "72h"
    var syncIntervalSeconds = 300
    var minServerAvailableMiB = 8192
    var stateDir = "~/Library/Application Support/OpenCloudBar/state"
    var kept: [KeptFolder] = []
    // Browser sign-in (see Auth.swift). All optional: a config without them is a "legacy" config
    // (SSH tunnel + named rclone remote) and behaves exactly as before. Same keys as the Go app.
    var serverURL: String?     // https://cloud.example.com, no trailing slash
    var auth: String?          // "oidc" or "rclone"; default oidc if serverURL is set
    var useTunnel: Bool?       // default: true if sshHost is set
    var webdavVendor: String?  // OIDC remote's rclone vendor; default "infinitescale" (tus chunked uploads)
    /// Kept folders don't sync while paused; the drive keeps working. Same key as the Go app.
    var paused = false
    /// Look for a new release once a day.
    var checkForUpdates = true
    /// Notifications for problems, automatic merges and updates.
    var notifications = true
    var commonFilters: [String] = [
        "- *.tmp", "- .*.bundle.tmp", "- *~", "- .DS_Store", "- ._*", "- Thumbs.db",
        "- .sync_*.db", "- .sync_*.db-*", "- .OpenCloudSync.log", "- .owncloudsync.log",
        "- .nextcloudsync.log", "- .sync-exclude.lst",
        "- **/target/**", "- **/node_modules/**", "- **/.venv/**", "- **/__pycache__/**",
        "- **/tmp_objdir-*/**",
    ]

    init() {}

    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: CodingKeys.self)
        let def = Config()
        sshHost = try c.decodeIfPresent(String.self, forKey: .sshHost) ?? def.sshHost
        serverPort = try c.decodeIfPresent(Int.self, forKey: .serverPort) ?? def.serverPort
        localPort = try c.decodeIfPresent(Int.self, forKey: .localPort) ?? def.localPort
        rcloneRemote = try c.decodeIfPresent(String.self, forKey: .rcloneRemote) ?? def.rcloneRemote
        davPath = try c.decodeIfPresent(String.self, forKey: .davPath) ?? def.davPath
        rclonePath = try c.decodeIfPresent(String.self, forKey: .rclonePath) ?? def.rclonePath
        localRoot = try c.decodeIfPresent(String.self, forKey: .localRoot) ?? def.localRoot
        mountPoint = try c.decodeIfPresent(String.self, forKey: .mountPoint) ?? def.mountPoint
        volumeName = try c.decodeIfPresent(String.self, forKey: .volumeName) ?? def.volumeName
        cacheMaxSize = try c.decodeIfPresent(String.self, forKey: .cacheMaxSize) ?? def.cacheMaxSize
        cacheMaxAge = try c.decodeIfPresent(String.self, forKey: .cacheMaxAge) ?? def.cacheMaxAge
        syncIntervalSeconds = try c.decodeIfPresent(Int.self, forKey: .syncIntervalSeconds) ?? def.syncIntervalSeconds
        minServerAvailableMiB = try c.decodeIfPresent(Int.self, forKey: .minServerAvailableMiB) ?? def.minServerAvailableMiB
        stateDir = try c.decodeIfPresent(String.self, forKey: .stateDir) ?? def.stateDir
        kept = try c.decodeIfPresent([KeptFolder].self, forKey: .kept) ?? def.kept
        commonFilters = try c.decodeIfPresent([String].self, forKey: .commonFilters) ?? def.commonFilters
        serverURL = try c.decodeIfPresent(String.self, forKey: .serverURL)
        auth = try c.decodeIfPresent(String.self, forKey: .auth)
        useTunnel = try c.decodeIfPresent(Bool.self, forKey: .useTunnel)
        webdavVendor = try c.decodeIfPresent(String.self, forKey: .webdavVendor)
        paused = try c.decodeIfPresent(Bool.self, forKey: .paused) ?? def.paused
        checkForUpdates = try c.decodeIfPresent(Bool.self, forKey: .checkForUpdates) ?? def.checkForUpdates
        notifications = try c.decodeIfPresent(Bool.self, forKey: .notifications) ?? def.notifications
    }

    /// "oidc" (browser sign-in) or "rclone" (the named rclone remote holds the credentials).
    var authMode: String { auth ?? ((serverURL ?? "").isEmpty ? "rclone" : "oidc") }
    var isOIDC: Bool { authMode == "oidc" }
    /// Whether traffic goes through the SSH tunnel to 127.0.0.1:localPort.
    var tunnel: Bool { useTunnel ?? !sshHost.isEmpty }
    var vendor: String { (webdavVendor ?? "").isEmpty ? "infinitescale" : webdavVendor! }
    /// Where rclone connects, before davPath.
    var baseURL: String {
        tunnel ? "http://127.0.0.1:\(localPort)" : (serverURL ?? "").trimmingCharacters(in: CharacterSet(charactersIn: "/"))
    }

    /// Complete settings. Whether an OIDC config is also signed in is the model's business (Keychain).
    var isConfigured: Bool {
        if davPath.contains("USER") { return false }
        if isOIDC { return !(serverURL ?? "").isEmpty }
        return !sshHost.isEmpty || (!(serverURL ?? "").isEmpty && !tunnel)
    }

    /// Problems that make the config unsafe to run with (nil if none).
    func validationError() -> String? {
        var states = Set<String>(), keys = [String]()
        for f in kept {
            if let p = KeptFolder.problem(with: f.path) { return "kept folder “\(f.path)”: \(p)" }
            if !states.insert(f.stateName).inserted { return "two kept folders share stateName “\(f.stateName)”" }
            let k = Config.foldKey(f.path)
            if let other = keys.first(where: { k == $0 || k.hasPrefix($0 + "/") || $0.hasPrefix(k + "/") }) {
                return "kept folders “\(f.path)” and “\(other)” overlap"
            }
            keys.append(k)
        }
        if davPath.contains("'") { return "davPath can't contain a single quote" }
        if (serverURL ?? "").contains("'") { return "serverURL can't contain a single quote" }
        if let v = webdavVendor, v.contains(where: { "',: ".contains($0) }) { return "webdavVendor isn't a valid rclone vendor name" }
        if let a = auth, a != "oidc" && a != "rclone" { return "auth must be \"oidc\" or \"rclone\", not “\(a)”" }
        if tunnel && sshHost.isEmpty { return "useTunnel needs sshHost" }
        return nil
    }

    // MARK: expanded paths
    var localRootURL: URL { URL(fileURLWithPath: Config.expand(localRoot)) }
    var mountURL: URL { URL(fileURLWithPath: Config.expand(mountPoint)) }
    var stateURL: URL { URL(fileURLWithPath: Config.expand(stateDir)) }
    /// Not in ~/Library/Caches: files written to the drive wait here until uploaded,
    /// and macOS may purge Caches under disk pressure.
    var cacheURL: URL { Config.supportURL.appendingPathComponent("vfs-cache") }
    var rcPort: Int { localPort + 100 }
    /// rclone remote for the space root. Legacy: the named remote (credentials in rclone.conf).
    /// OIDC: an on-the-fly webdav remote; the bearer token comes from the environment (AppModel).
    var remoteSpec: String {
        isOIDC ? ":webdav,vendor=\(vendor),url='\(baseURL)\(davPath)':" : "\(rcloneRemote),url='\(baseURL)\(davPath)':"
    }
    /// Identifies "which server, which account" for a kept folder's sync history.
    /// The legacy format is unchanged so existing sync state stays valid.
    var serverIdentity: String {
        isOIDC ? "oidc|\(serverURL ?? "")|\(davPath)" : "\(sshHost)|\(serverPort)|\(rcloneRemote)|\(davPath)"
    }
    /// Connection settings: changing any of these restarts the tunnel and drive.
    /// The rclone to run: rclonePath if set, else the copy inside the app bundle, else Homebrew's.
    var rclone: String {
        if !rclonePath.isEmpty { return (rclonePath as NSString).expandingTildeInPath }
        let candidates = [
            Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/rclone").path,
            "/opt/homebrew/bin/rclone", "/usr/local/bin/rclone",
        ]
        return candidates.first { FileManager.default.isExecutableFile(atPath: $0) } ?? "/opt/homebrew/bin/rclone"
    }

    var connectionKey: String {
        "\(sshHost)|\(serverPort)|\(localPort)|\(rcloneRemote)|\(davPath)|\(mountPoint)|\(rclonePath)"
            + "|\(serverURL ?? "")|\(authMode)|\(tunnel)|\(webdavVendor ?? "")"
    }

    static func expand(_ p: String) -> String { (p as NSString).expandingTildeInPath }
    /// Case- and Unicode-insensitive form of a path, matching how macOS disks compare names.
    static func foldKey(_ p: String) -> String {
        p.precomposedStringWithCanonicalMapping.folding(options: [.caseInsensitive], locale: nil)
    }

    // MARK: persistence
    static var supportURL: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("OpenCloudBar")
    }
    static var fileURL: URL { supportURL.appendingPathComponent("config.json") }
    static var logURL: URL {
        FileManager.default.urls(for: .libraryDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Logs/OpenCloudBar")
    }

    /// Loads the config. A missing file is created with defaults; a file that can't be read
    /// is left untouched and reported, never overwritten.
    static func load() -> (Config, String?) {
        guard FileManager.default.fileExists(atPath: fileURL.path) else {
            let c = Config(); c.save(); return (c, nil)
        }
        do {
            let c = try JSONDecoder().decode(Config.self, from: Data(contentsOf: fileURL))
            if let e = c.validationError() { return (Config(), "Settings problem: \(e)") }
            return (c, nil)
        } catch {
            return (Config(), "Couldn't read the settings file (\(error.localizedDescription)). Fix it, then choose Settings… ▸ General ▸ Reload Settings.")
        }
    }

    /// WebDAV path of a user's files.
    static func davPath(for username: String) -> String {
        var allowed = CharacterSet.urlPathAllowed
        allowed.remove(charactersIn: "/'")   // ' would end the single-quoted rclone value
        return "/remote.php/dav/files/" + (username.addingPercentEncoding(withAllowedCharacters: allowed) ?? username)
    }

    /// Records a finished sign-in in config.json. Edited as generic JSON so keys this version
    /// doesn't know about are kept. Without an sshHost the tunnel is off; with one, useTunnel
    /// stays as the user had it.
    static func applySetup(serverURL: String, username: String) throws {
        var obj: [String: Any] = [:]
        if let data = try? Data(contentsOf: fileURL) {
            guard let o = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                throw AuthError.message("Couldn't read the settings file")
            }
            obj = o
        } else if let d = try? JSONEncoder().encode(Config()), let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] {
            obj = o
        }
        obj["serverURL"] = serverURL
        obj["auth"] = "oidc"
        obj["davPath"] = davPath(for: username)
        if ((obj["sshHost"] as? String) ?? "").isEmpty { obj["useTunnel"] = false }
        try FileManager.default.createDirectory(at: supportURL, withIntermediateDirectories: true)
        let data = try JSONSerialization.data(withJSONObject: obj, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes])
        let tmp = fileURL.appendingPathExtension("tmp")
        FileManager.default.createFile(atPath: tmp.path, contents: data, attributes: [.posixPermissions: 0o600])
        _ = try FileManager.default.replaceItemAt(fileURL, withItemAt: tmp)
    }

    func save() { try? write() }

    /// Writes config.json. Keys this version doesn't know about (the Go app's extra settings,
    /// for example `mountEnabled` or `kept[].localPath`) are kept as they are in the file.
    func write() throws {
        try FileManager.default.createDirectory(at: Config.supportURL, withIntermediateDirectories: true)
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        var data = try enc.encode(self)
        let existing = try? Data(contentsOf: Config.fileURL)
        // Never replace a settings file that can't be read (a hand edit in progress, or damage).
        if let existing, (try? JSONSerialization.jsonObject(with: existing)) as? [String: Any] == nil {
            throw AuthError.message("The settings file can't be read. Fix it, then choose Reload Settings.")
        }
        if let existing,
           let old = try? JSONSerialization.jsonObject(with: existing) as? [String: Any],
           var new = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
            var merged = old
            // Optional settings this app owns are left out of the encoding when nil; drop them
            // from the old file too, so clearing one actually removes it.
            for k in ["serverURL", "auth", "useTunnel", "webdavVendor"] { merged.removeValue(forKey: k) }
            let oldKept = (old["kept"] as? [[String: Any]]) ?? []
            if let newKept = new["kept"] as? [[String: Any]] {
                new["kept"] = newKept.map { entry -> [String: Any] in
                    guard let path = entry["path"] as? String,
                          var base = oldKept.first(where: { ($0["path"] as? String) == path }) else { return entry }
                    base.merge(entry) { _, n in n }
                    return base
                }
            }
            merged.merge(new) { _, n in n }
            data = try JSONSerialization.data(withJSONObject: merged, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes])
        }
        try data.write(to: Config.fileURL, options: .atomic)
    }
}
