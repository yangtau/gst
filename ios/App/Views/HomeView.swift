import SwiftUI

/// The network: this machine, the switch that turns gst on, and every
/// machine with the way that packets take to it.
struct HomeView: View {
    @Environment(AppModel.self) private var model
    @State private var sheet: Sheet?
    @State private var failure: String?
    @State private var isSwitching = false
    private let addStep: AddMachineSheet.Step

    /// The network; `sheet` and `addStep` open it on a sheet, and on a
    /// step of adding a machine, which a demo does.
    init(sheet: Sheet? = nil, addStep: AddMachineSheet.Step = .form) {
        _sheet = State(initialValue: sheet)
        self.addStep = addStep
    }

    enum Sheet: Identifiable {
        case add, settings
        case machine(String)

        var id: String {
            switch self {
            case .add: "add"
            case .settings: "settings"
            case .machine(let id): "machine " + id
            }
        }
    }

    private var others: [Machine] {
        model.machines.filter { !$0.isHere && $0.id != model.info?.node.id }
    }

    private var hasLinks: Bool {
        model.info?.links.isEmpty == false
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 28) {
                    Header(online: others.filter(\.isOnline).count, total: others.count)
                    if !model.problems.isEmpty {
                        ProblemList(problems: model.problems)
                    }
                    if others.isEmpty && !hasLinks {
                        Welcome { sheet = .add }
                    } else {
                        MachineList(machines: others, isOn: model.isOn) { sheet = .machine($0.id) }
                    }
                }
                .padding(.horizontal, 20)
                .padding(.top, 12)
                .padding(.bottom, 32)
                .frame(maxWidth: 640)
                .frame(maxWidth: .infinity)
            }
            .background(Color(uiColor: .systemGroupedBackground))
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button("Settings", systemImage: "slider.horizontal.3") { sheet = .settings }
                }
            }
            .safeAreaInset(edge: .bottom) {
                ControlBar(state: model.tunnel.state, isSwitching: isSwitching, toggle: toggle) { sheet = .add }
            }
        }
        .sheet(item: $sheet) { sheet in
            switch sheet {
            case .add: AddMachineSheet(step: addStep)
            case .settings: SettingsSheet()
            case .machine(let id): MachineSheet(machineID: id)
            }
        }
        .alert("gst could not turn on", isPresented: Binding(get: { failure != nil }, set: { if !$0 { failure = nil } })) {
            Button("OK") { failure = nil }
        } message: {
            Text(failure ?? "")
        }
        .sensoryFeedback(.impact, trigger: model.isOn)
    }

    private func toggle() {
        guard !isSwitching else { return }
        isSwitching = true
        Task {
            defer { isSwitching = false }
            if model.tunnel.state == .off {
                do {
                    try await model.turnOn()
                } catch {
                    failure = error.localizedDescription
                }
            } else {
                await model.turnOff()
            }
        }
    }
}

/// This machine: its name, its gst IP, and how much of the network it reaches.
private struct Header: View {
    @Environment(AppModel.self) private var model
    let online: Int
    let total: Int

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                Circle()
                    .fill(model.isOn ? Color.green : Color.secondary.opacity(0.5))
                    .frame(width: 8, height: 8)
                Text(summary)
                    .font(.subheadline.weight(.medium))
                    .foregroundStyle(.secondary)
                    .contentTransition(.opacity)
            }
            Text(model.info?.node.name ?? "gst")
                .font(.system(size: 44, weight: .bold, design: .rounded))
                .lineLimit(1)
                .minimumScaleFactor(0.5)
            if let address = model.address {
                HStack(spacing: 10) {
                    Text(address)
                        .font(.body.monospaced())
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                    CopyButton(text: address)
                        .labelStyle(.iconOnly)
                        .foregroundStyle(.secondary)
                }
                .transition(.opacity.combined(with: .move(edge: .top)))
            }
        }
        .padding(.top, 8)
        .animation(.smooth, value: model.address)
        .animation(.smooth, value: model.tunnel.state)
    }

    private var summary: String {
        switch model.tunnel.state {
        case .off:
            return String(localized: "Off")
        case .connecting:
            return String(localized: "Turning on…")
        case .disconnecting:
            return String(localized: "Turning off…")
        case .on:
            if total == 0 {
                return String(localized: "On")
            }
            return String(localized: "\(online) of \(total) machines online")
        }
    }
}

/// What is wrong, in gst's words, as `Problems:` in `gst status`.
private struct ProblemList: View {
    let problems: [AppModel.Problem]

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            ForEach(problems) { problem in
                HStack(alignment: .firstTextBaseline, spacing: 12) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .foregroundStyle(.orange)
                    VStack(alignment: .leading, spacing: 4) {
                        Text(title(problem.kind).sentence)
                            .font(.subheadline.weight(.semibold))
                        if let detail = detail(problem.kind) {
                            Text(detail.sentence.withCode)
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(18)
        .background(.orange.opacity(0.12), in: .rect(cornerRadius: 22))
    }

    private func title(_ kind: AppModel.Problem.Kind) -> String {
        switch kind {
        case .removed(let by, let at):
            return String(localized: "\(by) removed this machine from the network \(at.ago).")
        case .network(let error, _):
            return error
        case .link(let destination, _):
            return String(localized: "The link to \(destination) is down.")
        }
    }

    private func detail(_ kind: AppModel.Problem.Kind) -> String? {
        switch kind {
        case .removed:
            return String(localized: "Add a machine to join a network again.")
        case .network(_, let hint):
            return hint
        case .link(_, let error):
            return error.isEmpty ? nil : error
        }
    }
}

/// The other machines of the network.
private struct MachineList: View {
    let machines: [Machine]
    let isOn: Bool
    let open: (Machine) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Machines")
                .font(.footnote.weight(.semibold))
                .foregroundStyle(.secondary)
                .textCase(.uppercase)
                .padding(.horizontal, 6)
            if machines.isEmpty {
                Text("The machines of the network show here once a link connects.")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(18)
                    .background(Color(uiColor: .secondarySystemGroupedBackground), in: .rect(cornerRadius: 22))
            } else {
                VStack(spacing: 0) {
                    ForEach(machines) { machine in
                        Button {
                            open(machine)
                        } label: {
                            MachineRow(machine: machine, isOn: isOn)
                        }
                        .buttonStyle(.plain)
                        if machine.id != machines.last?.id {
                            Divider().padding(.leading, 39)
                        }
                    }
                }
                .background(Color(uiColor: .secondarySystemGroupedBackground), in: .rect(cornerRadius: 22))
            }
        }
    }
}

private struct MachineRow: View {
    let machine: Machine
    /// While gst is off, no way leads anywhere, which says nothing of
    /// the machines: the row then says nothing of the way.
    let isOn: Bool

    var body: some View {
        HStack(spacing: 12) {
            StatusDot(way: machine.way)
            VStack(alignment: .leading, spacing: 3) {
                Text(machine.name)
                    .font(.body.weight(.medium))
                if let address = machine.address {
                    Text(address)
                        .font(.footnote.monospaced())
                        .foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 12)
            if isOn {
                VStack(alignment: .trailing, spacing: 3) {
                    Text(machine.way.text)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.trailing)
                        .lineLimit(2)
                    if case .offline(let seen?) = machine.way {
                        Text(seen.ago)
                            .font(.caption)
                            .foregroundStyle(.tertiary)
                    }
                }
            }
            Image(systemName: "chevron.right")
                .font(.footnote.weight(.semibold))
                .foregroundStyle(.tertiary)
        }
        .padding(.horizontal, 18)
        .padding(.vertical, 14)
        .contentShape(.rect)
        .opacity(machine.isOnline ? 1 : 0.6)
    }
}

/// What a machine that has joined no network shows: what gst is, and
/// the one thing to do.
private struct Welcome: View {
    let add: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 22) {
            VStack(alignment: .leading, spacing: 8) {
                Text("Join your network")
                    .font(.title2.bold())
                Text("gst links your machines into a private network over SSH. Add one machine that runs gst, and this machine reaches them all.")
                    .foregroundStyle(.secondary)
            }
            VStack(alignment: .leading, spacing: 16) {
                step("key.horizontal", "This machine has an SSH key of its own.")
                step("terminal", "Authorize it on a machine where you ran `gst up`.")
                step("point.3.connected.trianglepath.dotted", "Reach every machine by its name, such as devbox.gst.")
            }
            Button(action: add) {
                Label("Add a Machine", systemImage: "plus")
                    .font(.headline)
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 6)
            }
            .buttonStyle(.glassProminent)
            .controlSize(.large)
        }
        .padding(22)
        .background(Color(uiColor: .secondarySystemGroupedBackground), in: .rect(cornerRadius: 28))
    }

    private func step(_ symbol: String, _ text: LocalizedStringKey) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 14) {
            Image(systemName: symbol)
                .font(.body.weight(.medium))
                .foregroundStyle(.tint)
                .frame(width: 26)
            Text(text)
                .font(.subheadline)
        }
    }
}

/// The controls that float over the network: the switch that turns gst
/// on and off, and the button that adds a machine.
private struct ControlBar: View {
    let state: Tunnel.State
    let isSwitching: Bool
    let toggle: () -> Void
    let add: () -> Void

    private var isOn: Bool { state == .on }
    private var isBusy: Bool { isSwitching || state == .connecting || state == .disconnecting }

    var body: some View {
        GlassEffectContainer(spacing: 12) {
            HStack(spacing: 12) {
                Button(action: toggle) {
                    HStack(spacing: 10) {
                        if isBusy {
                            ProgressView()
                        } else {
                            Image(systemName: "power")
                                .font(.body.weight(.bold))
                                .foregroundStyle(isOn ? Color.green : Color.primary)
                        }
                        Text(title)
                            .font(.headline)
                            .contentTransition(.opacity)
                    }
                    .frame(maxWidth: .infinity)
                    .frame(height: 56)
                    .contentShape(.capsule)
                }
                .buttonStyle(.plain)
                .glassEffect(isOn ? .regular.tint(.green.opacity(0.18)).interactive() : .regular.interactive(), in: .capsule)
                .disabled(isBusy)

                Button(action: add) {
                    Image(systemName: "plus")
                        .font(.title3.weight(.semibold))
                        .frame(width: 56, height: 56)
                        .contentShape(.circle)
                }
                .buttonStyle(.plain)
                .glassEffect(.regular.interactive(), in: .circle)
                .accessibilityLabel(Text("Add a Machine"))
            }
        }
        .padding(.horizontal, 20)
        .padding(.bottom, 8)
        .animation(.smooth, value: state)
    }

    private var title: LocalizedStringKey {
        switch state {
        case .off: "Turn On"
        case .connecting: "Turning on…"
        case .on: "On"
        case .disconnecting: "Turning off…"
        }
    }
}
