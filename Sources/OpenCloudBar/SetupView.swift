import AppKit
import SwiftUI

/// State of the set-up window: the address field, progress and error text, and the sign-in task.
@MainActor
final class SetupState: ObservableObject {
    @Published var address = ""
    @Published var busy = false
    @Published var error: String?
    private var task: Task<Void, Never>?
    var onDone: ((String) -> Void)?

    func signIn() {
        guard !busy else { return }
        busy = true; error = nil
        let addr = address
        task = Task { [weak self] in
            do {
                let user = try await Auth.signIn(serverURL: addr)
                self?.busy = false
                self?.onDone?(user)
            } catch is CancellationError {
                self?.busy = false
            } catch {
                self?.busy = false
                self?.error = error.localizedDescription
            }
        }
    }

    func cancel() { task?.cancel(); task = nil; busy = false }
}

struct SetupView: View {
    @ObservedObject var state: SetupState
    var close: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Set Up OpenCloud Bar").font(.headline)
            Text("Enter your OpenCloud server address. Your browser opens on the OpenCloud sign-in page; there is nothing else to configure.")
                .fixedSize(horizontal: false, vertical: true)
            TextField("https://cloud.example.com", text: $state.address)
                .accessibilityLabel("Server address")
                .textFieldStyle(.roundedBorder)
                .disabled(state.busy)
                .onSubmit { state.signIn() }
            if state.busy {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small)
                    Text("Waiting for you to sign in in your browser…")
                }
            }
            if let e = state.error {
                Text(e).foregroundStyle(.red).fixedSize(horizontal: false, vertical: true)
            }
            HStack {
                Spacer()
                Button("Cancel") { state.cancel(); close() }.keyboardShortcut(.cancelAction).accessibilityLabel("Cancel")
                Button("Sign In with Browser") { state.signIn() }.accessibilityLabel("Sign In with Browser")
                    .keyboardShortcut(.defaultAction)
                    .disabled(state.busy || state.address.trimmingCharacters(in: .whitespaces).isEmpty)
            }
        }
        .padding(20)
        .frame(width: 420)
    }
}

/// A small window for the first-run sign-in. Closes itself on success.
@MainActor
final class SetupWindowController: NSObject, NSWindowDelegate {
    private let state = SetupState()
    private var window: NSWindow?

    init(model: AppModel) {
        super.init()
        state.address = model.config.serverURL ?? ""
        state.onDone = { [weak self, weak model] user in
            self?.closeWindow()
            model?.finishSetup(username: user)
        }
    }

    func show() {
        if window == nil {
            let w = NSWindow(contentViewController: NSHostingController(rootView: SetupView(state: state) { [weak self] in self?.closeWindow() }))
            w.title = "Set Up OpenCloud Bar"
            w.styleMask = [.titled, .closable]
            w.isReleasedWhenClosed = false
            w.delegate = self
            w.center()
            window = w
        }
        NSApp.activate(ignoringOtherApps: true)
        window?.makeKeyAndOrderFront(nil)
    }

    private func closeWindow() { window?.close(); window = nil }

    func windowWillClose(_ notification: Notification) { state.cancel() }
}
