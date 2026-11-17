import SwiftUI

/// The map of the network, as `gst ui` draws it: the machines on a
/// circle, this machine at the top with a ring around it, and the links
/// between them, each pointing at the machine that was linked to. This
/// machine's links take the accent color. Tapping another machine opens
/// it, as tapping it in the list does.
struct NetworkMap: View {
    /// The machines in the order that gst lists them, this machine
    /// first: the order of the circle, in which a machine keeps its
    /// place as it goes offline and comes back.
    private let machines: [Machine]
    private let links: [Topology.Link]
    private let me: String
    /// While gst is off, the links are the ones that the app last knew
    /// of, as the machines are: they show faint, and none as this
    /// machine's, which has none then.
    private let isOn: Bool
    private let open: (Machine) -> Void

    init(topology: Topology, machines: [Machine], isOn: Bool, open: @escaping (Machine) -> Void) {
        let known = Dictionary(machines.map { ($0.id, $0) }, uniquingKeysWith: { first, _ in first })
        self.machines = topology.nodes.compactMap { known[$0.id] }
        self.links = topology.links ?? []
        self.me = topology.me
        self.isOn = isOn
        self.open = open
    }

    var body: some View {
        MapFrame(count: machines.count) {
            GeometryReader { geometry in
                let frame = MapFrame(count: machines.count)
                let radius = frame.radius(width: geometry.size.width)
                let ring = Ring(
                    machines: machines.map(\.id), links: links,
                    center: CGPoint(x: geometry.size.width / 2, y: frame.reach(radius: radius).up),
                    radius: radius
                )
                ZStack {
                    Canvas { context, _ in
                        draw(ring.lines, in: &context)
                    }
                    .opacity(isOn ? 1 : 0.45)
                    Canvas { context, _ in
                        draw(ring.spots, in: &context)
                    }
                    ForEach(Array(zip(ring.spots, machines)).filter { $0.1.id != me }, id: \.0.id) { spot, machine in
                        Button {
                            open(machine)
                        } label: {
                            Color.clear
                                .frame(width: 44, height: 44)
                                .contentShape(.circle)
                        }
                        .buttonStyle(.plain)
                        .position(spot.point)
                    }
                }
            }
        }
        .dynamicTypeSize(...DynamicTypeSize.xLarge)
        .animation(.smooth, value: isOn)
        // The list below says the same to someone who does not see the map.
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text("Map of the links between the machines"))
    }

    private func draw(_ lines: [Ring.Line], in context: inout GraphicsContext) {
        let plain = Color(uiColor: .systemGray3)
        // This machine's links go last, over the others.
        for mine in [false, true] {
            for line in lines where (isOn && (line.from == me || line.to == me)) == mine {
                let color = mine ? Color.accentColor : plain
                var curve = Path()
                curve.move(to: line.start)
                curve.addQuadCurve(to: line.end, control: line.control)
                context.stroke(curve, with: .color(color), style: StrokeStyle(lineWidth: mine ? 1.75 : 1.25, lineCap: .round))
                var tip = Path()
                tip.addLines(line.tip)
                tip.closeSubpath()
                context.fill(tip, with: .color(color))
            }
        }
    }

    private func draw(_ spots: [Ring.Spot], in context: inout GraphicsContext) {
        for (spot, machine) in zip(spots, machines) {
            let isMe = machine.id == me
            // The dot of the list: filled while packets reach the machine.
            if machine.isOnline {
                context.fill(circle(at: spot.point, radius: 4.5), with: .color(machine.way.color))
            } else {
                context.stroke(circle(at: spot.point, radius: 3.75), with: .color(Color(uiColor: .tertiaryLabel)), lineWidth: 1.5)
            }
            if isMe {
                context.stroke(circle(at: spot.point, radius: 8.5), with: .color(.accentColor), lineWidth: 1.5)
            }
            let name = Text(machine.name.count > 12 ? String(machine.name.prefix(11)) + "…" : machine.name)
                .font(isMe ? .caption.weight(.semibold) : .caption)
                .foregroundStyle(isMe ? .primary : .secondary)
            switch spot.side {
            case .right: context.draw(name, at: CGPoint(x: spot.point.x + 13, y: spot.point.y), anchor: .leading)
            case .left: context.draw(name, at: CGPoint(x: spot.point.x - 13, y: spot.point.y), anchor: .trailing)
            case .above: context.draw(name, at: CGPoint(x: spot.point.x, y: spot.point.y - 12), anchor: .bottom)
            case .below: context.draw(name, at: CGPoint(x: spot.point.x, y: spot.point.y + 8), anchor: .top)
            }
        }
    }

    private func circle(at center: CGPoint, radius: CGFloat) -> Path {
        Path(ellipseIn: CGRect(x: center.x - radius, y: center.y - radius, width: 2 * radius, height: 2 * radius))
    }
}

/// Gives the map the height that its circle and the names around it
/// take at the width it gets.
private struct MapFrame: Layout {
    /// How many machines the map shows.
    let count: Int

    /// The circle's radius: larger with more machines, within the width,
    /// which leaves room for a name on either side.
    func radius(width: CGFloat) -> CGFloat {
        max(48, min(max(110, CGFloat(count) * 13), width / 2 - 96))
    }

    /// How far the map reaches above and below the circle's center: to
    /// its highest and lowest machines, with the room that a name above
    /// or below one takes, or beside it.
    func reach(radius: CGFloat) -> (up: CGFloat, down: CGFloat) {
        let spots = Ring(machines: (0..<count).map(String.init), links: [], center: .zero, radius: radius).spots
        let up = spots.map { -$0.point.y + ($0.side == .above ? 40 : 24) }.max() ?? 0
        let down = spots.map { $0.point.y + ($0.side == .below ? 40 : 24) }.max() ?? 0
        return (up, down)
    }

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.replacingUnspecifiedDimensions(by: CGSize(width: 340, height: 0)).width
        let reach = reach(radius: radius(width: width))
        return CGSize(width: width, height: reach.up + reach.down)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        for subview in subviews {
            subview.place(at: bounds.origin, proposal: ProposedViewSize(bounds.size))
        }
    }
}
