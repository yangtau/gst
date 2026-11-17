import Charts
import SwiftUI

/// The pings of a machine while its sheet is open: the way that the
/// latest took and its time, and a chart of the times of the latest
/// ones, each line in the color of its way.
struct PingView: View {
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
            chart
                .frame(height: 148)
        }
        .padding(.vertical, 6)
        .animation(.smooth(duration: 0.35), value: pings)
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
                        .foregroundStyle(color(isDirect: line.isDirect))
                }
                ForEach(line.points) { point in
                    LineMark(x: .value(Text(verbatim: "Ping"), point.id), y: .value(Text(verbatim: "Time"), point.time), series: .value(Text(verbatim: "Line"), line.id))
                        .interpolationMethod(.monotone)
                        .lineStyle(StrokeStyle(lineWidth: 2.5, lineCap: .round, lineJoin: .round))
                        .foregroundStyle(color(isDirect: line.isDirect))
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
                    .foregroundStyle(color(isDirect: last.isDirect))
            }
        }
        .chartXScale(domain: pings.span.lowerBound...pings.span.upperBound)
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

    private func color(isDirect: Bool) -> Color {
        (isDirect ? Machine.Way.direct(endpoint: nil) : .relay(via: [])).color
    }
}
