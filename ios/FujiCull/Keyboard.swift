import SwiftUI

// Hardware-keyboard culling for the viewer (iPad + Magic Keyboard): K/W keep,
// X/S reject, C clear, arrows navigate, Esc closes. onKeyPress is iOS 17+, so
// the modifier is a no-op on the 16.0 floor.
extension View {
    @ViewBuilder
    func keyCommands(_ handle: @escaping (String) -> Bool) -> some View {
        if #available(iOS 17.0, *) {
            self.focusable(true)
                .focusEffectDisabled()
                .onKeyPress { press in
                    let token: String
                    switch press.key {
                    case .leftArrow: token = "left"
                    case .rightArrow: token = "right"
                    case .upArrow: token = "up"
                    case .downArrow: token = "down"
                    case .escape: token = "esc"
                    case .return: token = "enter"
                    default: token = press.characters.lowercased()
                    }
                    return handle(token) ? .handled : .ignored
                }
        } else {
            self
        }
    }
}
