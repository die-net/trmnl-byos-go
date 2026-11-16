package adminui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/deviceapi"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
)

const (
	fwModel  = "xiao_epaper_display"
	fwChipS3 = 0x0009
	fwChipC3 = 0x0005
	canaryID = "ABC123"
)

// testImage builds a small ESP app image for the chip ID, with the given
// descriptor version, laid out as esp_image_format.h says: a 24-byte
// header, one segment that starts with the 256-byte app descriptor, padding
// with the checksum in its last byte, and an appended SHA-256.
func testImage(chip uint16, descVersion string) []byte {
	const dataLen = 256 + 64
	out := make([]byte, 24+8+dataLen)
	out[0], out[1], out[23] = 0xE9, 1, 1
	binary.LittleEndian.PutUint16(out[12:], chip)
	binary.LittleEndian.PutUint32(out[28:], dataLen)
	data := out[32:]
	binary.LittleEndian.PutUint32(data, 0xABCD5432)
	copy(data[16:], descVersion)
	copy(data[48:], "trmnl-firmware")
	copy(data[112:], "v5.3.2")
	sum := byte(0xEF)
	for _, b := range data {
		sum ^= b
	}
	pad := make([]byte, (len(out)+16)&^15-len(out))
	pad[len(pad)-1] = sum
	padded := slices.Concat(out, pad)
	h := sha256.Sum256(padded)
	return slices.Concat(padded, h[:])
}

// newFirmwareUI returns a test server that adds builds to its own firmware
// directory, which it also returns. The directory is not made until a
// build is added.
func newFirmwareUI(t *testing.T) (*testUI, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "firmware")
	return newTestUI(t, Config{FirmwareDir: dir}, true), dir
}

// addFirmware adds a good build of the model at version 1.8.19 and returns it.
func addFirmware(t *testing.T, tu *testUI, dir, model string) db.Firmware {
	t.Helper()
	fw, err := firmware.Add(t.Context(), tu.db, dir, model, "1.8.19", bytes.NewReader(testImage(chipFor(model), "1")), tu.now)
	if err != nil {
		t.Fatal(err)
	}
	return fw
}

// chipFor returns the chip ID of the model's chip.
func chipFor(model string) uint16 {
	return firmware.Models[model].Chip.ID
}

// addFirmwareDevice makes a device that reports the model and version and has
// updates on if updates is set, and returns its ID.
func addFirmwareDevice(t *testing.T, tu *testUI, friendly, name, model, version string, updates bool) int64 {
	t.Helper()
	dev, err := tu.db.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: "mac-" + friendly, ApiKeyHash: []byte(friendly), FriendlyID: friendly,
		Name: name, FwModel: model, FwVersion: version, CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := device.Update(t.Context(), tu.db, friendly, device.Change{UpdatesEnabled: &updates}); err != nil {
		t.Fatal(err)
	}
	return dev.ID
}

// firmwareFiles lists the names in the firmware directory.
func firmwareFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// listFirmware returns every build with its rollout.
func listFirmware(t *testing.T, tu *testUI) []firmware.Entry {
	t.Helper()
	rows, err := firmware.List(t.Context(), tu.db.Read)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// uploadFirmware posts the parts to /firmware signed in.
func (tu *testUI) uploadFirmware(t *testing.T, c *http.Cookie, parts ...formPart) (status int, location, body string) {
	t.Helper()
	b, ct := multipartBody(t, parts...)
	headers := []string{"Content-Type", ct}
	if c != nil {
		headers = append(headers, "Cookie", cookieHeader(c))
	}
	w := tu.do(t, "POST", "/firmware", b, headers...)
	return w.Code, w.Header().Get("Location"), w.Body.String()
}

// goodUpload is the parts a browser sends for a good build.
func goodUpload() []formPart {
	return []formPart{
		{field: "model", content: fwModel},
		{field: "version", content: "1.8.19"},
		{field: "firmware", filename: "trmnl-1.8.19.bin", content: string(testImage(fwChipS3, "1"))},
	}
}

func TestFirmwareEmpty(t *testing.T) {
	t.Parallel()
	tu, _ := newFirmwareUI(t)
	body := tu.get(t, tu.signIn(t), "/firmware").Body.String()
	for _, want := range []string{
		"<h1>Firmware</h1>", `href="#upload">Upload a build</a>`, "<h2>No firmware yet</h2>", "Upload a firmware build below.",
		`<form method="post" action="/firmware#upload" enctype="multipart/form-data">`,
		`<select id="model" name="model" required>`, `<option value="">Choose a model</option>`,
		`<option value="og_gen2">og_gen2</option>`, `<option value="xiao_epaper_display">xiao_epaper_display</option>`,
		`<input id="version" name="version" type="text"`, "Dotted numbers such as 1.8.19, from the release's file name. Leave empty only if the file states it.",
		`<input id="firmware" name="firmware" type="file" accept=".bin,application/octet-stream" required>`, "up to 3 MB.",
		`<p class="note">The server checks the file's chip, not its board. Upload only a build made for exactly this model.`,
		`<button class="btn btn-primary" type="submit">Upload</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty page lacks %q", want)
		}
	}
	if strings.Contains(body, `class="stack"`) {
		t.Error("empty page has a build list")
	}
	// The fields are in the order a browser posts them, which the handler
	// relies on to see the model before the file.
	if m, v, f := strings.Index(body, `name="model"`), strings.Index(body, `name="version"`), strings.Index(body, `name="firmware"`); m > v || v > f {
		t.Errorf("form fields are out of order: model %d, version %d, firmware %d", m, v, f)
	}
	models := strings.Count(body, `<option value="`) - 1
	if models != len(firmware.Models) {
		t.Errorf("%d model options, want %d", models, len(firmware.Models))
	}
}

func TestFirmwareStages(t *testing.T) {
	t.Parallel()
	const model = fwModel
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, tu *testUI, fw db.Firmware, canary int64)
		badge string
		offer string
		// start is the label of the link to the build page, and cancel is
		// whether there is a link to cancel.
		start  string
		cancel bool
		facts  []string
	}{
		{
			name: "not rolled out", setup: func(*testing.T, *testUI, db.Firmware, int64) {},
			badge: `<span class="badge badge-neutral">Not rolled out</span>`,
			offer: "Not rolled out, so it is offered to no device. A rollout starts with one device, the canary.",
			start: "Start rollout",
		},
		{
			name: "canary",
			setup: func(t *testing.T, tu *testUI, fw db.Firmware, _ int64) {
				startRollout(t, tu, fw.ID)
			},
			badge: `<span class="badge badge-warn">Canary only</span>`,
			offer: "ABC123 goes first, as the canary. The rollout opens to the other " + model + " devices once ABC123 reports 1.8.19 or newer.",
			start: "Restart rollout", cancel: true,
			facts: []string{
				`<dt>Canary</dt><dd><a href="/devices/ABC123">ABC123</a></dd>`, "<dt>Canary reports</dt><dd>1.8.17</dd>",
				"<dt>Canary last offered</dt><dd>Never</dd>", "<dt>Started</dt><dd>12 Jan 1970 19:16 TST</dd>",
			},
		},
		{
			name: "open",
			setup: func(t *testing.T, tu *testUI, fw db.Firmware, canary int64) {
				startRollout(t, tu, fw.ID)
				reportVersion(t, tu, canary, "1.8.19")
			},
			badge: `<span class="badge badge-ok">Open</span>`,
			offer: "ABC123 reports 1.8.19, so the rollout is open to the other " + model + " devices.",
			start: "Restart rollout", cancel: true,
			facts: []string{"<dt>Canary reports</dt><dd>1.8.19</dd>"},
		},
		{
			name: "stalled",
			setup: func(t *testing.T, tu *testUI, fw db.Firmware, canary int64) {
				startRollout(t, tu, fw.ID)
				for range firmware.MaxOffers {
					err := tu.db.Write.RecordFirmwareOffer(t.Context(), db.RecordFirmwareOfferParams{DeviceID: canary, FirmwareID: fw.ID, LastOfferedAt: 1})
					if err != nil {
						t.Fatal(err)
					}
				}
			},
			badge: `<span class="badge badge-warn">Stalled</span>`,
			offer: "ABC123 was offered this build 3 times and still reports 1.8.17, so the rollout is stalled. No device is offered the build. Restart it with a canary, or cancel it.",
			start: "Restart rollout", cancel: true,
			facts: []string{"<dt>Canary last offered</dt><dd>1 Jan 1970 05:30 TST</dd>"},
		},
		{
			name: "canary blocked",
			setup: func(t *testing.T, tu *testUI, fw db.Firmware, _ int64) {
				startRollout(t, tu, fw.ID)
				if _, err := device.Block(t.Context(), tu.db, canaryID, nil, tu.now); err != nil {
					t.Fatal(err)
				}
			},
			badge: `<span class="badge badge-warn">Canary blocked</span>`,
			offer: "ABC123, the canary, is blocked and still reports 1.8.17, so the rollout cannot go on. No device is offered the build. Restart it with another canary, or cancel it.",
			start: "Restart rollout", cancel: true,
		},
		{
			name: "canceled",
			setup: func(t *testing.T, tu *testUI, fw db.Firmware, _ int64) {
				startRollout(t, tu, fw.ID)
				if err := firmware.CancelRollout(t.Context(), tu.db, fw.ID); err != nil {
					t.Fatal(err)
				}
			},
			badge: `<span class="badge badge-neutral">Canceled</span>`,
			offer: "The rollout was canceled. The build is offered to no one and its file is not served.",
			start: "Start rollout",
			facts: []string{"<dt>Canary</dt>"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir := newFirmwareUI(t)
			canary := addFirmwareDevice(t, tu, canaryID, "<b>x</b>", model, "1.8.17", true)
			fw := addFirmware(t, tu, dir, model)
			tc.setup(t, tu, fw, canary)
			body := tu.get(t, tu.signIn(t), "/firmware").Body.String()
			aria := tc.start + " of " + model + " 1.8.19"
			want := []string{
				`<li class="panel" id="build-` + id(fw.ID) + `">`, "<h2>xiao_<wbr>epaper_<wbr>display 1.8.19</h2>", tc.badge,
				`<dt>Model</dt><dd><code>` + model + `</code></dd>`, "<dt>Chip</dt><dd>esp32s3</dd>",
				fmt.Sprintf("<dt>Size</dt><dd>%d bytes</dd>", fw.Bytes), "<dt>Uploaded</dt><dd>12 Jan 1970</dd>",
				`<p class="rel">` + tc.offer + `</p>`,
				`href="/firmware/builds/` + id(fw.ID) + `" aria-label="` + aria + `">` + tc.start + `</a>`,
				`<a class="btn btn-secondary" href="/firmware/builds/` + id(fw.ID) + `#devices" aria-label="Devices for ` + model + ` 1.8.19">Devices</a>`,
			}
			for _, w := range slices.Concat(want, tc.facts) {
				if !strings.Contains(body, w) {
					t.Errorf("page lacks %q:\n%s", w, body)
				}
			}
			if got := strings.Contains(body, `>Cancel rollout</a>`); got != tc.cancel {
				t.Errorf("cancel link present = %v, want %v", got, tc.cancel)
			}
			if tc.cancel && !strings.Contains(body, `href="/firmware/builds/`+id(fw.ID)+`/cancel" aria-label="Cancel rollout of `+model+` 1.8.19"`) {
				t.Error("the cancel link does not go to the confirm page with a full name")
			}
			if strings.Contains(body, "SHA-256") {
				t.Error("the catalog shows the SHA-256")
			}
			if strings.Contains(body, "No firmware yet") {
				t.Error("the list has the empty state")
			}
		})
	}
}

// startRollout starts the build's rollout with the canary device.
func startRollout(t *testing.T, tu *testUI, fwID int64) {
	t.Helper()
	if _, err := firmware.StartRollout(t.Context(), tu.db, fwID, canaryID, tu.now); err != nil {
		t.Fatal(err)
	}
}

// reportVersion records the firmware version a device reported.
func reportVersion(t *testing.T, tu *testUI, deviceID int64, version string) {
	t.Helper()
	err := tu.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwVersion: version, ID: deviceID})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFirmwareListsEveryModel(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	a := addFirmware(t, tu, dir, "og")
	b := addFirmware(t, tu, dir, fwModel)
	body := tu.get(t, tu.signIn(t), "/firmware").Body.String()
	first, second := strings.Index(body, `id="build-`+id(a.ID)+`"`), strings.Index(body, `id="build-`+id(b.ID)+`"`)
	if first < 0 || second < first {
		t.Errorf("builds are not in the list's order: %d, %d", first, second)
	}
	for _, m := range []string{"<code>og</code>", "<code>" + fwModel + "</code>"} {
		if !strings.Contains(body, m) {
			t.Errorf("list lacks the model %s", m)
		}
	}
}

func TestFirmwareUpload(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	c := tu.signIn(t)
	parts := append([]formPart{{field: "note", content: "ignored"}}, goodUpload()...)
	status, loc, body := tu.uploadFirmware(t, c, parts...)
	rows := listFirmware(t, tu)
	if status != http.StatusSeeOther || len(rows) != 1 {
		t.Fatalf("status %d with %d builds: %s", status, len(rows), body)
	}
	fw := rows[0]
	if want := "/firmware#build-" + id(fw.ID); loc != want {
		t.Errorf("redirect to %q, want %q", loc, want)
	}
	if fw.FwModel != fwModel || fw.Version != "1.8.19" || fw.Chip != "esp32s3" || fw.Source != firmware.SourceUpload || fw.UploadedAt != tu.now.Unix() || fw.Stage != "" {
		t.Errorf("build = %+v", fw)
	}
	if got, want := firmwareFiles(t, dir), []string{fw.Sha256 + ".bin"}; !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if page := tu.get(t, c, "/firmware").Body.String(); !strings.Contains(page, `id="build-`+id(fw.ID)+`"`) {
		t.Error("the list does not show the uploaded build")
	}
}

func TestFirmwareUploadVersionFromFile(t *testing.T) {
	t.Parallel()
	tu, _ := newFirmwareUI(t)
	status, _, body := tu.uploadFirmware(t, tu.signIn(t),
		formPart{field: "model", content: fwModel}, formPart{field: "version"},
		formPart{field: "firmware", filename: "a.bin", content: string(testImage(fwChipS3, "v1.8.20"))})
	if rows := listFirmware(t, tu); status != http.StatusSeeOther || len(rows) != 1 || rows[0].Version != "1.8.20" {
		t.Errorf("status %d, builds %+v: %s", status, rows, body)
	}
}

func TestFirmwareUploadRefused(t *testing.T) {
	t.Parallel()
	const choose = "Choose a firmware file to upload."
	good := string(testImage(fwChipS3, "1"))
	for _, tc := range []struct {
		name string
		// existing is whether a build of the model at 1.8.19 is there
		// already.
		existing bool
		parts    []formPart
		status   int
		want     string
	}{
		{"no file part", false, []formPart{{field: "model", content: fwModel}}, 400, choose},
		{"no parts", false, nil, 400, choose},
		{"empty file name", false, []formPart{{field: "model", content: fwModel}, {field: "firmware", empty: true}}, 400, choose},
		{"firmware field that is not a file", false, []formPart{{field: "model", content: fwModel}, {field: "firmware", content: good}}, 400, choose},
		{"no model", false, []formPart{{field: "version", content: "1.8.19"}, {field: "firmware", filename: "a.bin", content: good}}, 400, "Choose a model."},
		{"empty model", false, []formPart{{field: "model"}, {field: "firmware", filename: "a.bin", content: good}}, 400, "Choose a model."},
		{"file before model", false, []formPart{{field: "firmware", filename: "a.bin", content: good}, {field: "model", content: fwModel}}, 400, "Choose a model."},
		{"unknown model", false, goodWith("model", "toaster"), 400, `This server does not know the firmware model "toaster".`},
		{"model too long", false, goodWith("model", strings.Repeat("m", 257)), 400, "The model or version is too long."},
		{"wrong chip", false, goodWith("model", "og"), 400, "The image is built for esp32s3, and og uses esp32c3."},
		{"C3 image for an S3 model", false, []formPart{{field: "model", content: fwModel}, {field: "version", content: "1.8.19"}, {field: "firmware", filename: "a.bin", content: string(testImage(fwChipC3, "1"))}}, 400, "The image is built for esp32c3, and " + fwModel + " uses esp32s3."},
		{"corrupt image", false, []formPart{{field: "model", content: fwModel}, {field: "version", content: "1.8.19"}, {field: "firmware", filename: "a.bin", content: "not firmware at all, just text"}}, 400, "The file is not an ESP firmware image (it does not start with the image header)."},
		{"empty file", false, []formPart{{field: "model", content: fwModel}, {field: "version", content: "1.8.19"}, {field: "firmware", filename: "a.bin"}}, 400, "The file is too small to be an ESP firmware image."},
		{"bad version", false, goodWith("version", "latest"), 400, `The version "latest" is not dotted numbers, such as 1.8.19.`},
		{"version needed", false, []formPart{{field: "model", content: fwModel}, {field: "firmware", filename: "a.bin", content: good}}, 400, `The image does not say which firmware version it is (its app descriptor says "1"), so the version must be given.`},
		{"version differs from the file's", false, []formPart{{field: "model", content: fwModel}, {field: "version", content: "1.8.20"}, {field: "firmware", filename: "a.bin", content: string(testImage(fwChipS3, "1.8.19"))}}, 400, "The image says it is version 1.8.19, and the version given is 1.8.20."},
		{"same version again", true, []formPart{{field: "model", content: fwModel}, {field: "version", content: "1.8.19"}, {field: "firmware", filename: "a.bin", content: string(testImage(fwChipS3, "other"))}}, 409, "The catalog already has " + fwModel + " 1.8.19, from another file."},
		{"over the model's partition", false, []formPart{{field: "model", content: "og"}, {field: "version", content: "1.8.19"}, {field: "firmware", filename: "a.bin", content: strings.Repeat("a", 0x1D0001)}}, 413, "The file is larger than the app partition of og, which holds 1.8 MB."},
		{"over the largest partition", false, []formPart{{field: "model", content: "x"}, {field: "version", content: "1.8.19"}, {field: "firmware", filename: "a.bin", content: strings.Repeat("a", int(firmware.MaxUploadBytes())+1)}}, 413, "The file is larger than the app partition of x, which holds 3 MB."},
		{"large field before the file", false, []formPart{{field: "note", content: strings.Repeat("a", int(firmware.MaxUploadBytes())+maxFormBody)}, {field: "model", content: fwModel}, {field: "firmware", filename: "a.bin", content: good}}, 413, "The upload is too large."},
		{"body limit reached in the file", false, []formPart{{field: "note", content: strings.Repeat("a", maxFormBody)}, {field: "model", content: "x"}, {field: "version", content: "1.8.19"}, {field: "firmware", filename: "a.bin", content: strings.Repeat("a", int(firmware.MaxUploadBytes()))}}, 413, "The upload is too large."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir := newFirmwareUI(t)
			wantRows := 0
			if tc.existing {
				addFirmware(t, tu, dir, fwModel)
				wantRows = 1
			}
			status, _, body := tu.uploadFirmware(t, tu.signIn(t), tc.parts...)
			// The message is in the upload panel, just above the first field.
			wantHTML := `<form method="post" action="/firmware#upload" enctype="multipart/form-data">` + "\n" +
				`<p class="error" role="alert">` + html.EscapeString(tc.want) + "</p>\n" + `<div class="field">`
			if status != tc.status || !strings.Contains(body, wantHTML) || !strings.Contains(body, "<h1>Firmware</h1>") || strings.Count(body, `role="alert"`) != 1 {
				t.Errorf("status %d, body %s; want %d with %q above the first field", status, body, tc.status, tc.want)
			}
			if got := len(listFirmware(t, tu)); got != wantRows {
				t.Errorf("%d builds after a refused upload, want %d", got, wantRows)
			}
			if got := len(firmwareFiles(t, dir)); got != wantRows {
				t.Errorf("%d files in the firmware directory after a refused upload, want %d", got, wantRows)
			}
		})
	}
}

// TestFirmwareUploadKeepsForm checks that a refused upload shows the model
// and version that were sent again, escaped, and that no other model is
// selected.
func TestFirmwareUploadKeepsForm(t *testing.T) {
	t.Parallel()
	tu, _ := newFirmwareUI(t)
	status, _, body := tu.uploadFirmware(t, tu.signIn(t),
		formPart{field: "model", content: "og"}, formPart{field: "version", content: `1.8.x"><b>`},
		formPart{field: "firmware", filename: "a.bin", content: string(testImage(fwChipS3, "1"))})
	for _, want := range []string{`<option value="og" selected>og</option>`, `value="1.8.x&#34;&gt;&lt;b&gt;"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	if status != http.StatusBadRequest || strings.Count(body, " selected>") != 1 || strings.Contains(body, "<b>") {
		t.Errorf("status %d; want 400 with one model selected and the version escaped:\n%s", status, body)
	}
	// With no refusal, the form is empty.
	if page := tu.get(t, tu.signIn(t), "/firmware").Body.String(); strings.Contains(page, " selected>") || !strings.Contains(page, `autocomplete="off" value="">`) {
		t.Errorf("a fresh page has form values:\n%s", page)
	}
}

// TestFirmwareUploadCutOff checks that a client that drops the connection
// part way through an upload gets a client error and leaves no row or
// file, whether it is cut off in a field before the file or in the file,
// and whether the body ends cleanly (as a test's reader does) or with a
// read error (as a server's does when the client hangs up).
func TestFirmwareUploadCutOff(t *testing.T) {
	t.Parallel()
	body, ct := multipartBody(t, goodUpload()...)
	for _, tc := range []struct {
		name string
		cut  int
	}{
		{"in a field", strings.Index(body, fwModel) + 5},
		{"in the file", strings.Index(body, "\xe9") + 40},
	} {
		for _, ending := range []struct {
			name string
			err  error
		}{{"EOF", nil}, {"unexpected EOF", io.ErrUnexpectedEOF}} {
			t.Run(tc.name+" then "+ending.name, func(t *testing.T) {
				t.Parallel()
				tu, dir := newFirmwareUI(t)
				c := tu.signIn(t)
				var tail io.Reader = strings.NewReader("")
				if ending.err != nil {
					tail = iotest.ErrReader(ending.err)
				}
				req := httptest.NewRequestWithContext(t.Context(), "POST", "/firmware", io.MultiReader(strings.NewReader(body[:tc.cut]), tail))
				req.Header.Set("Content-Type", ct)
				req.Header.Set("Cookie", cookieHeader(c))
				w := tu.serve(req)
				const want = `<p class="error" role="alert">The upload was cut off or could not be read.</p>`
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
					t.Errorf("status %d, body %s; want 400 with %q", w.Code, w.Body, want)
				}
				if got := len(listFirmware(t, tu)); got != 0 {
					t.Errorf("%d builds after a cut-off upload", got)
				}
				if got := firmwareFiles(t, dir); len(got) != 0 {
					t.Errorf("a cut-off upload left %v in the firmware directory", got)
				}
			})
		}
	}
}

// TestFirmwareUploadClientGone checks that an upload whose client hung up
// is not answered as a server failure. The request's context has ended, so
// the catalog the refusal would show cannot be read, and there is no one to
// show it to.
func TestFirmwareUploadClientGone(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	c := tu.signIn(t)
	body, ct := multipartBody(t, goodUpload()...)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cut := strings.Index(body, "\xe9") + 40
	req := httptest.NewRequestWithContext(ctx, "POST", "/firmware", &hangUp{data: strings.NewReader(body[:cut]), cancel: cancel})
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookieHeader(c))
	w := tu.serve(req)
	if w.Code == http.StatusInternalServerError || w.Body.Len() != 0 {
		t.Errorf("status %d, body %q; want nothing written for a client that hung up", w.Code, w.Body)
	}
	if got := len(listFirmware(t, tu)); got != 0 {
		t.Errorf("%d builds after the client hung up", got)
	}
	if got := firmwareFiles(t, dir); len(got) != 0 {
		t.Errorf("an upload whose client hung up left %v in the firmware directory", got)
	}
}

// goodWith is the parts of a good upload with the field's value replaced.
func goodWith(field, value string) []formPart {
	parts := goodUpload()
	for i := range parts {
		if parts[i].field == field {
			parts[i].content = value
		}
	}
	return parts
}

func TestFirmwareUploadNotMultipart(t *testing.T) {
	t.Parallel()
	tu, _ := newFirmwareUI(t)
	w := tu.post(t, tu.signIn(t), "/firmware", url.Values{"model": {fwModel}})
	if body := w.Body.String(); w.Code != http.StatusBadRequest || !strings.Contains(body, "Choose a firmware file to upload.") {
		t.Errorf("status %d, body %s; want a 400 asking for a file", w.Code, body)
	}
}

func TestFirmwareNeedsSession(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	status, loc, _ := tu.uploadFirmware(t, nil, goodUpload()...)
	if status != http.StatusSeeOther || loc != "/login" {
		t.Errorf("upload without a session: %d to %q, want 303 to /login", status, loc)
	}
	if rows, files := listFirmware(t, tu), firmwareFiles(t, dir); len(rows) != 0 || len(files) != 0 {
		t.Errorf("an upload without a session left %d builds and %d files", len(rows), len(files))
	}
	for _, tc := range []struct{ method, path string }{
		{"GET", "/firmware"},
		{"GET", "/firmware/builds/1"},
		{"POST", "/firmware/builds/1/rollout"},
		{"GET", "/firmware/builds/1/cancel"},
		{"POST", "/firmware/builds/1/cancel"},
	} {
		w := tu.do(t, tc.method, tc.path, "")
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
			t.Errorf("%s %s without a session: %d to %q, want 303 to /login", tc.method, tc.path, w.Code, w.Header().Get("Location"))
		}
	}
}

// TestFirmwareRoutesRegisterWithDeviceAPI checks that the admin routes and
// the device download route, which owns /firmware/{name}, can share one mux.
// A pattern that conflicts with another panics when it is registered.
func TestFirmwareRoutesRegisterWithDeviceAPI(t *testing.T) {
	t.Parallel()
	tu, _ := newFirmwareUI(t)
	mux := http.NewServeMux()
	deviceapi.New(tu.db, nil, deviceapi.Config{}).Register(mux)
	tu.srv.Register(mux)
}

func TestFirmwareBuildNotFound(t *testing.T) {
	t.Parallel()
	tu, _ := newFirmwareUI(t)
	c := tu.signIn(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/firmware/builds/999"},
		{"GET", "/firmware/builds/abc"},
		{"POST", "/firmware/builds/999/rollout"},
		{"POST", "/firmware/builds/abc/rollout"},
		{"GET", "/firmware/builds/999/cancel"},
		{"POST", "/firmware/builds/999/cancel"},
		{"POST", "/firmware/builds/abc/cancel"},
	} {
		w := tu.do(t, tc.method, tc.path, "canary=ABC123", "Cookie", cookieHeader(c))
		body := w.Body.String()
		if w.Code != http.StatusNotFound || !strings.Contains(body, "<h2>No such firmware build</h2>") || !strings.Contains(body, `<a href="/firmware">Back to Firmware</a>`) {
			t.Errorf("%s %s: status %d, body %s; want the not-found page", tc.method, tc.path, w.Code, body)
		}
	}
}

func TestFirmwareBuildPage(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	fw := addFirmware(t, tu, dir, fwModel)
	addFirmwareDevice(t, tu, "ABC123", "<b>x</b>", fwModel, "1.8.17", true)
	addFirmwareDevice(t, tu, "DEF456", "", fwModel, "1.8.18", false)
	addFirmwareDevice(t, tu, "GHI789", "Other board", "reterminal_e1001", "1.8.17", true)
	addFirmwareDevice(t, tu, "JKL012", "Close name", fwModel+"_2", "1.8.17", true)
	c := tu.signIn(t)
	body := tu.get(t, c, "/firmware/builds/"+id(fw.ID)).Body.String()
	for _, want := range []string{
		`<a class="back" href="/firmware">`, "<h1>xiao_<wbr>epaper_<wbr>display 1.8.19</h1>",
		`<dt>Model</dt><dd><code>` + fwModel + `</code></dd>`, `<dt>SHA-256</dt><dd><code>` + fw.Sha256 + `</code></dd>`,
		`<span class="badge badge-neutral">Not rolled out</span>`, `<p class="rel">Not rolled out, so it is offered to no device.`,
		`<h2 id="rollout-title">Start a rollout</h2>`,
		"The canary is the one device offered this build first. The others can be offered it only after the canary reports 1.8.19 or newer.",
		`<form method="post" action="/firmware/builds/` + id(fw.ID) + `/rollout">`, `<select id="canary" name="canary" required>`,
		`<option value="">Choose a device</option>`,
		`<option value="ABC123">&lt;b&gt;x&lt;/b&gt; (firmware 1.8.17)</option>`,
		`<button class="btn btn-primary" type="submit">Start rollout</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("build page lacks %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"GHI789", "JKL012", "Other board", "<b>x", "Restarting gives", "SHA-256</dt><dd><code></code>"} {
		if strings.Contains(body, bad) {
			t.Errorf("build page has %q", bad)
		}
	}
	// A device that updates are off for is listed, with why, and cannot be
	// the canary.
	if !strings.Contains(body, `href="/devices/DEF456"`) || strings.Contains(body, `<option value="DEF456"`) {
		t.Error("a device with updates off is not listed, or can be the canary")
	}
}

// deviceItem returns the part of the page's device list for the device with
// the friendly ID.
func deviceItem(t *testing.T, body, friendly string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<a class="item-name" href="/devices/`+friendly+`">`)
	if !ok {
		t.Fatalf("page has no device %s:\n%s", friendly, body)
	}
	item, _, _ := strings.Cut(rest, "</li>")
	return item
}

// TestFirmwareBuildPageDevices checks the page's device list and canary
// picker against what firmware.DeviceOffers says: a device that is offered
// the build now, one that would be offered a newer build, the build's
// canary, one that cannot be offered anything, and a device of another
// model.
func TestFirmwareBuildPageDevices(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	fw := addFirmware(t, tu, dir, fwModel)
	newer, err := firmware.Add(t.Context(), tu.db, dir, fwModel, "1.8.20", bytes.NewReader(testImage(fwChipS3, "2")), tu.now)
	if err != nil {
		t.Fatal(err)
	}
	canary := addFirmwareDevice(t, tu, "CAN001", "<b>x</b>", fwModel, "1.8.17", true)
	addFirmwareDevice(t, tu, "DEF456", "", fwModel, "1.8.18", true)
	addFirmwareDevice(t, tu, "GHI789", "", fwModel, "", false)
	offered := addFirmwareDevice(t, tu, "JKL012", "Plain", fwModel, "1.8.18", true)
	addFirmwareDevice(t, tu, "MNO345", "Other board", "reterminal_e1001", "1.8.17", true)
	// CAN001 is the canary of the build, and reports it, which opens it to
	// the rest. DEF456 is the canary of a newer build, so it is offered that
	// one instead.
	if _, err := firmware.StartRollout(t.Context(), tu.db, fw.ID, "CAN001", tu.now); err != nil {
		t.Fatal(err)
	}
	reportVersion(t, tu, canary, "1.8.19")
	if _, err := firmware.StartRollout(t.Context(), tu.db, newer.ID, "DEF456", tu.now); err != nil {
		t.Fatal(err)
	}
	if err := tu.db.Write.RecordFirmwareOffer(t.Context(), db.RecordFirmwareOfferParams{DeviceID: offered, FirmwareID: fw.ID, LastOfferedAt: 1}); err != nil {
		t.Fatal(err)
	}
	body := tu.get(t, tu.signIn(t), "/firmware/builds/"+id(fw.ID)).Body.String()

	for _, want := range []string{
		`<section class="panel" id="devices" aria-labelledby="devices-title">`, `<h2 id="devices-title">Devices</h2>`,
		`<p class="rel">From each device's last report. A device is offered a build only on a wake that sends its model and version.</p>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, tc := range []struct {
		friendly string
		want     []string
		absent   []string
	}{
		{"CAN001", []string{
			`&lt;b&gt;x&lt;/b&gt;</a>`, `<span class="rel">Firmware 1.8.19</span>`, `<span class="badge badge-warn">Canary</span>`,
			`<span class="badge badge-neutral">Not offered</span>`, "The build&#39;s version 1.8.19 is not newer than the device&#39;s 1.8.19.",
		}, []string{"Offered now", "See build", "times"}},
		{"DEF456", []string{
			`DEF456</a>`, `<span class="rel">Firmware 1.8.18</span>`, `<span class="badge badge-neutral">Not offered</span>`,
			`The device is offered the newer version 1.8.20 instead. <a href="/firmware/builds/` + id(newer.ID) + `">See build</a>`,
		}, []string{"Offered now", "Canary", "times"}},
		{"GHI789", []string{
			`<span class="rel">Firmware unknown</span>`, `<span class="badge badge-neutral">Not offered</span>`, "Updates are off for the device.",
		}, []string{"Offered now", "Canary", "See build", "times"}},
		{"JKL012", []string{
			`Plain</a>`, `<span class="rel">Firmware 1.8.18</span>`, `<span class="badge badge-ok">Offered now</span>`,
			`<p class="rel">Offered 1 of 3 times, last 1 Jan 1970 05:30 TST</p>`,
		}, []string{"Not offered", "Canary", "See build"}},
	} {
		item := deviceItem(t, body, tc.friendly)
		for _, want := range tc.want {
			if !strings.Contains(item, want) {
				t.Errorf("%s lacks %q:\n%s", tc.friendly, want, item)
			}
		}
		for _, bad := range tc.absent {
			if strings.Contains(item, bad) {
				t.Errorf("%s has %q:\n%s", tc.friendly, bad, item)
			}
		}
	}
	if strings.Contains(body, "MNO345") || strings.Contains(body, "Other board") || strings.Contains(body, "<b>x") {
		t.Error("page lists a device of another model, or does not escape a name")
	}
	// The devices are in friendly ID order.
	for i, pair := range [][2]string{{"CAN001", "DEF456"}, {"DEF456", "GHI789"}, {"GHI789", "JKL012"}} {
		if a, b := strings.Index(body, `/devices/`+pair[0]+`"`), strings.Index(body, `/devices/`+pair[1]+`"`); a < 0 || b < a {
			t.Errorf("pair %d: %s is not before %s", i, pair[0], pair[1])
		}
	}
	// Only a device that can be the canary is in the picker. CAN001 and
	// GHI789 are on the build's version and have updates off.
	_, afterSelect, _ := strings.Cut(body, `<select id="canary"`)
	picker, _, _ := strings.Cut(afterSelect, "</select>")
	for _, want := range []string{`<option value="DEF456">DEF456 (firmware 1.8.18)</option>`, `<option value="JKL012">Plain (firmware 1.8.18)</option>`} {
		if !strings.Contains(picker, want) {
			t.Errorf("picker lacks %q:\n%s", want, picker)
		}
	}
	if got := strings.Count(picker, "<option"); got != 3 {
		t.Errorf("picker has %d options, want the first and two devices:\n%s", got, picker)
	}
}

func TestFirmwareBuildPageNoEligibleCanary(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	fw := addFirmware(t, tu, dir, fwModel)
	addFirmwareDevice(t, tu, "ABC123", "", fwModel, "1.8.17", false)
	body := tu.get(t, tu.signIn(t), "/firmware/builds/"+id(fw.ID)).Body.String()
	for _, want := range []string{
		`<p class="rel">No device of this model can be the canary now. The list below says why for each.</p>`,
		`href="/devices/ABC123"`, "Updates are off for the device.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `action="/firmware/builds/`) || strings.Contains(body, `<select`) {
		t.Error("page has a rollout form")
	}
}

func TestFirmwareBuildPageNoDevice(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	fw := addFirmware(t, tu, dir, fwModel)
	addFirmwareDevice(t, tu, "GHI789", "Other board", "reterminal_e1001", "1.8.17", true)
	body := tu.get(t, tu.signIn(t), "/firmware/builds/"+id(fw.ID)).Body.String()
	want := `<p class="rel">No device reports the model <code>` + fwModel + `</code>, so this build has no one to go to.</p>`
	if strings.Count(body, want) != 1 {
		t.Errorf("page does not have %q once", want)
	}
	if !strings.Contains(body, `id="devices"`) || strings.Contains(body, `action="/firmware/builds/`) || strings.Contains(body, "GHI789") || strings.Contains(body, "From each device") {
		t.Error("page lacks the device panel, or has a form, a device of another model or the note about reports")
	}
}

func TestFirmwareBuildPageRestart(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	fw := addFirmware(t, tu, dir, fwModel)
	addFirmwareDevice(t, tu, "ABC123", "", fwModel, "1.8.17", true)
	canary := addFirmwareDevice(t, tu, "DEF456", "", fwModel, "1.8.17", true)
	if _, err := firmware.StartRollout(t.Context(), tu.db, fw.ID, "DEF456", tu.now); err != nil {
		t.Fatal(err)
	}
	body := tu.get(t, tu.signIn(t), "/firmware/builds/"+id(fw.ID)).Body.String()
	for _, want := range []string{
		`<h2 id="rollout-title">Restart the rollout</h2>`, "Restarting gives the canary 3 more tries.",
		`<option value="DEF456" selected>DEF456 (firmware 1.8.17)</option>`, `<option value="ABC123">`,
		`<button class="btn btn-primary" type="submit">Restart rollout</button>`,
		`<p class="rel">DEF456 goes first, as the canary.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("restart page lacks %q", want)
		}
	}
	if strings.Contains(body, `<option value="ABC123" selected>`) {
		t.Error("the wrong canary is selected")
	}
	const openNote = "This rollout is open. Restarting it puts the build back at its canary stage"
	if strings.Contains(body, openNote) {
		t.Error("a rollout at its canary stage is said to be open")
	}
	// Once the canary reports the build, the rollout is open, and a restart
	// would close it to the other devices again. The page says so.
	reportVersion(t, tu, canary, "1.8.19")
	body = tu.get(t, tu.signIn(t), "/firmware/builds/"+id(fw.ID)).Body.String()
	if !strings.Contains(body, openNote+": the other devices are not offered it again until the new canary reports 1.8.19 or newer.") {
		t.Errorf("the page of an open rollout does not warn about a restart:\n%s", body)
	}
}

func TestFirmwareRollout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		form url.Values
		// updatesOff turns updates off for the device ABC123.
		updatesOff bool
		status     int
		want       string
		stage      string
	}{
		{"success", url.Values{"canary": {canaryID}}, false, 303, "", "canary"},
		{"canary with spaces", url.Values{"canary": {" " + canaryID + " "}}, false, 303, "", "canary"},
		{"updates off", url.Values{"canary": {canaryID}}, true, 400, "ABC123 cannot be the canary: updates are off for the device.", ""},
		{"no canary field", url.Values{}, false, 400, "Choose a canary device.", ""},
		{"empty canary", url.Values{"canary": {""}}, false, 400, "Choose a canary device.", ""},
		{"unknown device", url.Values{"canary": {"ZZZ999"}}, false, 400, "That device no longer exists.", ""},
		{"device of another model", url.Values{"canary": {"GHI789"}}, false, 400, "GHI789 cannot be the canary: the build is not for the device&#39;s model, reterminal_e1001.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir := newFirmwareUI(t)
			fw := addFirmware(t, tu, dir, fwModel)
			addFirmwareDevice(t, tu, canaryID, "", fwModel, "1.8.17", !tc.updatesOff)
			// Another device that can be the canary, so the form is shown
			// again after a refusal.
			addFirmwareDevice(t, tu, "DEF456", "", fwModel, "1.8.17", true)
			addFirmwareDevice(t, tu, "GHI789", "", "reterminal_e1001", "1.8.17", true)
			w := tu.post(t, tu.signIn(t), "/firmware/builds/"+id(fw.ID)+"/rollout", tc.form)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			rows := listFirmware(t, tu)
			if tc.status == http.StatusSeeOther {
				if want := "/firmware#build-" + id(fw.ID); w.Header().Get("Location") != want {
					t.Errorf("redirect to %q, want %q", w.Header().Get("Location"), want)
				}
				if rows[0].Stage != tc.stage || rows[0].Canary != canaryID || rows[0].StartedAt != tu.now.Unix() {
					t.Errorf("build = %+v", rows[0])
				}
				return
			}
			body := w.Body.String()
			for _, want := range []string{`<p class="error" role="alert">` + tc.want + `</p>`, `<select id="canary"`} {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			if rows[0].Stage != "" {
				t.Errorf("a refused rollout left stage %q", rows[0].Stage)
			}
		})
	}
}

// TestFirmwareRolloutRefusedNotOffered checks that a refusal is shown with
// the picker, which does not list the device that was refused.
func TestFirmwareRolloutRefusedNotOffered(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	fw := addFirmware(t, tu, dir, fwModel)
	addFirmwareDevice(t, tu, "ABC123", "", fwModel, "1.8.17", true)
	addFirmwareDevice(t, tu, "DEF456", "", fwModel, "1.8.17", false)
	w := tu.post(t, tu.signIn(t), "/firmware/builds/"+id(fw.ID)+"/rollout", url.Values{"canary": {"DEF456"}})
	body := w.Body.String()
	if w.Code != http.StatusBadRequest || !strings.Contains(body, "DEF456 cannot be the canary: updates are off for the device.") ||
		!strings.Contains(body, `<option value="ABC123">`) || strings.Contains(body, `<option value="DEF456"`) {
		t.Errorf("status %d; want the refusal with a picker without DEF456:\n%s", w.Code, body)
	}
}

func TestFirmwareRolloutRestart(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	fw := addFirmware(t, tu, dir, fwModel)
	canary := addFirmwareDevice(t, tu, canaryID, "", fwModel, "1.8.17", true)
	startRollout(t, tu, fw.ID)
	for range firmware.MaxOffers {
		err := tu.db.Write.RecordFirmwareOffer(t.Context(), db.RecordFirmwareOfferParams{DeviceID: canary, FirmwareID: fw.ID, LastOfferedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := listFirmware(t, tu)[0].Stage; got != "stalled" {
		t.Fatalf("stage %q before the restart, want stalled", got)
	}
	w := tu.post(t, tu.signIn(t), "/firmware/builds/"+id(fw.ID)+"/rollout", url.Values{"canary": {canaryID}})
	if got := listFirmware(t, tu)[0].Stage; w.Code != http.StatusSeeOther || got != "canary" {
		t.Errorf("status %d, stage %q after the restart, want 303 and canary", w.Code, got)
	}
}

func TestFirmwareCancelConfirm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, tu *testUI, fw db.Firmware)
		// active is whether the page has the Cancel button.
		active bool
	}{
		{"no rollout", func(*testing.T, *testUI, db.Firmware) {}, false},
		{"canary", func(t *testing.T, tu *testUI, fw db.Firmware) { startRollout(t, tu, fw.ID) }, true},
		{"canceled", func(t *testing.T, tu *testUI, fw db.Firmware) {
			startRollout(t, tu, fw.ID)
			if err := firmware.CancelRollout(t.Context(), tu.db, fw.ID); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir := newFirmwareUI(t)
			fw := addFirmware(t, tu, dir, fwModel)
			addFirmwareDevice(t, tu, canaryID, "", fwModel, "1.8.17", true)
			tc.setup(t, tu, fw)
			before := listFirmware(t, tu)[0].Stage
			w := tu.get(t, tu.signIn(t), "/firmware/builds/"+id(fw.ID)+"/cancel")
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.Contains(body, "<h1>Cancel rollout</h1>") {
				t.Fatalf("status %d: %s", w.Code, body)
			}
			if got := strings.Contains(body, "<form method=\"post\" action=\"/firmware/builds/"+id(fw.ID)+"/cancel\">"); got != tc.active {
				t.Errorf("form present = %v, want %v", got, tc.active)
			}
			if tc.active {
				for _, want := range []string{
					"<h2 id=\"cancel-title\">Cancel the rollout of " + fwModel + " 1.8.19?</h2>",
					"No device is offered this build after that, and its file is no longer served. A device partway through a download may fail it and keeps what it has. Devices that already installed it keep it.",
					`<button class="btn btn-primary btn-danger-solid" type="submit">Cancel rollout</button>`,
					`<a class="btn btn-secondary" href="/firmware#build-` + id(fw.ID) + `">Keep rollout</a>`,
				} {
					if !strings.Contains(body, want) {
						t.Errorf("page lacks %q", want)
					}
				}
			} else if want := fwModel + " 1.8.19 has no active rollout."; !strings.Contains(body, want) || !strings.Contains(body, ">Back</a>") {
				t.Errorf("page lacks %q or a Back link:\n%s", want, body)
			}
			if after := listFirmware(t, tu)[0].Stage; after != before {
				t.Errorf("the confirm page changed the stage from %q to %q", before, after)
			}
		})
	}
}

func TestFirmwareCancel(t *testing.T) {
	t.Parallel()
	tu, dir := newFirmwareUI(t)
	fw := addFirmware(t, tu, dir, fwModel)
	addFirmwareDevice(t, tu, canaryID, "", fwModel, "1.8.17", true)
	startRollout(t, tu, fw.ID)
	c := tu.signIn(t)
	path := "/firmware/builds/" + id(fw.ID) + "/cancel"

	w := tu.post(t, c, path, url.Values{})
	if want := "/firmware#build-" + id(fw.ID); w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
		t.Errorf("cancel: status %d to %q, want 303 to %q", w.Code, w.Header().Get("Location"), want)
	}
	if got := listFirmware(t, tu)[0].Stage; got != "canceled" {
		t.Errorf("stage %q after cancel, want canceled", got)
	}

	// Someone else canceled it first.
	w = tu.post(t, c, path, url.Values{})
	body := w.Body.String()
	if w.Code != http.StatusConflict || !strings.Contains(body, "has no active rollout.") || strings.Contains(body, "<form method=\"post\" action=\""+path) {
		t.Errorf("second cancel: status %d, body %s; want 409 with the page that has no form", w.Code, body)
	}
}
