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

    @MainActor
    var model: AppModel {
        switch self {
        case .welcome:
            AppModel(demo: AppInfo(node: Self.phone, publicKey: Self.publicKey, links: []), topology: nil, status: nil)
        case .off:
            AppModel(demo: Self.info, topology: Self.topology, status: nil)
        case .problems:
            AppModel(demo: Self.info, topology: Self.topology, status: Self.status(broken: true))
        default:
            AppModel(demo: Self.info, topology: Self.topology, status: Self.status(broken: false))
        }
    }

    var sheet: HomeView.Sheet? {
        switch self {
        case .hostKey, .authorize: .add
        case .machine: .machine(Self.id("b1"))
        case .settings: .settings
        default: nil
        }
    }

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

    // The network: a phone that links to devbox, and through it reaches
    // the rest.

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

    private static let topology: Topology = {
        func node(_ prefix: String, _ name: String, _ address: String, route: [String]? = nil, direct: Bool = false, seen: TimeInterval? = nil) -> Topology.Node {
            Topology.Node(
                id: id(prefix), name: name, software: software, reachable: route != nil, route: route?.map(id), direct: direct,
                seen: seen.map { Date.now.addingTimeInterval(-$0) }, net: .init(address: address)
            )
        }
        return Topology(me: id("4f"), subnet: "10.87.0.0/16", nodes: [
            node("4f", "iphone", "10.87.26.14", route: ["4f"]),
            node("b1", "devbox", "10.87.61.7", route: ["4f", "b1"], direct: true),
            node("c2", "homelab", "10.87.140.3", route: ["4f", "b1", "c2"]),
            node("d3", "laptop", "10.87.26.1", route: ["4f", "b1", "d3"], direct: true),
            node("e4", "attic", "10.87.9.44", seen: 3 * 3600),
        ])
    }()

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
            net: .init(
                subnet: "10.87.0.0/16", address: "10.87.26.14", interface: "utun4",
                error: broken ? "the subnet 10.87.0.0/16 overlaps 10.87.3.0/24 on this machine" : nil,
                hint: broken ? "choose another subnet with `gst subnet <cidr>`" : nil,
                peers: [
                    .init(node: id("b1"), name: "devbox", address: "10.87.61.7", way: "direct", endpoint: "203.0.113.7:41642", handshake: .now.addingTimeInterval(-12), received: 50_540_000, sent: 3_250_000),
                    .init(node: id("c2"), name: "homelab", address: "10.87.140.3", way: "relay", handshake: .now.addingTimeInterval(-64), received: 1_204_000, sent: 380_000),
                    .init(node: id("d3"), name: "laptop", address: "10.87.26.1", way: "direct", endpoint: "192.168.1.23:15355", handshake: .now.addingTimeInterval(-31), received: 912_000, sent: 77_000),
                ]
            )
        )
    }
}
#endif
