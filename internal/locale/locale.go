// Package locale lists the locales a render can be given.
package locale

import "slices"

// Default is the locale of a server whose locale setting has no row.
const Default = "en"

// codes are the locales trmnlp has translations for: the names of the files
// in db/data/locales of trmnlp v0.34.0, without raw. The server pins v0.34.1,
// which has the same list. A recipe's `trmnl.user.locale` is one of these.
var codes = []string{
	"cs", "da", "de", "de-AT", "en", "en-AU", "en-GB", "es-ES", "fr", "he",
	"hu", "id", "is", "it", "ja", "ko", "lt", "nl", "no", "pl", "pt-BR",
	"ro", "ru", "sk", "sv", "uk", "zh-CN", "zh-HK",
}

// Codes returns the locales, in a fixed order. The slice is a copy.
func Codes() []string {
	return slices.Clone(codes)
}

// Valid reports whether code is one of the locales. It is case sensitive,
// as trmnlp is.
func Valid(code string) bool {
	return slices.Contains(codes, code)
}
