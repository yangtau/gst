import SwiftUI

/// One machine: how packets reach it, where to reach it, the ports it
/// exposes, and its removal from the network. Its pings open over it.
struct MachinePage: View {
    let machineID: String

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var isPinging = false
    @State private var isConfirmingRemoval = false
    @State private var failure: String?
    @State private var exposes: [Expose] = []

    private var machine: Machine? {
        model.machines.first { $0.id == machineID }
    }

    var body: some View {
        if let machine {
            details(machine)
        } else {
            ContentUnavailableView("This machine is no longer in the network", systemImage: "minus.circle")
        }
    }

    private func details(_ machine: Machine) -> some View {
        Form {
            Section {
                LabeledContent("Connection") {
                    HStack(spacing: 8) {
                        StatusDot(way: machine.way)
                        Text(machine.way.text)
                    }
                }
                if case .offline(let seen?) = machine.way {
                    LabeledContent("Last seen", value: seen.ago)
                }
                if model.canPing(machine) {
                    Button("Ping", systemImage: PingSheet.symbol) { isPinging = true }
                }
            }

            Section {
                if let address = machine.address {
                    CopyableValue(title: "gst IP", value: address)
                    CopyableValue(title: "Name", value: machine.hostname)
                }
            } footer: {
                if machine.address == nil && machine.isOnline {
                    Text("This machine has no gst IP yet. Run `gst up` on it.")
                }
            }

            if !shownExposes.isEmpty {
                Section("Exposed Ports") {
                    ForEach(shownExposes, id: \.self) { expose in
                        ExposeRow(hostname: machine.hostname, expose: expose)
                    }
                }
            }

            if !machine.isHere && machine.isOnline {
                Section("Traffic") {
                    if let handshake = machine.handshake {
                        LabeledContent("Last handshake", value: handshake.ago)
                    }
                    LabeledContent("Received", value: machine.received.bytes)
                    LabeledContent("Sent", value: machine.sent.bytes)
                }
            }

            Section {
                if let software = machine.software {
                    LabeledContent("Version", value: software)
                }
                LabeledContent("ID") {
                    Text(machine.shortID).monospaced()
                }
            }

            if model.isOn {
                Section {
                    Button(machine.isHere ? "Leave the Network" : "Remove from the Network", role: .destructive) {
                        isConfirmingRemoval = true
                    }
                    // On the button, which is where the question shows.
                    .confirmationDialog(
                        machine.isHere ? "Leave the network?" : "Remove \(machine.name) from the network?",
                        isPresented: $isConfirmingRemoval,
                        titleVisibility: .visible
                    ) {
                        Button(machine.isHere ? "Leave" : "Remove", role: .destructive) {
                            Task { await remove(machine) }
                        }
                    } message: {
                        if machine.isHere {
                            Text("This machine drops its links and forgets the other machines. It joins a network again when you add a machine.")
                        } else {
                            Text("Every machine drops its links to it and forgets it. It comes back only when someone adds it again.")
                        }
                    }
                } footer: {
                    if let failure {
                        Text(failure).foregroundStyle(.red)
                    }
                }
            }
        }
        .sheet(isPresented: $isPinging) {
            PingSheet(machineID: machineID)
        }
        .task(id: model.isOn) {
            await loadExposes(machine)
        }
        .navigationTitle(machine.name)
        .toolbarTitleDisplayMode(.inline)
    }

    /// The ports that the machine exposes: this machine's from its
    /// status, which the app keeps up to date, another's as its daemon
    /// last said.
    private var shownExposes: [Expose] {
        let all = machine?.isHere == true ? model.status?.exposes ?? [] : exposes
        return all.sorted { ($0.port, $0.proto) < ($1.port, $1.proto) }
    }

    /// Asks another machine for the ports it exposes now and then, for as
    /// long as the page shows it and gst is on; what cannot be asked keeps
    /// what was there.
    private func loadExposes(_ machine: Machine) async {
        guard model.isOn, !machine.isHere else {
            exposes = []
            return
        }
        while !Task.isCancelled {
            if let found = try? await model.exposes(of: machine) {
                exposes = found
            }
            try? await Task.sleep(for: .seconds(30))
        }
    }

    private func remove(_ machine: Machine) async {
        do {
            _ = try await model.remove(machine)
            dismiss()
        } catch {
            failure = error.localizedDescription
        }
    }
}

/// A port that a machine exposes: where other machines reach it, which a
/// button copies, its protocol, and why it does not listen, when it
/// does not.
private struct ExposeRow: View {
    let hostname: String
    let expose: Expose

    private var place: String { "\(hostname):\(expose.port)" }

    var body: some View {
        HStack(spacing: 10) {
            VStack(alignment: .leading, spacing: 3) {
                Text(place)
                    .monospaced()
                    .textSelection(.enabled)
                Text(expose.proto.uppercased())
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                if expose.state != "running" {
                    Text((expose.error ?? expose.state).sentence)
                        .font(.footnote)
                        .foregroundStyle(expose.state == "failed" ? .red : .orange)
                }
            }
            Spacer(minLength: 12)
            CopyButton(text: place)
                .labelStyle(.iconOnly)
                .buttonStyle(.borderless)
        }
    }
}
