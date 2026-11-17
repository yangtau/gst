#if DEBUG
import Foundation

/// The app on a made-up network, without gst: what Xcode's previews
/// show, and what `-GstDemo <screen>` on the app's command line opens,
/// which is how the tests of the screens look at them (UITests).
enum Demo: String, CaseIterable {
    /// The first screen of a phone in a network of five.
    case home
    /// The same, with what `Problems:` in `gst status` would list.
    case problems
    /// A phone that has joined no network yet.
    case welcome
    /// A phone in a network, with gst off.
    case off
    /// Adding a machine: the server shows a host key for the first time.
    case hostKey = "host-key"
    /// Adding a machine: the server does not accept the phone's key yet.
    case authorize
    /// One machine.
    case machine
    case settings

    /// The screen that the command line asks for.
    static let current = UserDefaults.standard.string(forKey: "GstDemo").flatMap(Demo.init(rawValue:))

    /// Whether the command line asks for the dark appearance, whatever
    /// the system's is: `-GstDemoAppearance dark`.
    static let isDark = UserDefaults.standard.string(forKey: "GstDemoAppearance") == "dark"

    @MainActor
    var model: AppModel {
        switch self {
        case .welcome:
            AppModel(demo: AppInfo(node: Self.phone, publicKey: Self.publicKey, links: []), topology: nil, status: nil)
        case .off:
            AppModel(demo: Self.info, topology: Self.topology(broken: false), status: nil)
        case .problems:
            AppModel(demo: Self.info, topology: Self.topology(broken: true), status: Self.status(broken: true))
        default:
            AppModel(demo: Self.info, topology: Self.topology(broken: false), status: Self.status(broken: false))
        }
    }

    var sheet: HomeView.Sheet? {
        switch self {
        case .hostKey, .authorize: .add
        case .settings: .settings
        default: nil
        }
    }

    /// The machine whose page the network opens on.
    var machine: String? {
        self == .machine ? Self.id("b1") : nil
    }

    /// The ports that the other machines expose, by id.
    static let exposes = [
        id("b1"): [
            Expose(proto: "tcp", port: 5999, target: "127.0.0.1:8080", address: "10.87.61.7", state: "running"),
            Expose(proto: "udp", port: 5353, target: "127.0.0.1:5353", address: "10.87.61.7", state: "running"),
        ],
    ]

    /// The pings that the machine's page starts with: the first ones
    /// along the links, and the rest direct, once the UDP path was up.
    var pings: Pings {
        var pings = Pings()
        for time in [58.2, 61.7, 55.9, 63.4, 24.8, 22.1, 23.6, 21.9, 26.3, 22.7, 21.4, 34.9, 23.2, 22.0, 24.4, 21.7, 22.9, 23.8] {
            pings.answered(in: time, way: pings.latest.count < 4 ? .relay(via: []) : .direct(endpoint: "203.0.113.7:41642"))
        }
        return pings
    }

    /// What a ping of the made-up network comes back with.
    static let ping = PingReply(node: NodeInfo(id: id("b1"), name: "devbox", software: software), address: "10.87.61.7", way: "direct", endpoint: "203.0.113.7:41642", via: [], timeMs: 22.4)

    var addStep: AddMachineSheet.Step {
        switch self {
        case .hostKey:
            .hostKey(ServerCheck(
                destination: Self.destination, server: "ssh.example.com:22", user: "admin", state: .unknownHost,
                hostKey: .init(type: "ssh-ed25519", fingerprint: "SHA256:Hq3uT9cLw0mVxE2yRk5bZpN8sFaJd7oGi1PhWnXtYvA", key: "ssh-ed25519 AAAA")
            ))
        case .authorize:
            .authorize(ServerCheck(destination: Self.destination, server: "ssh.example.com:22", user: "admin", state: .unauthorized))
        default:
            .form
        }
    }

    // The network: a phone that links to devbox and to homelab, which
    // links to devbox too, as laptop does; the phone reaches laptop
    // through devbox. `broken` is the network while the phone's link to
    // homelab is down.

    private static func id(_ prefix: String) -> String {
        prefix + String(repeating: "0", count: 32 - prefix.count)
    }

    private static let software = "gst 0.7.0+a2ffc87"
    private static let destination = "admin@ssh.example.com"
    private static let phone = NodeInfo(id: id("4f"), name: "iphone", software: software)
    private static let publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBFJtRmWpNA4ljYiNsI30z0faYkrWoE85zrKftgc8Ar3 gst@iphone"

    private static let info = AppInfo(
        node: phone,
        publicKey: publicKey,
        links: [.init(destination: destination, target: id("b1")), .init(destination: "ssh://deploy@192.0.2.20:2222", target: id("c2"))]
    )

    private static func topology(broken: Bool) -> Topology {
        func node(_ prefix: String, _ name: String, _ address: String, route: [String]? = nil, direct: Bool = false, seen: TimeInterval? = nil) -> Topology.Node {
            Topology.Node(
                id: id(prefix), name: name, software: software, reachable: route != nil, route: route?.map(id), direct: direct,
                seen: seen.map { Date.now.addingTimeInterval(-$0) }, net: .init(address: address)
            )
        }
        func link(_ from: String, _ to: String) -> Topology.Link {
            Topology.Link(from: id(from), to: id(to))
        }
        // In the daemon's order: this machine, then the others by name.
        return Topology(me: id("4f"), subnet: "10.87.0.0/16", nodes: [
            node("4f", "iphone", "10.87.26.14", route: ["4f"]),
            node("e4", "attic", "10.87.9.44", seen: 3 * 3600),
            node("b1", "devbox", "10.87.61.7", route: ["4f", "b1"], direct: true),
            node("c2", "homelab", "10.87.140.3", route: broken ? ["4f", "b1", "c2"] : ["4f", "c2"], direct: true),
            node("d3", "laptop", "10.87.26.1", route: ["4f", "b1", "d3"]),
        ], links: (broken ? [] : [link("4f", "c2")]) + [link("4f", "b1"), link("c2", "b1"), link("d3", "b1")])
    }

    private static func status(broken: Bool) -> DaemonStatus {
        DaemonStatus(
            software: software,
            node: phone,
            links: [
                .init(destination: destination, state: .running, since: .now.addingTimeInterval(-5400), reconnects: 0, peer: .init(id: id("b1"), name: "devbox")),
                broken
                    ? .init(destination: "ssh://deploy@192.0.2.20:2222", state: .down, since: .now.addingTimeInterval(-90), error: "TCP connection failed", reconnects: 4)
                    : .init(destination: "ssh://deploy@192.0.2.20:2222", state: .running, since: .now.addingTimeInterval(-600), reconnects: 1, peer: .init(id: id("c2"), name: "homelab")),
            ],
            exposes: [Expose(proto: "tcp", port: 8080, target: "127.0.0.1:8080", address: "10.87.26.14", state: "running")],
            net: .init(
                subnet: "10.87.0.0/16", address: "10.87.26.14", interface: "utun4",
                error: broken ? "the subnet 10.87.0.0/16 overlaps 10.87.3.0/24 on this machine" : nil,
                hint: broken ? "choose another subnet with `gst subnet <cidr>`" : nil,
                peers: [
                    .init(node: id("b1"), name: "devbox", address: "10.87.61.7", way: "direct", endpoint: "203.0.113.7:41642", handshake: .now.addingTimeInterval(-12), received: 50_540_000, sent: 3_250_000),
                    .init(node: id("c2"), name: "homelab", address: "10.87.140.3", way: "direct", endpoint: "192.168.1.23:15355", handshake: .now.addingTimeInterval(-31), received: 1_204_000, sent: 380_000),
                    .init(node: id("d3"), name: "laptop", address: "10.87.26.1", way: "relay", handshake: .now.addingTimeInterval(-64), received: 912_000, sent: 77_000),
                ]
            )
        )
    }
}
#endif
