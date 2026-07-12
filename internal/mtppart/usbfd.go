package mtppart

import (
	"os"

	"github.com/zack/fuji-tools/internal/mtpcli"
)

// usbArgs returns the aft invocation arguments, routing through a
// pre-opened usbfs descriptor when one is configured (Android USB Host
// API — see mtpcli.SetUSBFD).
func usbArgs() []string {
	if mtpcli.USBFile() != nil {
		return []string{"-b", "--device-fd", "3"}
	}
	return []string{"-b"}
}

// usbExtraFiles hands the shared descriptor to the child as fd 3.
func usbExtraFiles() []*os.File {
	if f := mtpcli.USBFile(); f != nil {
		return []*os.File{f}
	}
	return nil
}
