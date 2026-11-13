package screen

import (
	"errors"
	"fmt"
	"image"
	"strconv"

	"golang.org/x/image/font"
)

// CalibrationSteps is how finely the calibration screen measures a gray:
// its dither patches have k of every CalibrationSteps pixels black, for k
// from 1 to CalibrationSteps-1.
const CalibrationSteps = 16

// bayer4 is the 4x4 ordered-dither threshold matrix. A pixel is black in
// the patch for k when its threshold is below k, which makes exactly k of
// every 16 pixels black, spread as evenly as a 4x4 tile allows.
var bayer4 = [4][4]int{
	{0, 8, 2, 10},
	{12, 4, 14, 6},
	{3, 11, 1, 9},
	{15, 7, 13, 5},
}

// ditherBlack reports whether the pixel at x, y is black in the patch with
// k of every 16 dots black, where a dot is dot by dot pixels. The pattern is
// anchored to the screen, not the patch, so every patch shows the same tile.
func ditherBlack(k, dot, x, y int) bool {
	return bayer4[(y/dot)&3][(x/dot)&3] < k
}

// Calibration draws a screen for measuring how dark each of the panel's
// middle gray levels looks. Each row, labeled A, B and so on from darkest,
// is one level. Each box in a row has that level as a solid fill on top and
// a black-and-white dither with k of every 16 pixels black below, numbered
// k. Seen from far enough that the dither blends, the box whose halves
// match puts the level about k/16 of the way from white to black.
//
// dot is the side of a dither dot in pixels. E-ink can blur isolated
// single-pixel dots, which makes sparse patches look lighter than they are,
// so measuring with 1 and 2 shows whether that skews the result.
func Calibration(p Profile, dot int) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if dot < 1 || dot > 4 {
		return nil, fmt.Errorf("render calibration: bad dot size %d", dot)
	}
	rows := p.Levels - 2
	if rows < 1 {
		return nil, errors.New("render calibration: a 2-level panel has no grays to calibrate")
	}
	scale := float64(p.Height) / baseHeight
	faces := make([]font.Face, 4)
	defer closeFaces(faces)
	var err error
	for i, f := range []struct {
		bold bool
		size float64
	}{{true, 26}, {false, 15}, {true, 22}, {false, 13}} {
		if faces[i], err = newFace(f.bold, f.size*scale); err != nil {
			return nil, err
		}
	}
	title, body, rowLabel, number := faces[0], faces[1], faces[2], faces[3]

	img := blank(p)
	margin := p.Width / 20
	y := margin / 2
	for _, l := range []struct {
		face font.Face
		text string
	}{
		{title, "Gray calibration"},
		{body, "From arm's length, find the box in each row whose top and bottom look most alike."},
		{body, fmt.Sprintf("Its number is how many of every 16 dots are black in the bottom half. Dots are %dx%d pixels.", dot, dot)},
	} {
		y += lineHeight(l.face)
		drawText(img, l.face, l.text, p.Width/2, y-l.face.Metrics().Descent.Round())
	}

	// Lay out rows of patches: a column for the row label, then
	// CalibrationSteps-1 patches with a gap between them.
	const gapFrac = 0.15
	patches := CalibrationSteps - 1
	labelW := 2 * rowLabel.Metrics().Height.Ceil()
	contentW := p.Width - 2*margin - labelW
	pw := int(float64(contentW) / (float64(patches) + gapFrac*float64(patches-1)))
	gap := int(float64(pw) * gapFrac)
	// The patch numbers head the columns once, above the first row.
	numH := lineHeight(number)
	rowGap := max(pw/3, 2)
	avail := p.Height - y - margin/2 - numH
	ph := min(2*pw, avail/rows-rowGap)
	if pw < 4 || ph < 8 {
		return nil, fmt.Errorf("render calibration: %dx%d is too small for %d rows", p.Width, p.Height, rows)
	}
	ph &^= 1 // Even, so the halves are equal.
	rowH := ph + rowGap
	y += (avail - rows*rowH) / 2

	light := p.light()
	left := margin + labelW
	y += numH
	for k := 1; k <= patches; k++ {
		x := left + (k-1)*(pw+gap)
		drawText(img, number, strconv.Itoa(k), x+pw/2, y-rowGap-number.Metrics().Descent.Round())
	}
	for r := range rows {
		level := r + 1
		top := y + r*rowH
		labelY := top + ph/2 + rowLabel.Metrics().Ascent.Round()/2
		drawText(img, rowLabel, string(rune('A'+r)), margin+labelW/2, labelY)
		for k := 1; k <= patches; k++ {
			x := left + (k-1)*(pw+gap)
			fill(img, image.Rect(x, top, x+pw, top+ph/2), light, level)
			for py := top + ph/2; py < top+ph; py++ {
				for px := x; px < x+pw; px++ {
					if ditherBlack(k, dot, px, py) {
						img.Pix[img.PixOffset(px, py)] = 0xff
					}
				}
			}
		}
	}
	return encode(img, p)
}
