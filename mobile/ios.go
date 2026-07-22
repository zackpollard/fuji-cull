package mobile

import (
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/zack/fuji-tools/internal/cull"
)

// StartLocal boots the engine against a local media tree (the `dir` backend)
// instead of a camera — the simulator / fake-backend path for the iOS app.
// mediaRoot contains "SLOT 1/DCIM/NNN_FUJI/DSCF*.JPG" (see SeedFakeCorpus).
// No exec, no camera: everything the loopback API serves comes off local files,
// so the whole SwiftUI UI is buildable and testable in the iOS simulator.
func StartLocal(dataDir, cacheDir, mediaRoot, session string) (*Engine, error) {
	os.Setenv("HOME", dataDir)
	sinks := []io.Writer{os.Stderr, engineLog}
	if f := openLogFile(dataDir); f != nil {
		sinks = append(sinks, f)
	}
	log.SetOutput(io.MultiWriter(sinks...))
	if session == "" {
		session = "default"
	}
	o := cull.Options{
		BackendName: "dir",
		Root:        mediaRoot,
		SessionName: session,
		CacheDir:    cacheDir,
		Ahead:       80,
		Behind:      30,
		EvictMargin: 300,
		Batch:       6,
		SkipImmich:  true,
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

// Transport is the camera link the host implements — on iOS, Swift's
// ICCTransport over ImageCaptureCore. gomobile only generates a
// host-implementable protocol for interfaces declared in the *bound* package,
// so this mirrors cull.Transport and transportAdapter bridges the two.
//
// Listings cross as JSON because only bytes/strings/ints may traverse the
// gomobile boundary:
//
//	Folders() -> [{"dir":"SLOT 1/DCIM/151_FUJI","folder":"151_FUJI"}]
//	Entries() -> [{"objectID":"12","name":"DSCF0001.JPG","size":123,"date":"2026-05-10"}]
//
// Implementations must serialize their own access: the engine assumes a
// single-threaded MTP link.
type Transport interface {
	Folders() ([]byte, error)
	Entries(dir string) ([]byte, error)
	ReadAt(objectID string, offset, size int64) ([]byte, error)
	Download(objectID, destPath string) error
	Connected() bool
}

// transportAdapter presents a host Transport to the engine.
type transportAdapter struct{ t Transport }

func (a transportAdapter) Folders() ([]byte, error)           { return a.t.Folders() }
func (a transportAdapter) Entries(dir string) ([]byte, error) { return a.t.Entries(dir) }
func (a transportAdapter) ReadAt(objectID string, offset, size int64) ([]byte, error) {
	return a.t.ReadAt(objectID, offset, size)
}
func (a transportAdapter) Download(objectID, destPath string) error {
	return a.t.Download(objectID, destPath)
}
func (a transportAdapter) Connected() bool { return a.t.Connected() }

// StartICC boots the engine against a camera reached through an injected
// Transport — on iOS that is the Swift ICCTransport over ImageCaptureCore,
// since iOS has neither exec nor usbfs for the patched aft binary. Partial
// reads (head sweep, orientation, chunked image pulls) all ride
// Transport.ReadAt; the breakers and magic-byte validation are unchanged,
// because the X-H2S stale-buffer bug is transport-agnostic.
func StartICC(dataDir, cacheDir string, t Transport, immichURL, immichKey, session string, immichStack bool) (*Engine, error) {
	os.Setenv("HOME", dataDir)
	sinks := []io.Writer{os.Stderr, engineLog}
	if f := openLogFile(dataDir); f != nil {
		sinks = append(sinks, f)
	}
	log.SetOutput(io.MultiWriter(sinks...))
	if session == "" {
		session = "default"
	}
	o := cull.Options{
		Transport:   transportAdapter{t},
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

// corpusEpoch anchors the synthetic corpus's capture dates, so the timeline
// groups into believable month/day sections.
var corpusEpoch = time.Date(2026, 5, 3, 9, 0, 0, 0, time.Local)

// SeedFakeCorpus writes a synthetic media tree under root when it is empty:
// `root/SLOT 1/DCIM/{151_FUJI,152_FUJI,...}/DSCF####.JPG`, one distinctly
// tinted JPEG per shot so grid tiles are visually separable. Idempotent — a
// non-empty tree is left untouched (the engine reports the shot count).
func SeedFakeCorpus(root string, folders, perFolder int) error {
	dcim := filepath.Join(root, "SLOT 1", "DCIM")
	if countJPEGs(dcim) > 0 {
		return nil
	}
	if folders < 1 {
		folders = 1
	}
	if perFolder < 1 {
		perFolder = 1
	}
	idx := 0
	total := folders * perFolder
	for f := 0; f < folders; f++ {
		dir := filepath.Join(dcim, fmt.Sprintf("%03d_FUJI", 151+f))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for n := 0; n < perFolder; n++ {
			path := filepath.Join(dir, fmt.Sprintf("DSCF%04d.JPG", 1000+f*perFolder+n))
			if err := writeTintedJPEG(path, idx, total); err != nil {
				return err
			}
			// spread the corpus over ~40 days so the timeline groups into real
			// month/day sections (the dir backend takes the day from mtime)
			shot := corpusEpoch.AddDate(0, 0, idx/6).Add(time.Duration(idx%6) * time.Hour)
			_ = os.Chtimes(path, shot, shot)
			idx++
		}
	}
	log.Printf("fake corpus: seeded %d synthetic shots under %s", idx, dcim)
	return nil
}

func countJPEGs(dcim string) int {
	n := 0
	filepath.Walk(dcim, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(info.Name()) == ".JPG" {
			n++
		}
		return nil
	})
	return n
}

// writeTintedJPEG renders an 800x600 frame: a diagonal gradient in a hue picked
// by the golden angle (so adjacent shots are clearly different colors) with the
// shot's 1-based number drawn large, so grid tiles and viewer frames are each
// identifiable at a glance.
func writeTintedJPEG(path string, idx, total int) error {
	const w, h = 800, 600
	hue := math.Mod(float64(idx)*137.508, 360) // golden angle spreads neighbors apart
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 0.28 + 0.55*float64(x+y)/float64(w+h) // diagonal brightness sweep
			r, g, b := hsv(hue, 0.5, v)
			img.Set(x, y, color.RGBA{r, g, b, 255})
		}
	}
	drawBigLabel(img, fmt.Sprintf("%04d", idx+1))
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	return jpeg.Encode(out, img, &jpeg.Options{Quality: 85})
}

// drawBigLabel centers a scaled-up bitmap-font label over a dark plate.
func drawBigLabel(dst *image.RGBA, label string) {
	face := basicfont.Face7x13
	tw, th := len(label)*7, 13
	small := image.NewRGBA(image.Rect(0, 0, tw, th))
	d := font.Drawer{Dst: small, Src: image.NewUniform(color.White), Face: face, Dot: fixed.P(0, 11)}
	d.DrawString(label)

	scale := float64(dst.Bounds().Dx()) * 0.42 / float64(tw)
	bw, bh := int(float64(tw)*scale), int(float64(th)*scale)
	bx, by := (dst.Bounds().Dx()-bw)/2, (dst.Bounds().Dy()-bh)/2
	plate := image.Rect(bx-24, by-16, bx+bw+24, by+bh+16)
	xdraw.Draw(dst, plate, image.NewUniform(color.RGBA{0, 0, 0, 110}), image.Point{}, xdraw.Over)
	xdraw.NearestNeighbor.Scale(dst, image.Rect(bx, by, bx+bw, by+bh), small, small.Bounds(), xdraw.Over, nil)
}

func hsv(hDeg, s, v float64) (uint8, uint8, uint8) {
	c := v * s
	hp := hDeg / 60
	x := c * (1 - absf(mod(hp, 2)-1))
	var r, g, b float64
	switch int(hp) % 6 {
	case 0:
		r, g, b = c, x, 0
	case 1:
		r, g, b = x, c, 0
	case 2:
		r, g, b = 0, c, x
	case 3:
		r, g, b = 0, x, c
	case 4:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	m := v - c
	return u8((r + m) * 255), u8((g + m) * 255), u8((b + m) * 255)
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
func mod(a, b float64) float64 {
	for a >= b {
		a -= b
	}
	return a
}
func u8(f float64) uint8 {
	if f < 0 {
		return 0
	}
	if f > 255 {
		return 255
	}
	return uint8(f)
}
