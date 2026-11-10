import SwiftUI

/// Adds a machine, as `gst add` does, with the steps that ssh takes
/// care of elsewhere: checking the server's host key, and authorizing
/// this machine's key on it.
struct AddMachineSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var server = ""
    @State private var port = ""
    @State private var step: Step = .form
    @FocusState private var isServerFocused: Bool

    private enum Step: Equatable {
        /// Asking where the machine is.
        case form
        /// Logging in to see how it goes.
        case checking
        /// The server showed a host key that is not recorded for it.
        case hostKey(ServerCheck)
        /// The server does not accept this machine's key.
        case authorize(ServerCheck)
        /// The link is added, and gst is connecting it.
        case joining
        /// The link runs; the machine at its other end is called this.
        case joined(String)
        /// The link is added but does not run; gst says why, if it knows.
        case waiting(String?)
        case failed(String)
    }

    private var destination: String? {
        Destination.make(server: server, port: port)
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                content
                    .padding(.horizontal, 24)
                    .padding(.top, 8)
                    .padding(.bottom, 24)
                    .frame(maxWidth: 560)
                    .frame(maxWidth: .infinity)
            }
            .scrollDismissesKeyboard(.interactively)
            .navigationTitle("Add a Machine")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button("Close", systemImage: "xmark") { dismiss() }
                }
            }
        }
        .presentationDetents([.large])
        .interactiveDismissDisabled(step == .checking || step == .joining)
        .animation(.smooth, value: step)
    }

    @ViewBuilder
    private var content: some View {
        switch step {
        case .form:
            form
        case .checking:
            progress("Connecting to the server…")
        case .hostKey(let check):
            hostKey(check)
        case .authorize(let check):
            authorize(check)
        case .joining:
            progress("Joining the network…")
        case .joined(let name):
            outcome(
                symbol: "checkmark.circle.fill", color: .green,
                title: "Joined through \(name)",
                text: "This machine is in the network now. Its machines show on the first screen as they answer."
            )
        case .waiting(let reason):
            waiting(reason)
        case .failed(let message):
            failed(message)
        }
    }

    // The steps.

    private var form: some View {
        VStack(alignment: .leading, spacing: 22) {
            heading(
                "Where is it?",
                "Enter the SSH login of a machine where you ran `gst up`. This machine links to it, and reaches every machine of its network."
            )
            HStack(spacing: 10) {
                TextField("user@host", text: $server)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .keyboardType(.emailAddress)
                    .submitLabel(.continue)
                    .focused($isServerFocused)
                    .onSubmit(check)
                    .padding(16)
                    .background(.fill.tertiary, in: .rect(cornerRadius: 16))
                TextField("22", text: $port)
                    .keyboardType(.numberPad)
                    .multilineTextAlignment(.center)
                    .frame(width: 56)
                    .padding(16)
                    .background(.fill.tertiary, in: .rect(cornerRadius: 16))
                    .accessibilityLabel(Text("Port"))
            }
            .font(.body.monospaced())
            primaryButton("Continue", action: check)
                .disabled(destination == nil)
        }
        .onAppear { isServerFocused = true }
    }

    private func hostKey(_ check: ServerCheck) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            if check.state == .changedHost {
                heading(
                    "The server's key changed",
                    "Another key is recorded for \(check.server). Unless someone replaced the server's key, something else answers in its place."
                )
            } else {
                heading(
                    "Is this the right server?",
                    "This machine has not seen \(check.server) before. Compare the fingerprint with the server's own before you trust it."
                )
            }
            if let key = check.hostKey {
                VStack(alignment: .leading, spacing: 8) {
                    caption("Fingerprint (\(key.type))")
                    CodeBlock(text: key.fingerprint)
                }
                VStack(alignment: .leading, spacing: 8) {
                    caption("To see it on the server, run")
                    let command = "ssh-keygen -lf /etc/ssh/" + hostKeyFile(key.type)
                    CodeBlock(text: command)
                    CopyButton(text: command, title: "Copy Command")
                        .font(.subheadline)
                }
                if check.state == .changedHost {
                    Button("Replace the Recorded Key", role: .destructive) { trust(key) }
                        .buttonStyle(.glass)
                        .controlSize(.large)
                } else {
                    primaryButton("Trust This Server") { trust(key) }
                }
            }
        }
    }

    private func authorize(_ check: ServerCheck) -> some View {
        let key = model.info?.publicKey ?? ""
        let command = "mkdir -p ~/.ssh && echo '\(key)' >> ~/.ssh/authorized_keys"
        return VStack(alignment: .leading, spacing: 22) {
            heading(
                "Authorize this machine",
                "The server does not accept this machine's key yet. Log in to it as \(check.user) from a machine that can, and run this command."
            )
            VStack(alignment: .leading, spacing: 12) {
                CodeBlock(text: command, lineLimit: 4)
                HStack(spacing: 20) {
                    CopyButton(text: command, title: "Copy Command")
                    ShareLink(item: command) {
                        Label("Share", systemImage: "square.and.arrow.up")
                    }
                }
                .font(.subheadline)
            }
            Text("It adds this machine's public key to `~/.ssh/authorized_keys`. The private key never leaves this machine.")
                .font(.footnote)
                .foregroundStyle(.secondary)
            primaryButton("Try Again", action: self.check)
        }
    }

    private func waiting(_ reason: String?) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            outcome(
                symbol: "clock.badge.checkmark", color: .orange,
                title: "Added, and still connecting",
                text: "gst keeps trying in the background, and the machines show on the first screen once the link connects."
            )
            if let reason {
                CodeBlock(text: reason)
                if reason.contains("gst is not up") || reason.contains("did not answer") || reason.contains("the remote gst failed") {
                    Text("Run `gst up` on that machine, with the same version of gst as here. The link then connects by itself.")
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }

    private func failed(_ message: String) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            Text("That did not work")
                .font(.title.bold())
                .padding(.top, 12)
            CodeBlock(text: message)
            primaryButton("Try Again", action: check)
            Button("Change the Login") { step = .form }
                .frame(maxWidth: .infinity)
        }
    }

    // What the steps are made of.

    private func heading(_ title: LocalizedStringKey, _ text: LocalizedStringKey) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(title)
                .font(.title.bold())
            Text(text)
                .foregroundStyle(.secondary)
        }
        .padding(.top, 12)
    }

    private func caption(_ text: LocalizedStringKey) -> some View {
        Text(text)
            .font(.footnote.weight(.medium))
            .foregroundStyle(.secondary)
    }

    private func primaryButton(_ title: LocalizedStringKey, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Text(title)
                .font(.headline)
                .frame(maxWidth: .infinity)
                .padding(.vertical, 6)
        }
        .buttonStyle(.glassProminent)
        .controlSize(.large)
    }

    private func progress(_ text: LocalizedStringKey) -> some View {
        VStack(spacing: 18) {
            ProgressView()
                .controlSize(.large)
            Text(text)
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity)
        .padding(.top, 96)
    }

    private func outcome(symbol: String, color: Color, title: LocalizedStringKey, text: LocalizedStringKey) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            Image(systemName: symbol)
                .font(.system(size: 56))
                .foregroundStyle(color)
                .symbolEffect(.bounce, value: step)
                .padding(.top, 28)
            VStack(alignment: .leading, spacing: 10) {
                Text(title)
                    .font(.title.bold())
                Text(text)
                    .foregroundStyle(.secondary)
            }
            primaryButton("Done") { dismiss() }
        }
        .sensoryFeedback(.success, trigger: step)
    }

    /// The file of a server's host key of a type, as OpenSSH names them.
    private func hostKeyFile(_ type: String) -> String {
        switch type {
        case "ssh-ed25519": "ssh_host_ed25519_key.pub"
        case "ssh-rsa": "ssh_host_rsa_key.pub"
        default: "ssh_host_ecdsa_key.pub"
        }
    }

    // What the steps do.

    private func check() {
        guard let destination else { return }
        isServerFocused = false
        step = .checking
        Task {
            do {
                let check = try await model.check(destination)
                switch check.state {
                case .ready:
                    await join(destination)
                case .unknownHost, .changedHost:
                    step = .hostKey(check)
                case .unauthorized:
                    step = .authorize(check)
                case .failed:
                    step = .failed(check.error ?? "")
                }
            } catch {
                step = .failed(error.localizedDescription)
            }
        }
    }

    private func trust(_ key: ServerCheck.HostKey) {
        guard let destination else { return }
        Task {
            do {
                try await model.trust(key, for: destination)
                check()
            } catch {
                step = .failed(error.localizedDescription)
            }
        }
    }

    private func join(_ destination: String) async {
        step = .joining
        do {
            try await model.add(destination)
        } catch {
            step = .failed(error.localizedDescription)
            return
        }
        switch await model.watchLink(to: destination) {
        case .running(let name):
            step = .joined(name)
        case .failed(let reason):
            step = .waiting(reason)
        case .pending:
            step = .waiting(nil)
        }
    }
}
