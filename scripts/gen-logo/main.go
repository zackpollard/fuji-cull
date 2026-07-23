// gen-logo renders the fuji-cull mark and emits every platform variant from
// one definition: an amber Mount Fuji with a cream snowcap on a charcoal
// photo tile, carried by the keep-green decision bar — the app's own tile
// grammar ("this one's a keeper") as the identity.
//
//	go run ./scripts/gen-logo
//
// Outputs:
//	assets/fuji-cull.png                     1024, rounded tile on transparency (desktop/dock)
//	assets/fuji-cull-square.png              1024, full-bleed opaque square (iOS, web, source feed)
//	ios/FujiCull/Assets.xcassets/AppIcon.appiconset/icon-1024.png
//	android mipmaps: legacy + adaptive foreground at all densities
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const hi = 4096 // render resolution; everything downsamples from here

type rgba = color.NRGBA

var (
	charcoal = rgba{11, 12, 11, 255}    // app background
	tileGrey = rgba{22, 24, 21, 255}    // photo tile
	amber    = rgba{255, 179, 46, 255}  // the app's accent
	cream    = rgba{245, 242, 233, 255} // snowcap
	green    = rgba{56, 214, 122, 255}  // keep decision bar
)

type poly [][2]float64

func (p poly) contains(x, y float64) bool {
	in := false
	for i, j := 0, len(p)-1; i < len(p); j, i = i, i+1 {
		xi, yi := p[i][0], p[i][1]
		xj, yj := p[j][0], p[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

// roundedRect signed test in unit coords.
func inRoundedRect(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Max(x0+r, math.Min(x, x1-r))
	cy := math.Max(y0+r, math.Min(y, y1-r))
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

// The mark, defined once in unit coordinates over the TILE (0..1).
var (
	// gently flared slopes, flattened crater — the Fuji profile
	mountain = poly{
		{-0.02, 0.885}, {0.10, 0.80}, {0.24, 0.62}, {0.40, 0.335},
		{0.435, 0.30}, {0.47, 0.325}, {0.50, 0.305}, {0.53, 0.325}, {0.565, 0.30},
		{0.60, 0.335}, {0.76, 0.62}, {0.90, 0.80}, {1.02, 0.885},
	}
	// snowcap: crest shared with the mountain, zigzag melt line below
	snow = poly{
		{0.395, 0.345}, {0.435, 0.30}, {0.47, 0.325}, {0.50, 0.305}, {0.53, 0.325}, {0.565, 0.30},
		{0.605, 0.345},
		{0.625, 0.375},
		{0.575, 0.475}, {0.535, 0.415}, {0.50, 0.49}, {0.465, 0.415}, {0.425, 0.475},
		{0.375, 0.375},
	}
)

// render draws the full-bleed square at hi resolution.
// tileInset: >0 shrinks the tile inside a transparent margin with the tile's
// own rounded corners (desktop); ==0 fills edge-to-edge on charcoal (iOS).
func render(tileInset float64, transparentOutside bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, hi, hi))
	t0, t1 := tileInset, 1-tileInset
	tw := t1 - t0
	corner := 0.11 * tw
	barTop := 0.895

	for py := 0; py < hi; py++ {
		for px := 0; px < hi; px++ {
			x := (float64(px) + 0.5) / hi
			y := (float64(py) + 0.5) / hi

			var c rgba
			switch {
			case !inRoundedRect(x, y, t0, t0, t1, t1, corner):
				if transparentOutside {
					c = rgba{0, 0, 0, 0}
				} else {
					c = charcoal
				}
			default:
				// tile-local coordinates
				tx := (x - t0) / tw
				ty := (y - t0) / tw
				switch {
				case ty >= barTop:
					c = green
				case snow.contains(tx, ty) && mountain.contains(tx, ty):
					c = cream
				case mountain.contains(tx, ty):
					c = amber
				default:
					c = tileGrey
				}
			}
			img.SetNRGBA(px, py, c)
		}
	}
	return img
}

// downsample box-filters hi-res to size (the supersampling).
func downsample(src *image.NRGBA, size int) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, size, size))
	f := hi / size
	for oy := 0; oy < size; oy++ {
		for ox := 0; ox < size; ox++ {
			var r, g, b, a, n uint64
			for sy := 0; sy < f; sy++ {
				for sx := 0; sx < f; sx++ {
					p := src.NRGBAAt(ox*f+sx, oy*f+sy)
					// premultiply so transparent margins don't darken edges
					r += uint64(p.R) * uint64(p.A)
					g += uint64(p.G) * uint64(p.A)
					b += uint64(p.B) * uint64(p.A)
					a += uint64(p.A)
					n++
				}
			}
			if a == 0 {
				out.SetNRGBA(ox, oy, rgba{0, 0, 0, 0})
				continue
			}
			out.SetNRGBA(ox, oy, rgba{
				uint8(r / a), uint8(g / a), uint8(b / a), uint8(a / n),
			})
		}
	}
	return out
}

func save(img image.Image, path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
	fmt.Println("wrote", path)
}

func main() {
	square := render(0, false)       // full-bleed: iOS icon, web, source feed
	tile := render(0.06, true)       // rounded tile on transparency: desktop
	adaptive := render(0.22, true)   // adaptive foreground: content in the 66% safe zone

	save(downsample(square, 1024), "assets/fuji-cull-square.png")
	save(downsample(tile, 1024), "assets/fuji-cull.png")
	save(downsample(square, 1024), "ios/FujiCull/Assets.xcassets/AppIcon.appiconset/icon-1024.png")

	// android: legacy launcher icons (full square; launchers mask them) and
	// adaptive foreground layers (108dp grid per density)
	legacy := map[string]int{"mdpi": 48, "hdpi": 72, "xhdpi": 96, "xxhdpi": 144, "xxxhdpi": 192}
	fg := map[string]int{"mdpi": 128, "hdpi": 128, "xhdpi": 256, "xxhdpi": 512, "xxxhdpi": 512}
	for d, sz := range legacy {
		save(downsample(square, sz), fmt.Sprintf("android/app/src/main/res/mipmap-%s/ic_launcher.png", d))
	}
	for d, sz := range fg {
		save(downsample(adaptive, sz), fmt.Sprintf("android/app/src/main/res/mipmap-%s/ic_launcher_foreground.png", d))
	}
}
