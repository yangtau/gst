import Charts
import SwiftUI

/// The pings of a machine, over the screen where someone asked for
/// them: the app pings the machine once a second for as long as the
/// sheet is open, and closing it is what stops them.
struct PingSheet: View {
    /// The symbol of a ping, wherever the app offers one.
    static let symbol = "dot.radiowaves.left.and.right"

    let machineID: String

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var pings: Pings

    /// The pings of a machine; `pings` are those that the sheet starts
    /// with, which a demo gives it.
    init(machineID: String, pings: Pings = Pings()) {
        self.machineID = machineID
        _pings = State(initialValue: pings)
    }

    private var machine: Machine? {
        model.machines.first { $0.id == machineID }
    }

    /// The machine while packets may reach it, which is when the app
    /// pings it.
    private var pinged: Machine? {
        guard let machine, model.canPing(machine) else { return nil }
        return machine
    }

    var body: some View {
        NavigationStack {
            PingView(pings: pings, way: machine?.way ?? .offline(seen: nil))
                .padding(.horizontal, 20)
                .padding(.bottom, 24)
                .navigationTitle(machine?.name ?? "")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .topBarTrailing) {
                        Button("Done", systemImage: "xmark") { dismiss() }
                    }
                }
        }
        .presentationDetents([.medium])
        .task(id: pinged?.id) {
            if let pinged {
                await ping(pinged)
            }
        }
    }

    private func ping(_ machine: Machine) async {
        while !Task.isCancelled {
            let next = ContinuousClock.now + .seconds(1)
            do {
                let reply = try await model.ping(machine)
                pings.answered(in: reply.timeMs, way: reply.way == "direct" ? .direct(endpoint: reply.endpoint) : .relay(via: reply.via.map(\.name), udp: reply.way == "udp-relay"))
            } catch {
                guard !Task.isCancelled else { return }
                pings.failed(error.localizedDescription)
            }
            try? await Task.sleep(until: next)
        }
    }
}

/// The pings of a machine: the way that the latest took and its time,
/// and a chart of the times of them all, which fills its width whether
/// they are few or many, and the height that it is given.
private struct PingView: View {
    let pings: Pings
    /// The way that gst reports to the machine, which the view shows
    /// until a ping comes back.
    let way: Machine.Way

    private var last: Pings.Ping? { pings.latest.last }

    var body: some View {
        let way = last?.way ?? way
        VStack(alignment: .leading, spacing: 16) {
            HStack(alignment: .firstTextBaseline, spacing: 12) {
                VStack(alignment: .leading, spacing: 4) {
                    HStack(spacing: 8) {
                        StatusDot(way: way)
                        Text(way.text)
                            .font(.headline)
                    }
                    if let error = last?.error {
                        Text(error.sentence)
                            .font(.footnote)
                            .foregroundStyle(.red)
                    } else if case .direct(let endpoint?) = way {
                        Text(endpoint)
                            .font(.footnote.monospaced())
                            .foregroundStyle(.secondary)
                            .textSelection(.enabled)
                    }
                }
                Spacer(minLength: 0)
                Text(verbatim: last?.time.map(Pings.text) ?? "–")
                    .font(.system(size: 34, weight: .semibold, design: .rounded))
                    .monospacedDigit()
                    .contentTransition(.numericText())
                    .foregroundStyle(last?.time == nil ? .secondary : .primary)
            }
            .animation(.smooth(duration: 0.35), value: pings) // the time rolls to the next
            // The chart takes each ping at once: animated, its line and the
            // mark of its last point move at different paces, and the mark
            // runs ahead.
            chart
                .frame(minHeight: 148, maxHeight: .infinity)
        }
        .padding(.vertical, 6)
        .accessibilityElement(children: .combine)
    }

    private var chart: some View {
        let scale = pings.scale
        return Chart {
            ForEach(pings.lines) { line in
                // A ping between two without an answer has no line to be on.
                if line.points.count == 1 {
                    PointMark(x: .value(Text(verbatim: "Ping"), line.points[0].id), y: .value(Text(verbatim: "Time"), line.points[0].time))
                        .symbolSize(18)
                        .foregroundStyle(.green)
                }
                ForEach(line.points) { point in
                    LineMark(x: .value(Text(verbatim: "Ping"), point.id), y: .value(Text(verbatim: "Time"), point.time), series: .value(Text(verbatim: "Line"), line.id))
                        .interpolationMethod(.monotone)
                        .lineStyle(StrokeStyle(lineWidth: 2.5, lineCap: .round, lineJoin: .round))
                        .foregroundStyle(.green)
                }
            }
            // A ping without an answer leaves its mark on the ground.
            ForEach(pings.latest.filter { $0.time == nil }) { ping in
                PointMark(x: .value(Text(verbatim: "Ping"), ping.id), y: .value(Text(verbatim: "Time"), 0))
                    .symbol(.cross)
                    .symbolSize(36)
                    .foregroundStyle(.red)
            }
            if let last, let time = last.time {
                PointMark(x: .value(Text(verbatim: "Ping"), last.id), y: .value(Text(verbatim: "Time"), time))
                    .symbolSize(54)
                    .foregroundStyle(.green)
            }
        }
        // The marks at the edges keep their whole shape.
        .chartXScale(domain: pings.span.lowerBound...pings.span.upperBound, range: .plotDimension(padding: 6))
        .chartYScale(domain: 0...scale.top)
        .chartXAxis(.hidden)
        .chartYAxis {
            AxisMarks(position: .trailing, values: Array(stride(from: 0, through: scale.top, by: scale.step))) { value in
                AxisGridLine(stroke: StrokeStyle(lineWidth: 0.5))
                    .foregroundStyle(Color(uiColor: .separator))
                AxisValueLabel {
                    if let time = value.as(Double.self) {
                        Text(verbatim: String(format: "%.0f ms", time))
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                }
            }
        }
        .chartLegend(.hidden)
        .accessibilityHidden(true)
    }
}
