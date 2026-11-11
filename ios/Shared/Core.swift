import Foundation
import GstKit

/// gst itself, in this process: the Go code behind `gstkit.h`. The app
/// and the packet tunnel extension each open it on the directory that
/// they share. The app calls its "app.…" methods; the extension runs the
/// daemon in it, and passes the app's calls of the daemon's methods on.
enum Core {
    /// What gst said went wrong.
    struct Failure: LocalizedError {
        let message: String
        var errorDescription: String? { message }
    }

    /// Opens gst with its files in `directory`. The first time, the
    /// machine gets an identity called after `machineName`, and a key.
    static func open(directory: URL, machineName: String) throws {
        if let failed = gst_open(directory.path, machineName) {
            throw take(failed)
        }
    }

    /// Calls a method: `request` is the JSON `{"method", "params"}`, and
    /// the result the JSON `{"result"}` or `{"error"}`. It blocks until
    /// gst answers, which a call that asks another machine does after
    /// seconds.
    static func call(_ request: Data) -> Data {
        guard let answer = gst_call(String(decoding: request, as: UTF8.self)) else {
            return Data()
        }
        defer { gst_free(answer) }
        return Data(bytes: answer, count: strlen(answer))
    }

    /// Runs the daemon in this process until `stop`. gst calls `apply`,
    /// on a thread of its own, whenever the system's interface should
    /// change; it returns once the system is set up, with nil, or with
    /// what failed.
    static func start(apply: @escaping (InterfaceSettings) -> String?) throws {
        let handler = ApplyHandler(apply)
        applying = handler
        // iOS keeps the interface, so the descriptor stays at -1, and
        // it sends gst only the names under .gst, so nothing is forwarded.
        let failed = gst_start({ context, settings, _ in
            guard let context, let settings else { return nil }
            let handler = Unmanaged<ApplyHandler>.fromOpaque(context).takeUnretainedValue()
            let data = Data(bytes: settings, count: strlen(settings))
            guard let decoded = try? JSONDecoder().decode(InterfaceSettings.self, from: data) else {
                return strdup("the settings are unreadable")
            }
            if let failure = handler.apply(decoded) {
                return strdup(failure)
            }
            return nil
        }, nil, Unmanaged.passUnretained(handler).toOpaque())
        if let failed {
            throw take(failed)
        }
    }

    /// Stops the daemon, and returns once it has.
    static func stop() {
        gst_stop()
    }

    /// The handler that gst calls, kept alive while the daemon may.
    private static var applying: ApplyHandler?

    private final class ApplyHandler {
        let apply: (InterfaceSettings) -> String?
        init(_ apply: @escaping (InterfaceSettings) -> String?) { self.apply = apply }
    }

    private static func take(_ text: UnsafeMutablePointer<CChar>) -> Failure {
        defer { gst_free(text) }
        return Failure(message: String(cString: text))
    }
}

/// What the system's interface should be, as gst says it.
struct InterfaceSettings: Decodable, Equatable {
    struct Route: Decodable, Equatable {
        let address: String
        let mask: String
    }

    /// This machine's gst IP.
    let address: String
    /// What goes to the interface: the subnet of the network, and `dns`.
    let routes: [Route]
    /// The address that answers for the names under `domain`.
    let dns: String
    let domain: String
    let mtu: Int
}
