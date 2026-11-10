import Foundation

// What gst reports, as its JSON says it: the types of internal/wire and
// internal/mobile that the app reads. Keys are snake_case there and
// camelCase here; the decoder of `Gst` converts them.

/// Who a machine is.
struct NodeInfo: Codable, Hashable {
    var id: String
    var name: String
    var software: String?
}

/// This machine and what its configuration holds ("app.info").
struct AppInfo: Decodable, Equatable {
    struct Link: Decodable, Hashable {
        var destination: String
        var target: String?
    }

    var node: NodeInfo
    /// The line for a server's `authorized_keys` that lets this machine log in.
    var publicKey: String
    var links: [Link]
}

/// How a login to a server went ("app.check").
struct ServerCheck: Decodable, Equatable {
    enum State: String, Decodable {
        case ready
        case unknownHost = "unknown_host"
        case changedHost = "changed_host"
        case unauthorized
        case failed
    }

    struct HostKey: Decodable, Equatable {
        var type: String
        var fingerprint: String
        var key: String
    }

    var destination: String
    var server: String
    var user: String
    var state: State
    var hostKey: HostKey?
    var error: String?
}

/// The end of the daemon's log ("app.log").
struct DaemonLog: Decodable {
    var text: String
}

/// The daemon and what it runs ("daemon.status").
struct DaemonStatus: Decodable, Equatable {
    struct Link: Decodable, Hashable, Identifiable {
        enum State: String, Decodable {
            case connecting, running, down
        }

        var destination: String
        var state: State
        var since: Date?
        var error: String?
        var reconnects: Int
        var peer: NodeInfo?

        var id: String { destination }
    }

    struct Network: Decodable, Equatable {
        struct Peer: Decodable, Hashable {
            var node: String
            var name: String
            var address: String
            var way: String
            var endpoint: String?
            var handshake: Date?
            var received: UInt64
            var sent: UInt64
        }

        var subnet: String?
        var address: String?
        var interface: String?
        var error: String?
        var hint: String?
        var peers: [Peer]
    }

    struct Removed: Decodable, Equatable {
        var by: NodeInfo
        var at: Date
    }

    var software: String
    var node: NodeInfo
    var links: [Link]
    var net: Network?
    var removed: Removed?
}

/// The network as this machine knows it ("mesh.topology").
struct Topology: Codable, Equatable {
    struct Node: Codable, Hashable {
        struct Net: Codable, Hashable {
            var address: String
        }

        var id: String
        var name: String
        var software: String?
        var reachable: Bool
        var route: [String]?
        var direct: Bool?
        var seen: Date?
        var net: Net?
    }

    /// This machine's id.
    var me: String
    var subnet: String
    var nodes: [Node]

    enum CodingKeys: String, CodingKey {
        case me = "self"
        case subnet, nodes
    }
}

/// A packet to a machine and back ("net.ping").
struct PingReply: Decodable, Equatable {
    var node: NodeInfo
    var address: String
    var way: String?
    var endpoint: String?
    var via: [NodeInfo]
    var timeMs: Double
}

/// What removing a machine did ("mesh.remove").
struct Removal: Decodable, Equatable {
    var node: NodeInfo
    /// It was this machine, which left the network.
    var `self`: Bool
    var stranded: [NodeInfo]
}
