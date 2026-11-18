import SwiftUI

/// Settings ▸ Transfers: what is moving right now. Kept folders come from rclone's progress
/// blocks while they sync; the drive's uploads from its upload queue. Read-only. rclone doesn't
/// say which way a file is going, so the wording is neutral and there is no arrow.
struct TransfersPage: View {
    @ObservedObject var model: AppModel

    private var folders: [(name: String, key: String, files: [Transfer])] {
        let names = Dictionary(model.config.kept.map { ($0.key, $0.displayName) }, uniquingKeysWith: { a, _ in a })
        return model.transfers.map { (names[$0.key] ?? model.activeNames[$0.key] ?? $0.key, $0.key, $0.value) }
            .sorted { ($0.name, $0.key) < ($1.name, $1.key) }
    }

    private static func progress(_ t: Transfer) -> String {
        t.percent < 0 ? "Transferring" : "\(t.percent)%"
    }

    var body: some View {
        let queue = model.driveQueue
        Group {
            if model.transfers.isEmpty && queue.total == 0 {
                Text("Nothing is transferring.").foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                List {
                    ForEach(folders, id: \.key) { folder in
                        Section(folder.name) {
                            ForEach(folder.files) { t in
                                HStack {
                                    Text(t.name).lineLimit(1).truncationMode(.middle)
                                    Spacer()
                                    Text(Self.progress(t)).monospacedDigit()
                                    Text(t.size.isEmpty ? "–" : t.size).frame(width: 90, alignment: .trailing)
                                    Text(t.speed.isEmpty ? "–" : t.speed).frame(width: 100, alignment: .trailing)
                                    Text(t.eta.isEmpty ? "–" : t.eta).frame(width: 60, alignment: .trailing)
                                }
                                .font(.callout)
                                .accessibilityElement(children: .combine)
                            }
                        }
                    }
                    if queue.total > 0 {
                        Section("OpenCloud Drive uploads") {
                            Text("\(queue.uploading) uploading, \(queue.queued) waiting" + (queue.countsOnly ? ". This rclone doesn't list the files." : ""))
                                .font(.caption).foregroundStyle(.secondary)
                            ForEach(queue.files) { u in
                                HStack {
                                    Text(u.name).lineLimit(1).truncationMode(.middle)
                                    Spacer()
                                    Text(ByteCountFormatter.string(fromByteCount: u.size, countStyle: .file))
                                    Text(u.uploading ? "Transferring" : "Waiting").frame(width: 100, alignment: .trailing)
                                }
                                .font(.callout)
                                .accessibilityElement(children: .combine)
                            }
                            if queue.total > queue.files.count && !queue.countsOnly {
                                Text("And \(queue.total - queue.files.count) more.").font(.caption).foregroundStyle(.secondary)
                            }
                        }
                    }
                }
            }
        }
        .task {
            model.transfersPageOpen = true   // the drive is asked every 5 s while this page is open
            defer { model.transfersPageOpen = false }
            while !Task.isCancelled {
                await model.refreshDriveQueue()
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
        }
    }
}
