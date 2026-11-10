import Foundation

/// An error that gst reports, in its own words.
struct GstError: LocalizedError, Decodable, Equatable {
    var code: String
    var message: String

    var errorDescription: String? { message }
}

/// Where a call goes: gst in this process, or the daemon in the packet
/// tunnel extension.
protocol GstTransport {
    func send(_ request: Data) async throws -> Data
}

/// gst in this process, which answers the "app.…" methods.
struct LocalTransport: GstTransport {
    func send(_ request: Data) async throws -> Data {
        await withCheckedContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                continuation.resume(returning: Core.call(request))
            }
        }
    }
}

/// Calls of gst's methods, typed. The methods and their results are
/// those of the gst command's own conversation with its daemon
/// (docs/design/mesh.md), and the "app.…" methods of internal/mobile.
struct Gst {
    let transport: GstTransport

    struct NoParams: Encodable {}
    struct Nothing: Decodable {}

    private struct Request<Params: Encodable>: Encodable {
        let method: String
        let params: Params
    }

    private struct Response<Result: Decodable>: Decodable {
        let result: Result?
        let error: GstError?
    }

    /// Calls `method` and returns its result.
    func call<Result: Decodable>(_ method: String, _ params: some Encodable = NoParams()) async throws -> Result {
        let request = try JSONEncoder().encode(Request(method: method, params: params))
        let answer = try await transport.send(request)
        let response = try Self.decoder.decode(Response<Result>.self, from: answer)
        if let error = response.error {
            throw error
        }
        if let result = response.result {
            return result
        }
        if let nothing = Nothing() as? Result {
            return nothing
        }
        throw GstError(code: "failed", message: "gst gave no answer")
    }

    /// Calls `method` for what it does.
    func run(_ method: String, _ params: some Encodable = NoParams()) async throws {
        let _: Nothing = try await call(method, params)
    }

    static let decoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        decoder.dateDecodingStrategy = .custom { decoder in
            let text = try decoder.singleValueContainer().decode(String.self)
            guard let date = parseTime(text) else {
                throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "not a time: \(text)"))
            }
            return date
        }
        return decoder
    }()

    /// Reads a time as Go writes it: RFC 3339, with any number of digits
    /// of a second's fraction, which the system's parser does not take.
    static func parseTime(_ text: String) -> Date? {
        var whole = text
        if let dot = whole.firstIndex(of: ".") {
            let end = whole[dot...].firstIndex { "Z+-".contains($0) } ?? whole.endIndex
            whole.removeSubrange(dot..<end)
        }
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        return formatter.date(from: whole)
    }
}

// The params of the methods.

struct NodeParams: Encodable {
    let node: String
}

struct LinkParams: Encodable {
    let destination: String
}

struct NameParams: Encodable {
    let name: String
}

struct TrustParams: Encodable {
    let destination: String
    let key: String
}
