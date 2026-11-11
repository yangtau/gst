import Foundation
import Observation
import UIKit

/// What the app shows and does: this machine, its links, and the network
/// as gst's daemon reports it while gst is on.
///
/// The app is to gst here what the gst command is elsewhere. What it
/// changes (the name, the links) goes into the configuration through gst
/// in this process, and the daemon then reloads it; what it shows comes
/// from the daemon, which runs in the packet tunnel.
@MainActor
@Observable
final class AppModel {
    let tunnel = Tunnel()

    /// This machine and its configuration; nil until gst is open.
    private(set) var info: AppInfo?
    /// What the daemon reports; nil while gst is off.
    private(set) var status: DaemonStatus?
    /// The network as the daemon last reported it, kept while gst is off
    /// and between launches, so that the machines stay on the screen.
    private(set) var topology: Topology?
    /// Why gst could not be opened, which nothing in the app gets past.
    private(set) var failure: String?

    private let app = Gst(transport: LocalTransport())
    private var daemon: Gst { Gst(transport: tunnel) }
    private static let topologyKey = "topology"
    /// The model shows a made-up network, and asks gst nothing; see Demo.
    private var isDemo = false

    var isOn: Bool { tunnel.state == .on }

    /// The machines of the network, this machine first.
    var machines: [Machine] {
        guard let topology else { return [] }
        return Machine.list(topology: topology, status: isOn ? status : nil)
    }

    /// This machine's gst IP, while it has one.
    var address: String? {
        isOn ? status?.net?.address : nil
    }

    /// What keeps this machine from the network, or from part of it, in
    /// gst's words: as `Problems:` in `gst status`.
    var problems: [Problem] {
        guard isOn, let status else { return [] }
        var problems: [Problem] = []
        if let removed = status.removed {
            problems.append(Problem(id: "removed", kind: .removed(by: removed.by.name, at: removed.at)))
        }
        if let error = status.net?.error {
            problems.append(Problem(id: "net", kind: .network(error: error, hint: status.net?.hint)))
        }
        for link in status.links where link.state == .down {
            problems.append(Problem(id: "link " + link.destination, kind: .link(destination: link.destination, error: link.error ?? "")))
        }
        return problems
    }

    struct Problem: Identifiable, Hashable {
        enum Kind: Hashable {
            case removed(by: String, at: Date)
            case network(error: String, hint: String?)
            case link(destination: String, error: String)
        }

        let id: String
        let kind: Kind
    }

    /// Opens gst and finds the VPN. Called once, when the app starts.
    func start() async {
        guard !isDemo else { return }
        if let data = UserDefaults.standard.data(forKey: Self.topologyKey) {
            topology = try? JSONDecoder().decode(Topology.self, from: data)
        }
        do {
            try Core.open(directory: Core.sharedDirectory(), machineName: UIDevice.current.name)
            info = try await app.call("app.info")
        } catch {
            failure = error.localizedDescription
            return
        }
        await tunnel.load()
        await refresh()
    }

    /// Asks the daemon how things are. The app calls it every few
    /// seconds while it is on the screen.
    func refresh() async {
        guard !isDemo else { return }
        guard isOn else {
            status = nil
            return
        }
        // The daemon also writes to the configuration: the machine that
        // a link reached, and the links to machines that were removed.
        if let info: AppInfo = try? await app.call("app.info") {
            self.info = info
        }
        guard let status: DaemonStatus = try? await daemon.call("daemon.status"),
              let topology: Topology = try? await daemon.call("mesh.topology") else {
            return
        }
        self.status = status
        if topology != self.topology {
            self.topology = topology
            UserDefaults.standard.set(try? JSONEncoder().encode(topology), forKey: Self.topologyKey)
        }
    }

    func turnOn() async throws {
        try await tunnel.turnOn()
    }

    func turnOff() async {
        await tunnel.turnOff()
        status = nil
    }

    // What the gst command does elsewhere.

    /// `gst name <name>`.
    func rename(to name: String) async throws {
        info = try await app.call("app.name", NameParams(name: name))
        await reload()
    }

    /// Logs in to the server at `destination` once, and says how that went.
    func check(_ destination: String) async throws -> ServerCheck {
        try await app.call("app.check", LinkParams(destination: destination))
    }

    /// Records the host key that a person checked.
    func trust(_ key: ServerCheck.HostKey, for destination: String) async throws {
        try await app.run("app.trust", TrustParams(destination: destination, key: key.key))
    }

    /// `gst add <destination>`: adds the link, and turns gst on, as the
    /// link joins this machine to a network only while gst runs.
    func add(_ destination: String) async throws {
        info = try await app.call("app.add", LinkParams(destination: destination))
        if tunnel.state == .off {
            try await turnOn()
        } else {
            await reload()
        }
    }

    /// How a link that was just added goes.
    enum LinkOutcome: Equatable {
        /// It runs; the machine at its other end is called this.
        case running(String)
        /// It failed, as gst says; it tries again by itself.
        case failed(String)
        /// It is still connecting.
        case pending
    }

    /// Watches the link to `destination` for a while, until it shows how
    /// it goes.
    func watchLink(to destination: String) async -> LinkOutcome {
        let deadline = Date.now.addingTimeInterval(20)
        var isReloaded = false
        while Date.now < deadline, !Task.isCancelled {
            // A daemon that was starting while the link was added may
            // have read the configuration before it.
            if isOn, !isReloaded {
                try? await daemon.run("daemon.reload")
                isReloaded = true
            }
            await refresh()
            if let link = status?.links.first(where: { $0.destination == destination }) {
                if link.state == .running {
                    return .running(link.peer?.name ?? destination)
                }
                if let error = link.error, !error.isEmpty {
                    return .failed(error)
                }
            }
            try? await Task.sleep(for: .milliseconds(300))
        }
        return .pending
    }

    /// `gst unlink <destination>`.
    func unlink(_ destination: String) async throws {
        info = try await app.call("app.unlink", LinkParams(destination: destination))
        await reload()
    }

    /// `gst ping <machine>`, once.
    func ping(_ machine: Machine) async throws -> PingReply {
        try await daemon.call("net.ping", NodeParams(node: machine.id))
    }

    /// `gst remove <machine>`.
    func remove(_ machine: Machine) async throws -> Removal {
        let removal: Removal = try await daemon.call("mesh.remove", NodeParams(node: machine.id))
        await refresh()
        return removal
    }

    /// Replaces this machine's key with a new one. Servers accept it once
    /// it is in their authorized_keys.
    func newKey() async throws {
        info = try await app.call("app.new_key")
    }

    /// The end of the daemon's log.
    func log() async throws -> String {
        let log: DaemonLog = try await app.call("app.log")
        return log.text
    }

    /// Makes the daemon take up what the app changed.
    private func reload() async {
        if isOn {
            try? await daemon.run("daemon.reload")
        }
        await refresh()
    }
}

#if DEBUG
extension AppModel {
    /// A model that shows what it is given and asks gst nothing, for
    /// Xcode's previews and for screenshots; see Demo.
    convenience init(demo info: AppInfo, topology: Topology?, status: DaemonStatus?) {
        self.init()
        isDemo = true
        self.info = info
        self.topology = topology
        self.status = status
        tunnel.pretend(status == nil ? .off : .on)
    }
}
#endif
