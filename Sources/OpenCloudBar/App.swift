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
        OpenCloudBarApp.main()
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
            Button("Set Up / Sign In…") { model.showSetup() }
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
            .disabled(model.syncing || model.needsSignIn)
            .keyboardShortcut("s")
        Button("Settings…") { model.openSettings() }
            .keyboardShortcut(",")
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
                Button {
                    model.openLocal(f)
                } label: {
                    Label((names[f.displayName] ?? 0) > 1 ? f.path : f.displayName,
                          systemImage: (model.status[f.path]?.state ?? .idle).symbol)
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
