// Package view turns arbitrary bytes into something a browser can show: a
// decoded text window in a caller-chosen character encoding, or a hex+ASCII
// dump. It never opens files itself — callers hand it a reader positioned at
// the start of the stream (a real file or an archive entry) plus the window
// they want, which keeps it usable for non-seekable archive streams.
package view

import (
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

// EncodingInfo is one entry of the SPA's encoding picker.
type EncodingInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Encoding ids that get special handling rather than a table lookup.
const (
	Auto  = "auto"
	UTF8  = "utf-8"
	Fixed = "windows-1252" // fallback when detection fails
)

type entry struct {
	id    string
	label string
	enc   encoding.Encoding // nil for "auto" and "utf-8" (handled directly)
}

// table is the picker order: detection first, Unicode, then Western, Cyrillic,
// Greek, Turkish, Arabic, CJK and finally the legacy IBM code pages.
var table = []entry{
	{Auto, "Auto-detect", nil},
	{UTF8, "UTF-8", nil},
	{"utf-16le", "UTF-16 LE", unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)},
	{"utf-16be", "UTF-16 BE", unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)},
	{"utf-32le", "UTF-32 LE", utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM)},
	{"utf-32be", "UTF-32 BE", utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM)},
	{"iso-8859-1", "ISO-8859-1 (Latin-1)", charmap.ISO8859_1},
	{"iso-8859-15", "ISO-8859-15 (Latin-9)", charmap.ISO8859_15},
	{"windows-1252", "Windows-1252 (Western)", charmap.Windows1252},
	{"iso-8859-2", "ISO-8859-2 (Central European)", charmap.ISO8859_2},
	{"windows-1250", "Windows-1250 (Central European)", charmap.Windows1250},
	{"iso-8859-5", "ISO-8859-5 (Cyrillic)", charmap.ISO8859_5},
	{"windows-1251", "Windows-1251 (Cyrillic)", charmap.Windows1251},
	{"koi8-r", "KOI8-R (Cyrillic)", charmap.KOI8R},
	{"iso-8859-7", "ISO-8859-7 (Greek)", charmap.ISO8859_7},
	{"windows-1253", "Windows-1253 (Greek)", charmap.Windows1253},
	{"iso-8859-9", "ISO-8859-9 (Turkish)", charmap.ISO8859_9},
	{"windows-1254", "Windows-1254 (Turkish)", charmap.Windows1254},
	{"windows-1256", "Windows-1256 (Arabic)", charmap.Windows1256},
	{"macintosh", "Mac OS Roman", charmap.Macintosh},
	{"shift_jis", "Shift_JIS (Japanese)", japanese.ShiftJIS},
	{"euc-jp", "EUC-JP (Japanese)", japanese.EUCJP},
	{"iso-2022-jp", "ISO-2022-JP (Japanese)", japanese.ISO2022JP},
	{"euc-kr", "EUC-KR (Korean)", korean.EUCKR},
	{"gbk", "GBK (Simplified Chinese)", simplifiedchinese.GBK},
	{"gb18030", "GB18030 (Simplified Chinese)", simplifiedchinese.GB18030},
	{"big5", "Big5 (Traditional Chinese)", traditionalchinese.Big5},
	{"ibm037", "IBM037 (EBCDIC US)", charmap.CodePage037},
	{"ibm437", "IBM437 (DOS US)", charmap.CodePage437},
	{"ibm850", "IBM850 (DOS Western)", charmap.CodePage850},
}

// aliases maps common spellings onto canonical ids. Anything not listed here
// still gets a chance through the IANA registry.
var aliases = map[string]string{
	"":              Auto,
	"utf8":          UTF8,
	"utf-8-bom":     UTF8,
	"ascii":         UTF8,
	"us-ascii":      UTF8,
	"utf16le":       "utf-16le",
	"utf-16":        "utf-16le",
	"utf16":         "utf-16le",
	"ucs-2":         "utf-16le",
	"utf16be":       "utf-16be",
	"utf32le":       "utf-32le",
	"utf-32":        "utf-32le",
	"utf32be":       "utf-32be",
	"latin1":        "iso-8859-1",
	"latin-1":       "iso-8859-1",
	"iso8859-1":     "iso-8859-1",
	"iso-latin-1":   "iso-8859-1",
	"latin9":        "iso-8859-15",
	"cp1250":        "windows-1250",
	"cp1251":        "windows-1251",
	"cp1252":        "windows-1252",
	"cp1253":        "windows-1253",
	"cp1254":        "windows-1254",
	"cp1256":        "windows-1256",
	"koi8r":         "koi8-r",
	"mac-roman":     "macintosh",
	"macroman":      "macintosh",
	"x-mac-roman":   "macintosh",
	"sjis":          "shift_jis",
	"shift-jis":     "shift_jis",
	"cp932":         "shift_jis",
	"eucjp":         "euc-jp",
	"euckr":         "euc-kr",
	"cp936":         "gbk",
	"cp950":         "big5",
	"cp037":         "ibm037",
	"ebcdic":        "ibm037",
	"cp437":         "ibm437",
	"cp850":         "ibm850",
	"windows-31j":   "shift_jis",
	"iso-2022-jp-1": "iso-2022-jp",
}

var byID = func() map[string]entry {
	m := make(map[string]entry, len(table))
	for _, e := range table {
		m[e.id] = e
	}
	return m
}()

// Encodings returns the picker list in display order.
func Encodings() []EncodingInfo {
	out := make([]EncodingInfo, 0, len(table))
	for _, e := range table {
		out = append(out, EncodingInfo{ID: e.id, Label: e.label})
	}
	return out
}

// canonical normalises a user-supplied encoding id.
func canonical(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if a, ok := aliases[id]; ok {
		return a
	}
	return id
}

// lookup resolves an encoding id to a transformer. The bool reports whether
// the id is known at all; a nil encoding with ok==true means "handled without
// a transformer" (auto/utf-8).
func lookup(id string) (encoding.Encoding, bool) {
	if id == Auto || id == UTF8 {
		return nil, true
	}
	if e, ok := byID[id]; ok {
		return e.enc, true
	}
	// Long tail: let the IANA registry resolve names we do not list.
	if enc, err := ianaindex.IANA.Encoding(id); err == nil && enc != nil {
		return enc, true
	}
	if enc, err := ianaindex.MIME.Encoding(id); err == nil && enc != nil {
		return enc, true
	}
	return nil, false
}
