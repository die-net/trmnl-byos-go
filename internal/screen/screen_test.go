package screen

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

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
		data, err := Render(Profile{Width: 800, Height: 480, Levels: tc.levels}, lines)
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
	if _, err := Render(Profile{Width: 1872, Height: 1404, Levels: 16}, []Line{{Text: "Hello", Size: 44}}); err != nil {
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
		if _, err := Render(p, nil); err == nil {
			t.Errorf("Render(%+v) succeeded", p)
		}
	}
}
