import AppKit
import Foundation

/// OpenCloud's own notifications as desktop notifications (2.7): what the web interface's bell
/// shows (shares, space membership, expiring shares, virus finds, mentions…). Same rules as the
/// Go app (cross/servernotes.go, which has the tests and the source references):
///
///     GET <server>/ocs/v2.php/apps/notifications/api/v1/notifications?format=json
///
/// (userlog service: opencloud-eu/opencloud services/userlog/pkg/server/http/server.go and
/// services/userlog/pkg/service/http/service.go; web client: opencloud-eu/web
/// packages/web-runtime/src/components/Topbar/Notifications.vue). Polled every 3 minutes, only
/// while signed in with the browser, not paused, not asleep and with the setting on. The first
/// successful fetch after install, sign-in or a server change only records the ids it sees. The
/// seen ids (at most 500, more only when the server itself lists more) are kept in
/// server-notifications.json in the state folder; sign-out, sign-in and a changed server or
/// account forget them (resetQuota). Errors back off, doubling up to 30 minutes; a 404 means no
/// such service: no more polling this session. Nothing is ever deleted on the server.
struct ServerNote: Equatable {
    var id: String
    var subject: String
    var message: String
}

enum ServerNotes {
    static let every: TimeInterval = 3 * 60
    static let maxBackoff: TimeInterval = 30 * 60
    static let maxSeen = 500
    static let maxToasts = 5
    static let path = "/ocs/v2.php/apps/notifications/api/v1/notifications"

    enum Fetch {
        case notes([ServerNote])
        case gone              // 404: the service isn't deployed
        case failed(String)
    }

    /// The server's answer. An empty body is no notifications, null data too; entries without an
    /// id are skipped and an id repeated is read once. nil: not what was expected.
    static func parse(_ data: Data) -> [ServerNote]? {
        if String(decoding: data.prefix(1 << 21), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return [] }
        guard let doc = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any],
              let ocs = doc["ocs"] as? [String: Any] else { return nil }
        if ocs["data"] == nil || ocs["data"] is NSNull { return [] }
        guard let list = ocs["data"] as? [[String: Any]] else { return nil }
        var out: [ServerNote] = []
        var seen = Set<String>()
        for d in list {
            let id: String
            if let s = d["notification_id"] as? String { id = s.trimmingCharacters(in: .whitespaces) }
            else if let n = d["notification_id"] as? NSNumber { id = n.stringValue }
            else { continue }
            if id.isEmpty || !seen.insert(id).inserted { continue }
            out.append(ServerNote(id: id, subject: d["subject"] as? String ?? "", message: d["message"] as? String ?? ""))
        }
        return out
    }

    /// What to show now (nothing for the first fetch, which only seeds) and the ids to keep:
    /// ids the server no longer lists stay until the cap pushes them out, oldest first; ids it
    /// does list are never pushed out.
    static func plan(seeded: Bool, seen: [String], got: [ServerNote]) -> (show: [ServerNote], seen: [String]) {
        let have = Set(seen)
        let cur = got.map(\.id)
        let inCur = Set(cur)
        let show = seeded ? got.filter { !have.contains($0.id) } : []
        var ids = seen.filter { !inCur.contains($0) } + cur
        let limit = max(maxSeen, cur.count)
        if ids.count > limit { ids = Array(ids.suffix(limit)) }
        return (show, ids)
    }

    /// The wait after `fails` errors in a row: 6 minutes, doubling, up to 30.
    static func backoff(fails: Int) -> TimeInterval {
        var d = every
        var i = 0
        while i < fails, d < maxBackoff { d *= 2; i += 1 }
        return min(d, maxBackoff)
    }

    /// Server text made safe to show: no control characters, one line, at most `max` characters.
    static func clean(_ s: String, max: Int) -> String {
        let one = s.components(separatedBy: CharacterSet.controlCharacters.union(.whitespacesAndNewlines)).filter { !$0.isEmpty }.joined(separator: " ")
        guard one.count > max else { return one }
        return String(one.prefix(max - 1)) + "…"
    }

    /// Each new notification, or for more than `maxToasts` the first ones and a summary.
    static func toasts(_ news: [ServerNote]) -> [Note] {
        func one(_ n: ServerNote) -> Note {
            var title = clean(n.subject, max: 100)
            var body = clean(n.message, max: 240)
            if title.isEmpty { title = "OpenCloud" }
            if body.isEmpty, title == "OpenCloud" { body = "You have a new notification." }
            return Note(title: title, body: body, page: .general)
        }
        let shown = news.count > maxToasts ? Array(news.prefix(maxToasts - 1)) : news
        var out = shown.map(one)
        let rest = news.count - shown.count
        if rest > 0 { out.append(Note(title: "OpenCloud", body: "\(rest) more notifications. Open OpenCloud to read them.", page: .general)) }
        return out
    }

    static func file(_ c: Config) -> URL { c.stateURL.appendingPathComponent("server-notifications.json") }

    /// The remembered ids; a missing or damaged file is "never seeded".
    static func readSeen(_ url: URL) -> (seeded: Bool, ids: [String]) {
        guard let d = try? Data(contentsOf: url),
              let o = (try? JSONSerialization.jsonObject(with: d)) as? [String: Any],
              let ids = o["ids"] as? [String] else { return (false, []) }
        return (true, ids)
    }

    static func writeSeen(_ url: URL, _ ids: [String]) -> Bool {
        try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        guard let d = try? JSONSerialization.data(withJSONObject: ["ids": ids]) else { return false }
        return (try? d.write(to: url, options: .atomic)) != nil
    }

    /// One GET to the configured server (the no-redirect session: the token never leaves it).
    static func fetch(_ c: Config) async -> Fetch {
        guard let base = try? Auth.normalizeServerURL(c.serverURL ?? ""),
              let url = URL(string: base.trimmingCharacters(in: CharacterSet(charactersIn: "/")) + path + "?format=json"),
              let token = try? await Auth.accessToken(serverURL: c.serverURL ?? "") else { return .failed("not signed in") }
        guard let (data, code) = try? await DavHistory.request("GET", url, token: token, headers: ["Accept": "application/json"]) else {
            return .failed("the server didn't answer")
        }
        switch code {
        case 200:
            guard let notes = parse(data) else { return .failed("the server's answer wasn't what was expected") }
            return .notes(notes)
        case 404: return .gone
        default: return .failed("the server answered \(code)")
        }
    }

    /// Run at startup in DEBUG builds (`--self-check` runs it and quits).
    static func selfCheck() {
        let json = #"{"ocs":{"meta":{"status":"ok"},"data":[{"notification_id":"n1","subject":"Ann shared","message":"A share"},{"notification_id":42,"subject":"num"},{"notification_id":"n1","subject":"dup"},{"subject":"no id"},{"notification_id":" ","subject":"blank"}]}}"#
        guard let p = parse(Data(json.utf8)) else { preconditionFailure("ServerNotes self-check: not parsed") }
        precondition(p == [ServerNote(id: "n1", subject: "Ann shared", message: "A share"), ServerNote(id: "42", subject: "num", message: "")], "ServerNotes self-check: \(p)")
        for ok in [#"{"ocs":{"data":null}}"#, #"{"ocs":{"data":[]}}"#, "", " \n"] { precondition(parse(Data(ok.utf8)) == [], "ServerNotes self-check: \(ok)") }
        for bad in ["<html>", #"{"ocs":{"data":"x"}}"#, "[]"] { precondition(parse(Data(bad.utf8)) == nil, "ServerNotes self-check: accepted \(bad)") }

        func ids(_ s: String...) -> [ServerNote] { s.map { ServerNote(id: $0, subject: "s \($0)", message: "") } }
        var r = plan(seeded: false, seen: [], got: ids("a", "b"))
        precondition(r.show.isEmpty && r.seen == ["a", "b"], "ServerNotes self-check: first fetch seeds")
        r = plan(seeded: true, seen: r.seen, got: ids("a", "b", "c"))
        precondition(r.show.map(\.id) == ["c"], "ServerNotes self-check: new id")
        r = plan(seeded: true, seen: r.seen, got: ids("a", "b", "c"))
        precondition(r.show.isEmpty, "ServerNotes self-check: shown twice")
        r = plan(seeded: true, seen: r.seen, got: ids("a", "c"))
        precondition(plan(seeded: true, seen: r.seen, got: ids("a", "b", "c")).show.isEmpty, "ServerNotes self-check: came back")
        precondition(plan(seeded: false, seen: [], got: []).seen.isEmpty && plan(seeded: true, seen: [], got: ids("x")).show.count == 1, "ServerNotes self-check: empty seed")

        var st: [String] = []
        for batch in 0..<12 {
            let got = (0..<100).map { ServerNote(id: "n\(batch)-\($0)", subject: "", message: "") }
            let x = plan(seeded: true, seen: st, got: got)
            precondition(x.show.count == 100 && x.seen.count <= maxSeen, "ServerNotes self-check: cap \(x.seen.count)")
            st = x.seen
        }
        precondition(st.count == maxSeen && st.first == "n7-0" && st.last == "n11-99", "ServerNotes self-check: newest kept")
        let big = (0..<700).map { ServerNote(id: "b\($0)", subject: "", message: "") }
        let y = plan(seeded: true, seen: [], got: big)
        precondition(y.seen.count == 700 && plan(seeded: true, seen: y.seen, got: big).show.isEmpty, "ServerNotes self-check: listed ids kept")

        let want: [TimeInterval] = [3, 6, 12, 24, 30, 30]
        for (i, w) in want.enumerated() { precondition(backoff(fails: i) == w * 60, "ServerNotes self-check: backoff \(i)") }
        precondition(backoff(fails: 1000) == maxBackoff, "ServerNotes self-check: backoff cap")

        let t = toasts([ServerNote(id: "1", subject: "  Ann\tshared\n“x” ", message: "line1\r\nline2\u{7}")])
        precondition(t.count == 1 && t[0].title == "Ann shared “x”" && t[0].body == "line1 line2", "ServerNotes self-check: toast \(t)")
        let many = toasts(ids("1", "2", "3", "4", "5", "6", "7"))
        precondition(many.count == maxToasts && many.last?.body.contains("3 more") == true, "ServerNotes self-check: flood")
        precondition(toasts(ids("1", "2", "3", "4", "5")).count == 5, "ServerNotes self-check: at the limit")
        let tmp = FileManager.default.temporaryDirectory.appendingPathComponent("ocb-selfcheck-\(UUID().uuidString)/server-notifications.json")
        precondition(!readSeen(tmp).seeded, "ServerNotes self-check: no file")
        precondition(writeSeen(tmp, ["a", "b"]) && readSeen(tmp) == (true, ["a", "b"]), "ServerNotes self-check: file")
        precondition(writeSeen(tmp, []) && readSeen(tmp) == (true, []), "ServerNotes self-check: empty file seeds")
        try? FileManager.default.removeItem(at: tmp.deletingLastPathComponent())
    }
}

/// Whether the Mac is asleep: no polling then (and one soon after it wakes).
@MainActor
final class SleepWatch {
    static let shared = SleepWatch()
    private(set) var asleep = false
    private var observers: [NSObjectProtocol] = []

    private init() {
        let nc = NSWorkspace.shared.notificationCenter
        observers.append(nc.addObserver(forName: NSWorkspace.willSleepNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.asleep = true }
        })
        observers.append(nc.addObserver(forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.asleep = false }
        })
    }
}

extension AppModel {
    /// From tick(): polls when 3 minutes have passed (browser sign-in, signed in, not paused, not
    /// asleep, the setting on, not given up for this session).
    func checkServerNotesIfDue() {
        guard config.isOIDC, !signedOut, !config.paused, config.serverNotifications, configError == nil, !quitting,
              !snBusy, !snOff, !SleepWatch.shared.asleep, Date() >= snAt else { return }
        // Another account or server than the seen ids are from: forget those first.
        let id = Self.quotaIdentity(config)
        if id != snIdentity {
            snIdentity = id
            resetServerNotes(stateFolders: [config.stateURL])
        }
        snBusy = true
        let c = config
        let gen = snGeneration
        Task {
            defer { snBusy = false }
            let result = await ServerNotes.fetch(c)
            guard gen == snGeneration else { return }   // reset meanwhile: an old answer
            switch result {
            case .gone:
                snOff = true   // quietly: this server has no such service
            case .failed(let why):
                Log.line("OpenCloud notifications: \(why)")
                snFails += 1
                snAt = Date().addingTimeInterval(ServerNotes.backoff(fails: snFails))
            case .notes(let got):
                applyServerNotes(got, config: c)
            }
        }
    }

    /// Forgets what was seen (the file in each of the state folders given too) and lets polling
    /// start again. For sign-out, sign-in and another server or account; called by resetQuota.
    func resetServerNotes(stateFolders: [URL]) {
        snGeneration += 1
        snAt = .distantPast
        snFails = 0
        snOff = false
        snSeen = []
        snSeeded = false
        snLoaded = false
        for u in Set(stateFolders) { try? FileManager.default.removeItem(at: u.appendingPathComponent("server-notifications.json")) }
    }

    private func applyServerNotes(_ got: [ServerNote], config c: Config) {
        let file = ServerNotes.file(c)
        if !snLoaded { (snSeeded, snSeen) = ServerNotes.readSeen(file); snLoaded = true }
        let plan = ServerNotes.plan(seeded: snSeeded, seen: snSeen, got: got)
        // When it can't be remembered nothing is shown: it would be shown again at every poll.
        guard ServerNotes.writeSeen(file, plan.seen) else {
            Log.line("OpenCloud notifications not shown (the seen ones couldn't be remembered)")
            snFails += 1
            snAt = Date().addingTimeInterval(ServerNotes.backoff(fails: snFails))
            return
        }
        snSeen = plan.seen
        snSeeded = true
        snFails = 0
        snAt = Date().addingTimeInterval(ServerNotes.every)
        // With notifications off they are recorded, so turning them on shows no flood.
        guard !plan.show.isEmpty, config.notifications else { return }
        Log.line("OpenCloud notifications: \(plan.show.count) new")
        for n in ServerNotes.toasts(plan.show) { notifier.show(n) }
    }
}
