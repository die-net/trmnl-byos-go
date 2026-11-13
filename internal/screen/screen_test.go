package screen

import (
	"bytes"
	"image"
	"image/png"
	"math"
	"testing"
)

// renderPNG renders into memory, which a test may do.
func renderPNG(p Profile, lines []Line) ([]byte, error) {
	var buf bytes.Buffer
	err := Render(&buf, p, lines)
	return buf.Bytes(), err
}

func calibrationPNG(p Profile, dot int) ([]byte, error) {
	var buf bytes.Buffer
	err := Calibration(&buf, p, dot)
	return buf.Bytes(), err
}

// TestRenderBitDepth checks that a screen comes out at the panel's size and
// at the smallest PNG bit depth for its gray levels, and that a 4-level
// screen has more than 2 colors, which is what puts an OG in gray mode.
func TestRenderBitDepth(t *testing.T) {
	lines := []Line{{Text: "Connected", Size: 44, Bold: true}, {Text: "small anti-aliased text", Size: 14}}
	for _, tc := range []struct {
		levels, depth int
	}{
		{2, 1},
		{4, 2},
		{16, 4},
	} {
		data, err := renderPNG(Profile{Width: 800, Height: 480, Levels: tc.levels}, lines)
		if err != nil {
			t.Fatalf("levels %d: %v", tc.levels, err)
		}
		// IHDR: bit depth at byte 24, color type at byte 25.
		if got := int(data[24]); got != tc.depth {
			t.Errorf("levels %d: bit depth %d, want %d", tc.levels, got, tc.depth)
		}
		if data[25] != 3 {
			t.Errorf("levels %d: color type %d, want 3 (indexed)", tc.levels, data[25])
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != 800 || b.Dy() != 480 {
			t.Errorf("levels %d: size %v", tc.levels, b)
		}
		used := map[uint8]bool{}
		for _, ix := range img.(*image.Paletted).Pix {
			used[ix] = true
		}
		if tc.levels > 2 && len(used) <= 2 {
			t.Errorf("levels %d: only %d colors used", tc.levels, len(used))
		}
	}
}

// TestRenderScales checks that a TRMNL X-sized screen renders.
func TestRenderScales(t *testing.T) {
	if _, err := renderPNG(Profile{Width: 1872, Height: 1404, Levels: 16}, []Line{{Text: "Hello", Size: 44}}); err != nil {
		t.Fatal(err)
	}
}

func TestRenderRejects(t *testing.T) {
	for _, p := range []Profile{
		{Width: 0, Height: 480, Levels: 4},
		{Width: 800, Height: -1, Levels: 4},
		{Width: 800, Height: 480, Levels: 1},
		{Width: 800, Height: 480, Levels: 257},
	} {
		if _, err := renderPNG(p, nil); err == nil {
			t.Errorf("Render(%+v) succeeded", p)
		}
	}
}

// TestSwatches checks that a swatch row shows every gray level the panel
// has, and fits on panels from the OG to the TRMNL X.
func TestSwatches(t *testing.T) {
	for _, p := range []Profile{
		{Width: 800, Height: 480, Levels: 2},
		{Width: 800, Height: 480, Levels: 4},
		{Width: 800, Height: 480, Levels: 16},
		{Width: 480, Height: 800, Levels: 16},
		{Width: 1872, Height: 1404, Levels: 16},
	} {
		data, err := renderPNG(p, []Line{{Swatches: true, Size: 36}})
		if err == nil && p.Levels == 4 {
			p.Light = ogLight
			data, err = renderPNG(p, []Line{{Swatches: true, Size: 36}})
		}
		if err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		pix := img.(*image.Paletted)
		used := map[uint8]bool{}
		for _, ix := range pix.Pix {
			used[ix] = true
		}
		if len(used) != p.Levels {
			t.Errorf("%+v: %d levels used, want %d", p, len(used), p.Levels)
		}
		// The outermost columns stay white, so no swatch was cut off.
		b := pix.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			white := p.Levels - 1
			if int(pix.ColorIndexAt(b.Min.X, y)) != white || int(pix.ColorIndexAt(b.Max.X-1, y)) != white {
				t.Fatalf("%+v: swatches reach the edge at y=%d", p, y)
			}
		}
	}
}

// TestDitherBlack checks that the patch for k has exactly k of every 16
// dots black in any 4x4-dot tile, for each dot size.
func TestDitherBlack(t *testing.T) {
	for _, dot := range []int{1, 2} {
		for k := range CalibrationSteps + 1 {
			for _, origin := range [][2]int{{0, 0}, {5, 7}} {
				black := 0
				for y := origin[1]; y < origin[1]+4*dot; y++ {
					for x := origin[0]; x < origin[0]+4*dot; x++ {
						if ditherBlack(k, dot, x, y) {
							black++
						}
					}
				}
				if black != k*dot*dot {
					t.Errorf("dot %d, k=%d at %v: %d black pixels, want %d", dot, k, origin, black, k*dot*dot)
				}
			}
		}
	}
}

// TestCalibration checks that the calibration screen uses every gray level
// on panels with grays, and that a 2-level panel is refused.
func TestCalibration(t *testing.T) {
	for _, p := range []Profile{
		{Width: 800, Height: 480, Levels: 4},
		{Width: 480, Height: 800, Levels: 4},
		{Width: 1872, Height: 1404, Levels: 16},
	} {
		data, err := calibrationPNG(p, 2)
		if err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		used := map[uint8]bool{}
		for _, ix := range img.(*image.Paletted).Pix {
			used[ix] = true
		}
		if len(used) != p.Levels {
			t.Errorf("%+v: %d levels used, want %d", p, len(used), p.Levels)
		}
	}
	if _, err := calibrationPNG(Profile{Width: 800, Height: 480, Levels: 2}, 1); err == nil {
		t.Error("calibrated a 2-level panel")
	}
	if _, err := calibrationPNG(Profile{Width: 800, Height: 480, Levels: 4}, 0); err == nil {
		t.Error("accepted dot size 0")
	}
}

// ogLight is the TRMNL OG DIY kit's measured light per level.
var ogLight = []float64{0, 2.0 / 16, 6.0 / 16, 1}

// TestQuantizeByLight checks that coverage maps to the level nearest in
// light, with measured levels and with the evenly spaced sRGB default.
func TestQuantizeByLight(t *testing.T) {
	for _, tc := range []struct {
		name  string
		light []float64
		cover float64
		want  uint8
	}{
		// OG cutoffs fall halfway between levels' light: 1/16, 4/16, 11/16.
		{"og solid", ogLight, 1, 0},
		{"og 95%", ogLight, 0.95, 0},
		{"og 90%", ogLight, 0.90, 1},
		{"og 76%", ogLight, 0.76, 1},
		{"og 74%", ogLight, 0.74, 2},
		{"og 32%", ogLight, 0.32, 2},
		{"og 30%", ogLight, 0.30, 3},
		{"og none", ogLight, 0, 3},
		// sRGB #555 and #aaa give off 0.091 and 0.402 of white's light.
		{"default 50%", nil, 0.50, 2},
		{"default 80%", nil, 0.80, 1},
	} {
		p := Profile{Width: 1, Height: 1, Levels: 4, Light: tc.light}
		img := blank(p)
		img.Pix[0] = uint8(math.Round(tc.cover * 255))
		if got := quantize(img, p).Pix[0]; got != tc.want {
			t.Errorf("%s: level %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestLevelFillsExact checks that filling with a level's ink encodes as that
// level, for measured and default light.
func TestLevelFillsExact(t *testing.T) {
	for _, light := range [][]float64{ogLight, nil} {
		p := Profile{Width: 4, Height: 1, Levels: 4, Light: light}
		img := blank(p)
		for i := range 4 {
			fill(img, image.Rect(i, 0, i+1, 1), p.light(), i)
		}
		q := quantize(img, p)
		for i := range 4 {
			if int(q.Pix[i]) != i {
				t.Errorf("light %v: level %d encodes as %d", light, i, q.Pix[i])
			}
		}
	}
}

func TestProfileLightRejects(t *testing.T) {
	for _, light := range [][]float64{
		{0, 1},              // wrong length
		{0, 0.5, 0.4, 1},    // not rising
		{0, 0.2, 0.5, 1.1},  // above 1
		{-0.1, 0.2, 0.5, 1}, // below 0
	} {
		if _, err := renderPNG(Profile{Width: 8, Height: 8, Levels: 4, Light: light}, nil); err == nil {
			t.Errorf("accepted light %v", light)
		}
	}
}
