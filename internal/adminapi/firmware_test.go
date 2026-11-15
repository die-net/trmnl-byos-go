package adminapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
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

const (
	chipS3 = 0x0009
	chipC3 = 0x0005
)

// upload posts a firmware file to the model, with version when it is not
// empty.
func (ta *testAPI) upload(t *testing.T, model, version string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{}
	if model != "" {
		q.Set("model", model)
	}
	if version != "" {
		q.Set("version", version)
	}
	return ta.do(t, "POST", "/api/v1/firmware?"+q.Encode(), body, "Content-Type", contentType)
}

// zeros reads n zero bytes.
type zeros int64

func (z *zeros) Read(p []byte) (int, error) {
	if *z <= 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), int64(*z))
	clear(p[:n])
	*z -= zeros(n)
	return int(n), nil
}

func (ta *testAPI) firmwareFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(ta.firmware)
	if os.IsNotExist(err) {
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

func TestFirmwareUpload(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	image := testImage(chipS3, "1a2b3c4")
	sum := sha256.Sum256(image)

	res := ta.upload(t, "xiao_epaper_display", "1.8.19", bytes.NewReader(image), "application/octet-stream")
	var fw firmwareJSON
	decodeBody(t, res, http.StatusCreated, &fw)
	if fw.Model != "xiao_epaper_display" || fw.Version != "1.8.19" || fw.Chip != "esp32s3" || fw.IDFVersion != "v5.3.2" ||
		fw.SHA256 != hex.EncodeToString(sum[:]) || fw.Bytes != int64(len(image)) || fw.Source != "upload" || fw.Rollout != nil || fw.UploadedAt != ta.now.Unix() {
		t.Errorf("firmware %+v", fw)
	}
	if names := ta.firmwareFiles(t); len(names) != 1 || names[0] != fw.SHA256+".bin" {
		t.Errorf("files %v", names)
	}
	var list []firmwareJSON
	ta.call(t, "GET", "/api/v1/firmware", "", 200, &list)
	if len(list) != 1 || list[0].ID != fw.ID || list[0].Rollout != nil {
		t.Errorf("list %+v", list)
	}

	// A refused upload says why, has the right status, and leaves nothing.
	for _, tc := range []struct {
		name        string
		model       string
		version     string
		body        io.Reader
		contentType string
		status      int
		contains    string
	}{
		{"duplicate", "xiao_epaper_display", "1.8.19", bytes.NewReader(image), "application/octet-stream", 409, "in the catalog"},
		{"same version, other file", "xiao_epaper_display", "1.8.19", bytes.NewReader(testImage(chipS3, "1.8.19 ")), "application/octet-stream", 409, "in the catalog"},
		{"no model", "", "1.8.19", bytes.NewReader(image), "application/octet-stream", 400, "not one this server knows"},
		{"unknown model", "toaster", "1.8.19", bytes.NewReader(image), "application/octet-stream", 400, "not one this server knows"},
		{"wrong chip", "og", "1.8.19", bytes.NewReader(image), "application/octet-stream", 400, "esp32s3"},
		{"C3 image for an S3 model", "xiao_epaper_display", "1.8.20", bytes.NewReader(testImage(chipC3, "1a2b3c4")), "application/octet-stream", 400, "esp32c3"},
		{"truncated", "xiao_epaper_display", "1.8.20", bytes.NewReader(image[:len(image)-40]), "application/octet-stream", 400, "before the checksum"},
		{"not an image", "xiao_epaper_display", "1.8.20", bytes.NewReader([]byte("hello, world, this is not firmware")), "application/octet-stream", 400, "first byte"},
		{"version differs from the file's", "xiao_epaper_display", "1.8.20", bytes.NewReader(testImage(chipS3, "v1.8.19")), "application/octet-stream", 400, "says version 1.8.19"},
		{"version needed", "xiao_epaper_display", "", bytes.NewReader(testImage(chipS3, "1a2b3c4")), "application/octet-stream", 400, "give the version"},
		{"bad version", "xiao_epaper_display", "latest", bytes.NewReader(image), "application/octet-stream", 400, "dotted numbers"},
		{"over the model's partition", "og", "1.8.20", new(zeros(0x1D0001)), "application/octet-stream", 413, "app partition"},
		{"over any partition", "x", "1.8.20", new(zeros(0x300001)), "application/octet-stream", 413, "over"},
		{"wrong content type", "xiao_epaper_display", "1.8.20", bytes.NewReader(image), "application/zip", 415, "octet-stream"},
	} {
		res := ta.upload(t, tc.model, tc.version, tc.body, tc.contentType)
		var e errorBody
		decodeBody(t, res, tc.status, &e)
		if !bytes.Contains([]byte(e.Error), []byte(tc.contains)) {
			t.Errorf("%s: error %q, want it to contain %q", tc.name, e.Error, tc.contains)
		}
	}
	if names := ta.firmwareFiles(t); len(names) != 1 {
		t.Errorf("refused uploads left files: %v", names)
	}
	ta.call(t, "GET", "/api/v1/firmware", "", 200, &list)
	if len(list) != 1 {
		t.Errorf("refused uploads left rows: %+v", list)
	}
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder, want int, out any) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
}

func TestFirmwareNeedsToken(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	for _, r := range []struct{ method, path string }{
		{"GET", "/api/v1/firmware"},
		{"POST", "/api/v1/firmware?model=og"},
		{"POST", "/api/v1/firmware/1/rollout"},
		{"DELETE", "/api/v1/firmware/1/rollout"},
		{"PATCH", "/api/v1/devices/" + testFriend},
	} {
		if w := ta.do(t, r.method, r.path, nil, "Authorization", ""); w.Code != 401 {
			t.Errorf("%s %s without a token: %d, want 401", r.method, r.path, w.Code)
		}
	}
}

func TestFirmwareRollout(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	dev := ta.addDevice(t)
	var fw firmwareJSON
	decodeBody(t, ta.upload(t, "xiao_epaper_display", "1.8.19", bytes.NewReader(testImage(chipS3, "1")), "application/octet-stream"), 201, &fw)
	id := strconv.FormatInt(fw.ID, 10)
	rollout := "/api/v1/firmware/" + id + "/rollout"

	// The device starts with updates off, on firmware too old to trust.
	ta.wantError(t, "POST", rollout, `{"canary": "`+testFriend+`"}`, 400, "updates are off")
	var d deviceJSON
	ta.call(t, "PATCH", "/api/v1/devices/"+testFriend, `{"updates_enabled": true}`, 200, &d)
	if !d.UpdatesEnabled || d.FWPin != "" {
		t.Errorf("device %+v", d)
	}
	ta.wantError(t, "POST", rollout, `{"canary": "`+testFriend+`"}`, 400, "older than 1.8.17")
	if err := ta.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwModel: "xiao_epaper_display", FwVersion: "1.8.17", ID: dev.ID}); err != nil {
		t.Fatal(err)
	}
	ta.call(t, "PATCH", "/api/v1/devices/"+testFriend, `{"fw_pin": "1.8.17"}`, 200, &d)
	ta.wantError(t, "POST", rollout, `{"canary": "`+testFriend+`"}`, 400, "pinned to 1.8.17")
	ta.call(t, "PATCH", "/api/v1/devices/"+testMAC, `{"fw_pin": ""}`, 200, &d)
	if !d.UpdatesEnabled || d.FWPin != "" {
		t.Errorf("device %+v", d)
	}

	ta.call(t, "POST", rollout, `{"canary": "`+testFriend+`"}`, 200, &fw)
	if fw.Rollout == nil || fw.Rollout.State != "canary" || fw.Rollout.Canary != testFriend || fw.Rollout.StartedAt != ta.now.Unix() {
		t.Errorf("rollout %+v", fw.Rollout)
	}
	// It is open once the canary reports the build's version.
	if err := ta.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwModel: "xiao_epaper_display", FwVersion: "1.8.19", ID: dev.ID}); err != nil {
		t.Fatal(err)
	}
	var list []firmwareJSON
	ta.call(t, "GET", "/api/v1/firmware", "", 200, &list)
	if len(list) != 1 || list[0].Rollout == nil || list[0].Rollout.State != "open" {
		t.Errorf("list %+v", list)
	}

	// A canary offered the build the most times it is, which still reports
	// the old version, stalls the rollout.
	if err := ta.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwModel: "xiao_epaper_display", FwVersion: "1.8.17", ID: dev.ID}); err != nil {
		t.Fatal(err)
	}
	for range firmware.MaxOffers {
		err := ta.db.Write.RecordFirmwareOffer(t.Context(), db.RecordFirmwareOfferParams{DeviceID: dev.ID, FirmwareID: fw.ID, LastOfferedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	ta.call(t, "GET", "/api/v1/firmware", "", 200, &list)
	if len(list) != 1 || list[0].Rollout == nil || list[0].Rollout.State != "stalled" ||
		list[0].Rollout.Canary != testFriend || list[0].Rollout.CanaryVersion != "1.8.17" || list[0].Rollout.LastOfferedAt != 1 {
		t.Errorf("list %+v", list)
	}
	// Restarting it with the same canary is the retry.
	ta.call(t, "POST", rollout, `{"canary": "`+testFriend+`"}`, 200, &fw)
	if fw.Rollout == nil || fw.Rollout.State != "canary" || fw.Rollout.LastOfferedAt != 0 {
		t.Errorf("rollout %+v", fw.Rollout)
	}

	ta.call(t, "DELETE", rollout, "", 204, nil)
	ta.call(t, "GET", "/api/v1/firmware", "", 200, &list)
	if list[0].Rollout == nil || list[0].Rollout.State != "canceled" {
		t.Errorf("list %+v", list)
	}
	ta.wantError(t, "DELETE", rollout, "", 400, "no active rollout")

	ta.wantError(t, "POST", rollout, `{}`, 400, "canary is required")
	ta.wantError(t, "POST", rollout, `{"canary": "ZZZ999"}`, 404, "no such device")
	ta.wantError(t, "POST", rollout, `{"canary": "x", "other": 1}`, 400, "unknown field")
	ta.wantError(t, "POST", "/api/v1/firmware/999/rollout", `{"canary": "`+testFriend+`"}`, 404, "no such firmware")
	ta.wantError(t, "POST", "/api/v1/firmware/abc/rollout", `{"canary": "`+testFriend+`"}`, 404, "no such firmware")
	ta.wantError(t, "DELETE", "/api/v1/firmware/999/rollout", "", 404, "no such firmware")
}

func TestPatchDevice(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ta.addDevice(t)
	var d deviceJSON
	ta.call(t, "PATCH", "/api/v1/devices/"+testFriend, `{"updates_enabled": true, "fw_pin": "1.08.10"}`, 200, &d)
	if !d.UpdatesEnabled || d.FWPin != "1.8.10" || d.ID != testFriend {
		t.Errorf("device %+v", d)
	}
	// A field left out is kept.
	ta.call(t, "PATCH", "/api/v1/devices/"+testFriend, `{"updates_enabled": false}`, 200, &d)
	if d.UpdatesEnabled || d.FWPin != "1.8.10" {
		t.Errorf("device %+v", d)
	}
	var got deviceJSON
	ta.call(t, "GET", "/api/v1/devices/"+testFriend, "", 200, &got)
	if got.UpdatesEnabled || got.FWPin != "1.8.10" {
		t.Errorf("stored %+v", got)
	}
	ta.wantError(t, "PATCH", "/api/v1/devices/"+testFriend, `{"fw_pin": "latest"}`, 400, "dotted numbers")
	ta.wantError(t, "PATCH", "/api/v1/devices/"+testFriend, `{"nickname": "x"}`, 400, "unknown field")
	ta.wantError(t, "PATCH", "/api/v1/devices/"+testFriend, ``, 400, "bad request body")
	ta.wantError(t, "PATCH", "/api/v1/devices/ZZZ999", `{"updates_enabled": true}`, 404, "no such device")
}

func TestPatchDeviceName(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ta.addDevice(t)
	path := "/api/v1/devices/" + testFriend
	var d deviceJSON
	ta.call(t, "PATCH", path, `{"name": "  Kitchen "}`, 200, &d)
	if d.Name != "Kitchen" {
		t.Errorf("name %q, want Kitchen", d.Name)
	}
	// A field left out is kept, and the name stays.
	ta.call(t, "PATCH", path, `{"updates_enabled": true}`, 200, &d)
	if d.Name != "Kitchen" || !d.UpdatesEnabled {
		t.Errorf("device %+v", d)
	}
	// A bad name changes nothing, the other fields included.
	ta.wantError(t, "PATCH", path, `{"name": "`+strings.Repeat("x", 65)+`", "updates_enabled": false}`, 400, "name is over 64 bytes")
	ta.call(t, "GET", path, "", 200, &d)
	if d.Name != "Kitchen" || !d.UpdatesEnabled {
		t.Errorf("a refused patch changed the device: %+v", d)
	}
	ta.wantError(t, "PATCH", path, `{"name": "a\nb"}`, 400, "control character")
	ta.call(t, "PATCH", path, `{"name": ""}`, 200, &d)
	if d.Name != "" {
		t.Errorf("name %q, want none", d.Name)
	}
}
