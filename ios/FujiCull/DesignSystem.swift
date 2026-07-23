import SwiftUI

// The fuji-cull design system (docs/design/ is the source of truth).
// Single dark theme — never a light mode. IBM Plex Mono carries all DATA
// (counters, filenames, EXIF, badges, logs); IBM Plex Sans carries PROSE
// (guidance, dialog copy, setting descriptions).
enum DS {
    // MARK: color tokens
    static let bg        = Color(hex: 0x0B0C0B) // app canvas (warm near-black)
    static let tile      = Color(hex: 0x161815) // tile / cell / input surface
    static let surface   = Color(hex: 0x1C1F1A) // panels, sheets, filmstrip
    static let surfaceHi = Color(hex: 0x242821) // hover, popover, active
    static let line      = Color(hex: 0x30352C) // hairline border
    static let text      = Color(hex: 0xECE9E0) // primary (warm cream)
    static let text2     = Color(hex: 0x9DA093) // secondary
    static let text3     = Color(hex: 0x6B6E62) // muted / disabled
    static let amber     = Color(hex: 0xFFB32E) // cursor, primary action, focus
    static let keep      = Color(hex: 0x38D67A)
    static let reject    = Color(hex: 0xFF5A3D)
    static let buffered  = Color(hex: 0x4EA6FF) // full-res cached locally
    static let immich    = Color(hex: 0x57C9C1) // already on the server
    static let inset     = Color(hex: 0x0F110E) // inset panel dark

    /// Undecided decision slot: "not yet", not "nothing".
    static let undecidedHairline = Color.white.opacity(0.08)

    // MARK: type scale (px / family / weight per the handoff)
    static func counter(_ size: CGFloat = 28) -> Font { .custom("IBMPlexMono-SemiBold", size: size) }
    static func title(_ size: CGFloat = 19) -> Font { .custom("IBMPlexMono-Medium", size: size) }
    static func label(_ size: CGFloat = 13) -> Font { .custom("IBMPlexMono-Medium", size: size) }
    static func mono(_ size: CGFloat = 13) -> Font { .custom("IBMPlexMono-Regular", size: size) }
    static func micro(_ size: CGFloat = 11) -> Font { .custom("IBMPlexMono-Regular", size: size) }
    static func body(_ size: CGFloat = 15) -> Font { .custom("IBMPlexSans-Regular", size: size) }
    static func emphasis(_ size: CGFloat = 15) -> Font { .custom("IBMPlexSans-SemiBold", size: size) }

    // MARK: spacing (4px base) & radii (density is a feature)
    static let s1: CGFloat = 4
    static let s2: CGFloat = 8
    static let s3: CGFloat = 12
    static let s4: CGFloat = 16
    static let s5: CGFloat = 24
    static let s6: CGFloat = 32
    static let rTile: CGFloat = 2
    static let rControl: CGFloat = 4
    static let rSheet: CGFloat = 8

    // MARK: motion — standard easing cubic-bezier(0.2,0,0,1)
    static let easing = Animation.timingCurve(0.2, 0, 0, 1, duration: 0.22)
    static let viewerSlide = Animation.timingCurve(0.2, 0, 0, 1, duration: 0.12)
}

extension Color {
    init(hex: UInt32) {
        self.init(red: Double((hex >> 16) & 0xFF) / 255,
                  green: Double((hex >> 8) & 0xFF) / 255,
                  blue: Double(hex & 0xFF) / 255)
    }
}
