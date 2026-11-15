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
    /// The OpenCloud space the folder is in (a project space or a share); nil is the personal
    /// space, as in every config before 1.2. `path` is then relative to that space's root.
    var space: SpaceRef?

    /// Identifies the folder: its path in the personal space (as before 1.2), else
    /// "<space id>:<path>", so the same path can be kept from two spaces. Same as the Go app.
    var key: String { space.map { "\($0.id):\(path)" } ?? path }
    /// For messages: “path”, or “path” in <space name>.
    var label: String { space.map { "“\(path)” in \($0.name)" } ?? "“\(path)”" }
    var id: String { key }
    var displayName: String { (path as NSString).lastPathComponent }

    init(path: String, stateName: String, extraFilters: [String] = [], space: SpaceRef? = nil) {
        self.path = path; self.stateName = stateName; self.extraFilters = extraFilters; self.space = space
    }

    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: CodingKeys.self)
        path = try c.decode(String.self, forKey: .path)
        stateName = try c.decode(String.self, forKey: .stateName)
        extraFilters = try c.decodeIfPresent([String].self, forKey: .extraFilters) ?? []
        space = try c.decodeIfPresent(SpaceRef.self, forKey: .space)
    }

    /// Unique, filesystem-safe state name: readable slug plus a hash of the case-folded path
    /// (and, in another space, of the space id too: the personal space's names are unchanged).
    static func makeStateName(for path: String, spaceID: String? = nil) -> String {
        let slug = path.replacingOccurrences(of: "[^A-Za-z0-9]+", with: "-", options: .regularExpression)
            .trimmingCharacters(in: CharacterSet(charactersIn: "-")).lowercased()
        var key = Config.foldKey(path)
        if let s = spaceID, !s.isEmpty { key = s + "\u{0}" + key }
        let hash = SHA256.hash(data: Data(key.utf8)).prefix(4).map { String(format: "%02x", $0) }.joined()
        return "\(slug.prefix(40))-\(hash)"
    }

    /// Why this path can't be a kept folder, or nil if it's fine.
    static func problem(with path: String) -> String? {
        if path.isEmpty { return "the whole drive can't be kept; choose a folder inside it" }
        if path.hasPrefix("/") { return "path must be relative to the drive" }
        if path.split(separator: "/").contains(where: { $0 == ".." || $0 == "." }) { return "path can't contain . or .." }
        if path.contains("'") { return "path can't contain a single quote" }
        if path.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) {
            return "path can't contain control characters (such as a line break)"
        }
        // Trailing spaces and dots are dropped by some disks and servers: the same folder.
        if let top = path.split(separator: "/", omittingEmptySubsequences: false).first,
           Config.foldKey(String(top).trimmingCharacters(in: .whitespaces).trimmingCharacters(in: CharacterSet(charactersIn: " ."))) == Config.foldKey(Backups.folderName) {
            return "\(Backups.folderName) holds the app's backup copies; it can't be kept"
        }
        for part in path.split(separator: "/", omittingEmptySubsequences: false) where part.isEmpty {
            return "path can't contain an empty folder name (two slashes in a row, or a slash at the end)"
        }
        return nil
    }

    /// `problem(with:)` plus folder-name rules that only apply when a folder is added (a
    /// settings file with such a folder kept by an earlier version still loads): no name that is
    /// only spaces, starts or ends with one, or ends with a dot. Same rules as the Windows/Linux app.
    static func problem(newPath path: String) -> String? {
        if let p = problem(with: path) { return p }
        for part in path.split(separator: "/") {
            let trimmed = part.trimmingCharacters(in: .whitespaces)
            if trimmed.isEmpty { return "a folder name can't be only spaces" }
            if trimmed != String(part) { return "folder name “\(part)” starts or ends with a space" }
            if part.hasSuffix(".") { return "folder name “\(part)” ends with a dot" }
        }
        return nil
    }
}

/// A space other than the personal one. davPath is the WebDAV path of its root as the server
/// reported it (root.webDavUrl), e.g. "/dav/spaces/<id>"; it is used like Config.davPath.
struct SpaceRef: Codable, Hashable {
    var id: String
    var name: String
    var davPath: String

    /// The space's folder under localRoot: its name, made safe for a folder the same way on
    /// every system (a settings file moves between computers; same rules as the Go app):
    /// characters Windows can't store become "-", trailing dots and spaces go, and a reserved
    /// Windows device name (CON, NUL, COM1, LPT1.txt, …) gets "_" appended.
    var dirName: String {
        var n = String(name.trimmingCharacters(in: .whitespaces).unicodeScalars.map {
            "/\\:<>\"|?*".unicodeScalars.contains($0) || CharacterSet.controlCharacters.contains($0) ? "-" : Character($0)
        })
        while n.hasSuffix(" ") || n.hasSuffix(".") { n.removeLast() }
        let stem = String(n.split(separator: ".", maxSplits: 1, omittingEmptySubsequences: false).first ?? "")
            .uppercased().trimmingCharacters(in: CharacterSet(charactersIn: " "))
        if ["CON", "PRN", "AUX", "NUL"].contains(stem)
            || (stem.count == 4 && (stem.hasPrefix("COM") || stem.hasPrefix("LPT")) && ("1"..."9").contains(String(stem.last!))) {
            n += "_"
        }
        if n.isEmpty || n == "." || n == ".." { n = "Space " + String(id.filter { $0.isLetter || $0.isNumber }.prefix(8)) }
        return n
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
    /// Upload / download limits for syncs and the drive, in rclone's units ("512K" = 512 KiB/s,
    /// "2M" = 2 MiB/s, a bare number is KiB/s). nil: no limit. Same keys as the Go app.
    var bwLimitUp: String?
    var bwLimitDown: String?
    /// Live sync (preview): watch kept folders, sync a folder soon after a change here or in OpenCloud. nil: off.
    var liveSync: Bool?
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
        // Either case, as rclone takes it ("512k" is "512K").
        bwLimitUp = try c.decodeIfPresent(String.self, forKey: .bwLimitUp)?.trimmingCharacters(in: .whitespaces).uppercased()
        bwLimitDown = try c.decodeIfPresent(String.self, forKey: .bwLimitDown)?.trimmingCharacters(in: .whitespaces).uppercased()
        liveSync = (try? c.decodeIfPresent(Bool.self, forKey: .liveSync)) == true ? true : nil
    }

    /// Why a bandwidth limit isn't valid (nil if fine; empty is no limit).
    static func bwLimitProblem(_ name: String, _ v: String) -> String? {
        if v.isEmpty { return nil }
        guard v.range(of: #"^[0-9]+(\.[0-9]+)?[KMG]?$"#, options: .regularExpression) != nil else {
            return "\(name) looks like 512K or 2M (K = KiB/s, M = MiB/s), or empty for no limit"
        }
        let number = v.trimmingCharacters(in: CharacterSet(charactersIn: "KMG"))
        if (Double(number) ?? 0) <= 0 { return "\(name) must be more than 0, or empty for no limit" }
        return nil
    }

    /// rclone's --bwlimit UP:DOWN ("off" for the side without a limit), or nothing.
    var bwLimitArgs: [String] {
        let up = bwLimitUp ?? "", down = bwLimitDown ?? ""
        if up.isEmpty && down.isEmpty { return [] }
        return ["--bwlimit", (up.isEmpty ? "off" : up) + ":" + (down.isEmpty ? "off" : down)]
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
        var states = Set<String>(), keys = [(space: String, path: String)](), locals = [(String, String)]()
        for f in kept {
            if let p = KeptFolder.problem(with: f.path) { return "kept folder “\(f.path)”: \(p)" }
            if !states.insert(f.stateName).inserted { return "two kept folders share stateName “\(f.stateName)”" }
            if let s = f.space {
                if !isOIDC { return "kept folder “\(f.path)”: spaces need the browser sign-in (serverURL)" }
                if s.id.isEmpty || !s.davPath.hasPrefix("/") || s.davPath.contains("'") || s.davPath.contains(" ") {
                    return "kept folder “\(f.path)”: its space needs an id and a davPath starting with /"
                }
            }
            let sid = f.space?.id ?? "", k = Config.foldKey(f.path)
            if let other = keys.first(where: { $0.space == sid && (k == $0.path || k.hasPrefix($0.path + "/") || $0.path.hasPrefix(k + "/")) }) {
                return "kept folders “\(f.path)” and “\(other.path)” overlap"
            }
            keys.append((sid, k))
            // No two kept folders, whatever their space, may share or nest a local folder
            // (resolved paths, case-insensitive like the Mac's disks).
            let l = Config.foldKey((localURL(f).path as NSString).standardizingPath)
            if let other = locals.first(where: { l == $0.0 || l.hasPrefix($0.0 + "/") || $0.0.hasPrefix(l + "/") }) {
                return "kept folder \(f.label) would share its folder on this Mac with \(other.1)"
            }
            locals.append((l, f.label))
        }
        if davPath.contains("'") { return "davPath can't contain a single quote" }
        if (serverURL ?? "").contains("'") { return "serverURL can't contain a single quote" }
        if let v = webdavVendor, v.contains(where: { "',: ".contains($0) }) { return "webdavVendor isn't a valid rclone vendor name" }
        if let a = auth, a != "oidc" && a != "rclone" { return "auth must be \"oidc\" or \"rclone\", not “\(a)”" }
        if tunnel && sshHost.isEmpty { return "useTunnel needs sshHost" }
        if let p = Config.bwLimitProblem("bwLimitUp", bwLimitUp ?? "") { return p }
        if let p = Config.bwLimitProblem("bwLimitDown", bwLimitDown ?? "") { return p }
        return nil
    }

    /// localRoot / mountPoint: a full path (or ~/…), not the disk root, no "..", no control
    /// characters or surrounding spaces. Same rules as the Windows/Linux app.
    static func folderSettingProblem(_ name: String, _ v: String) -> String? {
        let t = v.trimmingCharacters(in: .whitespacesAndNewlines)
        if t.isEmpty { return "\(name) can't be empty" }
        if t != v { return "\(name) can't start or end with a space" }
        if v.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) { return "\(name) can't contain control characters" }
        if v.split(separator: "/").contains("..") { return "\(name) can't contain .." }
        let p = Config.expand(v)
        guard p.hasPrefix("/") else { return "\(name) must be a full path (or start with ~/)" }
        if (p as NSString).standardizingPath == "/" { return "\(name) can't be the root of the disk" }
        return nil
    }

    /// Checks localRoot and mountPoint together (nil if fine).
    func folderSettingsError() -> String? {
        if let p = Config.folderSettingProblem("The local folder", localRoot) { return p }
        if let p = Config.folderSettingProblem("The mount location", mountPoint) { return p }
        let home = (NSHomeDirectory() as NSString).standardizingPath
        let m = (Config.expand(mountPoint) as NSString).standardizingPath
        let l = (Config.expand(localRoot) as NSString).standardizingPath
        if Config.foldKey(m) == Config.foldKey(home) { return "The mount location can't be your home folder" }
        let (mk, lk) = (Config.foldKey(m), Config.foldKey(l))
        if mk == lk || mk.hasPrefix(lk + "/") || lk.hasPrefix(mk + "/") {
            return "The mount location and the local folder can't be the same, or one inside the other"
        }
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
    /// The kept folder's copy on this Mac: <localRoot>/<path>, or <localRoot>/<space name>/<path>.
    func localURL(_ f: KeptFolder) -> URL {
        guard let s = f.space else { return localRootURL.appendingPathComponent(f.path) }
        return localRootURL.appendingPathComponent(s.dirName).appendingPathComponent(f.path)
    }
    /// The rclone remote of the folder's space (personal: remoteSpec): the same on-the-fly WebDAV
    /// remote pointed at the space, so the sign-in applies.
    func remote(for f: KeptFolder) -> String {
        guard let s = f.space else { return remoteSpec }
        // davPath comes from the space's webDavUrl and already holds any path prefix the server
        // lives under (https://host/opencloud/dav/spaces/…): it goes after the origin only.
        return ":webdav,vendor=\(vendor),url='\(Config.origin(baseURL))\(s.davPath)':"
    }
    func serverIdentity(for f: KeptFolder) -> String {
        guard let s = f.space else { return serverIdentity }
        return serverIdentity + "|space|" + s.davPath
    }

    /// scheme://host[:port] of a URL, without its path.
    static func origin(_ u: String) -> String {
        guard let c = URLComponents(string: u), let scheme = c.scheme, let host = c.host, !host.isEmpty else {
            return u.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        }
        let h = host.contains(":") ? "[\(host)]" : host
        return "\(scheme)://\(h)" + (c.port.map { ":\($0)" } ?? "")
    }

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
        // Nor one that doesn't load as settings (a bad hand edit): the kept list written here
        // would replace the folders in it.
        if let existing {
            do {
                if let e = try JSONDecoder().decode(Config.self, from: existing).validationError() {
                    throw AuthError.message("The settings file has a problem (\(e)). Fix or remove it, then choose Reload Settings.")
                }
            } catch let e as AuthError { throw e } catch {
                throw AuthError.message("The settings file can't be read (\(error.localizedDescription)). Fix or remove it, then choose Reload Settings.")
            }
        }
        if let existing,
           let old = try? JSONSerialization.jsonObject(with: existing) as? [String: Any],
           var new = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
            var merged = old
            // Optional settings this app owns are left out of the encoding when nil; drop them
            // from the old file too, so clearing one actually removes it.
            for k in ["serverURL", "auth", "useTunnel", "webdavVendor", "bwLimitUp", "bwLimitDown", "liveSync"] { merged.removeValue(forKey: k) }
            let oldKept = (old["kept"] as? [[String: Any]]) ?? []
            if let newKept = new["kept"] as? [[String: Any]] {
                new["kept"] = newKept.map { entry -> [String: Any] in
                    // Same folder: same path in the same space (keys the Go app adds survive).
                    func spaceID(_ e: [String: Any]) -> String { ((e["space"] as? [String: Any])?["id"] as? String) ?? "" }
                    guard let path = entry["path"] as? String,
                          var base = oldKept.first(where: { ($0["path"] as? String) == path && spaceID($0) == spaceID(entry) }) else { return entry }
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
