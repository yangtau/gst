import SwiftUI

/// The network: every machine, this one first, with whether packets
/// reach it; above them, for someone who asks for it, the map of the
/// links; and floating over them the button that turns gst on.
struct HomeView: View {
    @Environment(AppModel.self) private var model
    @State private var sheet: Sheet?
    @State private var path: [String]
    @State private var failure: String?
    @State private var isSwitching = false
    @AppStorage("map") private var showsMap = false
    private let addStep: AddMachineSheet.Step
    private let pings: Pings

    /// The network; `sheet`, `machine`, `addStep`, and `pings` open it on
    /// a sheet, on the page of a machine, on a step of adding a machine,
    /// and with pings that a machine's page starts with, which a demo
    /// does.
    init(sheet: Sheet? = nil, machine: String? = nil, addStep: AddMachineSheet.Step = .form, pings: Pings = Pings()) {
        _sheet = State(initialValue: sheet)
        _path = State(initialValue: machine.map { [$0] } ?? [])
        self.addStep = addStep
        self.pings = pings
    }

    enum Sheet: Identifiable {
        case add, settings

        var id: Self { self }
    }

    private var hasLinks: Bool {
        model.info?.links.isEmpty == false
    }

    var body: some View {
        let machines = model.machines
        // This machine, by its id: while gst is off, nothing says it is here.
        let me = model.info?.node.id
        let others = machines.filter { $0.id != me }
        // The map, when the network has one to show: a network of one has
        // nothing to map.
        let topology = others.isEmpty ? nil : model.topology
        NavigationStack(path: $path) {
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    if !model.problems.isEmpty {
                        ProblemList(problems: model.problems)
                    }
                    if others.isEmpty && !hasLinks {
                        Welcome { sheet = .add }
                    } else {
                        if showsMap, let topology {
                            NetworkMap(topology: topology, machines: machines, isOn: model.isOn) { path.append($0.id) }
                                .background(Color(uiColor: .secondarySystemGroupedBackground), in: .rect(cornerRadius: 22))
                                .transition(.opacity.combined(with: .scale(scale: 0.96, anchor: .top)))
                        }
                        MachineList(machines: machines, me: me, isOn: model.isOn) {
                            path.append($0.id)
                        }
                    }
                }
                .padding(.horizontal, 20)
                .padding(.top, 8)
                .padding(.bottom, 32)
                .frame(maxWidth: 640)
                .frame(maxWidth: .infinity)
            }
            .background(Color(uiColor: .systemGroupedBackground))
            .navigationTitle("Machines")
            .toolbar {
                if topology != nil {
                    ToolbarItem(placement: .topBarTrailing) {
                        Button(showsMap ? "Hide the Map" : "Show the Map", systemImage: "point.3.connected.trianglepath.dotted") {
                            withAnimation(.smooth) { showsMap.toggle() }
                        }
                        .symbolVariant(showsMap ? .fill : .none)
                        .foregroundStyle(showsMap ? Color.accentColor : Color.primary)
                        .accessibilityAddTraits(showsMap ? .isSelected : [])
                    }
                }
                ToolbarSpacer(.fixed, placement: .topBarTrailing)
                ToolbarItemGroup(placement: .topBarTrailing) {
                    Button("Add a Machine", systemImage: "plus") { sheet = .add }
                    Button("Settings", systemImage: "gearshape") { sheet = .settings }
                }
            }
            .navigationDestination(for: String.self) { id in
                MachinePage(machineID: id, pings: id == path.first ? pings : Pings())
            }
            .safeAreaInset(edge: .bottom) {
                ControlBar(state: model.tunnel.state, isSwitching: isSwitching, toggle: toggle)
            }
        }
        .sheet(item: $sheet) { sheet in
            switch sheet {
            case .add: AddMachineSheet(step: addStep)
            case .settings: SettingsSheet()
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

/// The machines of the network, this one first; while the others have
/// not shown up yet, what will bring them.
private struct MachineList: View {
    let machines: [Machine]
    /// This machine's id.
    let me: String?
    let isOn: Bool
    let open: (Machine) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            VStack(spacing: 0) {
                ForEach(machines) { machine in
                    Button {
                        open(machine)
                    } label: {
                        MachineRow(machine: machine, isMe: machine.id == me, isOn: isOn)
                    }
                    .buttonStyle(.plain)
                    if machine.id != machines.last?.id {
                        Divider().padding(.leading, 39)
                    }
                }
            }
            .background(Color(uiColor: .secondarySystemGroupedBackground), in: .rect(cornerRadius: 22))
            if machines.allSatisfy({ $0.id == me }) {
                Text("The machines of the network show here once a link connects.")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 18)
            }
        }
    }
}

private struct MachineRow: View {
    let machine: Machine
    let isMe: Bool
    /// While gst is off, nothing reaches the machines, which says nothing
    /// of them: the row then says nothing of when they were seen.
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
            if isMe {
                Text("This machine")
                    .font(.caption.weight(.medium))
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 3)
                    .background(.fill.tertiary, in: .capsule)
            }
            // The dot says whether packets reach the machine; the way they
            // take is in its page.
            if isOn, case .offline(let seen?) = machine.way {
                Text(seen.ago)
                    .font(.caption)
                    .foregroundStyle(.tertiary)
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
                    .onAccent()
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

/// The button that floats over the network and turns gst on and off,
/// green while it is on.
private struct ControlBar: View {
    let state: Tunnel.State
    let isSwitching: Bool
    let toggle: () -> Void

    private var isOn: Bool { state == .on }
    private var isBusy: Bool { isSwitching || state == .connecting || state == .disconnecting }

    var body: some View {
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
