// Package screen draws the server's own system screens, such as the welcome
// and "not registered" screens, as PNGs a TRMNL panel can show.
package screen

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// baseHeight is the panel height that Line sizes are given for. Text scales
// with the panel's height from there.
const baseHeight = 480

// Profile is the size and gray depth of the panel a screen is drawn for.
type Profile struct {
	Width  int
	Height int
	// Levels is the number of evenly spaced grays the panel shows: 2, 4 or
	// 16. The PNG is written at the smallest bit depth that holds them. A
	// TRMNL OG shows grays only from a 2-bit PNG with more than 2 colors.
	Levels int
}

// Line is one line of centered text.
type Line struct {
	Text string
	// Size is the font size in pixels on a 480-pixel-high panel.
	Size float64
	Bold bool
}

var fonts = sync.OnceValues(func() ([2]*opentype.Font, error) {
	regular, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return [2]*opentype.Font{}, fmt.Errorf("parse regular font: %w", err)
	}
	bold, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return [2]*opentype.Font{}, fmt.Errorf("parse bold font: %w", err)
	}
	return [2]*opentype.Font{regular, bold}, nil
})

// Render draws lines centered on a white screen and encodes it as a PNG.
// Text is anti-aliased, then reduced to p.Levels grays without dithering, so
// glyph edges become the intermediate grays.
func Render(p Profile, lines []Line) ([]byte, error) {
	if p.Width <= 0 || p.Height <= 0 {
		return nil, fmt.Errorf("render screen: bad size %dx%d", p.Width, p.Height)
	}
	if p.Levels < 2 || p.Levels > 256 {
		return nil, fmt.Errorf("render screen: bad gray levels %d", p.Levels)
	}
	ff, err := fonts()
	if err != nil {
		return nil, err
	}

	scale := float64(p.Height) / baseHeight
	faces := make([]font.Face, len(lines))
	defer func() {
		for _, f := range faces {
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	total := 0
	for i, l := range lines {
		ttf := ff[0]
		if l.Bold {
			ttf = ff[1]
		}
		face, err := opentype.NewFace(ttf, &opentype.FaceOptions{Size: l.Size * scale, DPI: 72, Hinting: font.HintingFull})
		if err != nil {
			return nil, fmt.Errorf("render screen: font face: %w", err)
		}
		faces[i] = face
		total += lineHeight(face)
	}

	img := image.NewGray(image.Rect(0, 0, p.Width, p.Height))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	y := (p.Height - total) / 2
	for i, l := range lines {
		face := faces[i]
		d := font.Drawer{Dst: img, Src: image.Black, Face: face}
		width := d.MeasureString(l.Text).Round()
		y += lineHeight(face)
		d.Dot = fixed.P((p.Width-width)/2, y-face.Metrics().Descent.Round())
		d.DrawString(l.Text)
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, quantize(img, p.Levels)); err != nil {
		return nil, fmt.Errorf("render screen: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// lineHeight is a line's height with some space between lines.
func lineHeight(face font.Face) int {
	return int(math.Ceil(float64(face.Metrics().Height.Ceil()) * 1.25))
}

// quantize maps each pixel to the nearest of n evenly spaced grays, darkest
// first. Go's PNG encoder writes an *image.Paletted at the smallest bit depth
// that holds its palette.
func quantize(g *image.Gray, n int) *image.Paletted {
	step := 255.0 / float64(n-1)
	pal := make(color.Palette, n)
	for i := range pal {
		pal[i] = color.Gray{Y: uint8(math.Round(float64(i) * step))}
	}
	p := image.NewPaletted(g.Bounds(), pal)
	for i, v := range g.Pix {
		p.Pix[i] = uint8(float64(v)/step + 0.5)
	}
	return p
}
