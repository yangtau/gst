import Foundation

/// A machine of the network, as the app shows it: what the topology
/// says of it, and what the virtual network knows of the packets to it.
struct Machine: Identifiable, Hashable {
    /// How packets reach a machine now.
    enum Way: Hashable {
        /// It is this machine.
        case here
        /// Over UDP, to the address given.
        case direct(endpoint: String?)
        /// Along the SSH links, through the machines named.
        case relay(via: [String])
        /// No way leads to it now; it was last known online then.
        case offline(seen: Date?)
    }

    var id: String
    var name: String
    var address: String?
    var software: String?
    var way: Way
    var handshake: Date?
    var received: UInt64 = 0
    var sent: UInt64 = 0

    var isHere: Bool { way == .here }

    var isOnline: Bool {
        if case .offline = way { return false }
        return true
    }

    /// The name that every program on the network reaches it by.
    var hostname: String { name + ".gst" }

    /// The id as gst status shows it.
    var shortID: String { String(id.prefix(8)) }

    /// The machines of `topology`: this machine, then the ones that
    /// packets reach, then the rest, each group by name.
    /// `status` adds what only a running network knows; without it, as
    /// while gst is off, every other machine is offline.
    static func list(topology: Topology, status: DaemonStatus?) -> [Machine] {
        let names = Dictionary(topology.nodes.map { ($0.id, $0.name) }, uniquingKeysWith: { first, _ in first })
        let peers = Dictionary((status?.net?.peers ?? []).map { ($0.node, $0) }, uniquingKeysWith: { first, _ in first })
        let subnet = Subnet(topology.subnet)
        let machines = topology.nodes.map { node -> Machine in
            let peer = peers[node.id]
            // An address of another subnet is one that the machine has
            // yet to leave: nothing reaches it there.
            let address = node.net?.address ?? peer?.address
            var machine = Machine(
                id: node.id,
                name: node.name,
                address: address.flatMap { subnet?.contains($0) == false ? nil : $0 },
                software: node.software,
                way: .offline(seen: node.seen),
                handshake: peer?.handshake,
                received: peer?.received ?? 0,
                sent: peer?.sent ?? 0
            )
            if node.id == topology.me {
                machine.way = status == nil ? .offline(seen: nil) : .here
            } else if status == nil {
                machine.way = .offline(seen: node.seen)
            } else if node.direct == true || peer?.way == "direct" {
                machine.way = .direct(endpoint: peer?.endpoint)
            } else if node.reachable {
                // The route runs from this machine to the machine; what
                // lies between is what the packets pass.
                let between = (node.route ?? []).dropFirst().dropLast()
                machine.way = .relay(via: between.map { names[$0] ?? String($0.prefix(8)) })
            }
            return machine
        }
        return machines.sorted { a, b in
            if (a.id == topology.me) != (b.id == topology.me) { return a.id == topology.me }
            if a.isOnline != b.isOnline { return a.isOnline }
            if a.name != b.name { return a.name < b.name }
            return a.id < b.id
        }
    }
}

/// An IPv4 prefix, to tell which addresses are the network's.
struct Subnet {
    private let base: UInt32
    private let mask: UInt32

    init?(_ text: String) {
        let parts = text.split(separator: "/")
        guard parts.count == 2, let bits = Int(parts[1]), (0...32).contains(bits), let address = Subnet.number(String(parts[0])) else {
            return nil
        }
        mask = bits == 0 ? 0 : ~UInt32(0) << UInt32(32 - bits)
        base = address & mask
    }

    func contains(_ address: String) -> Bool {
        guard let number = Subnet.number(address) else { return false }
        return number & mask == base
    }

    private static func number(_ address: String) -> UInt32? {
        let parts = address.split(separator: ".", omittingEmptySubsequences: false)
        guard parts.count == 4 else { return nil }
        var number: UInt32 = 0
        for part in parts {
            guard let byte = UInt8(part) else { return nil }
            number = number << 8 | UInt32(byte)
        }
        return number
    }
}

/// An SSH destination as a person types it on a phone: who and where,
/// and a port when it is not 22.
enum Destination {
    /// The destination that gst takes for `server` ("user@host") and
    /// `port`, or nil while they do not make one.
    static func make(server: String, port: String) -> String? {
        let server = server.trimmingCharacters(in: .whitespaces)
        let port = port.trimmingCharacters(in: .whitespaces)
        guard let at = server.lastIndex(of: "@"), at != server.startIndex else { return nil }
        let host = server[server.index(after: at)...]
        guard !host.isEmpty, !server.contains(" ") else { return nil }
        if port.isEmpty || port == "22" {
            return server
        }
        guard let number = Int(port), (1...65535).contains(number) else { return nil }
        // A bare IPv6 address needs brackets to keep its colons from the port's.
        let user = server[..<at]
        let bracketed = host.contains(":") && !host.hasPrefix("[") ? "[\(host)]" : String(host)
        return "ssh://\(user)@\(bracketed):\(number)"
    }
}
