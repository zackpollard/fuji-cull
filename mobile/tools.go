//go:build tools

package mobile

// Keeps golang.org/x/mobile in go.mod for `gomobile bind` (CI builds the
// Android AAR from this module).
import _ "golang.org/x/mobile/bind"
