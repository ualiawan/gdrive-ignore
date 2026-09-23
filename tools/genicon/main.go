// Command genicon renders the app icon into internal/assets (ICO + PNG).
//
//	go run ./tools/genicon
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

var sizes = []int{16, 20, 24, 32, 40, 48, 64, 256}

func main() {
	var pngs [][]byte
	for _, s := range sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, render(s)); err != nil {
			log.Fatal(err)
		}
		pngs = append(pngs, buf.Bytes())
	}
	out := filepath.Join("internal", "assets")
	if err := os.MkdirAll(out, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "icon.ico"), ico(pngs), 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "icon.png"), pngs[len(pngs)-1], 0o644); err != nil {
		log.Fatal(err)
	}
}

// ico packs PNG images into an .ico container (PNG entries are valid since Vista).
func ico(pngs [][]byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, uint16(len(pngs))})
	offset := 6 + 16*len(pngs)
	for i, p := range pngs {
		s := sizes[i]
		dim := byte(s)
		if s >= 256 {
			dim = 0
		}
		b.Write([]byte{dim, dim, 0, 0})
		binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})
		binary.Write(&b, binary.LittleEndian, [2]uint32{uint32(len(p)), uint32(offset)})
		offset += len(p)
	}
	for _, p := range pngs {
		b.Write(p)
	}
	return b.Bytes()
}

type rgba struct{ r, g, b, a float64 }

var (
	blue  = rgba{0x1a, 0x73, 0xe8, 1}
	white = rgba{0xff, 0xff, 0xff, 1}
	red   = rgba{0xea, 0x43, 0x35, 1}
)

// render draws the icon at size s with 4x4 supersampling.
func render(s int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, s, s))
	const ss = 4
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			var acc rgba
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u := (float64(x) + (float64(sx)+0.5)/ss) / float64(s)
					v := (float64(y) + (float64(sy)+0.5)/ss) / float64(s)
					c := shade(u, v, float64(s))
					acc.r += c.r * c.a
					acc.g += c.g * c.a
					acc.b += c.b * c.a
					acc.a += c.a
				}
			}
			n := float64(ss * ss)
			if acc.a == 0 {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(acc.r / acc.a), G: uint8(acc.g / acc.a), B: uint8(acc.b / acc.a),
				A: uint8(math.Round(acc.a / n * 255)),
			})
		}
	}
	return img
}

// shade returns the color at (u, v) in [0,1]^2.
func shade(u, v, px float64) rgba {
	// Rounded square background.
	r := 0.22
	if !inRoundRect(u, v, 0.02, 0.02, 0.98, 0.98, r) {
		return rgba{}
	}
	c := blue
	// White triangle outline (Drive-like), thicker at small sizes.
	stroke := math.Max(0.075, 1.6/px)
	ax, ay := 0.5, 0.2
	bx, by := 0.82, 0.76
	cx, cy := 0.18, 0.76
	d := math.Min(segDist(u, v, ax, ay, bx, by), math.Min(segDist(u, v, bx, by, cx, cy), segDist(u, v, cx, cy, ax, ay)))
	if d < stroke/2 {
		c = white
	}
	// Red slash: "not synced".
	slash := math.Max(0.1, 2.2/px)
	if segDist(u, v, 0.26, 0.84, 0.8, 0.22) < slash/2 {
		c = red
	}
	return c
}

func inRoundRect(u, v, x0, y0, x1, y1, r float64) bool {
	qx := math.Max(math.Max(x0+r-u, u-(x1-r)), 0)
	qy := math.Max(math.Max(y0+r-v, v-(y1-r)), 0)
	return qx*qx+qy*qy <= r*r && u >= x0 && u <= x1 && v >= y0 && v <= y1
}

func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	ex, ey := ax+t*dx-px, ay+t*dy-py
	return math.Sqrt(ex*ex + ey*ey)
}
