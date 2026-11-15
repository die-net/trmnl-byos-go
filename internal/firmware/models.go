package firmware

import (
	"fmt"
	"strconv"
	"strings"
)

// Chip is an ESP32 family, as the chip_id in an app image's header names it.
type Chip struct {
	// Name is the family's name, such as esp32s3.
	Name string
	// ID is the chip_id value, from ESP-IDF's esp_chip_id_t.
	ID uint16
}

// The chips TRMNL firmware is built for.
var (
	ChipESP32C3 = Chip{"esp32c3", 0x0005}
	ChipESP32S3 = Chip{"esp32s3", 0x0009}
	ChipESP32C5 = Chip{"esp32c5", 0x0017}
)

// chipName names a chip_id for a message, or shows the number.
func chipName(id uint16) string {
	for _, c := range []Chip{ChipESP32C3, ChipESP32S3, ChipESP32C5} {
		if c.ID == id {
			return c.Name
		}
	}
	return fmt.Sprintf("an unknown chip (id 0x%04X)", id)
}

// Model is what the server knows about the build for one firmware model.
type Model struct {
	// Chip is the chip the model's board has.
	Chip Chip
	// MaxBytes is the size of the board's app partition, the largest image
	// it can install.
	MaxBytes int64
}

// App partition sizes, from the partition tables in the firmware's boards
// directory: min_spiffs.csv, trmnl_gen2_partitions.csv and
// trmnl_x_partitions.csv.
const (
	partMinSPIFFS = 0x1D0000
	partGen2      = 0x280000
	partX         = 0x300000
)

// minReportedVersion is the oldest firmware version whose Model header
// names one build. Before 1.8.17 the firmware took the model from a board
// macro that two builds shared: "og" was the OG and the 4-color OG,
// "xiao_epaper_display" was the DIY kit and its 3-color variant, and "x"
// was two TRMNL X boards. A device that old may be a different board
// than its Model says, so it gets no offer.
var minReportedVersion = Version{1, 8, 17}

// Models maps the Model header the firmware sends, its DEVICE_MODEL build
// setting, to its chip and partition size. A model that is not here
// cannot have firmware added or offered.
//
// It was built from usetrmnl/trmnl-firmware at commit 744a955 (platformio.ini
// and boards/): each model's PlatformIO environment gives its board, which
// gives the chip, and its partition table. A model is left out when its
// environments disagree or the table cannot be read from the source:
//
//   - xteink_x4: two builds, the second for a push-to-refresh power button.
//   - trmnl_steam: a development build that waits for a serial console.
//   - crowpanel42 and m5_paper_mono: no partition table is set, so the board's
//     default applies, which the source does not say.
//   - sensoria_s3 and the generic seeed_esp32c3, seeed_esp32s3 and waveshare
//     names: no current environment sets them.
//   - A model that older firmware named differently, such as m5_papers3, which
//     1.8.16 called paper_s3, simply never matches an older device.
var Models = map[string]Model{
	"og":           {ChipESP32C3, partMinSPIFFS},
	"og_4clr":      {ChipESP32C3, partMinSPIFFS},
	"xteink_x3":    {ChipESP32C3, partMinSPIFFS},
	"og_gen2":      {ChipESP32C5, partGen2},
	"og_gen2_4clr": {ChipESP32C5, partX},

	"x":                {ChipESP32S3, partX},
	"lilygo_t5pro":     {ChipESP32S3, partX},
	"m5_papers3":       {ChipESP32S3, partX},
	"m5_paper_color":   {ChipESP32S3, partX},
	"seeed_sticky":     {ChipESP32S3, partX},
	"reterminal_e1003": {ChipESP32S3, partX},
	"reterminal_e1004": {ChipESP32S3, partX},

	"reterminal_e1001":    {ChipESP32S3, partMinSPIFFS},
	"reterminal_e1002":    {ChipESP32S3, partMinSPIFFS},
	"waveshare_397":       {ChipESP32S3, partMinSPIFFS},
	"xiao_epaper_display": {ChipESP32S3, partMinSPIFFS},
	"xiao_epaper_3clr":    {ChipESP32S3, partMinSPIFFS},
	"xiao_epaper_6clr":    {ChipESP32S3, partMinSPIFFS},
	"xiao_epaper_mini":    {ChipESP32S3, partMinSPIFFS},
}

// MaxUploadBytes is the largest app partition of any model, which bounds an
// upload before its model's own limit applies.
func MaxUploadBytes() int64 {
	var m int64
	for _, model := range Models {
		m = max(m, model.MaxBytes)
	}
	return m
}

// ByteSize describes a size in bytes, such as "1.2 MB".
func ByteSize(n int64) string {
	unit := func(v float64, name string) string {
		return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + " " + name
	}
	switch {
	case n >= 1<<20:
		return unit(float64(n)/(1<<20), "MB")
	case n >= 1<<10:
		return unit(float64(n)/(1<<10), "KB")
	}
	return strconv.FormatInt(n, 10) + " bytes"
}
