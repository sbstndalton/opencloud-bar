import CryptoKit
import Foundation

/// A folder kept on this Mac and synced both ways with the server.
struct KeptFolder: Codable, Hashable, Identifiable {
    /// Path relative to the drive root, e.g. "Documents/School".
    var path: String
    /// Name of the bisync state (work dir, marker, filters, log). Must be unique.
    var stateName: String
    /// Extra rclone filter lines for this folder only, e.g. "- /Noevia backup/**".
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
    var rclonePath = "/opt/homebrew/bin/rclone"
    var localRoot = "~/OpenCloud/Personal"
    var mountPoint = "~/OpenCloud-Drive"
    var volumeName = "OpenCloud"
    var cacheMaxSize = "20G"
    var cacheMaxAge = "72h"
    var syncIntervalSeconds = 300
    var minServerAvailableMiB = 8192
    var stateDir = "~/Library/Application Support/OpenCloudBar/state"
    var kept: [KeptFolder] = []
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
    }

    var isConfigured: Bool { !sshHost.isEmpty && !davPath.contains("USER") }

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
    /// rclone remote string that points the named remote at the SSH tunnel.
    var remoteSpec: String { "\(rcloneRemote),url='http://127.0.0.1:\(localPort)\(davPath)':" }
    /// Identifies "which server, which account" for a kept folder's sync history.
    var serverIdentity: String { "\(sshHost)|\(serverPort)|\(rcloneRemote)|\(davPath)" }
    /// Connection settings: changing any of these restarts the tunnel and drive.
    var connectionKey: String { "\(sshHost)|\(serverPort)|\(localPort)|\(rcloneRemote)|\(davPath)|\(mountPoint)|\(rclonePath)" }

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
            return (Config(), "Couldn't read the settings file (\(error.localizedDescription)). Fix it and choose Reload Settings.")
        }
    }

    func save() {
        try? FileManager.default.createDirectory(at: Config.supportURL, withIntermediateDirectories: true)
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        guard let data = try? enc.encode(self) else { return }
        try? data.write(to: Config.fileURL, options: .atomic)
    }
}
