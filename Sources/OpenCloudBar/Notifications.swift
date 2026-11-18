import AppKit
import Foundation
import SwiftUI
import UserNotifications

/// One notification. `page` is the Settings page a click opens.
struct Note: Equatable {
    var title: String
    var body: String
    var page: SettingsPage
}

/// Decides what to announce: one notification per change of state, never repeated while the
/// state lasts, folders that change the same way together share one. Same rules as the
/// Windows/Linux app (cross/notify.go, which has the tests).
struct NotePlanner {
    private var seeded = false
    private var kinds: [String: String] = [:]   // folder path -> kind announced
    private var wasSignedOut = false

    /// The notification kind for a state; nil while busy or skipped (the last kind stands, so
    /// a failure repeating at every sync is announced once).
    static func kind(_ s: FolderStatus.State) -> String?? {
        switch s {
        case .syncing, .skipped: return .none
        case .needsRepair: return "repair"
        case .bigDeletion: return "deletion"
        case .failed(let why):
            if why == AppModel.missingRemoteText { return "missing" }
            if why == "signed out" { return .some(nil) }
            return "failed"
        default: return .some(nil)
        }
    }

    mutating func plan(folders: [(KeptFolder, FolderStatus.State)], signedOut: Bool, autoMerged: [String]) -> [Note] {
        var fresh: [String: [(KeptFolder, FolderStatus.State)]] = [:]
        var seen = Set<String>()
        for (f, s) in folders {
            seen.insert(f.key)
            guard case .some(let k) = Self.kind(s) else { continue }
            if let k, kinds[f.key] != k, seeded { fresh[k, default: []].append((f, s)) }
            kinds[f.key] = k ?? ""
        }
        for p in kinds.keys where !seen.contains(p) { kinds[p] = nil }
        let wasOut = wasSignedOut
        wasSignedOut = signedOut
        guard seeded else { seeded = true; return [] }

        var out: [Note] = []
        if signedOut && !wasOut {
            out.append(Note(title: "Signed out of OpenCloud",
                            body: "Syncing has stopped. Choose Set Up / Sign In… in the OpenCloud Bar menu to continue.", page: .account))
        }
        for k in ["missing", "repair", "deletion", "failed"] {
            if let fs = fresh[k], !fs.isEmpty { out.append(Self.folderNote(k, fs)) }
        }
        if !autoMerged.isEmpty {
            let one = autoMerged.count == 1
            out.append(Note(title: one ? "“\(autoMerged[0])” was merged automatically" : "\(autoMerged.count) folders were merged automatically",
                            body: (one ? "It was" : "They were") + " out of sync. The newer copy of each file was kept; older copies are kept in the backups (\(Backups.folderName) in OpenCloud, and on this Mac).",
                            page: .folders))
        }
        return out
    }

    /// Where a blocked deletion happened, when there is one folder and rclone said.
    private static func side(_ fs: [(KeptFolder, FolderStatus.State)]) -> String {
        if fs.count == 1, case .bigDeletion(let s) = fs[0].1, !s.isEmpty { return s }
        return "on one side"
    }

    private static func folderNote(_ k: String, _ fs: [(KeptFolder, FolderStatus.State)]) -> Note {
        let names = fs.map(\.0.displayName).sorted()
        let one = names.count == 1
        let list = one ? "" : " “" + names.joined(separator: "”, “") + "”"
        func detail(_ alt: String) -> String {
            if one {
                switch fs[0].1 {
                case .needsRepair(let w) where !w.isEmpty, .failed(let w) where !w.isEmpty:
                    return w.prefix(1).uppercased() + w.dropFirst() + "."
                default: break
                }
            }
            return alt + list
        }
        switch k {
        case "missing":
            return Note(title: one ? "“\(names[0])” is missing or empty in OpenCloud" : "\(names.count) folders are missing or empty in OpenCloud",
                        body: "Syncing stopped, so nothing is deleted on this Mac. Check the folder in OpenCloud." + list, page: .folders)
        case "repair":
            return Note(title: one ? "“\(names[0])” needs repair" : "\(names.count) folders need repair",
                        body: detail("Open Settings ▸ Kept Folders and choose Repair."), page: .folders)
        case "deletion":
            return Note(title: one ? "Large deletion blocked in “\(names[0])”" : "Large deletions blocked in \(names.count) folders",
                        body: "More than half of the files were deleted \(side(fs)). Check, then allow the deletion or restore the files." + list,
                        page: .folders)
        default:
            return Note(title: one ? "Sync failed for “\(names[0])”" : "Sync failed for \(names.count) folders",
                        body: detail("OpenCloud Bar tries again at the next sync."), page: .folders)
        }
    }
}

/// Shows notifications with UserNotifications. The permission status is read on every
/// notification (never cached), and the prompt is only asked for while it is "not determined",
/// so allowing notifications later (Settings ▸ General, or System Settings) works without a
/// relaunch. When they are refused, notifications are skipped (Settings ▸ General then says
/// so, with a button to System Settings ▸ Notifications). A click opens Settings.
@MainActor
final class Notifier: NSObject, UNUserNotificationCenterDelegate {
    private weak var model: AppModel?
    /// The one permission request in flight; every caller waits for its answer (two notes at
    /// once used to see "already asked, not allowed yet" and drop the second). Cleared when it
    /// finishes, so a later request asks again.
    private var authorization: Task<Bool, Never>?

    /// What macOS says about this app's notifications.
    enum Status: Equatable, Sendable { case allowed, notDetermined, denied }

    init(model: AppModel) {
        self.model = model
        super.init()
        // UserNotifications needs a real app bundle (not a bare `swift run` binary).
        if Bundle.main.bundleIdentifier != nil { UNUserNotificationCenter.current().delegate = self }
    }

    /// The current status, asked each time. A bare `swift run` binary can't be asked: allowed.
    nonisolated static func status() async -> Status {
        guard Bundle.main.bundleIdentifier != nil else { return .allowed }
        switch await UNUserNotificationCenter.current().notificationSettings().authorizationStatus {
        case .denied: return .denied
        case .notDetermined: return .notDetermined
        default: return .allowed
        }
    }

    /// Asks macOS for permission through the single authorization task (joining one already
    /// running). True when it was granted; false when refused or when the request threw.
    func requestAuthorization() async -> Bool {
        if authorization == nil {
            authorization = Task { [weak self] in
                defer { self?.authorization = nil }
                do {
                    return try await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound])
                } catch {
                    Log.line("notification permission request failed: \(error.localizedDescription)")
                    return false
                }
            }
        }
        return await authorization?.value ?? false
    }

    func show(_ n: Note) {
        guard Bundle.main.bundleIdentifier != nil else { return }
        Task {
            var status = await Self.status()
            if status == .notDetermined {
                _ = await requestAuthorization()
                status = await Self.status()
            }
            guard status == .allowed else {
                Log.line("notifications not allowed; skipping this one")
                return
            }
            let c = UNMutableNotificationContent()
            c.title = n.title
            c.body = n.body
            c.userInfo = ["page": n.page.rawValue]
            let req = UNNotificationRequest(identifier: UUID().uuidString, content: c, trigger: nil)
            try? await UNUserNotificationCenter.current().add(req)
        }
    }

    /// System Settings ▸ Notifications, at this app when macOS knows it. Only opened when the
    /// user clicks; nothing is changed here.
    nonisolated static var settingsURL: URL? {
        let base = "x-apple.systempreferences:com.apple.Notifications-Settings.extension"
        if let id = Bundle.main.bundleIdentifier, let u = URL(string: base + "?id=" + id) { return u }
        return URL(string: base)
    }

    // A menu bar app is rarely "in front"; show the banner anyway.
    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                            withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                            withCompletionHandler completionHandler: @escaping () -> Void) {
        let raw = response.notification.request.content.userInfo["page"] as? String
        Task { @MainActor in
            self.model?.openSettings(raw.flatMap(SettingsPage.init(rawValue:)))
            completionHandler()
        }
    }
}

// MARK: - About

@MainActor
final class AboutWindowController: NSObject, NSWindowDelegate {
    private var window: NSWindow?
    private let model: AppModel

    init(model: AppModel) { self.model = model }

    func show() {
        if window == nil {
            let host = NSHostingController(rootView: AboutView(rclone: model.config.rclone))
            let w = NSWindow(contentViewController: host)
            w.title = "About OpenCloud Bar"
            w.styleMask = [.titled, .closable]
            w.isReleasedWhenClosed = false
            w.delegate = self
            w.center()
            window = w
        }
        NSApp.activate(ignoringOtherApps: true)
        window?.makeKeyAndOrderFront(nil)
    }

    func windowWillClose(_ notification: Notification) {
        DispatchQueue.main.async { [weak self] in self?.window = nil }
    }
}

struct AboutView: View {
    let rclone: String
    @Local private var rcloneVersion = "…"

    private static let links: [(String, String)] = [
        ("GitHub Project", "https://github.com/sbstndalton/opencloud-bar"),
        ("Releases", "https://github.com/sbstndalton/opencloud-bar/releases"),
        ("License (MIT)", "https://github.com/sbstndalton/opencloud-bar/blob/main/LICENSE"),
        ("rclone License", "https://github.com/rclone/rclone/blob/master/COPYING"),
    ]

    var body: some View {
        VStack(spacing: 10) {
            Image(nsImage: NSApp.applicationIconImage).resizable().frame(width: 96, height: 96)
            Text("OpenCloud Bar").font(.title2).bold()
            Text("Version \(Updates.running)\(Updates.isDevBuild ? " (development build)" : "")").textSelection(.enabled)
            Text("rclone \(rcloneVersion)").font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            Text("Two-way sync of chosen folders and an on-demand drive for OpenCloud, built on rclone. Open source under the MIT License.")
                .font(.caption).foregroundStyle(.secondary).multilineTextAlignment(.center).fixedSize(horizontal: false, vertical: true)
            HStack {
                ForEach(Self.links, id: \.0) { l in
                    Button(l.0) { if let u = URL(string: l.1) { NSWorkspace.shared.open(u) } }
                }
            }
        }
        .padding(24)
        .frame(width: 460)
        .task {
            // `rclone version` runs off the main thread (Shell.run waits in the background).
            let r = await Shell.run(rclone, ["version"], timeout: 15)
            let first = r.output.split(separator: "\n").first.map(String.init) ?? ""
            rcloneVersion = r.status == 0 && first.hasPrefix("rclone ") ? String(first.dropFirst(7)) : "not found"
        }
    }
}
