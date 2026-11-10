import SwiftUI

/// This machine: its name, the key that servers authorize, its links, and
/// what helps when something does not work.
struct SettingsSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var name = ""
    @State private var nameFailure: String?
    @State private var failure: String?

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    LabeledContent("Name") {
                        TextField("Name", text: $name)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .multilineTextAlignment(.trailing)
                            .submitLabel(.done)
                            .onSubmit(rename)
                    }
                    if let id = model.info?.node.id {
                        LabeledContent("ID") {
                            Text(id.prefix(8)).monospaced()
                        }
                    }
                    if let subnet = model.topology?.subnet, !subnet.isEmpty {
                        LabeledContent("Subnet") {
                            Text(subnet).monospaced()
                        }
                    }
                } header: {
                    Text("This Machine")
                } footer: {
                    if let nameFailure {
                        Text(nameFailure).foregroundStyle(.red)
                    } else {
                        Text("Other machines reach this machine as \(model.info?.node.name ?? "").gst.")
                    }
                }

                if let key = model.info?.publicKey {
                    Section {
                        Text(key)
                            .font(.footnote.monospaced())
                            .lineLimit(3)
                            .truncationMode(.middle)
                            .textSelection(.enabled)
                        CopyButton(text: key, title: "Copy Key")
                        ShareLink(item: key) {
                            Label("Share", systemImage: "square.and.arrow.up")
                        }
                    } header: {
                        Text("SSH Key")
                    } footer: {
                        Text("A machine lets this machine link to it once this line is in its `~/.ssh/authorized_keys`. The private key never leaves this machine.")
                    }
                }

                Section {
                    ForEach(model.info?.links ?? [], id: \.destination) { link in
                        LinkRow(destination: link.destination, state: model.status?.links.first { $0.destination == link.destination })
                            .swipeActions {
                                Button("Unlink", role: .destructive) { unlink(link.destination) }
                            }
                    }
                } header: {
                    Text("Links")
                } footer: {
                    if let failure {
                        Text(failure).foregroundStyle(.red)
                    } else if model.info?.links.isEmpty == false {
                        Text("Swipe a link to remove it. Its machine stays in the network.")
                    } else {
                        Text("This machine has no links yet. Add a machine on the first screen.")
                    }
                }

                Section {
                    NavigationLink("Log") {
                        LogView()
                    }
                    if let software = model.info?.node.software {
                        LabeledContent("Version", value: software)
                    }
                }
            }
            .navigationTitle("Settings")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button("Done", systemImage: "xmark") { dismiss() }
                }
            }
        }
        .onAppear { name = model.info?.node.name ?? "" }
    }

    /// `gst name`, with its rule for names.
    private func rename() {
        let wanted = name.trimmingCharacters(in: .whitespaces).lowercased()
        guard wanted != model.info?.node.name else {
            name = wanted
            nameFailure = nil
            return
        }
        guard wanted.range(of: "^[a-z0-9][a-z0-9-]{0,31}$", options: .regularExpression) != nil else {
            nameFailure = String(localized: "Use 1 to 32 lowercase letters, digits, or hyphens, starting with a letter or digit.")
            return
        }
        Task {
            do {
                try await model.rename(to: wanted)
                name = wanted
                nameFailure = nil
            } catch {
                nameFailure = error.localizedDescription
            }
        }
    }

    private func unlink(_ destination: String) {
        Task {
            do {
                try await model.unlink(destination)
                failure = nil
            } catch {
                failure = error.localizedDescription
            }
        }
    }
}

/// A link, named by its destination, and how it goes.
private struct LinkRow: View {
    let destination: String
    let state: DaemonStatus.Link?

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(destination)
                .font(.body.monospaced())
                .lineLimit(1)
                .truncationMode(.middle)
            HStack(spacing: 6) {
                Circle()
                    .fill(color)
                    .frame(width: 7, height: 7)
                Text(summary)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
            if let state, state.state == .down, let error = state.error, !error.isEmpty {
                Text(error.sentence.withCode)
                    .font(.footnote)
                    .foregroundStyle(.red)
            }
        }
        .padding(.vertical, 2)
    }

    private var color: Color {
        switch state?.state {
        case .running: .green
        case .connecting: .orange
        case .down: .red
        case nil: .secondary
        }
    }

    private var summary: String {
        switch state?.state {
        case .running:
            if let name = state?.peer?.name {
                return String(localized: "Running, to \(name)")
            }
            return String(localized: "Running")
        case .connecting:
            return String(localized: "Connecting…")
        case .down:
            return String(localized: "Down, trying again")
        case nil:
            return String(localized: "Off")
        }
    }
}

/// The end of gst's log: what the links did, without the names of
/// servers, which gst keeps out of it.
private struct LogView: View {
    @Environment(AppModel.self) private var model
    @State private var text = ""

    var body: some View {
        ScrollView {
            Text(text.isEmpty ? String(localized: "The log is empty.") : text)
                .font(.caption.monospaced())
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding()
        }
        .defaultScrollAnchor(.bottom)
        .navigationTitle("Log")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                ShareLink(item: text)
                    .disabled(text.isEmpty)
            }
        }
        .task {
            while !Task.isCancelled {
                if let log = try? await model.log() {
                    text = log
                }
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }
}
