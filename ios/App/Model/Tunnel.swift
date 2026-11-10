import Foundation
import NetworkExtension
import Observation

/// gst's VPN, as the system keeps it: the configuration that a person
/// allows once, and the packet tunnel that runs gst's daemon while it is
/// on. Turning it on is `gst up`, and it stays on: the system starts it
/// again after a restart, and whenever it ends.
@MainActor
@Observable
final class Tunnel: GstTransport {
    enum State {
        case off, connecting, on, disconnecting
    }

    private(set) var state: State = .off

    private var manager: NETunnelProviderManager?
    private var observer: NSObjectProtocol?

    /// Finds the configuration, if a person has allowed one, and follows
    /// what the system does with it.
    func load() async {
        let managers = (try? await NETunnelProviderManager.loadAllFromPreferences()) ?? []
        manager = managers.first
        if observer == nil {
            observer = NotificationCenter.default.addObserver(forName: .NEVPNStatusDidChange, object: nil, queue: .main) { [weak self] _ in
                Task { @MainActor in self?.refresh() }
            }
        }
        refresh()
    }

    /// Turns gst on. The first time, the system asks the person whether
    /// gst may add a VPN configuration, and this throws if they decline.
    func turnOn() async throws {
        let manager = self.manager ?? NETunnelProviderManager()
        let configuration = (manager.protocolConfiguration as? NETunnelProviderProtocol) ?? NETunnelProviderProtocol()
        configuration.providerBundleIdentifier = Bundle.main.bundleIdentifier.map { $0 + ".tunnel" }
        // gst has no one server; the system shows this beside the name.
        configuration.serverAddress = "gst"
        manager.protocolConfiguration = configuration
        manager.localizedDescription = "gst"
        manager.isEnabled = true
        manager.onDemandRules = [NEOnDemandRuleConnect()]
        manager.isOnDemandEnabled = true
        try await manager.saveToPreferences()
        // The system starts only a configuration that was read back.
        try await manager.loadFromPreferences()
        self.manager = manager
        try manager.connection.startVPNTunnel()
        refresh()
    }

    /// Turns gst off, and keeps the system from turning it on again.
    func turnOff() async {
        guard let manager else { return }
        manager.isOnDemandEnabled = false
        try? await manager.saveToPreferences()
        manager.connection.stopVPNTunnel()
        refresh()
    }

    /// Sends a call to the daemon in the packet tunnel.
    func send(_ request: Data) async throws -> Data {
        guard state == .on, let session = manager?.connection as? NETunnelProviderSession else {
            throw GstError(code: "failed", message: String(localized: "gst is off."))
        }
        return try await withCheckedThrowingContinuation { continuation in
            do {
                try session.sendProviderMessage(request) { answer in
                    if let answer {
                        continuation.resume(returning: answer)
                    } else {
                        continuation.resume(throwing: GstError(code: "failed", message: String(localized: "gst did not answer.")))
                    }
                }
            } catch {
                continuation.resume(throwing: error)
            }
        }
    }

    private func refresh() {
        switch manager?.connection.status {
        case .connected: state = .on
        case .connecting, .reasserting: state = .connecting
        case .disconnecting: state = .disconnecting
        default: state = .off
        }
    }
}
