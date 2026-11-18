import Foundation

/// The pings of one machine, sent a second apart while the sheet that
/// shows them is open: what the chart of their times draws.
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
    }

    /// A line of the chart: pings that came back one after another.
    struct Line: Equatable, Identifiable {
        /// Its first ping.
        let id: Int
        var points: [Point]
    }

    /// An answered ping on a line.
    struct Point: Equatable, Identifiable {
        let id: Int
        let time: Double
    }

    /// How many pings stay, the latest: about as many as the chart's
    /// width tells apart.
    static let most = 300

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
        latest.removeFirst(max(0, latest.count - Self.most))
    }

    /// The pings across the chart, by their ids: the ones that stay, from
    /// edge to edge, so the first few stand far apart and move closer as
    /// more come.
    var span: ClosedRange<Int> {
        let first = latest.first?.id ?? 0
        return first...max(latest.last?.id ?? 0, first + 1)
    }

    /// The chart's lines; a ping without an answer leaves a gap.
    var lines: [Line] {
        var lines: [Line] = []
        var isOpen = false
        for ping in latest {
            guard let time = ping.time else {
                isOpen = false
                continue
            }
            let point = Point(id: ping.id, time: time)
            if isOpen {
                lines[lines.count - 1].points.append(point)
            } else {
                lines.append(Line(id: ping.id, points: [point]))
            }
            isOpen = true
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
