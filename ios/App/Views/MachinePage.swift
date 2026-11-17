import SwiftUI

/// One machine: the pings that the app sends it while its page is
/// open, how to reach it, the ports it exposes, and its removal from the
/// network.
struct MachinePage: View {
    let machineID: String

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var pings: Pings
    @State private var isConfirmingRemoval = false
    @State private var failure: String?
    @State private var exposes: [Expose] = []

    /// The machine's page; `pings` are those that it starts with, which
    /// a demo gives it.
    init(machineID: String, pings: Pings = Pings()) {
        self.machineID = machineID
        _pings = State(initialValue: pings)
    }

    private var machine: Machine? {
        model.machines.first { $0.id == machineID }
    }

    /// The machine while packets may reach it, which is when the app
    /// pings it.
    private var pinged: Machine? {
        guard model.isOn, let machine, !machine.isHere, machine.isOnline else { return nil }
        return machine
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
                if pinged != nil {
                    PingView(pings: pings, way: machine.way)
                } else {
                    LabeledContent("Connection") {
                        HStack(spacing: 8) {
                            StatusDot(way: machine.way)
                            Text(machine.way.text)
                        }
                    }
                    if case .offline(let seen?) = machine.way {
                        LabeledContent("Last seen", value: seen.ago)
                    }
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

            if !exposes.isEmpty {
                Section("Exposed Ports") {
                    ForEach(exposes, id: \.self) { expose in
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
                } footer: {
                    if let failure {
                        Text(failure).foregroundStyle(.red)
                    }
                }
            }
        }
        .task(id: pinged?.id) {
            if let pinged {
                await ping(pinged)
            }
        }
        .task(id: model.isOn) {
            await loadExposes(machine)
        }
        .navigationTitle(machine.name)
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
    }

    /// Asks for the ports that the machine exposes every few seconds, for
    /// as long as the page shows it and gst is on; what cannot be asked
    /// keeps what was there.
    private func loadExposes(_ machine: Machine) async {
        guard model.isOn else {
            exposes = []
            return
        }
        while !Task.isCancelled {
            if let found = try? await model.exposes(of: machine) {
                exposes = found.sorted { ($0.port, $0.proto) < ($1.port, $1.proto) }
            }
            try? await Task.sleep(for: .seconds(5))
        }
    }

    /// Pings the machine once a second, for as long as the page shows it
    /// and packets may reach it.
    private func ping(_ machine: Machine) async {
        while !Task.isCancelled {
            let next = ContinuousClock.now + .seconds(1)
            do {
                let reply = try await model.ping(machine)
                pings.answered(in: reply.timeMs, way: reply.way == "direct" ? .direct(endpoint: reply.endpoint) : .relay(via: reply.via.map(\.name)))
            } catch {
                guard !Task.isCancelled else { return }
                pings.failed(error.localizedDescription)
            }
            try? await Task.sleep(until: next)
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
