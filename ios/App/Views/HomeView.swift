import SwiftUI

/// The network: every machine, this one first, with whether packets
/// reach it. A bar at the bottom holds the switch that turns gst on and
/// off, and what opens over the machines: the map of the links, adding
/// a machine, and the settings. Touching and holding a machine offers
/// its pings, which open over the machines too. While gst is off, the
/// screen is the way to turn it on.
struct HomeView: View {
    @Environment(AppModel.self) private var model
    @State private var sheet: Sheet?
    @State private var path: [String]
    @State private var failure: String?
    @State private var isSwitching = false
    private let addStep: AddMachineSheet.Step
    private let pings: Pings

    /// The network; `sheet`, `machine`, `addStep`, and `pings` open it on
    /// a sheet, on the page of a machine, on a step of adding a machine,
    /// and with pings that the sheet of a machine's pings starts with,
    /// which a demo does.
    init(sheet: Sheet? = nil, machine: String? = nil, addStep: AddMachineSheet.Step = .form, pings: Pings = Pings()) {
        _sheet = State(initialValue: sheet)
        _path = State(initialValue: machine.map { [$0] } ?? [])
        self.addStep = addStep
        self.pings = pings
    }

    enum Sheet: Hashable, Identifiable {
        case map, add, settings
        /// The pings of the machine with this id.
        case ping(String)

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
        let isWelcome = others.isEmpty && !hasLinks
        // A machine in a network, with gst off: nothing reaches the other
        // machines, so the screen offers the one thing to do. While gst
        // turns on, and while a network that changed takes it down for a
        // moment, the machines stay.
        let state = model.tunnel.state
        let isOff = !isWelcome && state == .off
        let topology = others.isEmpty || isOff ? nil : model.topology
        NavigationStack(path: $path) {
            Group {
                if isOff {
                    Off(isBusy: isBusy, turnOn: toggle)
                } else {
                    ScrollView {
                        VStack(alignment: .leading, spacing: 20) {
                            if !model.problems.isEmpty {
                                ProblemList(problems: model.problems)
                            }
                            if isWelcome {
                                Welcome { sheet = .add }
                            } else {
                                MachineList(machines: machines, me: me, isOn: model.isOn, canPing: model.canPing) {
                                    path.append($0.id)
                                } ping: {
                                    sheet = .ping($0.id)
                                }
                            }
                        }
                        .padding(.horizontal, 20)
                        .padding(.top, 8)
                        .padding(.bottom, 32)
                        .frame(maxWidth: 640)
                        .frame(maxWidth: .infinity)
                    }
                }
            }
            .background(Color(uiColor: .systemGroupedBackground))
            .navigationTitle("Machines")
            // The first screen has nothing at its top: all it offers is
            // in the bar at the bottom, where a thumb reaches.
            .toolbar(.hidden, for: .navigationBar)
            .toolbar {
                ToolbarItem(placement: .bottomBar) {
                    Switch(isOn: state == .on, isBusy: isBusy, toggle: toggle)
                }
                ToolbarSpacer(.flexible, placement: .bottomBar)
                ToolbarItemGroup(placement: .bottomBar) {
                    if topology != nil {
                        Button("Show the Map", systemImage: "point.3.connected.trianglepath.dotted") { sheet = .map }
                    }
                    Button("Add a Machine", systemImage: "plus") { sheet = .add }
                    Button("Settings", systemImage: "gearshape") { sheet = .settings }
                }
            }
            .navigationDestination(for: String.self) { id in
                MachinePage(machineID: id)
            }
        }
        .sheet(item: $sheet) { sheet in
            switch sheet {
            case .map:
                MapSheet(topology: topology, machines: machines, isOn: model.isOn) { machine in
                    self.sheet = nil
                    path.append(machine.id)
                }
            case .add: AddMachineSheet(step: addStep)
            case .settings: SettingsSheet()
            case .ping(let id): PingSheet(machineID: id, pings: pings)
            }
        }
        .alert("gst could not turn on", isPresented: Binding(get: { failure != nil }, set: { if !$0 { failure = nil } })) {
            Button("OK") { failure = nil }
        } message: {
            Text(failure ?? "")
        }
        .sensoryFeedback(.impact, trigger: model.isOn)
    }

    /// Whether gst is turning on or off, or the system has not said
    /// which it is, when the switch waits.
    private var isBusy: Bool {
        isSwitching || ![.on, .off].contains(model.tunnel.state)
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

/// The map of the links, over the machines; tapping a machine in it
/// closes it and opens the machine's page.
private struct MapSheet: View {
    /// The network, while there is one to map.
    let topology: Topology?
    let machines: [Machine]
    let isOn: Bool
    let open: (Machine) -> Void
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ScrollView {
                if let topology {
                    NetworkMap(topology: topology, machines: machines, isOn: isOn, open: open)
                        .padding(.horizontal, 20)
                }
            }
            .navigationTitle("Map")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button("Done", systemImage: "xmark") { dismiss() }
                }
            }
        }
        .presentationDetents([.medium, .large])
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
    let canPing: (Machine) -> Bool
    let open: (Machine) -> Void
    let ping: (Machine) -> Void

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
                    .contextMenu {
                        if canPing(machine) {
                            Button("Ping", systemImage: PingSheet.symbol) { ping(machine) }
                        }
                    }
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

/// The switch that turns gst on and off, green while it is on.
private struct Switch: View {
    let isOn: Bool
    let isBusy: Bool
    let toggle: () -> Void

    var body: some View {
        Button(action: toggle) {
            if isBusy {
                ProgressView()
            } else {
                Label(isOn ? "Turn Off" : "Turn On", systemImage: "power")
                    .fontWeight(.semibold)
            }
        }
        .accessibilityLabel(isOn ? "Turn Off" : "Turn On")
        .tint(isOn ? Color.green : Color.primary)
        .disabled(isBusy)
    }
}

/// What a machine in a network shows while gst is off: that it is, and
/// the button that turns it on.
private struct Off: View {
    let isBusy: Bool
    let turnOn: () -> Void

    var body: some View {
        ContentUnavailableView {
            Label("gst is off", systemImage: "power")
        } description: {
            Text("Turn it on to reach the machines of the network.")
        } actions: {
            Button(action: turnOn) {
                Group {
                    if isBusy {
                        ProgressView()
                    } else {
                        Text("Turn On")
                            .onAccent()
                    }
                }
                .font(.headline)
                .frame(width: 180)
                .padding(.vertical, 6)
            }
            .buttonStyle(.glassProminent)
            .controlSize(.large)
            .disabled(isBusy)
        }
    }
}
