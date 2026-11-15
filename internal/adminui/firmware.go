package adminui

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
	"github.com/die-net/trmnl-byos-go/internal/upload"
)

// chooseFirmware is what an upload with no file is told.
const chooseFirmware = "Choose a firmware file to upload."

const (
	// firmwareField is the name of the form field that holds the build.
	firmwareField = "firmware"
	// maxFieldBytes bounds the model and version fields of the upload form,
	// which are short text.
	maxFieldBytes = 256
)

var noBuild = notFoundView{Nav: "firmware", What: "No such firmware build", BackHref: "/firmware", BackName: "Firmware"}

// Why an upload was refused before firmware.Add saw the file.
var (
	errNoFirmwareFile = errors.New("no firmware file")
	errNoModel        = errors.New("no model")
	errFieldTooLong   = errors.New("form field too long")
)

// badgeView is a status badge.
type badgeView struct {
	// Class is the badge's modifier class, such as "badge-ok".
	Class, Label string
}

// firmwareView is a firmware build and its rollout as a page shows them.
type firmwareView struct {
	ID int64
	// Model is the exact firmware model the build is for. A device is
	// offered the build only if it reports this model.
	Model string
	// ModelParts is Model split after each underscore, so a heading can
	// break a long model name there and not in the middle of a word.
	ModelParts []string
	Version    string
	Chip       string
	// Size and Uploaded are the file's size and the day it was added.
	Size, Uploaded string
	// SHA is the file's SHA-256, shown only when ShowSHA is set.
	SHA     string
	ShowSHA bool
	Badge   badgeView
	// Offer says in a sentence who is offered the build now.
	Offer string
	// HasRollout is whether the build has a rollout, which the fields
	// below describe.
	HasRollout bool
	// Canary links to the canary device.
	Canary linkView
	// CanaryReports is the version the canary last reported, or words for
	// none.
	CanaryReports string
	// LastOffered is when the canary was last offered the build, and
	// Started when the rollout started.
	LastOffered, Started string
	// Href is the build's page. StartLabel is the text of the link to it
	// that starts or restarts the rollout, and StartAria its accessible
	// name.
	Href, StartLabel, StartAria string
	// DevicesHref is the build page's list of devices, and DevicesAria the
	// accessible name of the link to it.
	DevicesHref, DevicesAria string
	// CanCancel is whether the rollout can be canceled. CancelHref is the
	// page that asks to, and CancelAria the link's accessible name.
	CanCancel              bool
	CancelHref, CancelAria string
}

// canaryOption is a device the canary can be chosen from.
type canaryOption struct {
	// Value is the friendly ID the form posts.
	Value, Label string
	Selected     bool
}

// deviceOfferView is one device of a build's model, as the build page
// lists it, from a firmware.DeviceOffer.
type deviceOfferView struct {
	// Name is the device's name, or its friendly ID, and Href its page.
	Name, Href string
	// Firmware says which version the device last reported.
	Firmware string
	// Canary is whether the device is the build's canary.
	Canary bool
	// Offered is whether the device is offered the build now. When it is
	// not, Why says why in a sentence, and InsteadHref is the newer build
	// it is offered, if that is the reason.
	Offered     bool
	Why         string
	InsteadHref string
	// Offers says how often the device was offered the build and when it
	// last was, or is "" if never.
	Offers string
}

// buildView is the page of one build.
type buildView struct {
	Firmware firmwareView
	// Restart is whether the build already has a rollout to restart.
	Restart bool
	// Open is whether the rollout is open to devices other than the canary,
	// which a restart undoes until the new canary reports the build.
	Open bool
	// Tries is how many offers a restart gives the canary.
	Tries int
	// Action is the path the rollout form posts to.
	Action string
	// Devices lists every device of the build's model. With none, the page
	// has no rollout form.
	Devices []deviceOfferView
	// Canaries are the devices that can be the canary. With none, there is
	// no form.
	Canaries []canaryOption
}

// cancelView is the page that asks to confirm canceling a rollout.
type cancelView struct {
	// Name is the build, such as "xiao_epaper_display 1.8.19".
	Name string
	// Active is whether there is a rollout to cancel. Without one the page
	// has no form.
	Active bool
	// Action is the path the Cancel button posts to, and Keep the page the
	// link goes back to.
	Action, Keep string
}

// buildHref is the path of a build's pages.
func buildHref(id int64) string {
	return "/firmware/builds/" + strconv.FormatInt(id, 10)
}

// catalogHref is the catalog's entry for a build.
func catalogHref(id int64) string {
	return "/firmware#build-" + strconv.FormatInt(id, 10)
}

// stageBadge is the badge for a rollout's stage.
func stageBadge(stage string) badgeView {
	switch stage {
	case "canary":
		return badgeView{"badge-warn", "Canary only"}
	case "open":
		return badgeView{"badge-ok", "Open"}
	case "stalled":
		return badgeView{"badge-warn", "Stalled"}
	case "canceled":
		return badgeView{"badge-neutral", "Canceled"}
	}
	return badgeView{"badge-neutral", "Not rolled out"}
}

// stageText says in a sentence where the build's rollout stands. It
// describes the stage only. Which devices are offered the build now depends
// on each device too, and firmware.DeviceOffers says that on the build's
// page.
func stageText(e *firmware.Entry) string {
	switch e.Stage {
	case "canary":
		return fmt.Sprintf("%s goes first, as the canary. The rollout opens to the other %s devices once %s reports %s or newer.", e.Canary, e.FwModel, e.Canary, e.Version)
	case "open":
		return fmt.Sprintf("%s reports %s, so the rollout is open to the other %s devices.", e.Canary, e.CanaryVersion, e.FwModel)
	case "stalled":
		return fmt.Sprintf("%s was offered this build %d times and still reports %s, so the rollout is stalled. No device is offered the build. Restart it with a canary, or cancel it.", e.Canary, firmware.MaxOffers, e.CanaryVersion)
	case "canceled":
		return "The rollout was canceled. The build is offered to no one and its file is not served."
	}
	return "Not rolled out, so it is offered to no device. A rollout starts with one device, the canary."
}

// firmwareViewOf describes a build for a page.
func (s *Server) firmwareViewOf(e *firmware.Entry) firmwareView {
	name := e.FwModel + " " + e.Version
	v := firmwareView{
		ID: e.ID, Model: e.FwModel, ModelParts: strings.SplitAfter(e.FwModel, "_"), Version: e.Version, Chip: e.Chip, Size: firmware.ByteSize(e.Bytes), SHA: e.Sha256,
		Uploaded: time.Unix(e.UploadedAt, 0).In(s.zone).Format(dayLayout),
		Badge:    stageBadge(e.Stage), Offer: stageText(e),
		Href: buildHref(e.ID), StartLabel: "Start rollout",
	}
	if e.Stage != "" {
		v.HasRollout = true
		v.Canary = linkView{Name: e.Canary, Href: "/devices/" + e.Canary}
		v.CanaryReports = e.CanaryVersion
		if v.CanaryReports == "" {
			v.CanaryReports = "Nothing yet"
		}
		v.LastOffered = "Never"
		if e.CanaryLastOfferedAt != 0 {
			v.LastOffered = time.Unix(e.CanaryLastOfferedAt, 0).In(s.zone).Format(timeLayout)
		}
		v.Started = time.Unix(e.StartedAt, 0).In(s.zone).Format(timeLayout)
	}
	if hasActiveRollout(e) {
		v.StartLabel = "Restart rollout"
		v.CanCancel = true
		v.CancelHref = v.Href + "/cancel"
		v.CancelAria = "Cancel rollout of " + name
	}
	v.StartAria = v.StartLabel + " of " + name
	v.DevicesHref = v.Href + "#devices"
	v.DevicesAria = "Devices for " + name
	return v
}

func (s *Server) firmwareCatalog(w http.ResponseWriter, r *http.Request) {
	s.renderFirmware(w, r, http.StatusOK, "", &uploadForm{})
}

// renderFirmware sends the catalog, with the upload form's values, and msg
// in the upload panel if it is not "".
func (s *Server) renderFirmware(w http.ResponseWriter, r *http.Request, status int, msg string, form *uploadForm) {
	entries, err := firmware.List(r.Context(), s.db.Read)
	if err != nil {
		serverError(w, r, "list firmware", err)
		return
	}
	views := make([]firmwareView, len(entries))
	for i := range entries {
		views[i] = s.firmwareViewOf(&entries[i])
	}
	s.render(w, r, status, "firmware", pageData{
		Nav: "firmware", Heading: "Firmware", UploadError: msg, UploadForm: form, Firmware: views,
		FirmwareModels: slices.Sorted(maps.Keys(firmware.Models)), UploadLimit: firmware.ByteSize(firmware.MaxUploadBytes()),
	})
}

// uploadForm is the values of the upload form, which a refused upload
// shows again. The file cannot be kept.
type uploadForm struct {
	Model, Version string
}

// textField reads a short text part of an upload form. A body that ends
// early is marked as cut off.
func textField(part *multipart.Part) (string, error) {
	var sb strings.Builder
	n, err := io.Copy(&sb, io.LimitReader(upload.Reader(part), maxFieldBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxFieldBytes {
		return "", errFieldTooLong
	}
	return strings.TrimSpace(sb.String()), nil
}

// firmwarePart walks the parts of the upload form in order and returns the
// part that holds the file, with the model and version that came before it
// in form: a browser sends them first, and the file is handed on without
// being held. It returns an error from reading the body: a
// *http.MaxBytesError for a body over its limit, or one wrapping
// upload.ErrCutOff for a body that ended early or is not a form. Any other
// part is skipped without being read.
func firmwarePart(mr *multipart.Reader, form *uploadForm) (*multipart.Part, error) {
	for {
		part, err := upload.NextPart(mr)
		switch {
		case err != nil:
			return nil, err
		case part == nil:
			return nil, errNoFirmwareFile
		}
		switch name := part.FormName(); {
		case name == firmwareField && part.FileName() == "":
			// A browser sends the field with no name or content when no
			// file was chosen.
			return nil, errNoFirmwareFile
		case name == firmwareField:
			if form.Model == "" {
				return nil, errNoModel
			}
			return part, nil
		case name == "model" || name == "version":
			text, err := textField(part)
			if err != nil {
				return nil, err
			}
			if name == "model" {
				form.Model = text
			} else {
				form.Version = text
			}
		}
	}
}

// uploadFirmware adds the build the upload form posts. The body is
// streamed: the file part goes straight to firmware.Add, which checks it
// and bounds it by its model's app partition, so it has the same checks as
// the JSON API. A body that ends early is the client's, not a server
// failure, so the file is read through upload.Reader, and an error from
// walking the parts goes through upload.CutOff.
func (s *Server) uploadFirmware(w http.ResponseWriter, r *http.Request) {
	upload.AllowSlow(w)
	// Room for the largest build and the multipart framing around it, which
	// is a few hundred bytes per field.
	r.Body = http.MaxBytesReader(w, r.Body, firmware.MaxUploadBytes()+maxFormBody)
	form := &uploadForm{}
	mr, err := r.MultipartReader()
	if err != nil {
		s.renderFirmware(w, r, http.StatusBadRequest, chooseFirmware, form)
		return
	}
	var fw db.Firmware
	part, err := firmwarePart(mr, form)
	if err == nil {
		fw, err = firmware.Add(r.Context(), s.db, s.firmwareDir, form.Model, form.Version, upload.Reader(part), s.now())
	}
	var limit *http.MaxBytesError
	switch {
	case err == nil:
		http.Redirect(w, r, catalogHref(fw.ID), http.StatusSeeOther)
	case errors.As(err, &limit):
		// The body's limit is the only one here: firmware.Add bounds the
		// file itself by its model's partition and says so in its own
		// words. So this is the form as a whole, whichever part tripped it.
		s.renderFirmware(w, r, http.StatusRequestEntityTooLarge, "The upload is too large.", form)
	case errors.Is(err, errNoFirmwareFile):
		s.renderFirmware(w, r, http.StatusBadRequest, chooseFirmware, form)
	case errors.Is(err, errNoModel):
		s.renderFirmware(w, r, http.StatusBadRequest, "Choose a model.", form)
	case errors.Is(err, errFieldTooLong):
		s.renderFirmware(w, r, http.StatusBadRequest, "The model or version is too long.", form)
	case errors.Is(err, upload.ErrCutOff):
		s.renderFirmware(w, r, http.StatusBadRequest, "The upload was cut off or could not be read.", form)
	case errors.Is(err, errkind.ErrTooBig):
		s.renderFirmware(w, r, http.StatusRequestEntityTooLarge, kindText(err), form)
	case errors.Is(err, errkind.ErrInvalid):
		s.renderFirmware(w, r, http.StatusBadRequest, kindText(err), form)
	case errors.Is(err, errkind.ErrConflict):
		s.renderFirmware(w, r, http.StatusConflict, kindText(err), form)
	default:
		serverError(w, r, "add firmware", err)
	}
}

// buildFromPath returns the build the path names with its rollout.
func (s *Server) buildFromPath(w http.ResponseWriter, r *http.Request) (firmware.Entry, bool) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noBuild)
		return firmware.Entry{}, false
	}
	e, err := firmware.Get(r.Context(), s.db.Read, id)
	switch {
	case err == nil:
		return e, true
	case errors.Is(err, firmware.ErrNoFirmware):
		s.renderNotFound(w, r, noBuild)
	default:
		serverError(w, r, "get firmware", err)
	}
	return firmware.Entry{}, false
}

func (s *Server) buildPage(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noBuild)
		return
	}
	s.renderBuild(w, r, http.StatusOK, id, "", "")
}

// renderBuild sends the page of the build with the given ID, with the
// canary chosen in the form, or the rollout's own canary if that is "", and
// an error message above the form if msg is not "". An unknown build gets
// the not-found page. The build and its devices are read in one snapshot,
// so the stage, the list and the canary choices agree. The list is one
// firmware.DeviceOffers, and the canary choices are the devices it says can
// be the canary.
func (s *Server) renderBuild(w http.ResponseWriter, r *http.Request, status int, id int64, canary, msg string) {
	var (
		e      firmware.Entry
		offers []firmware.DeviceOffer
	)
	err := s.db.ReadTx(r.Context(), func(q *db.Queries) error {
		var err error
		if e, err = firmware.Get(r.Context(), q, id); err != nil {
			return err
		}
		offers, err = firmware.DeviceOffers(r.Context(), q, s.firmwareDir, id)
		return err
	})
	if errors.Is(err, firmware.ErrNoFirmware) {
		s.renderNotFound(w, r, noBuild)
		return
	}
	if err != nil {
		serverError(w, r, "read firmware build", err)
		return
	}
	if canary == "" {
		canary = e.Canary
	}
	fv := s.firmwareViewOf(&e)
	fv.ShowSHA = true
	b := &buildView{
		Firmware: fv, Restart: fv.CanCancel, Open: e.Stage == "open", Tries: firmware.MaxOffers, Action: fv.Href + "/rollout",
		Devices: make([]deviceOfferView, len(offers)),
	}
	for i := range offers {
		o := &offers[i]
		name := cmp.Or(o.Name, o.FriendlyID)
		b.Devices[i] = s.deviceOfferViewOf(o, name)
		if o.CanBeCanary {
			label := name
			if o.Version != "" {
				label += " (firmware " + o.Version + ")"
			}
			b.Canaries = append(b.Canaries, canaryOption{Value: o.FriendlyID, Label: label, Selected: o.FriendlyID == canary})
		}
	}
	s.render(w, r, status, "firmware_build", pageData{Nav: "firmware", Heading: e.FwModel + " " + e.Version, Error: msg, Build: b})
}

// deviceOfferViewOf describes a device's standing for a build. It shows
// what the offer says and decides nothing.
func (s *Server) deviceOfferViewOf(o *firmware.DeviceOffer, name string) deviceOfferView {
	v := deviceOfferView{
		Name: name, Href: "/devices/" + o.FriendlyID, Firmware: "Firmware " + cmp.Or(o.Version, "unknown"),
		Canary: o.Canary, Offered: o.Reason == "",
	}
	if !v.Offered {
		v.Why = sentence(o.Why)
	}
	if o.Reason == firmware.ReasonNewerBuild {
		v.InsteadHref = buildHref(o.Instead)
	}
	if o.Offers > 0 {
		v.Offers = fmt.Sprintf("Offered %d of %d times", o.Offers, firmware.MaxOffers)
		if o.LastOfferedAt != 0 {
			v.Offers += ", last " + time.Unix(o.LastOfferedAt, 0).In(s.zone).Format(timeLayout)
		}
	}
	return v
}

// startRollout starts or restarts a build's rollout with the canary the
// form chose. firmware.StartRollout decides whether the device can be the
// canary, and its reason is shown when it cannot.
func (s *Server) startRollout(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noBuild)
		return
	}
	if !parseForm(w, r, func(status int, msg string) { s.renderBuild(w, r, status, id, "", msg) }) {
		return
	}
	canary := strings.TrimSpace(r.PostForm.Get("canary"))
	if canary == "" {
		s.renderBuild(w, r, http.StatusBadRequest, id, "", "Choose a canary device.")
		return
	}
	_, err := firmware.StartRollout(r.Context(), s.db, id, canary, s.now())
	switch {
	case err == nil:
		http.Redirect(w, r, catalogHref(id), http.StatusSeeOther)
	case errors.Is(err, firmware.ErrNoFirmware):
		s.renderNotFound(w, r, noBuild)
	case errors.Is(err, errkind.ErrNotFound):
		s.renderBuild(w, r, http.StatusBadRequest, id, canary, "That device no longer exists.")
	case errors.Is(err, errkind.ErrInvalid):
		s.renderBuild(w, r, http.StatusBadRequest, id, canary, kindText(err))
	default:
		serverError(w, r, "start rollout", err)
	}
}

// renderCancel sends the confirm page for canceling a build's rollout,
// which has no Cancel button when active is false.
func (s *Server) renderCancel(w http.ResponseWriter, r *http.Request, status int, e *firmware.Entry, active bool) {
	s.render(w, r, status, "firmware_cancel", pageData{Nav: "firmware", Heading: "Cancel rollout", Cancel: &cancelView{
		Name: e.FwModel + " " + e.Version, Active: active, Action: buildHref(e.ID) + "/cancel", Keep: catalogHref(e.ID),
	}})
}

// hasActiveRollout reports whether the build has a rollout that can be
// canceled.
func hasActiveRollout(e *firmware.Entry) bool {
	return e.Stage != "" && e.Stage != "canceled"
}

// confirmCancelRollout asks whether to cancel a build's rollout. It changes
// nothing.
func (s *Server) confirmCancelRollout(w http.ResponseWriter, r *http.Request) {
	e, ok := s.buildFromPath(w, r)
	if !ok {
		return
	}
	s.renderCancel(w, r, http.StatusOK, &e, hasActiveRollout(&e))
}

// cancelRollout cancels a build's rollout.
func (s *Server) cancelRollout(w http.ResponseWriter, r *http.Request) {
	e, ok := s.buildFromPath(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r, func(status int, _ string) { s.renderCancel(w, r, status, &e, hasActiveRollout(&e)) }) {
		return
	}
	err := firmware.CancelRollout(r.Context(), s.db, e.ID)
	switch {
	case err == nil:
		http.Redirect(w, r, catalogHref(e.ID), http.StatusSeeOther)
	case errors.Is(err, firmware.ErrNoFirmware):
		s.renderNotFound(w, r, noBuild)
	case errors.Is(err, errkind.ErrInvalid):
		// Someone canceled it first.
		s.renderCancel(w, r, http.StatusConflict, &e, false)
	default:
		serverError(w, r, "cancel rollout", err)
	}
}
