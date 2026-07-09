//go:build darwin

package main

import (
	"os"
	"path/filepath"
)

// setupBundleEnv wires the environment when running from a fuji-cull.app
// bundle: helper tools (gphoto2, ffmpeg, exiftool, aft-mtp-cli-part) live in
// Contents/MacOS next to the app binary, and plugin/module trees live in
// Contents/Resources. Outside a bundle this is a no-op and the tools come
// from Homebrew via PATH as usual.
func setupBundleEnv() {
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
	os.Setenv("PERL5LIB", filepath.Join(res, "perl5")+":"+os.Getenv("PERL5LIB"))
}
