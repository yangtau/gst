import Foundation

/// The pings of one machine, sent a second apart while its sheet is
/// open: what the chart of their times draws.
struct Pings: Equatable {
    /// A packet to the machine and back.
    struct Ping: Equatable, Identifiable {
        /// Its place among the pings sent.
        let id: Int
        /// How long it took there and back, in milliseconds; nil when no
        /// answer came.
        let time: Double?
        /// The way it took, when an answer came.
        let way: Machine.Way?
        /// Why no answer came, in gst's words.
        let error: String?

        var isDirect: Bool {
            if case .direct = way { return true }
            return false
        }
    }

    /// A line of the chart: pings that came back one after another by
    /// the same kind of way, direct or relay.
    struct Line: Equatable, Identifiable {
        /// The first ping that took this way.
        let id: Int
        let isDirect: Bool
        var points: [Point]
    }

    /// An answered ping on a line.
    struct Point: Equatable, Identifiable {
        let id: Int
        let time: Double
    }

    /// How many pings the chart shows: the latest.
    static let shown = 30

    /// The latest pings, oldest first.
    private(set) var latest: [Ping] = []
    private var sent = 0

    /// A ping came back after `time` milliseconds by `way`.
    mutating func answered(in time: Double, way: Machine.Way) {
        add(Ping(id: sent, time: time, way: way, error: nil))
    }

    /// A ping got no answer, for the reason that gst gives.
    mutating func failed(_ error: String) {
        add(Ping(id: sent, time: nil, way: nil, error: error))
    }

    private mutating func add(_ ping: Ping) {
        sent += 1
        latest.append(ping)
        latest.removeFirst(max(0, latest.count - Self.shown))
    }

    /// The pings that the chart has room for, by their ids.
    var span: ClosedRange<Int> {
        let last = max(latest.last?.id ?? 0, Self.shown - 1)
        return (last - Self.shown + 1)...last
    }

    /// The chart's lines. Where the way changes, the next line starts at
    /// the last ping of the one before, so that the two meet; a ping
    /// without an answer leaves a gap.
    var lines: [Line] {
        var lines: [Line] = []
        var before: Ping?
        for ping in latest {
            defer { before = ping }
            guard let time = ping.time else { continue }
            let point = Point(id: ping.id, time: time)
            if let time = before?.time, let previous = before {
                if previous.isDirect == ping.isDirect {
                    lines[lines.count - 1].points.append(point)
                } else {
                    lines.append(Line(id: ping.id, isDirect: ping.isDirect, points: [Point(id: previous.id, time: time), point]))
                }
            } else {
                lines.append(Line(id: ping.id, isDirect: ping.isDirect, points: [point]))
            }
        }
        return lines
    }

    /// The time at the top of the chart, and the step between the times
    /// that it marks below, which is 1, 2, or 5 times a power of ten and
    /// never under a millisecond. The longest ping stays below the top.
    var scale: (step: Double, top: Double) {
        guard let longest = latest.compactMap(\.time).max() else { return (50, 100) }
        let rough = max(longest, 1) / 3
        let power = pow(10, floor(log10(rough)))
        let step = max(1, ([1, 2, 5].first { $0 * power >= rough } ?? 10) * power)
        return (step, step * max(2, (longest * 1.1 / step).rounded(.up)))
    }

    /// A time as the app writes it: "23 ms", and "1.4 ms" under ten.
    static func text(_ time: Double) -> String {
        String(format: time < 9.95 ? "%.1f ms" : "%.0f ms", time)
    }
}
