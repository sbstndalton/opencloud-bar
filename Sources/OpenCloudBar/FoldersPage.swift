import AppKit
import SwiftUI

extension FolderStatus.State {
    /// SF Symbol for the menu and the Kept Folders table.
    var symbol: String {
        switch self {
        case .idle: return "clock"
        case .syncing: return "arrow.triangle.2.circlepath"
        case .ok: return "checkmark.circle"
        case .skipped: return "pause.circle"
        case .failed, .needsRepair, .bigDeletion: return "exclamationmark.triangle.fill"
        }
    }

    var text: String {
        switch self {
        case .idle: return "Waiting"
        case .syncing: return "Syncing…"
        case .ok: return "Up to date"
        case .skipped(let why): return "Skipped (\(why))"
        case .failed(let why): return "Failed: \(why)"
        case .needsRepair(let why): return "Needs repair (\(why))"
        case .bigDeletion(let side): return side.isEmpty ? "Large deletion blocked" : "Large deletion blocked (\(side))"
        }
    }
}

/// Validates rclone filter lines ("- pattern" excludes, "+ pattern" includes, "#" comments).
enum FilterText {
    static func lines(_ text: String) -> [String] {
        text.split(whereSeparator: \.isNewline)
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
    }

    static func problem(in lines: [String]) -> String? {
        for l in lines where !(l.hasPrefix("- ") || l.hasPrefix("+ ") || l.hasPrefix("#") || l.hasPrefix(";") || l == "!") {
            return "“\(l)” isn't a filter rule. Start each line with “- ” to exclude or “+ ” to include, for example “- /Old backups/**”."
        }
        return nil
    }
}

/// An editable list of filter rules with Apply / Revert.
struct FilterEditor: View {
    let rules: [String]
    /// What the rules field is called for VoiceOver.
    var label = "Exclude rules"
    let disabledReason: String?
    var restoreDefaults: [String]?
    /// Called with the new rules; returns an error to show, or nil.
    let apply: ([String]) async -> String?

    @Local private var text = ""
    @Local private var error: String?
    @Local private var busy = false

    private var edited: [String] { FilterText.lines(text) }
    private var changed: Bool { edited != rules }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            TextEditor(text: $text)
                .accessibilityLabel(label)
                .font(.system(.body, design: .monospaced))
                .frame(minHeight: 90, maxHeight: 160)
                .overlay(RoundedRectangle(cornerRadius: 6).stroke(Color(nsColor: .separatorColor)))
                .disabled(disabledReason != nil)
            if let why = disabledReason {
                Text(why).font(.caption).foregroundStyle(.secondary)
            }
            if let error {
                Text(error).font(.caption).foregroundStyle(.red).fixedSize(horizontal: false, vertical: true)
            }
            HStack {
                Button("Apply") {
                    if let p = FilterText.problem(in: edited) { error = p; return }
                    busy = true
                    Task {
                        error = await apply(edited)
                        busy = false
                    }
                }
                .disabled(!changed || busy || disabledReason != nil)
                .accessibilityLabel("Apply")
                Button("Revert") { text = rules.joined(separator: "\n"); error = nil }
                    .disabled(!changed)
                    .accessibilityLabel("Revert")
                if let d = restoreDefaults {
                    Button("Restore Defaults") { text = d.joined(separator: "\n"); error = nil }
                        .accessibilityLabel("Restore Defaults")
                        .disabled(disabledReason != nil || edited == d)
                }
            }
        }
        .onAppear { text = rules.joined(separator: "\n") }
        .onChange(of: rules) { old, new in
            // Follow outside changes (Reload Settings) unless the user is mid-edit.
            if edited == old { text = new.joined(separator: "\n") }
        }
    }
}

struct FoldersPage: View {
    @ObservedObject var model: AppModel
    @ObservedObject var store: ServerInfoStore
    @Local private var selection: KeptFolder.ID?
    @Local private var keepingFromSpace = false
    @Local private var choosingSubfolders: KeptFolder?

    private var selected: KeptFolder? { model.config.kept.first { $0.id == selection } }

    private func localPath(_ f: KeptFolder) -> String {
        (model.config.localURL(f).path as NSString).abbreviatingWithTildeInPath
    }

    private func state(_ f: KeptFolder) -> FolderStatus.State { model.status[f.key]?.state ?? .idle }

    /// What VoiceOver says for a folder's row: its name, its space, its status.
    private func rowLabel(_ f: KeptFolder) -> String {
        f.displayName + (f.space.map { " in \($0.name)" } ?? "") + ", " + state(f).text
    }

    private var browserURL: URL? {
        if let w = store.account?.webURL, let u = URL(string: w) { return u }
        guard let s = model.config.serverURL, !s.isEmpty else { return nil }
        return URL(string: s)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Table(model.config.kept, selection: $selection) {
                TableColumn("Name") { f in
                    Group {
                        if let s = f.space {
                            (Text(f.displayName) + Text("  in \(s.name)").foregroundStyle(.secondary)).lineLimit(1)
                        } else {
                            Text(f.displayName).lineLimit(1)
                        }
                    }
                    .accessibilityLabel(rowLabel(f))
                }
                TableColumn("On This Mac") { f in
                    Text(localPath(f)).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                }
                TableColumn("Status") { f in
                    if case .syncing = state(f) {
                        HStack(spacing: 6) {
                            ProgressView().controlSize(.small).accessibilityHidden(true)
                            Text(model.activeFiles > 0 && model.activeFolder == f.displayName
                                 ? "Syncing… · \(model.activeFiles) file\(model.activeFiles == 1 ? "" : "s")" : "Syncing…")
                                .lineLimit(1)
                        }
                        .accessibilityElement(children: .combine)
                    } else if model.status[f.key]?.pending == true {
                        Label("Change detected, syncing soon", systemImage: "clock.arrow.circlepath")
                            .lineLimit(1)
                            .accessibilityLabel("Change detected, syncing soon")
                    } else {
                        Label(state(f).text, systemImage: state(f).symbol)
                            .lineLimit(1)
                            .foregroundStyle(model.hasProblem(f) ? Color.orange : Color.primary)
                            .accessibilityLabel(state(f).text)
                    }
                }
                TableColumn("Last Synced") { f in
                    TimelineView(.periodic(from: .now, by: 30)) { _ in
                        if let t = model.status[f.key]?.lastSuccess {
                            Text(AppModel.relative(t)).accessibilityLabel("Last synced \(AppModel.relative(t))")
                        } else {
                            Text("Never").foregroundStyle(.secondary).accessibilityLabel("Never synced")
                        }
                    }
                }
            }
            .sheet(isPresented: $keepingFromSpace) {
                KeepFromSpaceSheet(model: model) { keepingFromSpace = false }
            }
            .sheet(item: $choosingSubfolders) { f in
                ChooseSubfoldersSheet(model: model, folder: f) { choosingSubfolders = nil }
            }
            .contextMenu(forSelectionType: KeptFolder.ID.self) { ids in
                if let f = model.config.kept.first(where: { ids.contains($0.id) }) { actions(f) }
            } primaryAction: { ids in
                if let f = model.config.kept.first(where: { ids.contains($0.id) }) { model.openLocal(f) }
            }
            .overlay {
                if model.config.kept.isEmpty {
                    VStack(spacing: 6) {
                        Image(systemName: "folder.badge.plus").font(.largeTitle).foregroundStyle(.secondary)
                            .accessibilityHidden(true)
                        Text("No folders kept on this Mac").font(.headline)
                        Text("Choose Keep a Folder… to keep a copy of an OpenCloud folder here. It syncs both ways.")
                            .foregroundStyle(.secondary).multilineTextAlignment(.center)
                    }
                    .padding()
                }
            }
            .frame(minHeight: 150)
            .accessibilityLabel("Kept folders")

            Divider()
            if model.config.paused {
                HStack {
                    Image(systemName: "pause.circle.fill").foregroundStyle(.orange).accessibilityHidden(true)
                    Text("Syncing is paused. Kept folders don't sync until you resume; the drive keeps working.")
                    Spacer()
                    Button("Resume Syncing") { model.setPaused(false) }.accessibilityLabel("Resume Syncing")
                }
                .padding(10)
                .background(Color.orange.opacity(0.12))
            }
            HStack {
                Button("Keep a Folder…") { model.keepFolder() }
                    .disabled(model.needsSignIn)
                    .accessibilityLabel("Keep a Folder…")
                Button("Keep from a Space…") { keepingFromSpace = true }
                    .disabled(model.needsSignIn || !model.config.isOIDC)
                    .help("Project spaces and folders shared with you")
                    .accessibilityLabel("Keep from a Space…")
                Button(model.config.paused ? "Resume Syncing" : "Pause Syncing") { model.setPaused(!model.config.paused) }
                    .disabled(model.configError != nil || !model.config.isConfigured)
                    .accessibilityLabel(model.config.paused ? "Resume Syncing" : "Pause Syncing")
                Spacer()
                if let f = selected {
                    Menu("Actions for “\(f.displayName)”") { actions(f) }.fixedSize()
                        .accessibilityLabel("Actions for \(f.displayName)")
                }
            }
            .padding(10)

            if let f = selected {
                Divider()
                detail(f)
            }
        }
    }

    /// Per-folder actions: used by the context menu and the Actions menu.
    @ViewBuilder
    private func actions(_ f: KeptFolder) -> some View {
        Button("Open in Finder") { model.openLocal(f) }.accessibilityLabel("Open in Finder")
        Button("Open in Browser") { if let u = browserURL { NSWorkspace.shared.open(u) } }.accessibilityLabel("Open in Browser")
            .disabled(browserURL == nil)
        Button("Sync Now") { model.syncNow(f) }.accessibilityLabel("Sync Now")
            .disabled(model.syncing || model.needsSignIn || model.config.paused)
        switch state(f) {
        case .needsRepair:
            Button("Repair (Merge Both Sides)…") { model.repair(f) }.disabled(model.syncing).accessibilityLabel("Repair (Merge Both Sides)…")
        case .bigDeletion:
            Button("Allow Large Deletion…") { model.allowBigDeletion(f) }.disabled(model.syncing).accessibilityLabel("Allow Large Deletion…")
        default:
            EmptyView()
        }
        Button("Choose Subfolders…") { choosingSubfolders = f }.accessibilityLabel("Choose Subfolders…")
            .disabled(model.needsSignIn || model.folderBusy(f))
        Button("Copy Public Link…") { model.shareFrom(f) }.accessibilityLabel("Copy Public Link…")
            .disabled(model.needsSignIn || !model.config.isOIDC)
        Divider()
        Button("Stop Keeping on This Mac…") { model.stopKeeping(f) }.disabled(model.syncing).accessibilityLabel("Stop Keeping on This Mac…")
    }

    private func detail(_ f: KeptFolder) -> some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 10) {
                Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 4) {
                    GridRow {
                        Text("On this Mac").foregroundStyle(.secondary)
                        Text(model.config.localURL(f).path).textSelection(.enabled)
                    }
                    GridRow {
                        Text("In OpenCloud").foregroundStyle(.secondary)
                        Text("/" + f.path).textSelection(.enabled)
                    }
                    GridRow {
                        Text("Status").foregroundStyle(.secondary)
                        Text(state(f).text).textSelection(.enabled)
                    }
                    GridRow {
                        Text("Last synced").foregroundStyle(.secondary)
                        Text(model.status[f.key]?.lastSuccess.map { agoText($0) } ?? "Never")
                    }
                }
                HStack {
                    Text("Subfolders").font(.headline)
                    Spacer()
                    Button("Choose Subfolders…") { choosingSubfolders = f }
                        .disabled(model.needsSignIn || model.folderBusy(f))
                        .accessibilityLabel("Choose Subfolders of \(f.displayName)…")
                }
                Text("Choose which subfolders sync to this Mac.")
                    .font(.caption).foregroundStyle(.secondary)
                Text("Excludes for this folder").font(.headline)
                Text("Files matching these rules are not synced, on top of the excludes under Sync. One rule per line, for example “- /Old backups/**”.")
                    .font(.caption).foregroundStyle(.secondary)
                FilterEditor(rules: f.extraFilters,
                             label: "Excludes for \(f.displayName)",
                             disabledReason: model.folderBusy(f) ? "Wait for this folder's sync to finish to change its excludes." : nil,
                             restoreDefaults: nil) { lines in
                    guard model.confirm("Change the excludes for “\(f.displayName)”?",
                                        AppModel.filtersChangeText("“\(f.displayName)”"),
                                        button: "Change Excludes") else { return nil }
                    return await model.updateConfig { c in
                        if let i = c.kept.firstIndex(where: { $0.key == f.key }) { c.kept[i].extraFilters = lines }
                    }
                }
                .id(f.id)
            }
            .padding(12)
        }
        .frame(minHeight: 150, maxHeight: 280)
    }
}
