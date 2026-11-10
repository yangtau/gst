import SwiftUI

/// One machine: how to reach it, the way that packets take to it, a ping,
/// and its removal from the network.
struct MachineSheet: View {
    let machineID: String

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var pings: [Ping] = []
    @State private var isPinging = false
    @State private var isConfirmingRemoval = false
    @State private var failure: String?

    /// A ping and how it went, in the words of `gst ping`.
    private struct Ping: Identifiable {
        let id = UUID()
        let text: String
        let failed: Bool
    }

    private var machine: Machine? {
        model.machines.first { $0.id == machineID }
    }

    var body: some View {
        NavigationStack {
            Group {
                if let machine {
                    details(machine)
                } else {
                    ContentUnavailableView("This machine is no longer in the network", systemImage: "minus.circle")
                }
            }
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button("Done", systemImage: "xmark") { dismiss() }
                }
            }
        }
        .presentationDetents([.medium, .large])
    }

    private func details(_ machine: Machine) -> some View {
        Form {
            Section {
                if let address = machine.address {
                    CopyableValue(title: "gst IP", value: address)
                    CopyableValue(title: "Name", value: machine.hostname)
                }
                LabeledContent("Connection") {
                    HStack(spacing: 8) {
                        StatusDot(way: machine.way)
                        Text(machine.way.text)
                    }
                }
                if case .direct(let endpoint?) = machine.way {
                    LabeledContent("Address") {
                        Text(endpoint).monospaced()
                    }
                }
                if case .offline(let seen?) = machine.way {
                    LabeledContent("Last seen", value: seen.ago)
                }
            } footer: {
                if machine.address == nil && machine.isOnline {
                    Text("This machine has no gst IP yet. Run `gst up` on it.")
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

                Section {
                    Button {
                        Task { await ping(machine) }
                    } label: {
                        HStack {
                            Label("Ping", systemImage: "dot.radiowaves.left.and.right")
                            Spacer()
                            if isPinging {
                                ProgressView()
                            }
                        }
                    }
                    .disabled(isPinging || !model.isOn)
                    ForEach(pings) { ping in
                        Text(ping.text)
                            .font(.footnote.monospaced())
                            .foregroundStyle(ping.failed ? Color.red : Color.secondary)
                    }
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
        .scrollContentBackground(.hidden)
        .navigationTitle(machine.name)
        .navigationBarTitleDisplayMode(.inline)
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

    /// Three pings, a second apart, as `gst ping` sends them.
    private func ping(_ machine: Machine) async {
        isPinging = true
        pings = []
        defer { isPinging = false }
        for round in 0..<3 {
            if round > 0 {
                try? await Task.sleep(for: .seconds(1))
            }
            do {
                let reply = try await model.ping(machine)
                var way = reply.way == "direct" ? String(localized: "Direct") : String(localized: "Relay")
                if let endpoint = reply.endpoint, !endpoint.isEmpty {
                    way += " " + endpoint
                } else if !reply.via.isEmpty {
                    way = String(localized: "Relay via \(reply.via.map(\.name).joined(separator: ", "))")
                }
                pings.append(Ping(text: way + String(format: ", %.1f ms", reply.timeMs), failed: false))
            } catch {
                pings.append(Ping(text: error.localizedDescription, failed: true))
            }
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
