import AppKit
import SwiftUI

enum SettingsPage: String, CaseIterable, Identifiable {
    case folders, activity, conflicts, account, sync, drive, general, logs
    var id: String { rawValue }
    var title: String {
        switch self {
        case .folders: return "Kept Folders"
        case .activity: return "Activity"
        case .conflicts: return "Conflicts"
        case .account: return "Account"
        case .sync: return "Sync"
        case .drive: return "OpenCloud Drive"
        case .general: return "General"
        case .logs: return "Logs"
        }
    }
    var symbol: String {
        switch self {
        case .folders: return "folder"
        case .activity: return "clock.arrow.circlepath"
        case .conflicts: return "doc.on.doc"
        case .account: return "person.crop.circle"
        case .sync: return "arrow.triangle.2.circlepath"
        case .drive: return "externaldrive"
        case .general: return "gearshape"
        case .logs: return "doc.plaintext"
        }
    }
}

/// Stand-in for `@State`. The Swift command-line tools don't ship the SwiftUI macro plugin that
/// recent SDKs use to declare `@State`, so `@State` fails to compile with them. This keeps the
/// same behaviour (view-local state that lives as long as the view) using `@StateObject`.
final class LocalBox<Value>: ObservableObject {
    @Published var value: Value
    init(_ v: Value) { value = v }
}

@propertyWrapper
struct Local<Value>: DynamicProperty {
    @StateObject private var box: LocalBox<Value>
    init(wrappedValue: Value) { _box = StateObject(wrappedValue: LocalBox(wrappedValue)) }
    var wrappedValue: Value {
        get { box.value }
        nonmutating set { box.value = newValue }
    }
    var projectedValue: Binding<Value> { Binding(get: { box.value }, set: { box.value = $0 }) }
}

@MainActor
final class SettingsRouter: ObservableObject {
    @Published var page: SettingsPage = .folders
}

// MARK: - Window

/// The "OpenCloud Bar" settings window: one shared instance, rebuilt each time it is opened
/// (so its refresh loops stop while it is closed).
@MainActor
final class SettingsWindowController: NSObject, NSWindowDelegate {
    private unowned let model: AppModel
    private let router = SettingsRouter()
    private var window: NSWindow?

    init(model: AppModel) {
        self.model = model
        super.init()
    }

    func show(page: SettingsPage?) {
        if let page { router.page = page }
        if window == nil {
            let host = NSHostingController(rootView: SettingsView(model: model, router: router))
            host.sizingOptions = []
            let w = NSWindow(contentViewController: host)
            w.title = "OpenCloud Bar"
            w.styleMask = [.titled, .closable, .miniaturizable, .resizable]
            w.isReleasedWhenClosed = false
            w.contentMinSize = NSSize(width: 760, height: 520)
            w.delegate = self
            w.setContentSize(NSSize(width: 900, height: 620))
            if !w.setFrameUsingName("OpenCloudBarSettings") { w.center() }
            w.setFrameAutosaveName("OpenCloudBarSettings")
            window = w
        }
        NSApp.activate(ignoringOtherApps: true)
        window?.makeKeyAndOrderFront(nil)
    }

    func windowWillClose(_ notification: Notification) {
        // Drop the window (and its SwiftUI state) once AppKit is done closing it.
        DispatchQueue.main.async { [weak self] in self?.window = nil }
    }
}

// MARK: - Root

struct SettingsView: View {
    @ObservedObject var model: AppModel
    @ObservedObject var router: SettingsRouter
    @StateObject private var store = ServerInfoStore()

    var body: some View {
        NavigationSplitView {
            List(selection: Binding<SettingsPage?>(get: { router.page }, set: { if let p = $0 { router.page = p } })) {
                ForEach(SettingsPage.allCases) { p in
                    Label(p.title, systemImage: p.symbol).tag(p)
                }
            }
            .navigationSplitViewColumnWidth(min: 170, ideal: 190, max: 230)
        } detail: {
            VStack(spacing: 0) {
                if let e = model.configError { configErrorBanner(e) }
                switch router.page {
                case .folders: FoldersPage(model: model, store: store)
                case .activity: ActivityPage(model: model)
                case .conflicts: ConflictsPage(model: model)
                case .account: AccountPage(model: model, store: store)
                case .sync: SyncPage(model: model, store: store)
                case .drive: DrivePage(model: model)
                case .general: GeneralPage(model: model)
                case .logs: LogsPage(model: model)
                }
            }
            .navigationTitle(router.page.title)
        }
        .toolbar(removing: .sidebarToggle)
        .frame(minWidth: 760, minHeight: 520)
        .task { store.loadIfNeeded(model.config, signedIn: !model.needsSignIn) }
    }

    private func configErrorBanner(_ e: String) -> some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                .accessibilityLabel("Settings problem")
            VStack(alignment: .leading, spacing: 6) {
                Text(e).fixedSize(horizontal: false, vertical: true)
                Text("Settings can't be changed until the settings file is fixed.").font(.caption).foregroundStyle(.secondary)
                HStack {
                    Button("Open Settings File") { model.editConfig() }.accessibilityLabel("Open Settings File")
                    Button("Reload Settings") { model.reloadConfig() }.accessibilityLabel("Reload Settings")
                }
            }
            Spacer()
        }
        .padding(12)
        .background(Color.orange.opacity(0.12))
    }
}

// MARK: - Shared bits

/// A picker over fixed choices that also shows the current value if it isn't one of them
/// (for example a value typed into config.json).
struct ChoicePicker<T: Hashable>: View {
    let title: String
    let choices: [(value: T, label: String)]
    let current: T
    let customLabel: (T) -> String
    let onChange: (T) -> Void

    var body: some View {
        let all = choices.contains(where: { $0.value == current }) ? choices : choices + [(current, customLabel(current))]
        Picker(title, selection: Binding(get: { current }, set: { onChange($0) })) {
            ForEach(all, id: \.value) { Text($0.label).tag($0.value) }
        }
    }
}

struct InlineError: View {
    let text: String?
    var body: some View {
        if let text {
            Text(text).font(.caption).foregroundStyle(.red).fixedSize(horizontal: false, vertical: true)
        }
    }
}

private func byteString(_ n: Int64) -> String {
    ByteCountFormatter.string(fromByteCount: n, countStyle: .file)
}

// MARK: - Account

struct AccountPage: View {
    @ObservedObject var model: AppModel
    @ObservedObject var store: ServerInfoStore

    private var serverText: String {
        if let s = model.config.serverURL, !s.isEmpty { return s }
        return model.config.sshHost.isEmpty ? "Not set up" : "\(model.config.sshHost) (SSH tunnel)"
    }

    /// Legacy configs: the user name is the last part of davPath.
    private var legacyUser: String {
        ((model.config.davPath as NSString).lastPathComponent).removingPercentEncoding ?? model.config.davPath
    }

    private var webURL: URL? {
        if let w = store.account?.webURL, let u = URL(string: w) { return u }
        guard let s = model.config.serverURL, !s.isEmpty else { return nil }
        return URL(string: s)
    }

    var body: some View {
        Form {
            Section("Server") {
                LabeledContent("Address") { Text(serverText).textSelection(.enabled) }
                if model.config.isOIDC {
                    LabeledContent("OpenCloud version") { Text(store.facts?.version ?? "–").textSelection(.enabled) }
                    if let f = store.facts {
                        LabeledContent("Trash") { Text(f.undelete.map { $0 ? "Deleted files can be restored" : "Not available" } ?? "–") }
                        LabeledContent("File versions") { Text(f.versioning.map { $0 ? "Kept" : "Not kept" } ?? "–") }
                    }
                    InlineError(text: store.factsError)
                }
            }
            Section("Account") {
                if model.config.isOIDC {
                    let a = store.account
                    LabeledContent("Name") { Text(a?.displayName ?? "–").textSelection(.enabled) }
                    LabeledContent("Email") { Text(a?.mail ?? "–").textSelection(.enabled) }
                    LabeledContent("Username") {
                        Text(a?.username ?? Auth.readState()?.username ?? "–").textSelection(.enabled)
                    }
                    InlineError(text: store.accountError)
                } else {
                    LabeledContent("Username") { Text(legacyUser).textSelection(.enabled) }
                    Text("Signed in with an app token (advanced)").foregroundStyle(.secondary)
                }
            }
            if model.config.isOIDC, let a = store.account, let used = a.quotaUsed {
                Section("Storage") { quota(used: used, account: a) }
            }
            Section {
                HStack {
                    Button("Open OpenCloud in Browser") { if let u = webURL { NSWorkspace.shared.open(u) } }.accessibilityLabel("Open OpenCloud in Browser")
                        .disabled(webURL == nil)
                    if model.config.isOIDC {
                        Button("Refresh") { store.refresh(model.config, signedIn: !model.needsSignIn) }.accessibilityLabel("Refresh")
                            .disabled(store.loading)
                        if store.loading { ProgressView().controlSize(.small) }
                    }
                    Spacer()
                    if model.config.isOIDC {
                        if model.needsSignIn {
                            Button("Sign In…") { model.showSetup() }.accessibilityLabel("Sign In…")
                        } else {
                            Button("Sign Out…") { model.signOut() }.accessibilityLabel("Sign Out…")
                        }
                    }
                }
            }
        }
        .formStyle(.grouped)
        .task { store.loadIfNeeded(model.config, signedIn: !model.needsSignIn) }
    }

    @ViewBuilder
    private func quota(used: Int64, account a: AccountInfo) -> some View {
        let total = a.quotaTotal ?? 0
        if total > 0 {
            let frac = min(1, Double(used) / Double(total))
            ProgressView(value: frac).tint(quotaColor(a.quotaState))
            HStack {
                Text("\(byteString(used)) of \(byteString(total)) used")
                Spacer()
                Text(quotaLabel(a.quotaState)).foregroundStyle(quotaColor(a.quotaState))
            }
        } else {
            Text("\(byteString(used)) used (no storage limit)")
        }
    }

    private func quotaColor(_ state: String?) -> Color {
        switch state {
        case "nearing": return .orange
        case "critical", "exceeded": return .red
        default: return .green
        }
    }

    private func quotaLabel(_ state: String?) -> String {
        switch state {
        case "nearing": return "Nearly full"
        case "critical": return "Almost out of space"
        case "exceeded": return "Over the limit"
        default: return "OK"
        }
    }
}

// MARK: - Sync

struct SyncPage: View {
    @ObservedObject var model: AppModel
    @ObservedObject var store: ServerInfoStore
    @Local private var error: String?
    @Local private var bwUp = ""
    @Local private var bwDown = ""
    @Local private var bwError: String?

    private static let intervals: [(value: Int, label: String)] = [
        (60, "Every minute"), (300, "Every 5 minutes"), (900, "Every 15 minutes"),
        (1800, "Every 30 minutes"), (3600, "Every hour"),
    ]

    var body: some View {
        Form {
            Section("Kept Folders") {
                Toggle("Pause syncing", isOn: Binding(get: { model.config.paused }, set: { model.setPaused($0) }))
                    .disabled(model.configError != nil || !model.config.isConfigured)
                Text("While paused, kept folders don't sync. The OpenCloud drive keeps working.")
                    .font(.caption).foregroundStyle(.secondary)
                Toggle("Sync changes right away (preview)", isOn: Binding(
                    get: { model.config.liveSync == true },
                    set: { on in Task { error = await model.updateConfig { $0.liveSync = on ? true : nil } } }))
                    .disabled(model.configError != nil || !model.config.isConfigured)
                Text("Instead of waiting for the next interval: a kept folder syncs about 5 seconds after changes in it on this Mac stop (up to a minute while changes continue), and within about 30 seconds of a change in it in OpenCloud (checked with the browser sign-in).")
                    .font(.caption).foregroundStyle(.secondary)
                ChoicePicker(title: "Sync", choices: Self.intervals, current: model.config.syncIntervalSeconds,
                             customLabel: { "Every \($0) seconds" }) { v in
                    Task { error = await model.updateConfig { $0.syncIntervalSeconds = v } }
                }
                InlineError(text: error)
            }
            Section("Excludes for all folders") {
                Text("Files matching these rules are never synced. One rule per line: “- pattern” excludes, “+ pattern” includes.")
                    .font(.caption).foregroundStyle(.secondary)
                FilterEditor(rules: model.config.commonFilters,
                             label: "Excludes for all folders",
                             disabledReason: model.syncing ? "Wait for the running sync to finish to change excludes." : nil,
                             restoreDefaults: Config().commonFilters) { lines in
                    guard model.confirm("Change the excludes for all folders?",
                                        AppModel.filtersChangeText("Each kept folder"),
                                        button: "Change Excludes") else { return nil }
                    return await model.updateConfig { $0.commonFilters = lines }
                }
            }
            Section("Bandwidth") {
                TextField("Upload limit", text: $bwUp, prompt: Text("no limit"))
                TextField("Download limit", text: $bwDown, prompt: Text("no limit"))
                Text("For syncs and the drive, like 512K (KiB/s) or 2M (MiB/s). Empty: no limit.")
                    .font(.caption).foregroundStyle(.secondary)
                HStack {
                    Button("Apply") { applyBandwidth() }.accessibilityLabel("Apply")
                        .disabled(bwUp == (model.config.bwLimitUp ?? "") && bwDown == (model.config.bwLimitDown ?? ""))
                }
                InlineError(text: bwError)
            }
            Section("Where kept folders live") {
                LabeledContent("Location on this Mac") {
                    Text(model.config.localRoot).textSelection(.enabled)
                }
                HStack {
                    Button("Change…") { chooseRoot() }.disabled(!model.config.kept.isEmpty).accessibilityLabel("Change…")
                    if !model.config.kept.isEmpty {
                        Text("Stop keeping all folders first: kept folders stay where they are.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            if model.config.isOIDC {
                Section("Names the server doesn't accept") {
                    if let f = store.facts {
                        Text(f.blockedNames.isEmpty ? "None" : f.blockedNames.joined(separator: ", "))
                            .textSelection(.enabled)
                        Text("Files with these names can't be uploaded to OpenCloud. For information only.")
                            .font(.caption).foregroundStyle(.secondary)
                    } else {
                        Text(store.factsError ?? "Loading…").foregroundStyle(.secondary)
                    }
                }
            }
        }
        .formStyle(.grouped)
        .task { store.loadIfNeeded(model.config, signedIn: !model.needsSignIn) }
        .onAppear { bwUp = model.config.bwLimitUp ?? ""; bwDown = model.config.bwLimitDown ?? "" }
    }

    private func applyBandwidth() {
        let up = bwUp.trimmingCharacters(in: .whitespaces).uppercased(), down = bwDown.trimmingCharacters(in: .whitespaces).uppercased()
        if let p = Config.bwLimitProblem("The upload limit", up) ?? Config.bwLimitProblem("The download limit", down) {
            bwError = p; return
        }
        Task {
            bwError = await model.updateConfig { $0.bwLimitUp = up.isEmpty ? nil : up; $0.bwLimitDown = down.isEmpty ? nil : down }
            if bwError == nil { bwUp = up; bwDown = down }
        }
    }

    private func chooseRoot() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.allowsMultipleSelection = false
        panel.directoryURL = model.config.localRootURL.deletingLastPathComponent()
        panel.prompt = "Choose"
        panel.message = "Choose where folders kept on this Mac are stored."
        NSApp.activate(ignoringOtherApps: true)
        guard panel.runModal() == .OK, let url = panel.url else { return }
        let path = (url.path as NSString).abbreviatingWithTildeInPath
        Task { error = await model.updateConfig { $0.localRoot = path } }
    }
}

// MARK: - Drive

struct DrivePage: View {
    @ObservedObject var model: AppModel
    @Local private var mountPoint = ""
    @Local private var volumeName = ""
    @Local private var error: String?
    @Local private var cacheError: String?
    @Local private var stats: AppModel.DriveStats?

    private static let sizes: [(value: String, label: String)] = [
        ("5G", "5 GB"), ("10G", "10 GB"), ("20G", "20 GB"), ("50G", "50 GB"), ("100G", "100 GB"),
    ]
    private static let ages: [(value: String, label: String)] = [
        ("24h", "1 day"), ("72h", "3 days"), ("7d", "7 days"),
    ]

    private var dirty: Bool { mountPoint != model.config.mountPoint || volumeName != model.config.volumeName }

    var body: some View {
        Form {
            Section("OpenCloud Drive") {
                LabeledContent("Status") {
                    Text(model.driveMounted ? "Connected" : (model.driveWanted ? "Connecting…" : "Not connected"))
                }
                HStack {
                    Button("Open OpenCloud Drive") { model.openDrive() }.disabled(model.needsSignIn).accessibilityLabel("Open OpenCloud Drive")
                    if model.driveMounted {
                        Button("Eject") { model.ejectDrive() }.accessibilityLabel("Eject")
                    } else {
                        Button("Connect") { model.connectDrive() }.accessibilityLabel("Connect")
                            .disabled(model.driveWanted || model.needsSignIn)
                    }
                }
            }
            Section("Location") {
                TextField("Mount location", text: $mountPoint)
                TextField("Volume name", text: $volumeName)
                HStack {
                    Button("Choose…") { chooseMountPoint() }.accessibilityLabel("Choose…")
                    Button("Apply") { apply() }.disabled(!dirty).accessibilityLabel("Apply")
                    Button("Revert") { load() }.disabled(!dirty).accessibilityLabel("Revert")
                }
                Text("Changing the location or name reconnects the drive.").font(.caption).foregroundStyle(.secondary)
                InlineError(text: error)
            }
            Section("Cache") {
                ChoicePicker(title: "Cache size limit", choices: Self.sizes, current: model.config.cacheMaxSize,
                             customLabel: { "\($0) (custom)" }) { v in
                    Task { cacheError = await model.updateConfig { $0.cacheMaxSize = v } }
                }
                ChoicePicker(title: "Keep unused files for", choices: Self.ages, current: model.config.cacheMaxAge,
                             customLabel: { "\($0) (custom)" }) { v in
                    Task { cacheError = await model.updateConfig { $0.cacheMaxAge = v } }
                }
                Text("Files you open are kept on this Mac, up to this size, and removed again after this long unused. Changing these reconnects the drive once uploads have finished.")
                    .font(.caption).foregroundStyle(.secondary)
                InlineError(text: cacheError)
                if model.driveMounted {
                    if let s = stats {
                        LabeledContent("Cache in use") { Text(byteString(s.cacheBytes)) }
                        LabeledContent("Uploading") {
                            Text(s.uploading + s.queued == 0 ? "Nothing pending"
                                 : "\(s.uploading) in progress, \(s.queued) waiting")
                        }
                    } else {
                        LabeledContent("Cache in use") { Text("–") }
                    }
                } else {
                    Text("Live cache numbers show while the drive is connected.").font(.caption).foregroundStyle(.secondary)
                }
            }
        }
        .formStyle(.grouped)
        .onAppear { load() }
        .task {
            while !Task.isCancelled {
                stats = model.driveMounted ? await model.driveStats() : nil
                try? await Task.sleep(nanoseconds: 3_000_000_000)
            }
        }
    }

    private func load() {
        mountPoint = model.config.mountPoint
        volumeName = model.config.volumeName
        error = nil
    }

    private func apply() {
        let mp = mountPoint.trimmingCharacters(in: .whitespaces), vn = volumeName.trimmingCharacters(in: .whitespaces)
        guard !mp.isEmpty, mp.hasPrefix("/") || mp.hasPrefix("~/") else {
            error = "The mount location must be a full path, for example ~/OpenCloud-Drive."; return
        }
        guard !vn.isEmpty, !vn.contains("/"), !vn.contains(":") else {
            error = "The volume name can't be empty or contain / or :."; return
        }
        Task {
            error = await model.updateConfig { $0.mountPoint = mp; $0.volumeName = vn }
            if error == nil { load() }
        }
    }

    private func chooseMountPoint() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.allowsMultipleSelection = false
        panel.prompt = "Choose"
        panel.message = "Choose an empty folder where the OpenCloud drive appears."
        NSApp.activate(ignoringOtherApps: true)
        guard panel.runModal() == .OK, let url = panel.url else { return }
        mountPoint = (url.path as NSString).abbreviatingWithTildeInPath
    }
}

// MARK: - General

struct GeneralPage: View {
    @ObservedObject var model: AppModel
    @Local private var useTunnel = false
    @Local private var sshHost = ""
    @Local private var serverPort = ""
    @Local private var localPort = ""
    @Local private var rclonePath = ""
    @Local private var advancedOpen = false
    @Local private var advancedError: String?
    @Local private var memoryError: String?

    private static let memory: [(value: Int, label: String)] = [
        (2048, "2 GB"), (4096, "4 GB"), (8192, "8 GB"), (16384, "16 GB"),
    ]

    private var advancedDirty: Bool {
        let c = model.config
        return useTunnel != c.tunnel || sshHost != c.sshHost || serverPort != String(c.serverPort)
            || localPort != String(c.localPort) || rclonePath != c.rclonePath
    }

    @Local private var updateError: String?

    private var version: String {
        Updates.isDevBuild ? "\(Updates.running) (development build)" : Updates.running
    }

    private var updateText: String {
        let u = model.updateState
        if u.checking { return "Checking…" }
        if let e = u.error { return "The last check failed: \(e)" }
        guard let t = u.checkedAt else { return "Not checked yet." }
        let when = "Checked \(agoText(t))."
        if u.available, let v = u.latest { return "\(when) Version \(v) is available." }
        if Updates.isDevBuild { return "\(when) This is a development build; the latest release is \(u.latest ?? "unknown")." }
        return "\(when) You have the latest version."
    }

    var body: some View {
        Form {
            Section {
                Toggle("Open at Login", isOn: Binding(get: { model.opensAtLogin }, set: { _ in model.toggleOpenAtLogin() }))
                LabeledContent("Version") { Text(version).textSelection(.enabled) }
            }
            Section("Updates") {
                Toggle("Check for updates automatically", isOn: Binding(
                    get: { model.config.checkForUpdates },
                    set: { on in Task { updateError = await model.updateConfig { $0.checkForUpdates = on } } }))
                Text("Once a day OpenCloud Bar asks GitHub for the latest release. Nothing about you is sent.")
                    .font(.caption).foregroundStyle(.secondary)
                HStack {
                    Text(updateText).foregroundStyle(model.updateState.available ? Color.accentColor : Color.secondary)
                    Spacer()
                    if model.updateState.checking { ProgressView().controlSize(.small) }
                    Button("Check Now") { Task { await model.checkForUpdates(manual: true) } }.accessibilityLabel("Check Now")
                        .disabled(model.updateState.checking)
                    if model.updateState.available, let v = model.updateState.latest {
                        Button("Get v\(v)…") { model.openRelease() }.buttonStyle(.borderedProminent)
                    }
                }
                InlineError(text: updateError)
            }
            Section("Notifications") {
                Toggle("Show notifications", isOn: Binding(
                    get: { model.config.notifications },
                    set: { on in Task { updateError = await model.updateConfig { $0.notifications = on } } }))
                Text("When a folder needs attention or was merged automatically, when you are signed out, and when an update is available. Once per change, never repeated. Clicking one opens Settings.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            if model.config.tunnel {
                Section("Server") {
                    Toggle("Check server memory before syncing", isOn: Binding(
                        get: { model.config.minServerAvailableMiB > 0 },
                        set: { on in Task { memoryError = await model.updateConfig { $0.minServerAvailableMiB = on ? 8192 : 0 } } }))
                    if model.config.minServerAvailableMiB > 0 {
                        ChoicePicker(title: "Skip a sync when less than this is free", choices: Self.memory,
                                     current: model.config.minServerAvailableMiB,
                                     customLabel: { "\($0) MiB (custom)" }) { v in
                            Task { memoryError = await model.updateConfig { $0.minServerAvailableMiB = v } }
                        }
                    }
                    InlineError(text: memoryError)
                }
            }
            Section {
                DisclosureGroup("Advanced", isExpanded: $advancedOpen) {
                    VStack(alignment: .leading, spacing: 10) {
                        Toggle("Connect through an SSH tunnel", isOn: $useTunnel)
                        TextField("SSH host", text: $sshHost)
                        TextField("Server port", text: $serverPort)
                        TextField("Local tunnel port", text: $localPort)
                        TextField("rclone path", text: $rclonePath, prompt: Text(model.config.rclone))
                        Text("Leave the rclone path empty to use the copy inside the app, or Homebrew's. Changes here reconnect the drive.")
                            .font(.caption).foregroundStyle(.secondary)
                        InlineError(text: advancedError)
                        HStack {
                            Button("Apply") { applyAdvanced() }.disabled(!advancedDirty).accessibilityLabel("Apply")
                            Button("Revert") { loadAdvanced() }.disabled(!advancedDirty).accessibilityLabel("Revert")
                        }
                        Divider()
                        Text("Settings file").font(.headline)
                        Text("Everything above can be changed here. The file is only a fallback for hand edits.")
                            .font(.caption).foregroundStyle(.secondary)
                        HStack {
                            Button("Open Settings File") { model.editConfig() }.accessibilityLabel("Open Settings File")
                            Button("Reload Settings") { model.reloadConfig(); loadAdvanced() }.accessibilityLabel("Reload Settings")
                        }
                    }
                    .padding(.top, 6)
                }
            }
        }
        .formStyle(.grouped)
        .onAppear { loadAdvanced() }
    }

    private func loadAdvanced() {
        let c = model.config
        useTunnel = c.tunnel
        sshHost = c.sshHost
        serverPort = String(c.serverPort)
        localPort = String(c.localPort)
        rclonePath = c.rclonePath
        advancedError = nil
    }

    private func applyAdvanced() {
        let host = sshHost.trimmingCharacters(in: .whitespaces)
        let path = rclonePath.trimmingCharacters(in: .whitespaces)
        guard let sp = Int(serverPort.trimmingCharacters(in: .whitespaces)), (1...65535).contains(sp) else {
            advancedError = "The server port must be a number from 1 to 65535."; return
        }
        guard let lp = Int(localPort.trimmingCharacters(in: .whitespaces)), (1...65435).contains(lp) else {
            advancedError = "The local tunnel port must be a number from 1 to 65435 (the drive's control port is 100 higher)."; return
        }
        if !path.isEmpty, !FileManager.default.isExecutableFile(atPath: (path as NSString).expandingTildeInPath) {
            advancedError = "There is no program at that rclone path."; return
        }
        let tunnel = useTunnel
        Task {
            advancedError = await model.updateConfig { c in
                c.useTunnel = tunnel
                c.sshHost = host
                c.serverPort = sp
                c.localPort = lp
                c.rclonePath = path
            }
            if advancedError == nil { loadAdvanced() }
        }
    }
}
