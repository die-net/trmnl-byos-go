package device

import "slices"

// The commands a single button press can be set to do. They move the
// playlist cursor, and playlist.Advance carries them out.
const (
	// ButtonNext shows the next item that is shown now.
	ButtonNext = "next"
	// ButtonPrevious shows the item before the current one among the items
	// shown now, wrapping to the last.
	ButtonPrevious = "previous"
	// ButtonFirst shows the first item that is shown now.
	ButtonFirst = "first"
	// ButtonRefresh stays on the current item, so the device picks up a
	// newer render of it.
	ButtonRefresh = "refresh"
)

// ButtonCommands are the values a device's button command may be set to.
var ButtonCommands = []string{ButtonNext, ButtonPrevious, ButtonFirst, ButtonRefresh}

// The special functions a double-click can be set to do, named as the
// firmware names them.
const (
	// FuncNone turns the double-click off.
	FuncNone = "none"
	// FuncIdentify draws a screen that says which device this is.
	FuncIdentify = "identify"
	// FuncRestartPlaylist goes back to the first item shown now.
	FuncRestartPlaylist = "restart_playlist"
	// FuncRewind redraws the device's previous image.
	FuncRewind = "rewind"
	// FuncSleep puts the device to sleep until its sleep window ends.
	FuncSleep = "sleep"
	// FuncAddWiFi has the device open its Wi-Fi setup portal.
	FuncAddWiFi = "add_wifi"
)

// FuncSleepSeconds is how long, in seconds, a device that is told to sleep
// by FuncSleep sleeps when it has no sleep window: 8 hours, a night's sleep.
// The design did not name a number, so this is a choice, not a firmware
// limit. /api/display sends it, and the device page says it.
const FuncSleepSeconds int64 = 8 * 60 * 60

// SpecialFunctions are the values a device's special function may be set
// to. The firmware also has send_to_me, guest_mode and toggle_hold, which
// are TRMNL cloud features, so they cannot be set.
var SpecialFunctions = []string{FuncNone, FuncIdentify, FuncRestartPlaylist, FuncRewind, FuncSleep, FuncAddWiFi}

// ButtonCommandOf returns the button command in effect for a device whose
// button_command setting is stored: stored, or ButtonNext when it is empty.
// A value that is not one of ButtonCommands, which a release that drops a
// command can cause, gives ButtonNext, with one warning.
func ButtonCommandOf(stored string) string {
	if stored == "" {
		return ButtonNext
	}
	if !slices.Contains(ButtonCommands, stored) {
		warnOnce("button_command", stored, "device: stored button command is not one of the commands; using next")
		return ButtonNext
	}
	return stored
}

// SpecialFunctionOf returns the special function in effect for a device whose
// special_function setting is stored: stored, or FuncRewind when it is empty.
// A value that is not one of SpecialFunctions gives FuncRewind, with one
// warning.
func SpecialFunctionOf(stored string) string {
	if stored == "" {
		return FuncRewind
	}
	if !slices.Contains(SpecialFunctions, stored) {
		warnOnce("special_function", stored, "device: stored special function is not one of the functions; using rewind")
		return FuncRewind
	}
	return stored
}
