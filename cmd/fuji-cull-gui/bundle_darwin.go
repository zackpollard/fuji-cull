//go:build darwin

package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// tameCameraDaemons keeps macOS's PTP daemons (ptpcamerad/mscamerad — Image
// Capture's auto-claim) off the camera while the app runs. They grab any PTP
// device the moment it appears and fight our per-batch MTP sessions; under
// two masters the camera firmware degrades into stale-buffer mode far more
// often than on Linux. The daemons are on-demand launchd services — killing
// them is harmless (they respawn on the next device event), so sweep every
// few seconds for the app's lifetime.
func tameCameraDaemons() {
	go func() {
		logged := false
		for {
			killed := false
			for _, d := range []string{"ptpcamerad", "mscamerad", "PTPCamera"} {
				if exec.Command("pkill", "-x", d).Run() == nil {
					killed = true
				}
			}
			if killed && !logged {
				log.Printf("macos: stopping Image Capture camera daemons (they fight for the MTP device)")
				logged = true
			}
			time.Sleep(5 * time.Second)
		}
	}()
}

// setupBundleEnv wires the environment when running from a fuji-cull.app
// bundle: helper tools (gphoto2, ffmpeg, aft-mtp-cli-part) live in
// Contents/MacOS next to the app binary, and plugin/module trees live in
// Contents/Resources. Outside a bundle this is a no-op and the tools come
// from Homebrew via PATH as usual.
func setupBundleEnv() {
	tameCameraDaemons() // bundle or not: the daemons fight any MTP session
	exe, err := os.Executable()
	if err != nil {
		return
	}
	macos := filepath.Dir(exe)
	res := filepath.Join(macos, "..", "Resources")
	if _, err := os.Stat(filepath.Join(res, "libgphoto2")); err != nil {
		return // not a bundle
	}
	os.Setenv("PATH", macos+":"+filepath.Join(res, "bin")+":"+os.Getenv("PATH"))
	os.Setenv("CAMLIBS", filepath.Join(res, "libgphoto2"))
	os.Setenv("IOLIBS", filepath.Join(res, "libgphoto2_port"))
}
