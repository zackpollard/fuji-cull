import SwiftUI
import UIKit
import Libmpv

// libmpv-backed player, the port of Android's MpvPlayer.kt: ffmpeg software
// decode handles the 4:2:2 10-bit HEVC that AVPlayer will not sustain
// (probe-measured: Rext clips freeze seconds in with a healthy buffer, and
// AVPlayer's range crawl starves everything else), while hwdec=videotoolbox
// still covers ordinary clips with the hardware decoder. Streams the loopback
// /api/video URL exactly like the desktop GUI's mpv and Android do.
//
// Rendering is CAMetalLayer + vo=gpu-next over MoltenVK, the MPVKit-blessed
// path. One instance at a time (the viewer's active page enforces that).

struct MpvPlayerView: UIViewControllerRepresentable {
    @ObservedObject var model: GridModel
    let shot: Shot

    static let decoderSwitched = Notification.Name("mpvDecoderSwitched")

    func makeUIViewController(context: Context) -> MpvViewController {
        let vc = MpvViewController()
        vc.model = model
        vc.shot = shot
        return vc
    }

    func updateUIViewController(_ vc: MpvViewController, context: Context) {}
}

// MoltenVK workaround layer (from the MPVKit demo): it briefly forces a 1x1
// drawableSize to complete presentation, which must not stick.
final class MpvMetalLayer: CAMetalLayer {
    override var drawableSize: CGSize {
        get { super.drawableSize }
        set {
            if Int(newValue.width) > 1 && Int(newValue.height) > 1 {
                super.drawableSize = newValue
            }
        }
    }
}

final class MpvViewController: UIViewController {
    var model: GridModel?
    var shot: Shot?

    private var metalLayer = MpvMetalLayer()
    private var mpv: OpaquePointer?
    private let queue = DispatchQueue(label: "mpv", qos: .userInitiated)
    private var mpvLogsForwarded = 0
    // hardware-decode wedge detector: consecutive polls where the stream is
    // buffered but the frame never advances (the 4:2:2 signature on hardware
    // that rejects Rext). Once it trips we flip to software and stop checking.
    private var wedgePolls = 0
    private var softwareForced = false
    private var lastPos = -1.0
    private var pollTimer: Timer?
    private var probeSeconds = 0

    override func viewDidLoad() {
        super.viewDidLoad()
        view.backgroundColor = .black
        metalLayer.frame = view.frame
        metalLayer.contentsScale = UIScreen.main.nativeScale
        metalLayer.framebufferOnly = true
        metalLayer.backgroundColor = UIColor.black.cgColor
        view.layer.addSublayer(metalLayer)

        setupMpv()
        if let shot, let url = model?.videoURL(shot.id) {
            command("loadfile", [url.absoluteString, "replace"])
        }
        startPoll()
    }

    override func viewDidLayoutSubviews() {
        super.viewDidLayoutSubviews()
        metalLayer.frame = view.frame
    }

    override func touchesEnded(_ touches: Set<UITouch>, with event: UIEvent?) {
        super.touchesEnded(touches, with: event)
        togglePause()
    }

    deinit {
        pollTimer?.invalidate()
        if let mpv {
            mpv_terminate_destroy(mpv)
        }
    }

    private func setupMpv() {
        guard let handle = mpv_create() else {
            model?.logEvent("mpv: create failed")
            return
        }
        mpv = handle
        // mpv's own diagnostics into the app log: remote debugging of
        // playback failures needs mpv's voice, not guesses
        mpv_request_log_messages(handle, "warn")

        var wid = Int64(Int(bitPattern: Unmanaged.passUnretained(metalLayer).toOpaque()))
        _ = mpv_set_option(handle, "wid", MPV_FORMAT_INT64, &wid)
        opt("vo", "gpu-next")
        opt("gpu-api", "vulkan")
        opt("gpu-context", "moltenvk")
        // Start on hardware. 4:2:0 decodes here instantly; if VideoToolbox
        // rejects 4:2:2 the wedge poll below flips to software at runtime
        // (mpv keeps the demuxer buffer, so no re-pull from the camera).
        opt("hwdec", "videotoolbox")
        // when we DO fall to software (4:2:2), make it as fast as possible so
        // it stays watchable for culling: skip the deblocking loop filter
        // (big HEVC speedup, minor quality loss — fine for review) and allow
        // fast/inexact decode; drop late frames rather than stall.
        opt("profile", "fast")
        opt("vd-lavc-fast", "yes")
        opt("vd-lavc-skiploopfilter", "all")
        opt("vd-lavc-framedrop", "nonref")
        opt("framedrop", "vo")
        opt("vd-lavc-threads", "0")
        // generous in-memory demuxer cache: everything streamed this session
        // stays seekable without re-pulling from the camera
        opt("cache", "yes")
        opt("demuxer-max-bytes", "512MiB")
        opt("demuxer-max-back-bytes", "256MiB")

        guard mpv_initialize(handle) >= 0 else {
            model?.logEvent("mpv: initialize failed")
            mpv_terminate_destroy(handle)
            mpv = nil
            return
        }
        mpv_set_wakeup_callback(handle, { ctx in
            let me = unsafeBitCast(ctx, to: MpvViewController.self)
            me.readEvents()
        }, UnsafeMutableRawPointer(Unmanaged.passUnretained(self).toOpaque()))
    }

    private func opt(_ name: String, _ value: String) {
        guard let mpv else { return }
        let r = mpv_set_option_string(mpv, name, value)
        if r < 0 {
            model?.logEvent("mpv: option \(name)=\(value) rejected (\(String(cString: mpv_error_string(r))))")
        }
    }

    // MARK: - playback state

    func togglePause() {
        guard let mpv else { return }
        var flag = Int64()
        mpv_get_property(mpv, "pause", MPV_FORMAT_FLAG, &flag)
        var next: Int64 = flag > 0 ? 0 : 1
        mpv_set_property(mpv, "pause", MPV_FORMAT_FLAG, &next)
    }

    private func getDouble(_ name: String) -> Double {
        guard let mpv else { return 0 }
        var v = Double()
        mpv_get_property(mpv, name, MPV_FORMAT_DOUBLE, &v)
        return v
    }

    private func getFlag(_ name: String) -> Bool {
        guard let mpv else { return false }
        var v = Int64()
        mpv_get_property(mpv, name, MPV_FORMAT_FLAG, &v)
        return v > 0
    }

    /// Playback poll: drives the wedge detector and (probe-armed) the pstate
    /// trace that lets playback be verified without touching the screen.
    private func startPoll() {
        pollTimer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
            guard let self, let mpv = self.mpv else { return }
            let pos = self.getDouble("time-pos")
            let paused = self.getFlag("pause")
            let buffering = self.getFlag("paused-for-cache")
            let cached = self.getDouble("demuxer-cache-duration")

            if DebugProbe.openVideoRequested, self.probeSeconds < 20 {
                self.probeSeconds += 1
                let hw = self.softwareForced ? "sw" : "hw"
                self.model?.logEvent(String(format: "pstate: t=%.1f %@%@ cache=%.1fs [%@]",
                                            pos, paused ? "paused" : "playing",
                                            buffering ? "+buffering" : "", cached, hw))
            }

            // wedge: not paused, not waiting on cache, frame frozen
            if !self.softwareForced {
                if !paused && !buffering && pos == self.lastPos && pos > 0 {
                    self.wedgePolls += 1
                    if self.wedgePolls >= 3 {
                        self.softwareForced = true
                        self.model?.logEvent("mpv: hardware decode wedged (\(self.shot?.base ?? "?")) — flipping to software")
                        mpv_set_property_string(mpv, "hwdec", "no")
                        // surface it: the design's "switching decoder" toast
                        // makes the one-off stutter read as known, not broken
                        NotificationCenter.default.post(name: MpvPlayerView.decoderSwitched, object: nil)
                    }
                } else {
                    self.wedgePolls = 0
                }
            }
            self.lastPos = pos
        }
    }

    // MARK: - events

    private func readEvents() {
        queue.async { [weak self] in
            guard let self else { return }
            while let mpv = self.mpv {
                guard let event = mpv_wait_event(mpv, 0), event.pointee.event_id != MPV_EVENT_NONE else { break }
                switch event.pointee.event_id {
                case MPV_EVENT_LOG_MESSAGE:
                    if let msg = UnsafeMutablePointer<mpv_event_log_message>(OpaquePointer(event.pointee.data)),
                       self.mpvLogsForwarded < 25 {
                        self.mpvLogsForwarded += 1
                        let line = "mpv[\(String(cString: msg.pointee.prefix))] \(String(cString: msg.pointee.text).trimmingCharacters(in: .whitespacesAndNewlines))"
                        Task { @MainActor [weak self] in self?.model?.logEvent(line) }
                    }
                case MPV_EVENT_SHUTDOWN:
                    return
                default:
                    break
                }
            }
        }
    }

    private func command(_ name: String, _ args: [String]) {
        guard let mpv else { return }
        var cargs: [UnsafePointer<CChar>?] = ([name] + args).map { UnsafePointer(strdup($0)) }
        cargs.append(nil)
        defer { cargs.forEach { if let p = $0 { free(UnsafeMutablePointer(mutating: p)) } } }
        let r = mpv_command(mpv, &cargs)
        if r < 0 {
            model?.logEvent("mpv: \(name) failed (\(String(cString: mpv_error_string(r))))")
        }
    }
}
