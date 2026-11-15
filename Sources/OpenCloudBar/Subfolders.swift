import Foundation
import SwiftUI

/// Choose Subfolders (selective sync): a checklist of a kept folder's subfolders, two levels
/// deep. An unchecked subfolder is one exclude rule `- /<Sub>/**` (or `- /<Sub>/<Child>/**`) in
/// the folder's extraFilters. Only rules of exactly that form are managed; every other rule
/// stays as it is, in place. Saving goes the same way as Change Excludes: the folder then asks
/// for a Repair (a merge that deletes nothing). Same rules as the Go app (cross/subfolders.go),
/// which has the tests.
enum SubfolderRules {
    static let meta = Set("*?[]{}\\")

    static func escape(_ name: String) -> String {
        var s = ""
        for c in name { if meta.contains(c) { s.append("\\") }; s.append(c) }
        return s
    }

    /// nil for a hand-written pattern (an unescaped glob character) or a stray backslash.
    static func unescape(_ s: String) -> String? {
        var out = "", esc = false
        for c in s {
            if esc {
                guard meta.contains(c) else { return nil }
                out.append(c); esc = false
            } else if c == "\\" {
                esc = true
            } else if meta.contains(c) {
                return nil
            } else {
                out.append(c)
            }
        }
        return esc ? nil : out
    }

    /// A single folder name a rule can be written for.
    static func choosable(_ n: String) -> Bool {
        !n.isEmpty && n != "." && n != ".." && !n.contains("/")
            && !n.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) })
            && n.trimmingCharacters(in: .whitespaces) == n
    }

    static func rule(_ sub: String) -> String {
        "- /" + sub.split(separator: "/", omittingEmptySubsequences: false).map { escape(String($0)) }.joined(separator: "/") + "/**"
    }

    /// The subfolder ("A" or "A/B") a rule of exactly the checklist's form excludes.
    static func parse(_ line: String) -> String? {
        let l = line.trimmingCharacters(in: .whitespaces)
        guard l.hasPrefix("- /"), l.hasSuffix("/**"), l.count > "- //**".count else { return nil }
        let mid = String(l.dropFirst(3).dropLast(3))
        let parts = mid.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard parts.count <= 2 else { return nil }
        var names: [String] = []
        for p in parts {
            guard let n = unescape(p), choosable(n) else { return nil }
            names.append(n)
        }
        return names.joined(separator: "/")
    }

    /// Names match exactly, case included: Photos and photos are two folders on the server.
    static func excluded(_ rules: [String]) -> Set<String> { Set(rules.compactMap(parse)) }

    /// Applies the checklist's changes (subfolder → included). The checklist's rules come first,
    /// as one block (rclone uses the first rule that matches), the other rules follow in their
    /// order. In the block, unchanged rules keep their order and new ones go at its end.
    /// Unchecking a folder drops its own subfolders' rules (it excludes them already).
    static func apply(_ rules: [String], _ changes: [String: Bool]) -> [String] {
        func parentOff(_ s: String) -> Bool {
            guard s.contains("/") else { return false }
            return changes[String(s.split(separator: "/")[0])] == false
        }
        var managed: [String] = [], other: [String] = [], have = Set<String>()
        for l in rules {
            guard let s = parse(l) else { other.append(l); continue }
            if changes[s] == true || parentOff(s) || have.contains(s) { continue }
            have.insert(s)
            managed.append(l)
        }
        let add = changes.filter { sub, inc in
            if inc || have.contains(sub) || parentOff(sub) { return false }
            if sub.contains("/"), have.contains(String(sub.split(separator: "/")[0])) { return false }
            return true
        }.keys.sorted()
        return managed + add.map(rule) + other
    }
}

/// Kept Folders ▸ Choose Subfolders…
struct ChooseSubfoldersSheet: View {
    @ObservedObject var model: AppModel
    let folder: KeptFolder
    var close: () -> Void
    @Local private var top: [String]?
    @Local private var children: [String: [String]] = [:]
    @Local private var changes: [String: Bool] = [:]
    @Local private var error: String?
    @Local private var busy = false

    private var excluded: Set<String> { SubfolderRules.excluded(currentRules) }
    private var currentRules: [String] { model.config.kept.first { $0.key == folder.key }?.extraFilters ?? folder.extraFilters }

    private func included(_ rel: String) -> Bool { changes[rel] ?? !excluded.contains(rel) }

    private func toggle(_ rel: String, on: Bool, enabled: Bool, name: String) -> some View {
        Toggle(isOn: Binding(get: { on }, set: { changes[rel] = $0 })) { Text(name + "/") }
            .toggleStyle(.checkbox)
            .disabled(!enabled)
            .accessibilityLabel(rel.replacingOccurrences(of: "/", with: " ▸ "))
            .accessibilityValue(on ? "synced" : "not synced")
            .help(SubfolderRules.choosable(name) ? "" : "This folder's name can't be unchecked here.")
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Choose Subfolders of “\(folder.displayName)”").font(.headline)
            Text("Unchecked folders stop syncing to this Mac. Nothing is deleted from OpenCloud, and the copy on this Mac stays where it is; move it to the Trash yourself if you don't need it.")
                .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if let top {
                List {
                    if top.isEmpty { Text("No subfolders in OpenCloud.").foregroundStyle(.secondary) }
                    ForEach(top, id: \.self) { name in
                        let on = included(name), ok = SubfolderRules.choosable(name)
                        HStack {
                            toggle(name, on: on, enabled: ok, name: name)
                            Spacer()
                            if ok, on, children[name] == nil {
                                Button("Show Subfolders") { load(name) }.buttonStyle(.link)
                                    .accessibilityLabel("Show subfolders of \(name)")
                            }
                        }
                        if let kids = children[name] {
                            ForEach(kids, id: \.self) { k in
                                toggle(name + "/" + k, on: on && included(name + "/" + k), enabled: on && SubfolderRules.choosable(k), name: k)
                                    .padding(.leading, 20)
                            }
                        }
                    }
                }
                .frame(minHeight: 200)
                .accessibilityLabel("Subfolders of \(folder.displayName)")
            } else if error == nil {
                ProgressView("Listing subfolders…")
            }
            InlineError(text: error)
            HStack {
                if top == nil, error != nil { Button("Retry") { error = nil; load("") }.accessibilityLabel("Retry") }
                Spacer()
                Button("Cancel") { close() }.keyboardShortcut(.cancelAction).accessibilityLabel("Cancel")
                Button("Save") {
                    busy = true
                    Task {
                        let r = await model.saveSubfolders(folder, changes: changes)
                        busy = false
                        switch r {
                        case .saved: close()
                        case .cancelled: break   // the sheet stays, choices as they were
                        case .failed(let e): error = e
                        }
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(changes.isEmpty || busy)
                .accessibilityLabel("Save")
            }
        }
        .padding(20)
        .frame(width: 480)
        .task { load("") }
    }

    private func load(_ sub: String) {
        Task {
            let path = sub.isEmpty ? folder.path : folder.path + "/" + sub
            switch await model.listDirs(in: folder, path: path) {
            case .success(let d):
                if sub.isEmpty { top = d } else { children[sub] = d }
                error = nil
            case .failure(let e):
                error = e.localizedDescription
            }
        }
    }
}
