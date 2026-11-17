import CoreGraphics
import Foundation

/// Where the map of the network puts the machines and the links between
/// them, as `gst ui` draws them. The machines sit on a circle, the first
/// at the top, so that a line between two of them never passes a third,
/// however the network grows. Lines are links: they bow towards the
/// center, and point at the machine that was linked to.
struct Ring: Equatable {
    /// A machine's place.
    struct Spot: Equatable, Identifiable {
        /// Where its name goes, which is outside the circle.
        enum Side: Equatable {
            case right, left, above, below
        }

        let id: String
        let point: CGPoint
        let side: Side
    }

    /// A link's line, a curve from `start` by `control` to `end`, and
    /// the arrowhead at its end.
    struct Line: Equatable {
        let from: String
        let to: String
        let start: CGPoint
        let control: CGPoint
        let end: CGPoint
        let tip: [CGPoint]
    }

    let spots: [Spot]
    let lines: [Line]

    /// The map of `machines`, by their ids, on a circle of `radius`
    /// around `center`. A link of a machine that is not among them has
    /// no line.
    init(machines: [String], links: [Topology.Link], center: CGPoint, radius: CGFloat) {
        var places: [String: CGPoint] = [:]
        spots = machines.enumerated().map { index, id in
            let angle = -CGFloat.pi / 2 + 2 * .pi * CGFloat(index) / CGFloat(machines.count)
            let point = CGPoint(x: center.x + radius * cos(angle), y: center.y + radius * sin(angle))
            places[id] = point
            let side: Spot.Side = cos(angle) > 0.25 ? .right : cos(angle) < -0.25 ? .left : sin(angle) < 0 ? .above : .below
            return Spot(id: id, point: point, side: side)
        }

        var drawn: [Set<String>: Int] = [:]
        lines = links.compactMap { link in
            guard link.from != link.to, let a = places[link.from], let b = places[link.to] else { return nil }
            // Two machines may each link to the other: the second line
            // bows further, to the other side of the first.
            let again = drawn[[link.from, link.to], default: 0]
            drawn[[link.from, link.to]] = again + 1
            let middle = CGPoint(x: (a.x + b.x) / 2, y: (a.y + b.y) / 2)
            let length = max(hypot(b.x - a.x, b.y - a.y), 1)
            let aside = CGFloat(again) * 16 * (link.from < link.to ? 1 : -1)
            let control = CGPoint(
                x: middle.x + (center.x - middle.x) * 0.3 - (b.y - a.y) / length * aside,
                y: middle.y + (center.y - middle.y) * 0.3 + (b.x - a.x) / length * aside
            )
            // The line stops short of the machines, and further from the
            // one it points at, where the arrowhead goes.
            func short(of point: CGPoint, by distance: CGFloat) -> CGPoint {
                let length = max(hypot(control.x - point.x, control.y - point.y), 1)
                return CGPoint(x: point.x + (control.x - point.x) / length * distance, y: point.y + (control.y - point.y) / length * distance)
            }
            let start = short(of: a, by: 9), end = short(of: b, by: 12)
            let heading = atan2(end.y - control.y, end.x - control.x)
            func ahead(_ distance: CGFloat, turned: CGFloat) -> CGPoint {
                CGPoint(x: end.x + distance * cos(heading + turned), y: end.y + distance * sin(heading + turned))
            }
            return Line(
                from: link.from, to: link.to, start: start, control: control, end: end,
                tip: [ahead(3, turned: 0), ahead(5, turned: 2.5), ahead(5, turned: -2.5)]
            )
        }
    }
}
