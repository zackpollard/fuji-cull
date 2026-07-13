package mtpcli

import (
	"os"
	"strconv"
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
	usbMu.Unlock()
}

// ClearUSBFD drops the descriptor after the platform reports the device
// gone; a replug must call SetUSBFD again.
func ClearUSBFD() {
	usbMu.Lock()
	usbFile = nil
	usbMu.Unlock()
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
