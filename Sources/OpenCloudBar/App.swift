import AppKit
import SwiftUI

/// Routes every way of quitting (menu, logout, shutdown) through the model's clean shutdown.
final class AppDelegate: NSObject, NSApplicationDelegate {
    @MainActor lazy var model = AppModel()

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        MainActor.assumeIsolated {
            if model.quitting { return .terminateLater }
            model.quit()
            return .terminateLater
        }
    }
}

/// Entry point. `--token` (rclone's bearer token command) prints a token and exits before any UI starts.
@main
enum Launcher {
    static func main() {
        if CommandLine.arguments.contains("--token") { Auth.runTokenMode() }
        guard SingleInstance.claim() else {
            // A second copy would start a second drive and a second sync of the same folders.
            NSApplication.shared.setActivationPolicy(.accessory)
            let a = NSAlert()
            a.messageText = "OpenCloud Bar is already running"
            a.informativeText = "Look for its cloud icon in the menu bar."
            NSApp.activate(ignoringOtherApps: true)
            a.runModal()
            exit(0)
        }
        OpenCloudBarApp.main()
    }
}

/// One copy per user: an exclusive lock on a file, held until the process exits (the system
/// drops it even after a crash, so there is no stale lock to clean up).
enum SingleInstance {
    nonisolated(unsafe) private static var fd: Int32 = -1

    static func claim() -> Bool {
        let dir = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("OpenCloudBar")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let path = dir.appendingPathComponent("instance.lock").path
        let f = open(path, O_RDWR | O_CREAT, 0o600)
        guard f >= 0 else { return true }   // can't tell: don't refuse to start
        if flock(f, LOCK_EX | LOCK_NB) != 0 {
            close(f)
            return errno != EWOULDBLOCK
        }
        fd = f
        return true
    }
}

struct OpenCloudBarApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: delegate.model)
        } label: {
            MenuIcon(model: delegate.model)
        }
        .menuBarExtraStyle(.menu)
    }
}

struct MenuIcon: View {
    @ObservedObject var model: AppModel
    var body: some View { Image(systemName: model.symbol) }
}

struct MenuContent: View {
    @ObservedObject var model: AppModel

    var body: some View {
        Text(model.summary)
        if let msg = model.lastMessage { Text(msg) }
        if model.needsSignIn {
            Button(model.waitingForKeychain ? "Sign In Again…" : "Set Up / Sign In…") { model.showSetup() }
        }
        if model.updateState.available, let v = model.updateState.latest {
            Button("Update Available: v\(v)…") { model.openRelease() }
        }
        Divider()

        Section("OpenCloud Drive") {
            Button("Open OpenCloud Drive") { model.openDrive() }
                .disabled(model.needsSignIn)
            if model.driveMounted {
                Button("Eject Drive") { model.ejectDrive() }
            } else {
                Button(model.driveWanted ? "Connecting…" : "Connect Drive") { model.connectDrive() }
                    .disabled(model.driveWanted || model.needsSignIn)
            }
        }
        Divider()

        keptFoldersMenu
        Divider()

        Button("Sync Now") { model.syncNow() }
            .disabled(model.syncing || model.needsSignIn || model.config.paused)
            .keyboardShortcut("s")
        Button(model.config.paused ? "Resume Syncing" : "Pause Syncing") { model.setPaused(!model.config.paused) }
            .disabled(!model.config.isConfigured || model.configError != nil)
        Button("Settings…") { model.openSettings() }
            .keyboardShortcut(",")
        Button("Check for Updates…") { Task { await model.checkForUpdates(manual: true) } }
            .disabled(model.updateState.checking)
        Button("About OpenCloud Bar") { model.showAbout() }
        Divider()
        Button("Quit OpenCloud Bar") { NSApp.terminate(nil) }
            .keyboardShortcut("q")
    }

    private static let menuLimit = 15

    /// One compact submenu, however many folders are kept. Folders that need attention come
    /// first; past `menuLimit` folders only the most relevant ones are listed.
    private var keptFoldersMenu: some View {
        let all = model.config.kept
        let attention = all.filter { model.hasProblem($0) }
        var shown = attention + all.filter { !model.hasProblem($0) }
        if all.count > Self.menuLimit {
            let rest = all.filter { !model.hasProblem($0) }
                .sorted { (model.status[$0.path]?.lastSuccess ?? .distantPast) > (model.status[$1.path]?.lastSuccess ?? .distantPast) }
            shown = Array((attention + rest).prefix(Self.menuLimit))
        }
        let names = Dictionary(grouping: all, by: \.displayName).mapValues(\.count)
        return Menu("Kept Folders (\(all.count))") {
            ForEach(shown) { f in
                let name = (names[f.displayName] ?? 0) > 1 ? f.path : f.displayName
                let symbol = (model.status[f.path]?.state ?? .idle).symbol
                if let why = model.attentionReason(f) {
                    // A folder that needs the user: say why, and offer the fix right here.
                    Menu {
                        Text(why.prefix(1).uppercased() + why.dropFirst())
                        Divider()
                        switch model.status[f.path]?.state {
                        case .needsRepair:
                            Button("Repair (Merge Both Sides)…") { model.repair(f) }.disabled(model.syncing)
                        case .bigDeletion:
                            Button("Allow Large Deletion…") { model.allowBigDeletion(f) }.disabled(model.syncing)
                        default:
                            Button("Sync Now") { model.syncNow(f) }.disabled(model.syncing || model.needsSignIn)
                        }
                        Button("Open in Finder") { model.openLocal(f) }
                        Button("Show in Settings…") { model.openSettings(.folders) }
                    } label: {
                        Label("\(name) — \(model.menuDetail(f))", systemImage: symbol)
                    }
                } else {
                    Button {
                        model.openLocal(f)
                    } label: {
                        Label("\(name) — \(model.menuDetail(f))", systemImage: symbol)
                    }
                }
            }
            if all.count > Self.menuLimit {
                Button("All \(all.count) Folders…") { model.openSettings(.folders) }
            }
            if !all.isEmpty { Divider() }
            Button("Keep a Folder…") { model.keepFolder() }
                .disabled(model.syncing || model.needsSignIn)
            if !all.isEmpty {
                Button("Manage Folders…") { model.openSettings(.folders) }
            }
        }
    }
}
