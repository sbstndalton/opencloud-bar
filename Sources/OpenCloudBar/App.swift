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

@main
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
        Divider()

        Section("OpenCloud Drive") {
            Button("Open OpenCloud Drive") { model.openDrive() }
            if model.driveMounted {
                Button("Eject Drive") { model.ejectDrive() }
            } else {
                Button(model.driveWanted ? "Connecting…" : "Connect Drive") { model.connectDrive() }
                    .disabled(model.driveWanted)
            }
        }
        Divider()

        Section("Kept on This Mac") {
            ForEach(model.config.kept) { f in
                Menu(label(for: f)) {
                    Button("Open in Finder") { model.openLocal(f) }
                    switch model.status[f.path]?.state {
                    case .needsRepair:
                        Button("Repair (Merge Both Sides)…") { model.repair(f) }.disabled(model.syncing)
                    case .bigDeletion:
                        Button("Allow Large Deletion…") { model.allowBigDeletion(f) }.disabled(model.syncing)
                    default:
                        EmptyView()
                    }
                    Divider()
                    Button("Stop Keeping on This Mac…") { model.stopKeeping(f) }
                        .disabled(model.syncing)
                }
            }
            Button("Keep a Folder on This Mac…") { model.keepFolder() }
                .disabled(model.syncing)
        }
        Divider()

        Button("Sync Now") { model.syncNow() }
            .disabled(model.syncing || !model.config.isConfigured)
            .keyboardShortcut("s")
        Toggle("Open at Login", isOn: Binding(get: { model.opensAtLogin },
                                              set: { _ in model.toggleOpenAtLogin() }))
        Menu("Advanced") {
            Button("Show Logs") { model.openLogs() }
            Button("Edit Settings File") { model.editConfig() }
            Button("Reload Settings") { model.reloadConfig() }
        }
        Divider()
        Button("Quit OpenCloud Bar") { NSApp.terminate(nil) }
            .keyboardShortcut("q")
    }

    private func label(for f: KeptFolder) -> String {
        let s = model.status[f.path]
        switch s?.state {
        case .syncing: return "\(f.displayName) — syncing…"
        case .failed(let why): return "\(f.displayName) — \(why)"
        case .needsRepair(let why): return "\(f.displayName) — needs repair (\(why))"
        case .bigDeletion: return "\(f.displayName) — large deletion blocked"
        case .skipped(let why): return "\(f.displayName) — skipped (\(why))"
        default:
            if let t = s?.lastSuccess { return "\(f.displayName) — \(AppModel.relative(t))" }
            return "\(f.displayName) — waiting"
        }
    }
}
