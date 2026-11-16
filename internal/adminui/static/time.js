// Writes each time on the page again in the browser's time zone.
//
// The server writes a time as <time datetime="..." data-local="KIND"> with
// the instant in UTC and, as its text, the time in the server's zone, which
// is what shows without this script. KIND says how the text is written:
// "full" is "2 Jan 2006 15:04 MST", "clock" is "15:04 MST", "day" is
// "2 Jan 2006", and "title" leaves the text, a relative time, and writes the
// full form into the title.
//
// A device's page also has a line with data-device-zone, the device's time
// zone. It is written again to name the browser's zone, and hidden when the
// two keep the same time.
(function () {
  "use strict";

  var months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

  function two(n) {
    return n < 10 ? "0" + n : String(n);
  }

  // One formatter serves every time on the page. It is null in a browser
  // that cannot make it, where a time is written without its zone's name.
  var zoneFormat = null;
  try {
    zoneFormat = new Intl.DateTimeFormat("en-US", { timeZoneName: "short" });
  } catch (e) {
    zoneFormat = null;
  }

  // zoneName is the browser's short name for its zone at d, such as "PST"
  // or "GMT+1", or "" where the browser cannot say.
  function zoneName(d) {
    if (!zoneFormat || !zoneFormat.formatToParts) {
      return "";
    }
    var parts = zoneFormat.formatToParts(d);
    for (var i = 0; i < parts.length; i++) {
      if (parts[i].type === "timeZoneName") {
        return parts[i].value;
      }
    }
    return "";
  }

  function day(d) {
    return d.getDate() + " " + months[d.getMonth()] + " " + d.getFullYear();
  }

  function clock(d) {
    var zone = zoneName(d);
    return two(d.getHours()) + ":" + two(d.getMinutes()) + (zone ? " " + zone : "");
  }

  function sameDay(a, b) {
    return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  }

  function rewrite(el, now) {
    var d = new Date(el.getAttribute("datetime"));
    if (isNaN(d.getTime())) {
      return;
    }
    switch (el.getAttribute("data-local")) {
      case "full":
        el.textContent = day(d) + " " + clock(d);
        break;
      case "clock":
        // The server left the day out because it is today there. It may not
        // be today here.
        el.textContent = sameDay(d, now) ? clock(d) : day(d) + " " + clock(d);
        break;
      case "day":
        el.textContent = day(d);
        break;
      case "title":
        el.title = day(d) + " " + clock(d);
        break;
    }
  }

  var now = new Date();
  var times = document.querySelectorAll("time[data-local]");
  for (var i = 0; i < times.length; i++) {
    try {
      rewrite(times[i], now);
    } catch (e) {
      // This time keeps the server's text, and the rest are still written.
    }
  }

  // wallClock is the date and time of day in the zone at d, as text to
  // compare. It throws for a zone the browser does not know.
  function wallClock(zone, d) {
    return new Intl.DateTimeFormat("en-US", {
      timeZone: zone, year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric", hourCycle: "h23"
    }).format(d);
  }

  // sameZone reports whether two zone names keep the same time: now and at
  // two more times of the year, so two names for one zone, such as
  // Asia/Kolkata and Asia/Calcutta, are the same, and two zones that agree
  // only for part of the year are not.
  function sameZone(a, b) {
    if (a === b) {
      return true;
    }
    try {
      for (var k = 0; k < 3; k++) {
        var d = new Date(now.getTime() + k * 120 * 24 * 3600 * 1000);
        if (wallClock(a, d) !== wallClock(b, d)) {
          return false;
        }
      }
      return true;
    } catch (e) {
      return false;
    }
  }

  var note = document.querySelector("[data-device-zone]");
  if (note) {
    var device = note.getAttribute("data-device-zone");
    var here = "";
    try {
      here = new Intl.DateTimeFormat().resolvedOptions().timeZone || "";
    } catch (e) {
      // An old browser, which cannot name its zone.
    }
    var schedules = " Schedules are read in this device's time zone, " + device + ".";
    if (here) {
      note.textContent = "The times above are in " + here + ", this browser's time zone." + schedules;
      note.hidden = sameZone(here, device);
    } else {
      // The times above are no longer in the server's zone, so the line
      // must not say they are.
      note.textContent = "The times above are in this browser's time zone." + schedules;
      note.hidden = false;
    }
  }
})();
