// loupe-camfeed feeds Handset Camera, the camera Handset's camera extension
// adds to the Mac, so an AVD booted on it (-camera-back webcamN) sees frames
// loupe is sent without restarting.
//
// loupe starts it and writes frames to stdin, each a 4-byte big-endian length
// and that many bytes of PNG or JPEG. A zero length stops the feed, and the
// extension shows its placeholder. The latest frame is sent on at 30 fps, so
// a client that posts a few frames a second still gives a steady picture.
//
// It prints one line when it starts: "ready", or "missing" (exit 3) when the
// Mac has no Handset Camera. It prints "gone" and exits 4 when the camera goes
// away later, and "busy" and exits 5 when another process already feeds the
// camera (a sink takes one feeder, so a second's frames are never read). A
// process only sees cameras that existed when it first asked CoreMediaIO, so
// loupe starts a new one after the extension is installed or updated rather
// than waiting in this one.
//
// Any process of the user's can feed the camera; the extension cannot check
// a client's signature from its sandbox.

import CoreGraphics
import CoreMedia
import CoreMediaIO
import CoreVideo
import Foundation
import ImageIO

setvbuf(stdout, nil, _IOLBF, 0)

let cameraName = ProcessInfo.processInfo.environment["LOUPE_CAMFEED_NAME"] ?? "Handset Camera"
let width = 1280, height = 720
let fps: Int32 = 30

// Frames arrive from a launchd job; without this the system throttles it to a
// few frames a second.
let activity = ProcessInfo.processInfo.beginActivity(
    options: [.userInitiated, .latencyCritical], reason: "Feeding Handset Camera")

func fail(_ code: Int32, _ line: String) -> Never {
    print(line)
    exit(code)
}

func address(_ selector: Int) -> CMIOObjectPropertyAddress {
    CMIOObjectPropertyAddress(
        mSelector: CMIOObjectPropertySelector(selector),
        mScope: CMIOObjectPropertyScope(kCMIOObjectPropertyScopeGlobal),
        mElement: CMIOObjectPropertyElement(kCMIOObjectPropertyElementMain))
}

func ids(_ object: CMIOObjectID, _ selector: Int) -> [CMIOObjectID] {
    var a = address(selector)
    var size: UInt32 = 0
    guard CMIOObjectGetPropertyDataSize(object, &a, 0, nil, &size) == 0, size > 0 else { return [] }
    var out = [CMIOObjectID](repeating: 0, count: Int(size) / MemoryLayout<CMIOObjectID>.stride)
    var used: UInt32 = 0
    guard CMIOObjectGetPropertyData(object, &a, 0, nil, size, &used, &out) == 0 else { return [] }
    return out
}

func name(_ object: CMIOObjectID) -> String? {
    var a = address(kCMIOObjectPropertyName)
    var cf: Unmanaged<CFString>?
    var used: UInt32 = 0
    let size = UInt32(MemoryLayout<Unmanaged<CFString>?>.size)
    guard CMIOObjectGetPropertyData(object, &a, 0, nil, size, &used, &cf) == 0, let cf else { return nil }
    return cf.takeRetainedValue() as String
}

func direction(_ stream: CMIOStreamID) -> UInt32? {
    var a = address(kCMIOStreamPropertyDirection)
    var value: UInt32 = 0
    var used: UInt32 = 0
    guard CMIOObjectGetPropertyData(stream, &a, 0, nil, UInt32(MemoryLayout<UInt32>.size), &used, &value) == 0 else { return nil }
    return value
}

guard let device = ids(CMIOObjectID(kCMIOObjectSystemObject), kCMIOHardwarePropertyDevices)
    .first(where: { name($0) == cameraName }) else { fail(3, "missing") }
// The stream clients write into. A camera's own stream reports direction 1;
// a sink reports 0.
let streams = ids(device, kCMIODevicePropertyStreams)
guard let sink = streams.first(where: { name($0)?.hasSuffix("Sink") == true })
    ?? streams.first(where: { direction($0) == 0 }) else { fail(1, "error: \(cameraName) has no sink stream") }

var unmanagedQueue: Unmanaged<CMSimpleQueue>?
guard CMIOStreamCopyBufferQueue(sink, { _, _, _ in }, nil, &unmanagedQueue) == 0,
      let queue = unmanagedQueue?.takeRetainedValue() else { fail(1, "error: no buffer queue") }

var format: CMVideoFormatDescription?
CMVideoFormatDescriptionCreate(
    allocator: nil, codecType: kCVPixelFormatType_32BGRA,
    width: Int32(width), height: Int32(height), extensions: nil, formatDescriptionOut: &format)

let state = DispatchQueue(label: "camfeed")
var latest: CVPixelBuffer?
var streaming = false
// enqueued counts the frames put on the sink queue since streaming last
// started; streamingStartedAt is when that was. The extension reads from only
// one feeder's queue, so when another process holds the camera none of ours is
// taken: the busy watchdog catches enqueued frames that never drain.
var enqueued = 0
var streamingStartedAt: DispatchTime?

/// Where a picture of w×h goes in the frame: fitted whole into the centred
/// 3:4 strip, 540×720. An AVD's back sensor is mounted at 90°, so the
/// emulator keeps only that strip of a 16:9 host camera and scales it up.
/// Filled to 1280×720, a webcam lost 58% of its width and looked zoomed in
/// next to the same camera in other apps. Measured on pixel_fold_hinge
/// (API 36): a labeled 1280×720 grid saved a 960×1280 photo of x 370-908
/// and every row.
enum Layout {
    static func pictureRect(width w: Double, height h: Double, frameWidth: Double, frameHeight: Double) -> CGRect {
        let scale = min(frameHeight * 3 / 4 / w, frameHeight / h)
        let pw = w * scale, ph = h * scale
        return CGRect(x: (frameWidth - pw) / 2, y: (frameHeight - ph) / 2, width: pw, height: ph)
    }
}

/// The image as a 1280×720 frame, on black, placed by Layout.pictureRect.
func pixelBuffer(_ data: Data) -> CVPixelBuffer? {
    guard let src = CGImageSourceCreateWithData(data as CFData, nil),
          let image = CGImageSourceCreateImageAtIndex(src, 0, nil) else { return nil }
    var pb: CVPixelBuffer?
    let attrs = [kCVPixelBufferIOSurfacePropertiesKey: [:]] as CFDictionary
    guard CVPixelBufferCreate(nil, width, height, kCVPixelFormatType_32BGRA, attrs, &pb) == kCVReturnSuccess, let pb else { return nil }
    CVPixelBufferLockBaseAddress(pb, [])
    defer { CVPixelBufferUnlockBaseAddress(pb, []) }
    guard let ctx = CGContext(
        data: CVPixelBufferGetBaseAddress(pb), width: width, height: height, bitsPerComponent: 8,
        bytesPerRow: CVPixelBufferGetBytesPerRow(pb), space: CGColorSpaceCreateDeviceRGB(),
        bitmapInfo: CGImageAlphaInfo.premultipliedFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue) else { return nil }
    ctx.setFillColor(CGColor(red: 0, green: 0, blue: 0, alpha: 1))
    ctx.fill(CGRect(x: 0, y: 0, width: width, height: height))
    ctx.interpolationQuality = .high
    ctx.draw(image, in: Layout.pictureRect(width: Double(image.width), height: Double(image.height),
                                    frameWidth: Double(width), frameHeight: Double(height)))
    return pb
}

func setStreaming(_ on: Bool) {
    guard on != streaming else { return }
    let status = on ? CMIODeviceStartStream(device, sink) : CMIODeviceStopStream(device, sink)
    if status != 0 { FileHandle.standardError.write("camfeed: \(on ? "start" : "stop") stream: \(status)\n".data(using: .utf8)!) }
    streaming = on && status == 0
    enqueued = 0
    streamingStartedAt = streaming ? .now() : nil
}

let timer = DispatchSource.makeTimerSource(queue: state)
timer.schedule(deadline: .now(), repeating: .nanoseconds(1_000_000_000 / Int(fps)))
timer.setEventHandler {
    guard streaming, let pb = latest, let format,
          CMSimpleQueueGetCount(queue) < CMSimpleQueueGetCapacity(queue) else { return }
    var timing = CMSampleTimingInfo(
        duration: CMTime(value: 1, timescale: fps),
        presentationTimeStamp: CMClockGetTime(CMClockGetHostTimeClock()), decodeTimeStamp: .invalid)
    var sample: CMSampleBuffer?
    guard CMSampleBufferCreateReadyWithImageBuffer(
        allocator: kCFAllocatorDefault, imageBuffer: pb, formatDescription: format,
        sampleTiming: &timing, sampleBufferOut: &sample) == noErr, let sample else { return }
    CMSimpleQueueEnqueue(queue, element: Unmanaged.passRetained(sample).toOpaque())
    enqueued += 1
}
timer.resume()

// The extension serves one feeder from a camera's sink; a second feeder's
// frames are enqueued but never read. If nothing we put on the queue has
// drained a few seconds into streaming, another process holds the camera, so
// this feed is doing nothing: say so and exit for loupe to report.
let busyGrace = 2.5
let busy = DispatchSource.makeTimerSource(queue: state)
busy.schedule(deadline: .now() + busyGrace, repeating: 1)
busy.setEventHandler {
    guard streaming, let startedAt = streamingStartedAt, enqueued > 0 else { return }
    let elapsed = Double(DispatchTime.now().uptimeNanoseconds &- startedAt.uptimeNanoseconds) / 1e9
    guard elapsed > busyGrace else { return }
    // enqueued is every frame queued; the queue holds those not yet read. When
    // they are equal the reader has taken none of ours.
    if enqueued - Int(CMSimpleQueueGetCount(queue)) == 0 {
        FileHandle.standardError.write("camfeed: another process is feeding \(cameraName); this feed is doing nothing\n".data(using: .utf8)!)
        print("busy")
        exit(5)
    }
}
busy.resume()

// An update or removal of the extension takes the camera away, and this
// process would never see the new one, so it exits and loupe starts another.
let watch = DispatchSource.makeTimerSource(queue: state)
watch.schedule(deadline: .now() + 2, repeating: 2)
watch.setEventHandler {
    if !ids(CMIOObjectID(kCMIOObjectSystemObject), kCMIOHardwarePropertyDevices).contains(device) {
        print("gone")
        exit(4)
    }
}
watch.resume()

print("ready")

// stdin: frames until EOF, which means loupe went away.
Thread.detachNewThread {
    let input = FileHandle.standardInput
    func read(_ n: Int) -> Data? {
        var out = Data()
        while out.count < n {
            let chunk = input.readData(ofLength: n - out.count)
            if chunk.isEmpty { return nil }
            out.append(chunk)
        }
        return out
    }
    while let header = read(4) {
        let n = header.reduce(0) { $0 << 8 | Int($1) }
        if n == 0 {
            state.sync { latest = nil; setStreaming(false) }
            continue
        }
        guard n <= 32 << 20, let body = read(n) else { break }
        guard let pb = pixelBuffer(body) else {
            FileHandle.standardError.write("camfeed: frame of \(n) bytes is not a PNG or JPEG\n".data(using: .utf8)!)
            continue
        }
        state.sync { latest = pb; setStreaming(true) }
    }
    state.sync { setStreaming(false) }
    _ = activity
    exit(0)
}

// CoreMediaIO's callbacks run on the main run loop.
RunLoop.main.run()
