import UIKit
import XCTest

// The runner is one UI test that never returns. It listens on the phone's
// own 127.0.0.1, which loupe reaches through usbmuxd over the cable, and
// reads one command per line, answering each with `ok` or `err <why>` as
// simcap's input process does:
//
//   size                       ok <width> <height>, the screen in pixels
//   tap <x> <y>
//   swipe <x1> <y1> <x2> <y2> <ms>
//   touch down|move|up <x> <y>
//   ping
//
// Coordinates are screen pixels, the space loupe's screenshots use.
//
// XCUICoordinate has no live touch: a gesture is synthesized whole. So
// `touch down` and `touch move` are recorded and answered `ok` at once, and
// `touch up` plays the gesture and answers once it has: a short, still touch
// is a tap, a long still one a press, anything that moved a press and drag
// from the first point to the last. The path between them is not replayed.
//
// The port comes from LOUPE_PORT, which xcodebuild sets from
// TEST_RUNNER_LOUPE_PORT in its own environment.
final class RunnerTests: XCTestCase {
    // UIScreen in the runner's own process is letterboxed (960x1440 on an
    // iPhone 16 Pro), so the screen is measured through XCUI instead: a
    // screenshot's pixels over SpringBoard's frame in points.
    private var scale: CGFloat = 0
    private var pixels: CGSize = .zero
    private var screen = XCUIApplication(bundleIdentifier: "com.apple.springboard")
    private var origin: XCUICoordinate!
    private var downAt: (point: CGPoint, time: Date)?
    private var last: CGPoint = .zero

    func testServe() throws {
        continueAfterFailure = true
        executionTimeAllowance = 60 * 60 * 24 * 365
        origin = screen.coordinate(withNormalizedOffset: .zero)
        measure()
        let port = UInt16(ProcessInfo.processInfo.environment["LOUPE_PORT"] ?? "") ?? 7110
        let listener = try listen(port: port)
        NSLog("loupe runner listening on 127.0.0.1:%d", Int(port))
        while true {
            let fd = accept(listener, nil, nil)
            if fd < 0 { continue }
            serve(fd)
            close(fd)
            downAt = nil
        }
    }

    private func listen(port: UInt16) throws -> Int32 {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { throw posixError("socket") }
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, socklen_t(MemoryLayout<Int32>.size))
        var addr = sockaddr_in()
        addr.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = port.bigEndian
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        let bound = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
        guard bound == 0 else { throw posixError("bind") }
        guard Darwin.listen(fd, 1) == 0 else { throw posixError("listen") }
        return fd
    }

    private func measure() {
        let shot = XCUIScreen.main.screenshot().image
        pixels = CGSize(width: shot.size.width * shot.scale, height: shot.size.height * shot.scale)
        let points = screen.frame.width
        scale = points > 0 ? pixels.width / points : shot.scale
        NSLog("loupe runner screen %.0fx%.0f px, scale %.2f", pixels.width, pixels.height, scale)
    }

    private func posixError(_ what: String) -> NSError {
        NSError(domain: NSPOSIXErrorDomain, code: Int(errno),
                userInfo: [NSLocalizedDescriptionKey: "\(what): \(String(cString: strerror(errno)))"])
    }

    private func serve(_ fd: Int32) {
        var pending = Data()
        var buf = [UInt8](repeating: 0, count: 4096)
        while true {
            let n = read(fd, &buf, buf.count)
            if n <= 0 { return }
            pending.append(buf, count: n)
            while let nl = pending.firstIndex(of: 0x0a) {
                let line = String(decoding: pending[pending.startIndex..<nl], as: UTF8.self)
                pending.removeSubrange(pending.startIndex...nl)
                let reply = handle(line.trimmingCharacters(in: .whitespaces)) + "\n"
                let bytes = Array(reply.utf8)
                if write(fd, bytes, bytes.count) != bytes.count { return }
            }
        }
    }

    private func handle(_ line: String) -> String {
        let f = line.split(separator: " ").map(String.init)
        guard let verb = f.first else { return "err empty command" }
        let n = f.dropFirst().compactMap { Double($0) }
        switch (verb, n.count) {
        case ("ping", 0):
            return "ok"
        case ("size", 0):
            measure()
            return "ok \(Int(pixels.width)) \(Int(pixels.height))"
        case ("tap", 2):
            at(n[0], n[1]).tap()
            return "ok"
        case ("swipe", 5):
            drag(from: point(n[0], n[1]), to: point(n[2], n[3]), seconds: n[4] / 1000)
            return "ok"
        case ("touch", 2) where f.count == 4:
            return touch(f[1], point(n[0], n[1]))
        case ("touch2", _):
            return "err one finger only on this route"
        default:
            return "err unknown command: \(verb)"
        }
    }

    private func touch(_ phase: String, _ p: CGPoint) -> String {
        switch phase {
        case "down":
            downAt = (p, Date())
            last = p
        case "move":
            guard downAt != nil else { return "err move without down" }
            last = p
        case "up":
            guard let down = downAt else { return "err up without down" }
            downAt = nil
            let held = Date().timeIntervalSince(down.time)
            if hypot(p.x - down.point.x, p.y - down.point.y) < 10 {
                if held < 0.4 {
                    offset(down.point).tap()
                } else {
                    offset(down.point).press(forDuration: held)
                }
            } else {
                drag(from: down.point, to: p, seconds: held)
            }
        default:
            return "err unknown phase: \(phase)"
        }
        return "ok"
    }

    // A drag holds briefly at the start, then moves at the speed the finger
    // did. A press shorter than about 0.05s is not reliably a drag.
    private func drag(from a: CGPoint, to b: CGPoint, seconds: Double) {
        let distance = hypot(b.x - a.x, b.y - a.y)
        let velocity = XCUIGestureVelocity(max(100, distance / max(seconds, 0.05)))
        offset(a).press(forDuration: 0.05, thenDragTo: offset(b), withVelocity: velocity, thenHoldForDuration: 0)
    }

    private func point(_ x: Double, _ y: Double) -> CGPoint {
        CGPoint(x: x / scale, y: y / scale)
    }

    private func at(_ x: Double, _ y: Double) -> XCUICoordinate { offset(point(x, y)) }

    private func offset(_ p: CGPoint) -> XCUICoordinate {
        origin.withOffset(CGVector(dx: p.x, dy: p.y))
    }
}
