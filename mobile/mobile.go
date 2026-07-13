// Package mobile is the gomobile-bind facade over the fuji-cull engine for
// the Android app: the whole pure-Go core (catalog, prefetcher, head sweep,
// Immich checking, importer) plus its HTTP API on a loopback port. The
// Kotlin UI drives control via these bindings and fetches image/video bytes
// over localhost HTTP (ExoPlayer plays /api/video ranges directly, including
// camera streaming).
//
// Camera access on Android: Kotlin obtains the USB device via UsbManager,
// opens it, and passes the raw file descriptor to SetUSBFD — every aft
// invocation then rides that descriptor (--device-fd).
package mobile

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/zack/fuji-tools/internal/cull"
	"github.com/zack/fuji-tools/internal/mtpcli"
)

// logRing keeps the tail of the engine log so the connect screen can show
// what discovery is actually doing — adb is rarely at hand in the field.
type logRing struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ln := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		// drop the date half of the stdlib log prefix: screen space is tight
		if len(ln) > 11 && ln[4] == '/' && ln[7] == '/' && ln[10] == ' ' {
			ln = ln[11:]
		}
		r.lines = append(r.lines, ln)
	}
	if len(r.lines) > 60 {
		r.lines = r.lines[len(r.lines)-60:]
	}
	return len(p), nil
}

func (r *logRing) tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) > n {
		return strings.Join(r.lines[len(r.lines)-n:], "\n")
	}
	return strings.Join(r.lines, "\n")
}

var engineLog = &logRing{}

// Engine is a running fuji-cull core serving HTTP on a loopback port.
type Engine struct {
	app  *cull.App
	ln   net.Listener
	port int
}

// Start launches the engine. dataDir holds sessions and settings, cacheDir
// the image buffer and thumbnails; aftPath is the bundled patched aft binary
// (Android: nativeLibraryDir + "/libaftcli.so"). immichURL/immichKey may be
// empty to disable Immich integration; session defaults to "default";
// immichStack stacks RAF+JPG pairs after upload.
func Start(dataDir, cacheDir, aftPath, immichURL, immichKey, session string, immichStack bool) (*Engine, error) {
	// The engine resolves sessions/settings under HOME.
	os.Setenv("HOME", dataDir)
	// stderr still reaches logcat (GoLog); the ring feeds the connect screen
	log.SetOutput(io.MultiWriter(os.Stderr, engineLog))
	if aftPath != "" {
		os.Setenv("FUJI_AFT", aftPath)      // bulk transfers
		os.Setenv("FUJI_AFT_PART", aftPath) // partial reads (same patched binary)
	}
	if session == "" {
		session = "default"
	}

	o := cull.Options{
		BackendName: "cli",
		CameraRoot:  "/SLOT 1/DCIM,/SLOT 2/DCIM",
		SessionName: session,
		ImmichStack: immichStack,
		CacheDir:    cacheDir,
		Ahead:       80,
		Behind:      30,
		EvictMargin: 300,
		Batch:       6,
		ImmichURL:   immichURL,
		ImmichKey:   immichKey,
		SkipImmich:  immichURL == "" || immichKey == "",
		Retries:     3,
		UploadConc:  2,
		HashConc:    2,
	}
	app, handler, err := cull.Start(o)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		app.Close()
		return nil, err
	}
	e := &Engine{app: app, ln: ln, port: ln.Addr().(*net.TCPAddr).Port}
	go http.Serve(ln, handler)
	return e, nil
}

// SetUSBFD hands the engine a USB device opened by the platform (Android's
// UsbDeviceConnection.getFileDescriptor()). Discovery retries pick it up
// within seconds; call again after replugging.
func (e *Engine) SetUSBFD(fd int) { mtpcli.SetUSBFD(fd) }

// ClearUSBFD forgets the descriptor after the platform reports the device
// detached, so aft stops being handed a dead fd.
func (e *Engine) ClearUSBFD() { mtpcli.ClearUSBFD() }

// RecentLog returns the last engine log lines for on-screen diagnostics.
func (e *Engine) RecentLog() string { return engineLog.tail(10) }

// Nudge wakes the fetch pipeline after the app returns to the foreground —
// breakers and backoffs probe immediately instead of waiting out their timers.
func (e *Engine) Nudge() { e.app.Nudge() }

// SetEnv sets a process environment variable through the Go runtime.
// Android's android.system.Os.setenv only mutates the C environ, which Go
// snapshotted at startup — values set there never reach exec'd children.
func SetEnv(key, value string) { os.Setenv(key, value) }

// Port is where the HTTP API and web assets are served on 127.0.0.1.
func (e *Engine) Port() int { return e.port }

// Ready reports whether camera discovery completed.
func (e *Engine) Ready() bool { return e.app.Ready() }

// DiscoveryStatus is a human-readable one-liner for the connect screen.
func (e *Engine) DiscoveryStatus() string {
	stage, files, errMsg := e.app.Discovery()
	if errMsg != "" {
		return "waiting for camera: " + errMsg
	}
	if files > 0 {
		return fmt.Sprintf("reading camera index: %s (%d files)", stage, files)
	}
	return "reading camera index"
}

// ShotCount returns the catalog size once ready (0 before).
func (e *Engine) ShotCount() int {
	if !e.app.Ready() {
		return 0
	}
	return len(e.app.Shots())
}

// Stop shuts the engine down.
func (e *Engine) Stop() {
	e.ln.Close()
	e.app.Close()
}
