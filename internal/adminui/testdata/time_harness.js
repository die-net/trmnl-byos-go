// Runs static/time.js against a stand-in for a page and prints what it
// wrote, as JSON. The script's path is the argument. The page is JSON on
// standard input: "device" is the device's zone for the zone line, "times"
// is each time as "KIND=INSTANT", and "noIntl" runs the script as in a
// browser that has no Intl.
"use strict";

var fs = require("fs");
var vm = require("vm");

var script = process.argv[2];
var page = JSON.parse(fs.readFileSync(0, "utf8"));
var args = page.times || [];
var deviceZone = page.device;
var noIntl = !!page.noIntl;

function element(attrs) {
  return {
    attrs: attrs,
    textContent: "server text",
    title: "",
    hidden: true,
    getAttribute: function (name) {
      return this.attrs[name];
    }
  };
}

var times = args.map(function (arg) {
  var at = arg.indexOf("=");
  return element({ "data-local": arg.slice(0, at), datetime: arg.slice(at + 1) });
});
var note = element({ "data-device-zone": deviceZone });

var context = {
  document: {
    querySelectorAll: function () {
      return times;
    },
    querySelector: function () {
      return note;
    }
  },
  Date: Date,
  isNaN: isNaN,
  String: String
};
// A new context has its own Intl unless it is taken away.
context.Intl = noIntl ? undefined : Intl;
vm.runInNewContext(fs.readFileSync(script, "utf8"), context);

console.log(JSON.stringify({
  times: times.map(function (el) {
    return { text: el.textContent, title: el.title };
  }),
  note: note.textContent,
  hidden: note.hidden
}));
