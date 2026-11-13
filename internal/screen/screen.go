// Package screen draws the server's own system screens, such as the welcome
// and "not registered" screens, as PNGs a TRMNL panel can show.
//
// Screens are drawn as ink coverage, not gray values: each pixel holds how
// much of it is covered by black, which for an anti-aliased glyph edge is
// the fraction of the pixel the glyph covers. Coverage is proportional to
// light, so the encoder can map each pixel to the panel level whose measured
// brightness is nearest the light the pixel should give off.
package screen

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
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
	// Levels is the number of grays the panel shows: 2, 4 or 16. The PNG is
	// written at the smallest bit depth that holds them. A TRMNL OG shows
	// grays only from a 2-bit PNG with more than 2 colors.
	Levels int
	// Light is how bright each level looks, darkest first, as a fraction
	// of the way from the panel's black (0) to its white (1), measured with
	// the calibration screen. Nil assumes the levels are evenly spaced in
	// sRGB values, as TRMNL's palettes are.
	Light []float64
}

// Line is one centered line: text, or a row of gray swatches.
type Line struct {
	Text string
	// Size is the font size, or the swatch size, in pixels on a
	// 480-pixel-high panel.
	Size float64
	Bold bool
	// Swatches draws one outlined box per gray level the panel shows,
	// darkest to lightest, in place of text. A panel that shows them all as
	// distinct shades is in its gray mode.
	Swatches bool
}

// swatchGap is the space between swatches, as a fraction of their size.
const swatchGap = 0.25

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

// Render draws lines centered on a white screen and writes it to w as a
// PNG. Text is anti-aliased, and each edge pixel becomes the level nearest
// the light it should give off, without dithering.
func Render(w io.Writer, p Profile, lines []Line) error {
	if err := p.validate(); err != nil {
		return err
	}
	scale := float64(p.Height) / baseHeight
	faces := make([]font.Face, len(lines))
	defer closeFaces(faces)
	total := 0
	for i, l := range lines {
		if l.Swatches {
			total += swatchLineHeight(swatchSize(p, l, scale))
			continue
		}
		face, err := newFace(l.Bold, l.Size*scale)
		if err != nil {
			return err
		}
		faces[i] = face
		total += lineHeight(face)
	}

	img := blank(p)
	y := (p.Height - total) / 2
	for i, l := range lines {
		if l.Swatches {
			size := swatchSize(p, l, scale)
			drawSwatches(img, p, size, y)
			y += swatchLineHeight(size)
			continue
		}
		y += lineHeight(faces[i])
		drawText(img, faces[i], l.Text, p.Width/2, y-faces[i].Metrics().Descent.Round())
	}
	return encode(w, img, p)
}

func (p Profile) validate() error {
	if p.Width <= 0 || p.Height <= 0 {
		return fmt.Errorf("render screen: bad size %dx%d", p.Width, p.Height)
	}
	if p.Levels < 2 || p.Levels > 256 {
		return fmt.Errorf("render screen: bad gray levels %d", p.Levels)
	}
	if p.Light == nil {
		return nil
	}
	if len(p.Light) != p.Levels {
		return fmt.Errorf("render screen: %d light values for %d levels", len(p.Light), p.Levels)
	}
	for i, l := range p.Light {
		if l < 0 || l > 1 || (i > 0 && l <= p.Light[i-1]) {
			return errors.New("render screen: light values must rise from 0 to 1")
		}
	}
	return nil
}

// light returns p.Light, or the light of evenly spaced sRGB values.
func (p Profile) light() []float64 {
	if p.Light != nil {
		return p.Light
	}
	l := make([]float64, p.Levels)
	for i := range l {
		l[i] = srgbToLinear(float64(i) / float64(p.Levels-1))
	}
	return l
}

// srgbToLinear converts an sRGB value in [0, 1] to linear light.
func srgbToLinear(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

// newFace returns a Go font face of the given pixel size.
func newFace(bold bool, size float64) (font.Face, error) {
	ff, err := fonts()
	if err != nil {
		return nil, err
	}
	ttf := ff[0]
	if bold {
		ttf = ff[1]
	}
	face, err := opentype.NewFace(ttf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, fmt.Errorf("render screen: font face: %w", err)
	}
	return face, nil
}

func closeFaces(faces []font.Face) {
	for _, f := range faces {
		if f != nil {
			_ = f.Close()
		}
	}
}

// blank returns an uncovered, white canvas the panel's size. A canvas pixel
// is ink coverage: 0 is white and 255 is solid black.
func blank(p Profile) *image.Alpha {
	return image.NewAlpha(image.Rect(0, 0, p.Width, p.Height))
}

// drawText draws text in black, centered on x, with its baseline at y.
// Glyph coverage composites as coverage, so overlapping edges add up the
// way their light would.
func drawText(img *image.Alpha, face font.Face, text string, x, y int) {
	d := font.Drawer{Dst: img, Src: image.Opaque, Face: face}
	d.Dot = fixed.P(x-d.MeasureString(text).Round()/2, y)
	d.DrawString(text)
}

// levelInk is the coverage that gives exactly the light of level i, so a
// fill with it encodes as that level.
func levelInk(light []float64, i int) color.Alpha {
	return color.Alpha{A: uint8(math.Round((1 - light[i]) * 255))}
}

// fill covers r with level i.
func fill(img *image.Alpha, r image.Rectangle, light []float64, i int) {
	draw.Draw(img, r, image.NewUniform(levelInk(light, i)), image.Point{}, draw.Src)
}

// encode reduces img to the panel's levels and writes it to w as a PNG.
func encode(w io.Writer, img *image.Alpha, p Profile) error {
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(w, quantize(img, p)); err != nil {
		return fmt.Errorf("render screen: encode: %w", err)
	}
	return nil
}

// levelGray is the palette entry for level i of n, darkest first. The
// firmware reads only the order: it maps a 2-bit palette's grays to its
// levels by their top bits, so these stay evenly spaced whatever the
// measured light.
func levelGray(i, n int) color.Gray {
	return color.Gray{Y: uint8(math.Round(float64(i) * 255 / float64(n-1)))}
}

// swatchSize is the side of one swatch in pixels: the line's size scaled to
// the panel, shrunk if needed so the row fits in 90% of the panel's width.
func swatchSize(p Profile, l Line, scale float64) int {
	size := l.Size * scale
	if fit := 0.9 * float64(p.Width) / (float64(p.Levels) + swatchGap*float64(p.Levels-1)); size > fit {
		size = fit
	}
	return max(int(size), 3)
}

func swatchLineHeight(size int) int {
	return int(math.Ceil(float64(size) * 1.25))
}

// drawSwatches draws a row of boxes, one filled with each of the panel's
// levels and outlined in black so white shows too. top is the top of the
// line.
func drawSwatches(img *image.Alpha, p Profile, size, top int) {
	gap := int(float64(size) * swatchGap)
	width := p.Levels*size + (p.Levels-1)*gap
	x := (img.Bounds().Dx() - width) / 2
	y := top + (swatchLineHeight(size)-size)/2
	border := max(size/20, 1)
	light := p.light()
	for i := range p.Levels {
		box := image.Rect(x, y, x+size, y+size)
		draw.Draw(img, box, image.Opaque, image.Point{}, draw.Src)
		fill(img, box.Inset(border), light, i)
		x += size + gap
	}
}

// lineHeight is a line's height with some space between lines.
func lineHeight(face font.Face) int {
	return int(math.Ceil(float64(face.Metrics().Height.Ceil()) * 1.25))
}

// quantize maps each pixel's coverage to the level nearest in light: a
// pixel with coverage c should give off 1-c of white's light. Go's PNG
// encoder writes an *image.Paletted at the smallest bit depth that holds
// its palette.
func quantize(img *image.Alpha, p Profile) *image.Paletted {
	light := p.light()
	pal := make(color.Palette, p.Levels)
	for i := range pal {
		pal[i] = levelGray(i, p.Levels)
	}
	var lut [256]uint8
	for v := range lut {
		want := 1 - float64(v)/255
		// A uint8 index, since a palette index is one byte; validate keeps
		// light to at most 256 levels.
		var best uint8
		for i := uint8(1); int(i) < len(light); i++ {
			if math.Abs(light[i]-want) < math.Abs(light[best]-want) {
				best = i
			}
			if i == math.MaxUint8 {
				break
			}
		}
		lut[v] = best
	}
	out := image.NewPaletted(img.Bounds(), pal)
	for i, v := range img.Pix {
		out.Pix[i] = lut[v]
	}
	return out
}
