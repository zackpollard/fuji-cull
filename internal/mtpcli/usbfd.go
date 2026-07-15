package mtpcli

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// Android's USB Host API hands apps a pre-opened usbfs file descriptor
// instead of /dev/bus/usb access. When set, every aft invocation (here and
// in mtppart) inherits the fd as descriptor 3 and gets --device-fd 3.
var (
	usbMu   sync.Mutex
	usbFile *os.File // wrapped once and rooted forever: a GC'd wrapper would close the app's fd
)

func init() {
	if v := os.Getenv("FUJI_USB_FD"); v != "" {
		if fd, err := strconv.Atoi(v); err == nil && fd >= 0 {
			SetUSBFD(fd)
		}
	}
}

// SetUSBFD routes all camera access through a pre-opened usbfs descriptor.
func SetUSBFD(fd int) {
	usbMu.Lock()
	if fd < 0 {
		usbFile = nil
	} else {
		usbFile = os.NewFile(uintptr(fd), "usb-device")
	}
	brokenStreak = 0 // fresh connection gets the benefit of the doubt
	usbMu.Unlock()
}

// ClearUSBFD drops the descriptor after the platform reports the device
// gone; a replug must call SetUSBFD again.
func ClearUSBFD() {
	usbMu.Lock()
	usbFile = nil
	resetPending = false
	usbMu.Unlock()
}

var resetPending bool

// RequestReset schedules a best-effort USBDEVFS_RESET (aft -R) on the next
// invocation — the software equivalent of replugging the cable, for links
// that reconnect in a degraded state (URB submissions failing).
func RequestReset() {
	usbMu.Lock()
	resetPending = true
	usbMu.Unlock()
}

// ConsumeReset reports whether the next invocation should reset the device,
// clearing the flag (one attempt per request).
func ConsumeReset() bool {
	usbMu.Lock()
	defer usbMu.Unlock()
	if resetPending && usbFile != nil {
		resetPending = false
		return true
	}
	return false
}

// TransportBroken recognizes stderr from a degraded USB link that a device
// reset may cure (seen after the camera drops off the bus and reconnects).
func TransportBroken(out string) bool {
	return strings.Contains(out, "submitting urb") || strings.Contains(out, "REAPURB") ||
		strings.Contains(out, "timeout reaping") || strings.Contains(out, "DISCARDURB")
}

var brokenStreak int

// NoteTransportResult tracks consecutive broken-transport failures so the
// app can tell "camera momentarily busy" from "link is dead" (a wedged
// camera after an unclean shutdown survives even USBDEVFS_RESET and needs
// a cable replug or power cycle).
func NoteTransportResult(broken bool) {
	usbMu.Lock()
	if broken {
		brokenStreak++
	} else {
		brokenStreak = 0
	}
	usbMu.Unlock()
}

// LinkDead reports a persistently unresponsive USB link.
func LinkDead() bool {
	usbMu.Lock()
	defer usbMu.Unlock()
	return brokenStreak >= 3
}

// USBFile returns the shared descriptor wrapper, or nil in normal
// /dev/bus/usb discovery mode.
func USBFile() *os.File {
	usbMu.Lock()
	defer usbMu.Unlock()
	return usbFile
}

// AftBin resolves the stock aft-mtp-cli binary (env override for platforms
// without a PATH-installed copy, e.g. Android's nativeLibraryDir).
func AftBin() string {
	if p := os.Getenv("FUJI_AFT"); p != "" {
		return p
	}
	return "aft-mtp-cli"
}
