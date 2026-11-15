// Package profile resolves what a device's panel can show: its size, the
// palettes it takes, the largest image its firmware accepts, and how bright
// its gray levels measured. It reads a vendored copy of TRMNL's model and
// palette registry, so nothing is fetched per request.
package profile

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
)

//go:generate curl -fsS -A trmnl-byos-go -o registry/models.json https://trmnl.com/api/models
//go:generate curl -fsS -A trmnl-byos-go -o registry/palettes.json https://trmnl.com/api/palettes

//go:embed registry/models.json registry/palettes.json
var registryFS embed.FS

// Model is one entry in TRMNL's model registry, with the fields the server
// uses.
type Model struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Palettes []string `json:"palette_ids"`
	// DefaultPalette is the palette TRMNL renders with when nothing asks
	// for another.
	DefaultPalette string `json:"default_palette_id"`
	// ImageSizeLimit is the largest image, in bytes, the model accepts. 0
	// means the registry gives none.
	ImageSizeLimit int64 `json:"image_size_limit"`
}

// Palette is one entry in TRMNL's palette registry.
type Palette struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Grays is the number of gray levels for a gray palette, 0 for a color
	// one.
	Grays int `json:"grays"`
}

type registry struct {
	models   map[string]Model
	palettes map[string]Palette
	// paletteList is the palettes in the registry file's order.
	paletteList []Palette
}

// loadRegistry parses the embedded registry on first use. A broken embedded
// file is a build defect, so it panics rather than returning an error.
var loadRegistry = sync.OnceValue(func() *registry {
	var m struct {
		Data []Model `json:"data"`
	}
	decode("registry/models.json", &m)
	var p struct {
		Data []Palette `json:"data"`
	}
	decode("registry/palettes.json", &p)
	r := &registry{models: map[string]Model{}, palettes: map[string]Palette{}}
	for _, model := range m.Data {
		r.models[model.Name] = model
	}
	r.paletteList = p.Data
	for _, pal := range p.Data {
		r.palettes[pal.ID] = pal
	}
	return r
})

func decode(name string, v any) {
	f, err := registryFS.Open(name)
	if err != nil {
		panic(fmt.Sprintf("profile: open %s: %v", name, err))
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(v); err != nil {
		panic(fmt.Sprintf("profile: decode %s: %v", name, err))
	}
}

// firmwareModel is what the server knows about a firmware Model header value.
type firmwareModel struct {
	// registry is the TRMNL registry model the board is.
	registry string
	// imageSizeLimit, when not 0, replaces the registry's limit, for a build
	// known to accept more.
	imageSizeLimit int64
	// light is the measured light of each level of the board's richest gray
	// palette, darkest first, from 0 for black to 1 for white.
	light []float64
}

// firmwareModels maps the Model header the TRMNL firmware sends, its
// DEVICE_MODEL build setting, to what the server knows about the board. A
// Model not listed here or in the registry gets the fallback profile.
// Firmware 1.8.10 and older sends "og" for the 4-color OG too; a device
// override will cover that.
var firmwareModels = map[string]firmwareModel{
	"og":           {registry: "og_plus"},
	"og_4clr":      {registry: "og_bwry"},
	"og_gen2":      {registry: "og_plus"},
	"og_gen2_4clr": {registry: "og_bwry"},
	"x":            {registry: "v2"},
	// The TRMNL 7.5" OG DIY kit. Its build gets BOARD_HAS_PSRAM from the
	// XIAO ESP32-S3 board definition, so the firmware's MAX_IMAGE_SIZE is
	// 750000 rather than the OG's 90000. Its grays were measured on
	// firmware 1.8.10 with the 1x1-dot calibration screen at room
	// temperature: dark gray matched 14 of 16 black, light gray 10 of 16.
	"xiao_epaper_display": {registry: "og_plus", imageSizeLimit: 750000, light: []float64{0, 2.0 / 16, 6.0 / 16, 1}},
	"reterminal_e1001":    {registry: "seeed_e1001"},
	"reterminal_e1002":    {registry: "seeed_e1002"},
	"reterminal_e1003":    {registry: "seeed_e1003"},
	"reterminal_e1004":    {registry: "seeed_e1004"},
	"m5_papers3":          {registry: "m5_paper_s3"},
	"waveshare":           {registry: "waveshare_7_5_bw"},
}

// Fallback values for a device whose model is unknown: every panel shows
// black and white, and 90000 bytes is the smallest limit TRMNL firmware has.
const (
	FallbackWidth          = 800
	FallbackHeight         = 480
	FallbackImageSizeLimit = 90000
	maxDimension           = 4096
)

// Profile is what a device's panel can show.
type Profile struct {
	// Model is the TRMNL registry model, or "" when the device's model is
	// unknown.
	Model  string
	Width  int
	Height int
	// Palettes are the palette IDs the panel takes, as the registry lists
	// them.
	Palettes []string
	// DefaultPalette is the palette to render with when nothing asks for
	// another.
	DefaultPalette string
	// ImageSizeLimit is the largest image, in bytes, the device accepts. 0
	// means no limit is known.
	ImageSizeLimit int64
	// Light is the measured light of each level of GrayLevels, darkest
	// first, or nil when not measured.
	Light []float64
}

// Resolve returns the profile for a device by its firmware Model header,
// falling back to the Width and Height it reports, and then to 800x480.
// width and height are nil when the device did not send them.
func Resolve(fwModel string, width, height *int64) Profile {
	reg := loadRegistry()
	fm, known := firmwareModels[fwModel]
	if !known {
		fm = firmwareModel{registry: fwModel}
	}
	if m, ok := reg.models[fm.registry]; ok {
		p := Profile{
			Model:          m.Name,
			Width:          m.Width,
			Height:         m.Height,
			Palettes:       m.Palettes,
			DefaultPalette: m.DefaultPalette,
			ImageSizeLimit: m.ImageSizeLimit,
			Light:          fm.light,
		}
		if fm.imageSizeLimit != 0 {
			p.ImageSizeLimit = fm.imageSizeLimit
		}
		return p
	}
	p := Profile{
		Width:          FallbackWidth,
		Height:         FallbackHeight,
		Palettes:       []string{"bw"},
		DefaultPalette: "bw",
		ImageSizeLimit: FallbackImageSizeLimit,
	}
	if width != nil && height != nil && *width > 0 && *height > 0 && *width <= maxDimension && *height <= maxDimension {
		p.Width, p.Height = int(*width), int(*height)
	}
	return p
}

// GrayLevels is the number of grays in the richest gray palette the panel
// takes, which system screens use to show what the panel can do. A panel
// with only color palettes gets 2: black and white are in all of them.
func (p Profile) GrayLevels() int {
	reg := loadRegistry()
	levels := 2
	for _, id := range p.Palettes {
		if g := reg.palettes[id].Grays; g > levels {
			levels = g
		}
	}
	return levels
}

// Fits reports whether an image of size bytes is one the device accepts.
func (p Profile) Fits(size int64) bool {
	return p.ImageSizeLimit == 0 || size <= p.ImageSizeLimit
}

// RenderPalette is the palette a render for the device uses: want, the
// palette a plugin instance asks for, when the panel takes it, and the
// panel's default otherwise.
func (p Profile) RenderPalette(want string) string {
	if want != "" && slices.Contains(p.Palettes, want) {
		return want
	}
	return p.DefaultPalette
}

// KnownPalette reports whether id is a palette in TRMNL's registry.
func KnownPalette(id string) bool {
	_, ok := loadRegistry().palettes[id]
	return ok
}

// Palettes returns every palette in TRMNL's registry, in the order of the
// registry file. The caller must not change the result.
func Palettes() []Palette {
	return loadRegistry().paletteList
}
