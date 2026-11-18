import Foundation

/// The size check before a folder is kept (2.7), and the path mapping Finder's "Keep on This Mac
/// with OpenCloud Bar" service uses. Same rules as the Go app (cross/keepcheck.go, which has the
/// tests): a PROPFIND for `oc:size`, Depth 0, to the configured server only (the no-redirect
/// session of DavHistory), compared with the free space at the local folder. A folder bigger than
/// the free space there, or bigger than 5 GB, is confirmed first. A size that can't be read never
/// blocks. oc:size on a folder is its whole size (opencloud-eu/reva
/// internal/http/services/owncloud/ocdav/propfind/propfind.go, "size").
enum KeepSize {
    /// More than this (5 GB, 1024-based like the rest of the app) is confirmed.
    static let warnBytes: Int64 = 5 << 30

    /// Unknown values (nil or negative) never count against the folder.
    static func needsConfirm(size: Int64?, free: Int64?) -> Bool {
        guard let size, size >= 0 else { return false }
        if let free, free >= 0, size > free { return true }
        return size > warnBytes
    }

    private static func bytes(_ n: Int64) -> String { ByteCountFormatter.string(fromByteCount: n, countStyle: .binary) }

    static func message(name: String, size: Int64, free: Int64?) -> String {
        var s = "“\(name)” is \(bytes(size)) in OpenCloud. "
        if let free, free >= 0, size > free {
            s += "Only \(bytes(free)) is free on this Mac, so it may not fit. "
        } else {
            s += "That is a lot to download and keep on this Mac. "
        }
        return s + "It will sync both ways. Keep it anyway?"
    }

    /// The folder's size from a PROPFIND answer; nil unless it holds a plain number (a 404 for the
    /// property is an empty element).
    static func parseSize(_ data: Data) -> Int64? {
        let text = String(decoding: data.prefix(1 << 20), as: UTF8.self)
        guard let r = text.range(of: #"<(?:[A-Za-z0-9]+:)?size>\s*([0-9]+)\s*</"#, options: .regularExpression) else { return nil }
        let digits = text[r].filter(\.isNumber)
        return Int64(digits)
    }

    static let body = #"<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:size/></d:prop></d:propfind>"#

    /// The folder's size, or nil for anything that goes wrong.
    static func fetch(_ url: URL, token: String) async -> Int64? {
        guard let (data, code) = try? await DavHistory.request("PROPFIND", url, token: token, headers: ["Depth": "0"], body: body),
              code == 207 else { return nil }
        return parseSize(data)
    }

    /// Free space on the volume holding `url`, or its nearest existing parent (nil: unknown). The
    /// "important usage" figure is what Finder shows: it counts space macOS can purge.
    static func free(at url: URL) -> Int64? {
        var dir = url.standardizedFileURL
        for _ in 0..<64 {
            var isDir: ObjCBool = false
            if FileManager.default.fileExists(atPath: dir.path, isDirectory: &isDir), isDir.boolValue {
                let v = try? dir.resourceValues(forKeys: [.volumeAvailableCapacityForImportantUsageKey, .volumeAvailableCapacityKey])
                if let n = v?.volumeAvailableCapacityForImportantUsage { return n }
                if let n = v?.volumeAvailableCapacity { return Int64(n) }
                return nil
            }
            let up = dir.deletingLastPathComponent()
            if up.path == dir.path { return nil }
            dir = up
        }
        return nil
    }

    /// The path in OpenCloud of `path`, a path inside the mounted drive at `root`; nil for a path
    /// outside it and for the drive itself. Tried as written, then with symbolic links resolved
    /// (a mount point reached through a link).
    nonisolated static func driveRel(root: String, path: String) -> String? {
        func rel(_ r: String, _ p: String) -> String? {
            let rr = URL(fileURLWithPath: r).standardizedFileURL.path
            let pp = URL(fileURLWithPath: p).standardizedFileURL.path
            let prefix = rr.hasSuffix("/") ? rr : rr + "/"
            guard pp.hasPrefix(prefix) else { return nil }
            let rest = pp.dropFirst(prefix.count).split(separator: "/").joined(separator: "/")
            return rest.isEmpty ? nil : rest
        }
        if let x = rel(root, path) { return x }
        return rel(URL(fileURLWithPath: root).resolvingSymlinksInPath().path, URL(fileURLWithPath: path).resolvingSymlinksInPath().path)
    }

    /// Run at startup in DEBUG builds (`--self-check` runs it and quits).
    static func selfCheck() {
        let gb: Int64 = 1 << 30
        let cases: [(Int64?, Int64?, Bool)] = [
            (1 * gb, 10 * gb, false), (5 * gb, 100 * gb, false), (5 * gb + 1, 100 * gb, true), (20 * gb, 500 * gb, true),
            (3 * gb, 2 * gb, true), (2 * gb, 2 * gb, false), (2 * gb + 1, 2 * gb, true), (0, 0, false),
            (nil, 0, false), (nil, nil, false), (-1, 0, false), (100 * 1024, nil, false), (6 * gb, nil, true),
        ]
        for (size, free, want) in cases {
            precondition(needsConfirm(size: size, free: free) == want, "KeepSize self-check: \(String(describing: size)) \(String(describing: free))")
        }
        let ok = #"<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/dav/x/</d:href><d:propstat><d:prop><oc:size>123456789012</oc:size></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>"#
        precondition(parseSize(Data(ok.utf8)) == 123456789012, "KeepSize self-check: size")
        let none = #"<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:propstat><d:prop><oc:size/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response></d:multistatus>"#
        for bad in [none, "", "<<<", ok.replacingOccurrences(of: "123456789012", with: "lots"), ok.replacingOccurrences(of: "123456789012", with: "-5")] {
            precondition(parseSize(Data(bad.utf8)) == nil, "KeepSize self-check: accepted \(bad.prefix(30))")
        }
        let root = "/Volumes/OpenCloud"
        let rels: [(String, String?)] = [
            ("/Volumes/OpenCloud/Docs/Work", "Docs/Work"), ("/Volumes/OpenCloud/Docs/Work/", "Docs/Work"), ("/Volumes/OpenCloud//Docs", "Docs"),
            ("/Volumes/OpenCloud/..foo", "..foo"), ("/Volumes/OpenCloud", nil), ("/Volumes/OpenCloud/", nil),
            ("/Volumes/OpenCloudX/Docs", nil), ("/Volumes/OpenCloud/Docs/../../etc", nil), ("/Users/sam/Docs", nil), ("/Volumes", nil),
        ]
        for (p, want) in rels {
            precondition(driveRel(root: root, path: p) == want, "KeepSize self-check: driveRel \(p) = \(String(describing: driveRel(root: root, path: p)))")
        }
        precondition(driveRel(root: "/Volumes/OpenCloud/", path: "/Volumes/OpenCloud/A") == "A", "KeepSize self-check: root with a slash")
        precondition(message(name: "Photos", size: 8 * gb, free: 2 * gb).contains("may not fit"), "KeepSize self-check: message")
        precondition(message(name: "Photos", size: 8 * gb, free: nil).contains("a lot"), "KeepSize self-check: message 2")
    }
}
