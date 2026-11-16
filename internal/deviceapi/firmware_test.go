package deviceapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
)

// testFirmwareModel is the model displayHeaders reports.
const testFirmwareModel = "xiao_epaper_display"

// testFirmwareVersion is the version of the builds the tests add.
const testFirmwareVersion = "1.8.19"

// addFirmware puts a build of the test model in the catalog,
// with a file of the given bytes, and returns its row. The bytes are not a
// real image, since the offer and download paths do not parse one.
func (ts *testServer) addFirmware(t *testing.T, body []byte) db.Firmware {
	t.Helper()
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(ts.fwDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firmware.Path(ts.fwDir, sha), body, 0o600); err != nil {
		t.Fatal(err)
	}
	fw, err := ts.db.Write.CreateFirmware(t.Context(), db.CreateFirmwareParams{
		FwModel: testFirmwareModel, Version: testFirmwareVersion, Sha256: sha, Chip: "esp32s3", IdfVersion: "v5.3.2",
		Bytes: int64(len(body)), Source: firmware.SourceUpload, UploadedAt: ts.now.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return fw
}

// firmwareDisplayHeaders is displayHeaders for a device on the given
// firmware version.
func firmwareDisplayHeaders(token, version string) map[string]string {
	h := displayHeaders(testMAC, token)
	h["FW-Version"] = version
	return h
}

func TestDisplayOffersFirmware(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	token := ts.register(t)
	body := []byte("pretend this is a firmware image")
	fw := ts.addFirmware(t, body)

	// Before anything is asked for, no device is offered firmware, and the
	// response does not carry a firmware_url at all.
	m := ts.getJSON(t, "/api/display", firmwareDisplayHeaders(token, "1.8.17"))
	if m["update_firmware"] != false || m["firmware_url"] != nil {
		t.Fatalf("no rollout: %v", m)
	}

	// A rollout alone is not enough: the device has updates off.
	if _, err := firmware.StartRollout(t.Context(), ts.db, fw.ID, testMAC, ts.now); err == nil {
		t.Fatal("rollout started for a canary with updates off")
	}
	if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{UpdatesEnabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	m = ts.getJSON(t, "/api/display", firmwareDisplayHeaders(token, "1.8.17"))
	if m["update_firmware"] != false {
		t.Fatalf("no rollout yet: %v", m)
	}
	if _, err := firmware.StartRollout(t.Context(), ts.db, fw.ID, testMAC, ts.now); err != nil {
		t.Fatal(err)
	}

	m = ts.getJSON(t, "/api/display", firmwareDisplayHeaders(token, "1.8.17"))
	url, _ := m["firmware_url"].(string)
	if m["update_firmware"] != true || url != ts.URL+"/firmware/"+fw.Sha256+".bin" || m["status"] != float64(statusOK) {
		t.Fatalf("canary: %v", m)
	}
	// The image is still sent: the device draws it if the update fails.
	if m["image_url"] == "" || m["filename"] == "" {
		t.Errorf("an update response has no image: %v", m)
	}
	code, got := ts.do(t, http.MethodGet, strings.TrimPrefix(url, ts.URL), nil, nil)
	if code != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("download = %d, %q", code, got)
	}

	// The same version, an older device model's name, a newer device: none.
	for name, headers := range map[string]map[string]string{
		"same version":   firmwareDisplayHeaders(token, "1.8.19"),
		"newer version":  firmwareDisplayHeaders(token, "1.8.20"),
		"old firmware":   firmwareDisplayHeaders(token, "1.8.10"),
		"odd version":    firmwareDisplayHeaders(token, "1.8.17-dev"),
		"no version":     firmwareDisplayHeaders(token, ""),
		"another model":  withModel(firmwareDisplayHeaders(token, "1.8.17"), "xiao"),
		"no model":       withModel(firmwareDisplayHeaders(token, "1.8.17"), ""),
		"a model of two": withModel(firmwareDisplayHeaders(token, "1.8.17"), "og"),
	} {
		if m := ts.getJSON(t, "/api/display", headers); m["update_firmware"] != false || m["firmware_url"] != nil {
			t.Errorf("%s: offered firmware: %v", name, m)
		}
	}

	// Pinned, updates off, and a canceled rollout each stop the offer.
	for name, stop := range map[string]func(){
		"pinned": func() {
			if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{FwPin: ptr("1.8.17")}); err != nil {
				t.Fatal(err)
			}
		},
		"updates off": func() {
			if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{FwPin: ptr(""), UpdatesEnabled: ptr(false)}); err != nil {
				t.Fatal(err)
			}
		},
		"canceled": func() {
			if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{UpdatesEnabled: ptr(true)}); err != nil {
				t.Fatal(err)
			}
			if err := firmware.CancelRollout(t.Context(), ts.db, fw.ID); err != nil {
				t.Fatal(err)
			}
		},
	} {
		stop()
		if m := ts.getJSON(t, "/api/display", firmwareDisplayHeaders(token, "1.8.17")); m["update_firmware"] != false || m["firmware_url"] != nil {
			t.Errorf("%s: offered firmware: %v", name, m)
		}
	}
	// A canceled rollout's file is not served either.
	if code, _ := ts.do(t, http.MethodGet, "/firmware/"+fw.Sha256+".bin", nil, nil); code != http.StatusNotFound {
		t.Errorf("canceled rollout's file: HTTP %d, want 404", code)
	}
}

func ptr[T any](v T) *T { return &v }

func withModel(h map[string]string, v string) map[string]string {
	h["Model"] = v
	return h
}

// TestDisplayFirmwareOnlyToTheCanary checks that a second device is offered
// nothing until the canary reports the new version.
func TestDisplayFirmwareOnlyToTheCanary(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	canaryToken := ts.register(t)
	// A second device of the same model.
	const otherMAC = "D8:3B:DA:12:34:57"
	ts.openJoinWindow(t)
	setup := ts.getJSON(t, "/api/setup", setupHeaders(otherMAC))
	otherToken, _ := setup["api_key"].(string)
	if otherToken == "" {
		t.Fatalf("setup = %v", setup)
	}
	for _, mac := range []string{testMAC, otherMAC} {
		if _, err := device.Update(t.Context(), ts.db, mac, device.Change{UpdatesEnabled: ptr(true)}); err != nil {
			t.Fatal(err)
		}
	}
	fw := ts.addFirmware(t, []byte("image"))
	ask := func(mac, token, version string) bool {
		h := firmwareDisplayHeaders(token, version)
		h["ID"] = mac
		return ts.getJSON(t, "/api/display", h)["update_firmware"] == true
	}
	// Setup recorded 1.8.10. Each device reports its real version.
	ask(testMAC, canaryToken, "1.8.17")
	ask(otherMAC, otherToken, "1.8.17")
	if _, err := firmware.StartRollout(t.Context(), ts.db, fw.ID, testMAC, ts.now); err != nil {
		t.Fatal(err)
	}
	if ask(otherMAC, otherToken, "1.8.17") {
		t.Fatal("a device that is not the canary was offered the build")
	}
	if !ask(testMAC, canaryToken, "1.8.17") {
		t.Fatal("the canary was not offered the build")
	}
	// The canary comes back on the new version.
	if ask(testMAC, canaryToken, "1.8.19") {
		t.Fatal("the canary was offered the version it is on")
	}
	if !ask(otherMAC, otherToken, "1.8.17") {
		t.Fatal("another device was not offered the build after the canary reported it")
	}
}

func TestFirmwareDownloadRefusals(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ts.register(t)
	fw := ts.addFirmware(t, []byte("image"))
	// A file with no rollout, and an unknown hash that is a valid name.
	unknown := hex.EncodeToString(make([]byte, sha256.Size))
	for name, path := range map[string]string{
		"not in a rollout": "/firmware/" + fw.Sha256 + ".bin",
		"unknown hash":     "/firmware/" + unknown + ".bin",
		"no extension":     "/firmware/" + fw.Sha256,
		"upper case":       "/firmware/" + strings.ToUpper(fw.Sha256) + ".bin",
		"short":            "/firmware/" + fw.Sha256[:63] + ".bin",
		"traversal":        "/firmware/..%2F" + fw.Sha256[3:] + ".bin",
		"dot dot":          "/firmware/..",
		"png":              "/firmware/" + fw.Sha256 + ".png",
		"empty":            "/firmware/",
	} {
		if code, _ := ts.do(t, http.MethodGet, path, nil, nil); code != http.StatusNotFound {
			t.Errorf("%s: HTTP %d, want 404", name, code)
		}
	}
}

// TestFirmwareDownloadHeaders checks that the file is served by
// http.ServeContent, with its length, so a client can resume.
func TestFirmwareDownloadHeaders(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ts.register(t)
	if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{UpdatesEnabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("0123456789abcdef"), 1000)
	fw := ts.addFirmware(t, body)
	if err := ts.db.Write.StartFirmwareRollout(t.Context(), db.StartFirmwareRolloutParams{FirmwareID: fw.ID, CanaryDeviceID: 1, StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/firmware/"+fw.Sha256+".bin", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.ContentLength != int64(len(body)) || res.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("status %d, length %d, type %q", res.StatusCode, res.ContentLength, res.Header.Get("Content-Type"))
	}
}

// TestDisplayFirmwareOfferCap checks that a canary whose update never takes
// is told to update three times and then not again, however many times it
// wakes.
func TestDisplayFirmwareOfferCap(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	token := ts.register(t)
	if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{UpdatesEnabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	ts.getJSON(t, "/api/display", firmwareDisplayHeaders(token, "1.8.17"))
	fw := ts.addFirmware(t, []byte("mislabeled"))
	if _, err := firmware.StartRollout(t.Context(), ts.db, fw.ID, testMAC, ts.now); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		m := ts.getJSON(t, "/api/display", firmwareDisplayHeaders(token, "1.8.17"))
		if want := i <= firmware.MaxOffers; (m["update_firmware"] == true) != want || (m["firmware_url"] != nil) != want {
			t.Fatalf("wake %d: %v, want update_firmware %v", i, m, want)
		}
	}
	e, err := firmware.Get(t.Context(), ts.db.Read, fw.ID)
	if err != nil || e.Stage != "stalled" {
		t.Errorf("entry %+v, %v", e, err)
	}
}

// TestDisplayNoModelHeaderNoOffer checks that a request that changes the
// firmware version and sends no Model header is offered nothing, even
// though the stored model would qualify. The stored copy keeps its model.
func TestDisplayNoModelHeaderNoOffer(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	token := ts.register(t)
	fw := ts.addFirmware(t, []byte("image"))
	if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{UpdatesEnabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	// Record 1.8.17 as the device's version, so the next request changes it.
	ts.getJSON(t, "/api/display", firmwareDisplayHeaders(token, "1.8.17"))
	if _, err := firmware.StartRollout(t.Context(), ts.db, fw.ID, testMAC, ts.now); err != nil {
		t.Fatal(err)
	}
	m := ts.getJSON(t, "/api/display", withModel(firmwareDisplayHeaders(token, "1.8.18"), ""))
	if m["update_firmware"] != false || m["firmware_url"] != nil {
		t.Fatalf("offered firmware to a request with no Model: %v", m)
	}
	dev, err := device.FindDevice(t.Context(), ts.db.Read, testMAC)
	if err != nil {
		t.Fatal(err)
	}
	if dev.FwModel != testFirmwareModel || dev.FwVersion != "1.8.18" {
		t.Errorf("stored firmware = %q %q, want %q 1.8.18", dev.FwModel, dev.FwVersion, testFirmwareModel)
	}
}
