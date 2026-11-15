import AppKit
import SwiftUI

/// Reads the end of a log file without loading all of it.
enum LogReader {
    static func tail(_ url: URL, maxLines: Int = 3000, maxBytes: UInt64 = 1_000_000) -> String? {
        guard let h = try? FileHandle(forReadingFrom: url) else { return nil }
        defer { try? h.close() }
        guard let size = try? h.seekToEnd() else { return nil }
        let start = size > maxBytes ? size - maxBytes : 0
        guard (try? h.seek(toOffset: start)) != nil else { return nil }
        let data = (try? h.readToEnd()) ?? Data()
        // A log cleared while rclone still holds it open can contain NUL bytes.
        let text = String(decoding: data.filter { $0 != 0 }, as: UTF8.self)
        var lines = text.split(separator: "\n", omittingEmptySubsequences: false)
        if start > 0, !lines.isEmpty { lines.removeFirst() }   // the first line is cut off
        return lines.suffix(maxLines).joined(separator: "\n")
    }
}

struct LogSource: Hashable, Identifiable {
    var file: String
    var title: String
    var id: String { file }
    var url: URL { Config.logURL.appendingPathComponent(file) }
}

/// Read-only monospaced text that keeps its scroll position unless it was at the bottom.
struct LogTextView: NSViewRepresentable {
    let text: String
    let sourceID: String

    final class Coordinator {
        var lastText: String?
        var lastSource: String?
    }

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeNSView(context: Context) -> NSScrollView {
        let sv = NSTextView.scrollableTextView()
        sv.hasVerticalScroller = true
        sv.borderType = .lineBorder
        if let tv = sv.documentView as? NSTextView {
            tv.isEditable = false
            tv.isSelectable = true
            tv.isRichText = false
            tv.textContainerInset = NSSize(width: 6, height: 6)
            tv.backgroundColor = .textBackgroundColor
        }
        return sv
    }

    func updateNSView(_ sv: NSScrollView, context: Context) {
        let c = context.coordinator
        guard c.lastText != text || c.lastSource != sourceID, let tv = sv.documentView as? NSTextView else { return }
        let sourceChanged = c.lastSource != sourceID
        c.lastText = text
        c.lastSource = sourceID

        let visible = sv.contentView.bounds
        let atBottom = visible.maxY >= tv.frame.height - 24
        let oldY = visible.origin.y

        let font = NSFont.monospacedSystemFont(ofSize: 11, weight: .regular)
        let out = NSMutableAttributedString()
        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            let lower = line.lowercased()
            let color: NSColor
            if lower.contains("error") || lower.contains("failed") || lower.contains("critical") || lower.contains("fatal") {
                color = .systemRed
            } else if lower.contains("warn") || lower.contains("notice:") {
                color = .systemOrange
            } else {
                color = .labelColor
            }
            out.append(NSAttributedString(string: String(line) + "\n", attributes: [.font: font, .foregroundColor: color]))
        }
        tv.textStorage?.setAttributedString(out)
        if atBottom || sourceChanged {
            tv.scrollToEndOfDocument(nil)
        } else {
            sv.contentView.scroll(to: NSPoint(x: 0, y: oldY))
            sv.reflectScrolledClipView(sv.contentView)
        }
    }
}

struct LogsPage: View {
    @ObservedObject var model: AppModel
    @Local private var sourceID = "app.log"
    @Local private var text = ""
    @Local private var exists = true

    private var sources: [LogSource] {
        var out = [LogSource(file: "app.log", title: "App log")]
        let fm = FileManager.default
        if fm.fileExists(atPath: Config.logURL.appendingPathComponent("drive.log").path) {
            out.append(LogSource(file: "drive.log", title: "Drive (rclone)"))
        }
        if model.config.tunnel || fm.fileExists(atPath: Config.logURL.appendingPathComponent("tunnel.log").path) {
            out.append(LogSource(file: "tunnel.log", title: "SSH tunnel"))
        }
        for f in model.config.kept {
            out.append(LogSource(file: "sync-\(f.stateName).log", title: "Sync: \(f.path)"))
        }
        return out
    }

    private var current: LogSource {
        sources.first { $0.id == sourceID } ?? sources[0]
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Picker("Log", selection: $sourceID) {
                    ForEach(sources) { Text($0.title).tag($0.id) }
                }
                .frame(maxWidth: 360)
                Spacer()
                Button("Copy") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(text, forType: .string)
                }
                .disabled(text.isEmpty)
                Button("Reveal in Finder") {
                    let u = current.url
                    if FileManager.default.fileExists(atPath: u.path) {
                        NSWorkspace.shared.activateFileViewerSelecting([u])
                    } else {
                        model.openLogs()
                    }
                }
                Button("Clear…") { clear() }.accessibilityLabel("Clear…")
                    .disabled(!exists || current.file == "drive.log")
                    .help(current.file == "drive.log" ? "rclone keeps this file open while the drive runs" : "")
            }
            ZStack {
                LogTextView(text: text, sourceID: current.id)
                if !exists || text.isEmpty {
                    Text(exists ? "This log is empty." : "Nothing has been logged here yet.")
                        .foregroundStyle(.secondary)
                }
            }
            Text("Showing the last 3000 lines. Updates every 2 seconds.")
                .font(.caption).foregroundStyle(.secondary)
        }
        .padding(16)
        .task(id: sourceID) {
            while !Task.isCancelled {
                let u = current.url
                let read = await Task.detached { LogReader.tail(u) }.value
                exists = read != nil
                if (read ?? "") != text { text = read ?? "" }
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
        }
    }

    private func clear() {
        let src = current
        guard model.confirm("Clear “\(src.title)”?", "The log file is emptied. This can't be undone.", button: "Clear") else { return }
        if let h = try? FileHandle(forWritingTo: src.url) {
            try? h.truncate(atOffset: 0)
            try? h.close()
        }
        text = ""
    }
}
