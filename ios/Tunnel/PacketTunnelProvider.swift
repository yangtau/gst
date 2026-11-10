import NetworkExtension

/// The packet tunnel: the process that the system runs for gst's VPN.
/// gst's daemon runs in it, with the links, WireGuard, and the answers
/// for `.gst`. The system makes the interface; gst says what address,
/// routes, and resolver it gets, and then reads and writes its packets.
final class PacketTunnelProvider: NEPacketTunnelProvider {
    private let lock = NSLock()
    /// Answers the system's start, once: when the interface is up, or
    /// with what kept it down.
    private var started: ((Error?) -> Void)?

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        lock.withLock { started = completionHandler }
        do {
            try Core.open(directory: Core.sharedDirectory(), machineName: "")
            try Core.start { [weak self] settings in
                guard let self else { return "the tunnel is gone" }
                return self.apply(settings)
            }
        } catch {
            finishStart(error)
            return
        }
        // gst sets the interface up within a moment of starting. If it
        // cannot, the system should not show a VPN that connects forever.
        DispatchQueue.global().asyncAfter(deadline: .now() + 30) { [weak self] in
            self?.finishStart(Core.Failure(message: "gst could not set up the network. See its log in the app."))
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        DispatchQueue.global(qos: .userInitiated).async {
            Core.stop()
            completionHandler()
        }
    }

    /// The app's calls of the daemon's methods: a request in, its answer out.
    override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)?) {
        DispatchQueue.global(qos: .userInitiated).async {
            completionHandler?(Core.call(messageData))
        }
    }

    /// Sets the system's interface up as gst says, and waits until the
    /// system has. gst calls it on a thread of its own, at the start and
    /// whenever this machine's gst IP or the network's subnet changes.
    private func apply(_ settings: InterfaceSettings) -> String? {
        // gst has no one server; the system only wants an address here.
        let network = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        let ipv4 = NEIPv4Settings(addresses: [settings.address], subnetMasks: ["255.255.255.255"])
        ipv4.includedRoutes = settings.routes.map {
            NEIPv4Route(destinationAddress: $0.address, subnetMask: $0.mask)
        }
        network.ipv4Settings = ipv4
        // Only the names under .gst go to gst; every other name stays
        // with the resolver that the phone already uses.
        let dns = NEDNSSettings(servers: [settings.dns])
        dns.matchDomains = [settings.domain]
        dns.matchDomainsNoSearch = true
        network.dnsSettings = dns
        network.mtu = NSNumber(value: settings.mtu)

        let done = DispatchSemaphore(value: 0)
        let outcome = Outcome()
        setTunnelNetworkSettings(network) { error in
            outcome.error = error
            done.signal()
        }
        done.wait()
        if let error = outcome.error {
            return error.localizedDescription
        }
        finishStart(nil)
        return nil
    }

    /// What the system answered, for the thread that waits for it.
    private final class Outcome {
        var error: Error?
    }

    private func finishStart(_ error: Error?) {
        let answer = lock.withLock { () -> ((Error?) -> Void)? in
            defer { started = nil }
            return started
        }
        guard let answer else { return }
        if error != nil {
            Core.stop()
        }
        answer(error)
    }
}
