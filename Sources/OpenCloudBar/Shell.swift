import Foundation

/// Small helpers for running command-line tools.
enum Shell {
    static let path = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

    /// Extra variables for every tool we run (rclone's bearer token command in OIDC mode).
    private static let extraLock = NSLock()
    nonisolated(unsafe) private static var extra: [String: String] = [:]
    static func setExtraEnvironment(_ e: [String: String]) { extraLock.lock(); extra = e; extraLock.unlock() }

    static func environment() -> [String: String] {
        var env = ProcessInfo.processInfo.environment
        env["PATH"] = path
        extraLock.lock(); env.merge(extra) { _, new in new }; extraLock.unlock()
        return env
    }

    struct Result { let status: Int32; let output: String }

    /// Runs a tool to completion off the main thread and returns its exit status and output.
    /// `timeout` kills the process if it runs too long (status is then 124).
    static func run(_ exe: String, _ args: [String], timeout: TimeInterval? = nil,
                    logTo log: URL? = nil) async -> Result {
        await withCheckedContinuation { cont in
            let p = Process()
            p.executableURL = URL(fileURLWithPath: exe)
            p.arguments = args
            p.environment = environment()
            let pipe = Pipe()
            p.standardOutput = pipe
            p.standardError = pipe
            let buffer = OutputBuffer()
            pipe.fileHandleForReading.readabilityHandler = { h in
                let d = h.availableData
                if !d.isEmpty { buffer.append(d) }
            }
            let timedOut = Flag()
            p.terminationHandler = { proc in
                pipe.fileHandleForReading.readabilityHandler = nil
                buffer.append(pipe.fileHandleForReading.readDataToEndOfFile())
                let text = buffer.string
                if let log { Log.append(text, to: log) }
                cont.resume(returning: Result(status: timedOut.value ? 124 : proc.terminationStatus, output: text))
            }
            do { try p.run() } catch {
                cont.resume(returning: Result(status: -1, output: "\(error)"))
                return
            }
            if let timeout {
                DispatchQueue.global().asyncAfter(deadline: .now() + timeout) {
                    if p.isRunning { timedOut.set(); p.terminate() }
                }
            }
        }
    }

    /// Starts a long-running tool and returns the process; `onExit` runs on the main actor.
    /// `extraEnv` is added for this process only (the drive's rc login, kept off the command line).
    static func launch(_ exe: String, _ args: [String], log: URL, extraEnv: [String: String] = [:],
                       onExit: @escaping @MainActor (Int32) -> Void) throws -> Process {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.environment = environment().merging(extraEnv) { _, new in new }
        if !FileManager.default.fileExists(atPath: log.path) {
            FileManager.default.createFile(atPath: log.path, contents: nil)
        }
        let h = try FileHandle(forWritingTo: log)
        h.seekToEndOfFile()
        p.standardOutput = h
        p.standardError = h
        p.terminationHandler = { proc in
            try? h.close()
            let s = proc.terminationStatus
            Task { @MainActor in onExit(s) }
        }
        try p.run()
        return p
    }

    /// True if something is listening on 127.0.0.1:port.
    /// A free TCP port on 127.0.0.1 (the system picks one; it is closed again right away).
    static func freePort() -> Int? {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { return nil }
        defer { close(fd) }
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = 0
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        let ok = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) }
        }
        guard ok == 0 else { return nil }
        var len = socklen_t(MemoryLayout<sockaddr_in>.size)
        let got = withUnsafeMutablePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &len) }
        }
        guard got == 0 else { return nil }
        return Int(UInt16(bigEndian: addr.sin_port))
    }

    static func portOpen(_ port: Int) -> Bool {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { return false }
        defer { close(fd) }
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = in_port_t(UInt16(port).bigEndian)
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        let r = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
        return r == 0
    }

    /// True if `url` is currently a mount point (listed by getmntinfo, no blocking stat).
    static func isMounted(_ url: URL) -> Bool {
        var mnt: UnsafeMutablePointer<statfs>?
        let n = getmntinfo(&mnt, MNT_NOWAIT)
        guard n > 0, let mnt else { return false }
        let target = url.standardizedFileURL.path
        for i in 0..<Int(n) {
            var m = mnt[i]
            let on = withUnsafePointer(to: &m.f_mntonname) {
                $0.withMemoryRebound(to: CChar.self, capacity: Int(MAXPATHLEN)) { String(cString: $0) }
            }
            if on == target { return true }
        }
        return false
    }
}

/// Thread-safe byte buffer for collecting process output.
final class OutputBuffer: @unchecked Sendable {
    private var data = Data()
    private var tail = Data()
    private var dropped = false
    private let lock = NSLock()
    /// nil: keep everything. Else keep the first `headMax` bytes and the last `tailMax` (a
    /// bisync at INFO level writes a line per file; how it ended is at the end).
    private let headMax: Int?
    private let tailMax: Int
    init(headMax: Int? = nil, tailMax: Int = 0) { self.headMax = headMax; self.tailMax = tailMax }

    func append(_ d: Data) {
        lock.lock(); defer { lock.unlock() }
        guard let headMax else { data.append(d); return }
        var rest = d
        if data.count < headMax {
            let k = min(headMax - data.count, rest.count)
            data.append(rest.prefix(k))
            rest = rest.dropFirst(k)
        }
        guard !rest.isEmpty else { return }
        tail.append(rest)
        if tail.count > tailMax { tail = Data(tail.suffix(tailMax)); dropped = true }
    }
    var string: String {
        lock.lock(); defer { lock.unlock() }
        return String(decoding: data, as: UTF8.self) + (dropped ? "\n[…]\n" : "") + String(decoding: tail, as: UTF8.self)
    }
}

/// Appends to a log file as output arrives, and starts a new file (the old one becomes .1)
/// when it passes 5 MB, also in the middle of a long sync.
final class LogStream: @unchecked Sendable {
    private let url: URL
    private var handle: FileHandle?
    private var size = 0
    private let lock = NSLock()
    static let max = 5_000_000

    init(_ url: URL) {
        self.url = url
        open()
    }

    private func open() {
        let fm = FileManager.default
        if let s = (try? fm.attributesOfItem(atPath: url.path))?[.size] as? Int, s > Self.max { rotate() }
        if !fm.fileExists(atPath: url.path) { fm.createFile(atPath: url.path, contents: nil) }
        handle = try? FileHandle(forWritingTo: url)
        size = Int((try? handle?.seekToEnd()) ?? 0)
    }

    private func rotate() {
        try? handle?.close()
        handle = nil
        let old = url.appendingPathExtension("1")
        try? FileManager.default.removeItem(at: old)
        try? FileManager.default.moveItem(at: url, to: old)
    }

    func write(_ d: Data) {
        lock.lock(); defer { lock.unlock() }
        if size + d.count > Self.max { rotate(); open() }
        try? handle?.write(contentsOf: d)
        size += d.count
    }

    func close() { lock.lock(); try? handle?.close(); handle = nil; lock.unlock() }
}

/// Thread-safe one-way boolean.
final class Flag: @unchecked Sendable {
    private var v = false
    private let lock = NSLock()
    func set() { lock.lock(); v = true; lock.unlock() }
    var value: Bool { lock.lock(); defer { lock.unlock() }; return v }
}

enum Log {
    static func url(_ name: String) -> URL {
        try? FileManager.default.createDirectory(at: Config.logURL, withIntermediateDirectories: true)
        return Config.logURL.appendingPathComponent(name)
    }

    static func append(_ text: String, to url: URL) {
        guard !text.isEmpty else { return }
        rotate(url)
        if let h = try? FileHandle(forWritingTo: url) {
            h.seekToEndOfFile(); h.write(Data(text.utf8)); try? h.close()
        } else {
            try? Data(text.utf8).write(to: url)
        }
    }

    static func line(_ text: String, to name: String = "app.log") {
        let stamp = ISO8601DateFormatter().string(from: Date())
        append("\(stamp) \(text)\n", to: url(name))
    }

    /// Keep logs under ~5 MB by moving a full log to .1.
    private static func rotate(_ url: URL) {
        guard let size = (try? FileManager.default.attributesOfItem(atPath: url.path))?[.size] as? Int,
              size > 5_000_000 else { return }
        let old = url.appendingPathExtension("1")
        try? FileManager.default.removeItem(at: old)
        try? FileManager.default.moveItem(at: url, to: old)
    }
}
