import XCTest

/// What the app does without the system: gst's own answers through
/// `Core`, read as the app's types, and what the app makes of them. The
/// VPN itself only runs on a device.
final class GstTests: XCTestCase {
    private let gst = Gst(transport: LocalTransport())

    override func setUpWithError() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("gst-" + UUID().uuidString)
        try Core.open(directory: directory, machineName: "Test Phone")
    }

    func testTheAppsMethods() async throws {
        let info: AppInfo = try await gst.call("app.info")
        XCTAssertEqual(info.node.name, "test-phone")
        XCTAssertTrue(info.publicKey.hasPrefix("ssh-ed25519 "))
        XCTAssertTrue(info.publicKey.hasSuffix(" gst@test-phone"))
        XCTAssertEqual(info.links, [])

        do {
            let _: ServerCheck = try await gst.call("app.check", LinkParams(destination: "nobody"))
            XCTFail("an invalid destination was checked")
        } catch let error as GstError {
            XCTAssertEqual(error.code, "invalid_params")
        }
        // Nothing listens there.
        let check: ServerCheck = try await gst.call("app.check", LinkParams(destination: "ssh://tester@127.0.0.1:1"))
        XCTAssertEqual(check.state, .failed)
        XCTAssertEqual(check.server, "127.0.0.1:1")
        XCTAssertEqual(check.user, "tester")

        let renamed: AppInfo = try await gst.call("app.name", NameParams(name: "phone"))
        XCTAssertEqual(renamed.node.name, "phone")
        XCTAssertEqual(renamed.node.id, info.node.id)
    }

    /// The daemon starts, tells the system how to set the interface up,
    /// and answers the methods that the app shows the network with. No
    /// interface exists here, which it reports.
    func testTheDaemon() async throws {
        do {
            let _: DaemonStatus = try await gst.call("daemon.status")
            XCTFail("the daemon answered before it started")
        } catch let error as GstError {
            XCTAssertTrue(error.message.contains("not running"), error.message)
        }

        let asked = expectation(description: "gst says how to set the interface up")
        asked.assertForOverFulfill = false
        let applied = Applied()
        try Core.start { settings in
            applied.set(settings)
            asked.fulfill()
            return "this test has no interface"
        }
        defer { Core.stop(why: "the test is done") }
        await fulfillment(of: [asked], timeout: 20)
        let settings = try XCTUnwrap(applied.get())
        XCTAssertEqual(settings.dns, "198.18.0.53")
        XCTAssertEqual(settings.domain, "gst")
        XCTAssertEqual(settings.mtu, 1168)
        XCTAssertEqual(settings.routes.count, 2)
        XCTAssertEqual(settings.routes.first?.mask, "255.255.0.0")
        XCTAssertEqual(settings.routes.last, .init(address: "198.18.0.53", mask: "255.255.255.255"))

        let status: DaemonStatus = try await gst.call("daemon.status")
        XCTAssertEqual(status.node.name, "test-phone")
        XCTAssertEqual(status.links, [])
        let topology: Topology = try await gst.call("mesh.topology")
        XCTAssertEqual(topology.me, status.node.id)
        XCTAssertEqual(topology.nodes.count, 1)
        let machines = Machine.list(topology: topology, status: status)
        XCTAssertEqual(machines.map(\.hostname), ["test-phone.gst"])
        XCTAssertTrue(machines[0].isHere)
        XCTAssertFalse(Machine.list(topology: topology, status: nil)[0].isOnline)
        // The app keeps the topology for when gst is off.
        XCTAssertEqual(try JSONDecoder().decode(Topology.self, from: JSONEncoder().encode(topology)), topology)

        do {
            let _: PingReply = try await gst.call("net.ping", NodeParams(node: "nobody"))
            XCTFail("pinged a machine that is not there")
        } catch let error as GstError {
            XCTAssertEqual(error.code, "not_found")
        }
        try await gst.run("daemon.reload")
    }

    func testGoTimes() throws {
        let reference = Date(timeIntervalSince1970: 1_794_333_455)
        for text in [
            "2026-11-10T17:57:35Z", "2026-11-10T17:57:35.123456789Z", "2026-11-10T17:57:35.5Z",
            "2026-11-11T01:57:35.123456+08:00", "2026-11-10T09:57:35.000001-08:00",
        ] {
            XCTAssertEqual(Gst.parseTime(text), reference, text)
        }
        XCTAssertNil(Gst.parseTime("yesterday"))
    }

    func testWhatGstReports() throws {
        let link = try decode(DaemonStatus.Link.self, #"{"destination":"a@b","state":"down","since":"2026-11-10T17:57:35.123456789Z","error":"x","reconnects":3,"peer":{"id":"1","name":"n"}}"#)
        XCTAssertEqual(link.state, .down)
        XCTAssertEqual(link.reconnects, 3)
        XCTAssertEqual(link.peer?.name, "n")
        XCTAssertEqual(link.since, Date(timeIntervalSince1970: 1_794_333_455))

        let reply = try decode(PingReply.self, #"{"node":{"id":"1","name":"n"},"address":"10.1.0.2","way":"relay","via":[{"id":"2","name":"mid"}],"time_ms":12.5}"#)
        XCTAssertEqual(reply.timeMs, 12.5)
        XCTAssertEqual(reply.via.first?.name, "mid")

        let removal = try decode(Removal.self, #"{"node":{"id":"1","name":"n"},"self":true,"stranded":[]}"#)
        XCTAssertTrue(removal.`self`)

        let check = try decode(ServerCheck.self, #"{"destination":"a@b","server":"b:22","user":"a","state":"changed_host","host_key":{"type":"ssh-ed25519","fingerprint":"SHA256:x","key":"ssh-ed25519 AAAA"}}"#)
        XCTAssertEqual(check.state, .changedHost)
        XCTAssertEqual(check.hostKey?.fingerprint, "SHA256:x")
    }

    /// A network of four, as the daemon reports it, becomes the list on
    /// the first screen.
    func testMachines() throws {
        let seen = Date(timeIntervalSince1970: 1_794_333_455)
        let topology = try decode(Topology.self, #"""
        {"self":"aa","subnet":"10.87.0.0/16","links":[{"from":"aa","to":"bb"}],"nodes":[
         {"id":"aa","name":"phone","reachable":true,"route":["aa"],"net":{"address":"10.87.0.1","public_key":"k"}},
         {"id":"cc","name":"homelab","reachable":true,"route":["aa","bb","cc"],"seen":"2026-11-10T17:57:35Z","net":{"address":"10.87.0.3","public_key":"k"}},
         {"id":"bb","name":"devbox","reachable":true,"direct":true,"route":["aa","bb"],"net":{"address":"10.87.0.2","public_key":"k"}},
         {"id":"dd","name":"attic","reachable":false,"seen":"2026-11-10T17:57:35Z","net":{"address":"10.99.0.4","public_key":"k"}}]}
        """#)
        let status = try decode(DaemonStatus.self, #"""
        {"software":"gst 0.7.0","build":"","pid":1,"config":"c","node":{"id":"aa","name":"phone"},"links":[],
         "net":{"subnet":"10.87.0.0/16","address":"10.87.0.1","interface":"utun4","peers":[
          {"node":"bb","name":"devbox","address":"10.87.0.2","way":"direct","endpoint":"203.0.113.7:41642","handshake":"2026-11-10T17:57:35Z","received":10,"sent":20}]}}
        """#)
        let machines = Machine.list(topology: topology, status: status)
        // This machine, the ones that packets reach, and then the rest.
        XCTAssertEqual(topology.links, [Topology.Link(from: "aa", to: "bb")])
        XCTAssertEqual(machines.map(\.name), ["phone", "devbox", "homelab", "attic"])
        XCTAssertEqual(machines[0].way, .here)
        XCTAssertEqual(machines[1].way, .direct(endpoint: "203.0.113.7:41642"))
        XCTAssertEqual(machines[1].sent, 20)
        XCTAssertEqual(machines[1].handshake, seen)
        XCTAssertEqual(machines[2].way, .relay(via: ["devbox"]))
        XCTAssertEqual(machines[2].address, "10.87.0.3")
        // An address of another subnet is one that nothing reaches.
        XCTAssertEqual(machines[3].way, .offline(seen: seen))
        XCTAssertNil(machines[3].address)
        // While gst is off, nothing is online.
        XCTAssertTrue(Machine.list(topology: topology, status: nil).allSatisfy { !$0.isOnline })
    }

    /// The chart of the pings draws them from edge to edge, leaves a gap
    /// where no answer came, and keeps the longest below its top.
    func testPings() {
        var pings = Pings()
        XCTAssertEqual(pings.lines, [])
        XCTAssertEqual(pings.span, 0...1)
        XCTAssertEqual(pings.scale.top, 100)

        let direct = Machine.Way.direct(endpoint: "203.0.113.7:41642")
        pings.answered(in: 58, way: .relay(via: ["devbox"]))
        pings.answered(in: 61, way: .relay(via: ["devbox"]))
        pings.answered(in: 24, way: direct)
        pings.failed("no answer")
        pings.answered(in: 22, way: direct)
        // The way changes nothing of the line; the ping without an answer
        // ends it.
        XCTAssertEqual(pings.lines.map { $0.points.map(\.id) }, [[0, 1, 2], [4]])
        XCTAssertEqual(pings.span, 0...4)
        XCTAssertEqual(pings.latest.last?.way, direct)
        XCTAssertEqual(pings.latest[3].error, "no answer")

        for (longest, step, top) in [(61.0, 50.0, 100.0), (23.0, 10.0, 30.0), (218.0, 100.0, 300.0), (320.0, 200.0, 400.0), (1.2, 1.0, 2.0), (0.3, 1.0, 2.0)] {
            var pings = Pings()
            pings.answered(in: longest, way: direct)
            XCTAssertEqual(pings.scale.step, step, "\(longest)")
            XCTAssertEqual(pings.scale.top, top, "\(longest)")
        }

        // Only the latest stay, and the chart moves along with them.
        for _ in 0..<Pings.most + 10 {
            pings.answered(in: 20, way: direct)
        }
        XCTAssertEqual(pings.latest.count, Pings.most)
        XCTAssertEqual(pings.span, 15...(Pings.most + 14))
        XCTAssertEqual(pings.latest.first?.id, 15)

        XCTAssertEqual(Pings.text(1.44), "1.4 ms")
        XCTAssertEqual(Pings.text(23.6), "24 ms")
    }

    /// The map puts the machines on a circle, the first at the top, and
    /// draws each link as a line that points at the machine linked to.
    func testRing() {
        let center = CGPoint(x: 100, y: 100)
        let links = [("aa", "cc"), ("cc", "aa"), ("bb", "aa"), ("aa", "gone"), ("bb", "bb")].map { Topology.Link(from: $0.0, to: $0.1) }
        let ring = Ring(machines: ["aa", "bb", "cc", "dd"], links: links, center: center, radius: 80)
        XCTAssertEqual(ring.spots.map(\.id), ["aa", "bb", "cc", "dd"])
        for (spot, (x, y)) in zip(ring.spots, [(100.0, 20.0), (180.0, 100.0), (100.0, 180.0), (20.0, 100.0)]) {
            XCTAssertEqual(spot.point.x, x, accuracy: 0.001)
            XCTAssertEqual(spot.point.y, y, accuracy: 0.001)
        }
        // Names go outside the circle.
        XCTAssertEqual(ring.spots.map(\.side), [.above, .right, .below, .left])

        // A link to a machine that is not on the map, or to itself, has no line.
        XCTAssertEqual(ring.lines.map { [$0.from, $0.to] }, [["aa", "cc"], ["cc", "aa"], ["bb", "aa"]])
        // A line leaves a little way from its machine, and ends further
        // from the one it points at, with the arrowhead before it.
        let down = ring.lines[0]
        XCTAssertEqual(down.start.x, 100, accuracy: 0.001)
        XCTAssertEqual(down.start.y, 29, accuracy: 0.001)
        XCTAssertEqual(down.end.y, 168, accuracy: 0.001)
        XCTAssertEqual(down.tip[0].y, 171, accuracy: 0.001)
        // The link back bows aside, so that both show.
        XCTAssertEqual(down.control.x, 100, accuracy: 0.001)
        XCTAssertGreaterThan(abs(ring.lines[1].control.x - 100), 10)
        // A line between neighbors bows towards the center.
        let side = ring.lines[2]
        XCTAssertEqual(side.control.x, 140 - 40 * 0.3, accuracy: 0.001)
        XCTAssertEqual(side.control.y, 60 + 40 * 0.3, accuracy: 0.001)

        // What an earlier version of the app kept has no links.
        let kept = try? JSONDecoder().decode(Topology.self, from: Data(#"{"self":"aa","subnet":"","nodes":[]}"#.utf8))
        XCTAssertNotNil(kept)
        XCTAssertNil(kept?.links)
    }

    func testDestinations() {
        XCTAssertEqual(Destination.make(server: "admin@ssh.example.com", port: ""), "admin@ssh.example.com")
        XCTAssertEqual(Destination.make(server: " admin@ssh.example.com ", port: "22"), "admin@ssh.example.com")
        XCTAssertEqual(Destination.make(server: "admin@ssh.example.com", port: "2222"), "ssh://admin@ssh.example.com:2222")
        XCTAssertEqual(Destination.make(server: "admin@2001:db8::7", port: "2222"), "ssh://admin@[2001:db8::7]:2222")
        for (server, port) in [
            ("ssh.example.com", ""), ("@ssh.example.com", ""), ("admin@", ""),
            ("admin@ssh.example.com", "0"), ("admin@ssh.example.com", "port"), ("admin@ssh example.com", ""),
        ] {
            XCTAssertNil(Destination.make(server: server, port: port), "\(server) \(port)")
        }
        XCTAssertEqual(Subnet("10.87.0.0/16")?.contains("10.87.255.1"), true)
        XCTAssertEqual(Subnet("10.87.0.0/16")?.contains("10.88.0.1"), false)
        XCTAssertNil(Subnet("nonsense"))
    }

    private func decode<T: Decodable>(_ type: T.Type, _ json: String) throws -> T {
        try Gst.decoder.decode(type, from: Data(json.utf8))
    }
}

/// What gst asked the system for, from gst's thread to the test's.
private final class Applied: @unchecked Sendable {
    private let lock = NSLock()
    private var settings: InterfaceSettings?

    func set(_ settings: InterfaceSettings) {
        lock.withLock { self.settings = settings }
    }

    func get() -> InterfaceSettings? {
        lock.withLock { settings }
    }
}
