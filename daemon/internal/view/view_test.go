package view

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"golang.org/x/text/transform"

	"unraid-filebrowser/internal/types"
)

// encodeWith produces test bytes in a given encoding using x/text's encoder,
// so the round-trip tests exercise the real tables rather than hand-typed hex.
func encodeWith(t *testing.T, id, s string) []byte {
	t.Helper()
	enc, ok := lookup(id)
	if !ok || enc == nil {
		t.Fatalf("lookup(%q): not an encodable id", id)
	}
	b, _, err := transform.Bytes(enc.NewEncoder(), []byte(s))
	if err != nil {
		t.Fatalf("encode %q as %s: %v", s, id, err)
	}
	return b
}

func decodeAll(t *testing.T, b []byte, id string) types.ViewResult {
	t.Helper()
	res, err := Decode(bytes.NewReader(b), int64(len(b)), id, 0, 1<<20)
	if err != nil {
		t.Fatalf("Decode(%s): %v", id, err)
	}
	return res
}

func TestEncodingsList(t *testing.T) {
	list := Encodings()
	if len(list) < 25 {
		t.Fatalf("expected at least 25 encodings, got %d", len(list))
	}
	if list[0].ID != Auto {
		t.Errorf("first picker entry = %q, want %q", list[0].ID, Auto)
	}
	seen := map[string]bool{}
	for _, e := range list {
		if e.Label == "" {
			t.Errorf("encoding %q has no label", e.ID)
		}
		if seen[e.ID] {
			t.Errorf("duplicate encoding id %q", e.ID)
		}
		seen[e.ID] = true
		if _, ok := lookup(e.ID); !ok {
			t.Errorf("listed encoding %q does not resolve", e.ID)
		}
	}
	for _, want := range []string{
		"utf-8", "utf-16le", "utf-16be", "utf-32le", "utf-32be",
		"iso-8859-1", "iso-8859-2", "iso-8859-5", "iso-8859-7", "iso-8859-9", "iso-8859-15",
		"windows-1250", "windows-1251", "windows-1252", "windows-1253", "windows-1254",
		"windows-1256", "koi8-r", "macintosh", "shift_jis", "euc-jp", "iso-2022-jp",
		"euc-kr", "gbk", "gb18030", "big5", "ibm037", "ibm437", "ibm850",
	} {
		if !seen[want] {
			t.Errorf("required encoding %q missing from picker", want)
		}
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	cases := []struct{ id, text string }{
		{"utf-8", "hello — grüße"},
		{"iso-8859-1", "Grüße aus München"},
		{"iso-8859-15", "Preis: 20€"},
		{"iso-8859-2", "Příliš žluťoučký kůň"},
		{"iso-8859-5", "Привет мир"},
		{"iso-8859-7", "Καλημέρα κόσμε"},
		{"iso-8859-9", "Türkçe ğüşiöç"},
		{"windows-1250", "Příliš žluťoučký"},
		{"windows-1251", "Привет мир"},
		{"windows-1252", "Grüße — café"},
		{"windows-1253", "Καλημέρα"},
		{"windows-1254", "Türkçe"},
		{"windows-1256", "مرحبا بالعالم"},
		{"koi8-r", "Привет мир"},
		{"macintosh", "Café résumé"},
		{"shift_jis", "こんにちは世界"},
		{"euc-jp", "こんにちは世界"},
		{"iso-2022-jp", "こんにちは世界"},
		{"euc-kr", "안녕하세요"},
		{"gbk", "你好世界"},
		{"gb18030", "你好世界"},
		{"big5", "你好世界"},
		{"ibm037", "HELLO ebcdic 123"},
		{"ibm437", "DOS text abc"},
		{"ibm850", "DOS Grüße"},
		{"utf-16le", "hello 世界"},
		{"utf-16be", "hello 世界"},
		{"utf-32le", "hello 世界"},
		{"utf-32be", "hello 世界"},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			var raw []byte
			if c.id == "utf-8" {
				raw = []byte(c.text)
			} else {
				raw = encodeWith(t, c.id, c.text)
			}
			res := decodeAll(t, raw, c.id)
			if res.Text != c.text {
				t.Errorf("round trip mismatch\n got: %q\nwant: %q", res.Text, c.text)
			}
			if res.Encoding != c.id {
				t.Errorf("Encoding = %q, want %q", res.Encoding, c.id)
			}
			if res.Lossy {
				t.Errorf("clean round trip reported lossy")
			}
			if res.Truncated {
				t.Errorf("whole file reported truncated")
			}
			if res.Length != int64(len(raw)) {
				t.Errorf("Length = %d, want %d", res.Length, len(raw))
			}
		})
	}
}

func TestDecodeAutoBOM(t *testing.T) {
	cases := []struct {
		name    string
		raw     []byte
		sniffed string
		want    string
	}{
		{"utf8-bom", append([]byte{0xEF, 0xBB, 0xBF}, []byte("héllo")...), "utf-8", "héllo"},
		{"utf16le-bom", append([]byte{0xFF, 0xFE}, encodeWith(t, "utf-16le", "héllo")...), "utf-16le", "héllo"},
		{"utf16be-bom", append([]byte{0xFE, 0xFF}, encodeWith(t, "utf-16be", "héllo")...), "utf-16be", "héllo"},
		{"utf32le-bom", append([]byte{0xFF, 0xFE, 0x00, 0x00}, encodeWith(t, "utf-32le", "héllo")...), "utf-32le", "héllo"},
		{"utf32be-bom", append([]byte{0x00, 0x00, 0xFE, 0xFF}, encodeWith(t, "utf-32be", "héllo")...), "utf-32be", "héllo"},
		{"plain-utf8", []byte("héllo"), "utf-8", "héllo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := decodeAll(t, c.raw, Auto)
			if res.Sniffed != c.sniffed {
				t.Errorf("Sniffed = %q, want %q", res.Sniffed, c.sniffed)
			}
			if res.Encoding != c.sniffed {
				t.Errorf("Encoding = %q, want %q (auto follows the sniff)", res.Encoding, c.sniffed)
			}
			if res.Text != c.want {
				t.Errorf("Text = %q, want %q (BOM must be stripped)", res.Text, c.want)
			}
			if res.Length != int64(len(c.raw)) {
				t.Errorf("Length = %d, want %d (BOM counts as consumed)", res.Length, len(c.raw))
			}
		})
	}
}

func TestDecodeAutoFallsBackToWindows1252(t *testing.T) {
	raw := encodeWith(t, "windows-1252", "Grüße")
	res := decodeAll(t, raw, Auto)
	if res.Sniffed != Fixed {
		t.Fatalf("Sniffed = %q, want %q", res.Sniffed, Fixed)
	}
	if res.Text != "Grüße" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestDecodeSniffedReportedWhenForced(t *testing.T) {
	raw := []byte("héllo") // valid UTF-8
	res := decodeAll(t, raw, "iso-8859-1")
	if res.Encoding != "iso-8859-1" {
		t.Errorf("Encoding = %q, want the forced one", res.Encoding)
	}
	if res.Sniffed != UTF8 {
		t.Errorf("Sniffed = %q, want utf-8 even though the user forced latin-1", res.Sniffed)
	}
	if res.Text == "héllo" {
		t.Errorf("forcing latin-1 should mojibake the UTF-8 bytes, got %q", res.Text)
	}
}

func TestDecodeBinaryAsLatin1NeverErrors(t *testing.T) {
	raw := make([]byte, 256)
	for i := range raw {
		raw[i] = byte(i)
	}
	res, err := Decode(bytes.NewReader(raw), int64(len(raw)), "iso-8859-1", 0, 1024)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := len([]rune(res.Text)); got != 256 {
		t.Errorf("decoded %d runes, want 256", got)
	}
	if res.Lossy {
		t.Errorf("latin-1 maps every byte; lossy must be false")
	}
	if res.Length != 256 {
		t.Errorf("Length = %d, want 256", res.Length)
	}
}

func TestDecodeLossyMarksReplacements(t *testing.T) {
	// 0x81 is undefined in windows-1252.
	res := decodeAll(t, []byte{'a', 0x81, 'b'}, "windows-1252")
	if !res.Lossy {
		t.Errorf("undefined byte should produce a lossy window: %q", res.Text)
	}
	res = decodeAll(t, []byte{'a', 0xFF, 'b'}, UTF8)
	if !res.Lossy {
		t.Errorf("invalid UTF-8 should produce a lossy window: %q", res.Text)
	}
	if res.Text != "a�b" {
		t.Errorf("Text = %q, want a<U+FFFD>b", res.Text)
	}
}

func TestDecodeWindowing(t *testing.T) {
	raw := []byte("0123456789abcdef")
	res, err := Decode(bytes.NewReader(raw), int64(len(raw)), UTF8, 4, 5)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Text != "45678" {
		t.Errorf("Text = %q, want 45678", res.Text)
	}
	if res.Offset != 4 || res.Length != 5 {
		t.Errorf("Offset/Length = %d/%d, want 4/5", res.Offset, res.Length)
	}
	if !res.Truncated {
		t.Errorf("Truncated should be true, 7 bytes remain")
	}

	// Tail window: nothing left after it.
	res, err = Decode(bytes.NewReader(raw), int64(len(raw)), UTF8, 12, 100)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Text != "cdef" || res.Truncated {
		t.Errorf("tail window = %q truncated=%v", res.Text, res.Truncated)
	}

	// Past EOF: empty, not an error.
	res, err = Decode(bytes.NewReader(raw), int64(len(raw)), UTF8, 500, 100)
	if err != nil {
		t.Fatalf("Decode past EOF: %v", err)
	}
	if res.Text != "" || res.Length != 0 {
		t.Errorf("past-EOF window = %q length=%d", res.Text, res.Length)
	}
}

func TestDecodeCutsOnCodePointBoundary(t *testing.T) {
	raw := []byte("aé") // 'é' is two bytes
	res, err := Decode(bytes.NewReader(raw), int64(len(raw)), UTF8, 0, 2)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Text != "a" {
		t.Errorf("Text = %q, want %q (half a rune must not be emitted)", res.Text, "a")
	}
	if res.Length != 1 {
		t.Errorf("Length = %d, want 1 so the next window resumes at the rune", res.Length)
	}
	if res.Lossy {
		t.Errorf("a boundary cut is not lossy")
	}
	if !res.Truncated {
		t.Errorf("Truncated should be true")
	}

	// Same for UTF-16: an odd trailing byte is left for the next window.
	u16 := encodeWith(t, "utf-16le", "ab")
	res, err = Decode(bytes.NewReader(u16), int64(len(u16)), "utf-16le", 0, 3)
	if err != nil {
		t.Fatalf("Decode utf-16le: %v", err)
	}
	if res.Text != "a" || res.Length != 2 {
		t.Errorf("utf-16 window = %q length=%d, want %q/2", res.Text, res.Length, "a")
	}
}

func TestDecodeUnknownEncoding(t *testing.T) {
	_, err := Decode(bytes.NewReader([]byte("x")), 1, "klingon-1", 0, 16)
	var apiErr *types.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != types.ErrEncoding {
		t.Fatalf("err = %v, want ENCODING_ERROR", err)
	}
}

func TestDecodeAliasesAndBadParams(t *testing.T) {
	res := decodeAll(t, []byte("abc"), "LATIN1")
	if res.Encoding != "iso-8859-1" {
		t.Errorf("alias latin1 resolved to %q", res.Encoding)
	}
	if _, err := Decode(bytes.NewReader(nil), 0, UTF8, -1, 10); err == nil {
		t.Errorf("negative offset should fail")
	}
	if _, err := Decode(bytes.NewReader(nil), 0, UTF8, 0, 0); err == nil {
		t.Errorf("zero length should fail")
	}
}

func TestDecodeUnknownSizeTruncation(t *testing.T) {
	raw := []byte(strings.Repeat("x", 10))
	res, err := Decode(bytes.NewReader(raw), -1, UTF8, 0, 10)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !res.Truncated {
		t.Errorf("a full window with unknown size must report truncated")
	}
}

func TestHexRows(t *testing.T) {
	raw := []byte("Hello, hex!\x00\x01\xff\xfeMORE")
	rows, trunc, err := HexRows(bytes.NewReader(raw), int64(len(raw)), 0, 16)
	if err != nil {
		t.Fatalf("HexRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Offset != 0 {
		t.Errorf("Offset = %d", r.Offset)
	}
	want := "48 65 6c 6c 6f 2c 20 68 65 78 21 00 01 ff fe 4d"
	if r.Hex != want {
		t.Errorf("Hex = %q\nwant %q", r.Hex, want)
	}
	if r.ASCII != "Hello, hex!....M" {
		t.Errorf("ASCII = %q", r.ASCII)
	}
	if len(strings.Split(r.Hex, " ")) != 16 {
		t.Errorf("hex row must hold 16 byte pairs")
	}
	if !trunc {
		t.Errorf("truncated should be true, 3 bytes remain")
	}
}

func TestHexRowsPartialFinalRowAndOffset(t *testing.T) {
	raw := []byte("0123456789abcdefXY")
	rows, trunc, err := HexRows(bytes.NewReader(raw), int64(len(raw)), 0, 1024)
	if err != nil {
		t.Fatalf("HexRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[1].Offset != 16 {
		t.Errorf("second row Offset = %d, want 16", rows[1].Offset)
	}
	if rows[1].Hex != "58 59" || rows[1].ASCII != "XY" {
		t.Errorf("final row = %q / %q", rows[1].Hex, rows[1].ASCII)
	}
	if trunc {
		t.Errorf("whole file read, truncated must be false")
	}

	rows, _, err = HexRows(bytes.NewReader(raw), int64(len(raw)), 16, 1024)
	if err != nil {
		t.Fatalf("HexRows offset: %v", err)
	}
	if len(rows) != 1 || rows[0].Offset != 16 || rows[0].ASCII != "XY" {
		t.Fatalf("offset window wrong: %+v", rows)
	}
}

func TestHexRowsBadParams(t *testing.T) {
	if _, _, err := HexRows(bytes.NewReader(nil), 0, -1, 16); err == nil {
		t.Errorf("negative offset should fail")
	}
	if _, _, err := HexRows(bytes.NewReader(nil), 0, 0, -1); err == nil {
		t.Errorf("negative length should fail")
	}
}

func TestSniffMime(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"notes.txt", nil, "text/plain"},
		{"photo.PNG", nil, "image/png"},
		{"archive.tar.gz", nil, "application/gzip"},
		{"", []byte("\x89PNG\r\n\x1a\n"), "image/png"},
		{"", nil, ""},
	}
	for _, c := range cases {
		got := SniffMime(c.head, c.name)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("SniffMime(%q) = %q, want prefix %q", c.name, got, c.want)
		}
	}
	if MimeByName("mystery.zzz") != "" {
		t.Errorf("unknown extension should yield an empty MIME type")
	}
}

// pageThrough walks a stream the way the SPA does: request a window, append the
// text, advance by ViewResult.Length, stop when Truncated goes false. Each call
// gets a fresh reader positioned at 0, because that is what an archive stream
// looks like. It fails the test if paging ever fails to advance.
func pageThrough(t *testing.T, raw []byte, enc string, window int64) (string, bool) {
	t.Helper()
	var sb strings.Builder
	var off int64
	lossy := false
	for step := 0; ; step++ {
		if step > len(raw)+16 {
			t.Fatalf("paging %s with window %d did not terminate (stuck at offset %d)", enc, window, off)
		}
		res, err := Decode(bytes.NewReader(raw), int64(len(raw)), enc, off, window)
		if err != nil {
			t.Fatalf("Decode(%s, offset %d): %v", enc, off, err)
		}
		sb.WriteString(res.Text)
		lossy = lossy || res.Lossy
		if res.Length == 0 {
			if res.Truncated {
				t.Fatalf("%s window at offset %d consumed 0 bytes but claims more remain: paging is stuck", enc, off)
			}
			break
		}
		off += res.Length
		if !res.Truncated {
			break
		}
	}
	if off != int64(len(raw)) {
		t.Errorf("%s paging consumed %d of %d bytes", enc, off, len(raw))
	}
	return sb.String(), lossy
}

// Paging with a window at least as wide as the widest character must round-trip
// the whole file exactly, with every window cut on a character boundary.
func TestDecodePagingReconstructsTheFile(t *testing.T) {
	cases := []struct{ id, text string }{
		{"utf-8", "héllo 世界😀 tail"},
		{"utf-16le", "héllo 世界😀 tail"},
		{"utf-16be", "héllo 世界😀 tail"},
		{"utf-32le", "héllo 世界 tail"},
		{"utf-32be", "héllo 世界 tail"},
		{"shift_jis", "こんにちは world"},
		{"euc-jp", "こんにちは world"},
		{"euc-kr", "안녕하세요 world"},
		{"gbk", "你好世界 world"},
		{"gb18030", "你好世界 world"},
		{"big5", "你好世界 world"},
		{"windows-1252", "Grüße — café"},
		{"iso-8859-1", "Grüße"},
	}
	// ISO-2022-JP is deliberately absent: it is a stateful escape encoding, so a
	// window that starts mid-stream has lost the charset-shift context. Paging
	// through it is a v2 problem; the whole-file path is covered above.
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			var raw []byte
			if c.id == "utf-8" {
				raw = []byte(c.text)
			} else {
				raw = encodeWith(t, c.id, c.text)
			}
			for _, window := range []int64{4, 5, 7, 8, 16, 64} {
				got, lossy := pageThrough(t, raw, c.id, window)
				if got != c.text {
					t.Errorf("window %d: paged %q, want %q", window, got, c.text)
				}
				if lossy {
					t.Errorf("window %d: clean paging reported lossy", window)
				}
			}
		})
	}
}

// A window too narrow to hold even one character must still advance: returning
// Length 0 with Truncated true would loop the client forever. The incomplete
// character becomes a replacement char, which Lossy reports.
func TestDecodeTinyWindowsAlwaysAdvance(t *testing.T) {
	for _, id := range []string{"utf-8", "utf-16le", "utf-16be", "utf-32le", "utf-32be", "shift_jis", "gb18030", "big5"} {
		t.Run(id, func(t *testing.T) {
			text := "世界"
			var raw []byte
			if id == "utf-8" {
				raw = []byte(text)
			} else {
				raw = encodeWith(t, id, text)
			}
			for _, window := range []int64{1, 2, 3} {
				res, err := Decode(bytes.NewReader(raw), int64(len(raw)), id, 0, window)
				if err != nil {
					t.Fatalf("window %d: %v", window, err)
				}
				if res.Length <= 0 {
					t.Fatalf("window %d consumed %d bytes: the client can never page past offset 0", window, res.Length)
				}
				if res.Length > window {
					t.Errorf("window %d consumed %d bytes, more than it was given", window, res.Length)
				}
				// And the whole file still pages to completion.
				pageThrough(t, raw, id, window)
			}
		})
	}
}

// A UTF-16 surrogate pair is four bytes and one code point: a window that can
// hold only its lead half must hold the whole pair back for the next window.
func TestDecodeKeepsSurrogatePairsWhole(t *testing.T) {
	raw := encodeWith(t, "utf-16le", "a\U0001F600b") // 8 bytes: a, pair, b
	if len(raw) != 8 {
		t.Fatalf("fixture is %d bytes, want 8", len(raw))
	}
	for _, window := range []int64{4, 5} {
		res, err := Decode(bytes.NewReader(raw), int64(len(raw)), "utf-16le", 0, window)
		if err != nil {
			t.Fatalf("window %d: %v", window, err)
		}
		if res.Text != "a" {
			t.Errorf("window %d text = %q, want %q — half a surrogate pair leaked", window, res.Text, "a")
		}
		if res.Length != 2 {
			t.Errorf("window %d Length = %d, want 2 so the next window starts at the pair", window, res.Length)
		}
		if res.Lossy {
			t.Errorf("window %d: holding a pair back is not lossy", window)
		}
	}
	// Given room for the pair, it decodes as one rune.
	res, err := Decode(bytes.NewReader(raw), int64(len(raw)), "utf-16le", 0, 6)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "a\U0001F600" || res.Length != 6 {
		t.Errorf("text = %q length = %d, want the whole pair", res.Text, res.Length)
	}
}

// The last window of a file must flush a trailing partial character instead of
// holding it back forever — there is no next window to hand it to.
func TestDecodeFlushesPartialCharacterAtEOF(t *testing.T) {
	raw := []byte{'a', 0xC3} // a + a dangling UTF-8 lead byte
	res, err := Decode(bytes.NewReader(raw), int64(len(raw)), UTF8, 0, 64)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Length != 2 {
		t.Errorf("Length = %d, want 2: the whole file was read", res.Length)
	}
	if res.Truncated {
		t.Errorf("nothing follows the last byte, Truncated must be false")
	}
	if !res.Lossy {
		t.Errorf("a truncated character at EOF is a lossy decode: %q", res.Text)
	}

	// Same for UTF-16: an odd trailing byte at EOF.
	u16 := append(encodeWith(t, "utf-16le", "ab"), 0x41)
	res, err = Decode(bytes.NewReader(u16), int64(len(u16)), "utf-16le", 0, 64)
	if err != nil {
		t.Fatalf("Decode utf-16le: %v", err)
	}
	if res.Length != int64(len(u16)) || res.Truncated {
		t.Errorf("Length = %d truncated = %v, want %d/false", res.Length, res.Truncated, len(u16))
	}
}

// Size -1 (an archive entry whose size the format does not record) still pages.
func TestDecodeUnknownSizePages(t *testing.T) {
	raw := []byte("héllo wörld, a longer line to page over")
	var sb strings.Builder
	var off int64
	for range 100 {
		res, err := Decode(bytes.NewReader(raw[off:]), -1, UTF8, 0, 7)
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString(res.Text)
		if res.Length == 0 {
			break
		}
		off += res.Length
	}
	if sb.String() != string(raw) {
		t.Errorf("paged %q, want %q", sb.String(), raw)
	}
}

func TestHexRowsUnknownSize(t *testing.T) {
	raw := []byte("0123456789abcdefXY")
	rows, trunc, err := HexRows(bytes.NewReader(raw), -1, 0, 16)
	if err != nil {
		t.Fatalf("HexRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !trunc {
		t.Errorf("a full window with unknown size must report truncated")
	}
	rows, trunc, err = HexRows(bytes.NewReader(raw), -1, 0, 1024)
	if err != nil {
		t.Fatalf("HexRows: %v", err)
	}
	if len(rows) != 2 || trunc {
		t.Errorf("short read with unknown size: rows=%d truncated=%v", len(rows), trunc)
	}
}

func TestHexRowsPastEOF(t *testing.T) {
	raw := []byte("abc")
	rows, trunc, err := HexRows(bytes.NewReader(raw), int64(len(raw)), 100, 16)
	if err != nil {
		t.Fatalf("HexRows past EOF: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want none", rows)
	}
	if trunc {
		t.Errorf("nothing follows a past-EOF window")
	}
}

func TestCanonicalAliases(t *testing.T) {
	cases := map[string]string{
		"":            Auto,
		"  UTF-8  ":   UTF8,
		"utf8":        UTF8,
		"ASCII":       UTF8,
		"Latin-1":     "iso-8859-1",
		"CP1252":      "windows-1252",
		"sjis":        "shift_jis",
		"windows-31J": "shift_jis",
		"UTF-16":      "utf-16le",
		"ebcdic":      "ibm037",
		"macroman":    "macintosh",
	}
	for in, want := range cases {
		if got := canonical(in); got != want {
			t.Errorf("canonical(%q) = %q, want %q", in, got, want)
		}
		if _, ok := lookup(canonical(in)); !ok {
			t.Errorf("canonical(%q) = %q does not resolve", in, canonical(in))
		}
	}
	if _, ok := lookup("definitely-not-an-encoding"); ok {
		t.Errorf("an unknown id must not resolve")
	}
}

// Detection reports what "auto" would have picked regardless of what the client
// forced, so the SPA can offer "we think this is X" next to the picker.
func TestSniffedIsAlwaysAutosAnswer(t *testing.T) {
	sjis := encodeWith(t, "shift_jis", "こんにちは")
	res := decodeAll(t, sjis, "shift_jis")
	if res.Sniffed != Fixed {
		t.Errorf("Sniffed = %q, want the windows-1252 fallback auto would have chosen", res.Sniffed)
	}
	if res.Encoding != "shift_jis" {
		t.Errorf("Encoding = %q, want the forced one", res.Encoding)
	}

	// Mid-file windows never treat leading bytes as a BOM.
	raw := append([]byte{0xEF, 0xBB, 0xBF}, []byte("héllo")...)
	res, err := Decode(bytes.NewReader(raw), int64(len(raw)), Auto, 3, 64)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "héllo" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Offset != 3 || res.Length != int64(len(raw)-3) {
		t.Errorf("offset/length = %d/%d", res.Offset, res.Length)
	}
}

// ------------------------------------------------------------ seeking

// countingReader is a seekable reader that records how many bytes were
// actually read through it, so a test can tell a Seek from a discard.
type countingReader struct {
	*bytes.Reader
	read  int64
	seeks int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingReader) Seek(off int64, whence int) (int64, error) {
	c.seeks++
	return c.Reader.Seek(off, whence)
}

// streamOnly hides Seek: an archive member stream.
type streamOnly struct{ io.Reader }

// A window deep inside a seekable file must be reached with a Seek, not by
// reading everything before it: a request near the end of a 40 GB file must
// not read 40 GB.
func TestDecodeSeeksInsteadOfDiscarding(t *testing.T) {
	data := []byte(strings.Repeat("0123456789", 100000)) // 1 MB
	const offset, length = 900000, 64

	cr := &countingReader{Reader: bytes.NewReader(data)}
	res, err := Decode(cr, int64(len(data)), Auto, offset, length)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Text != string(data[offset:offset+length]) {
		t.Errorf("Decode window = %q", res.Text)
	}
	if cr.seeks == 0 {
		t.Errorf("Decode read %d bytes without seeking on a seekable reader", cr.read)
	}
	if cr.read > length {
		t.Errorf("Decode read %d bytes for a %d-byte window at offset %d: it discarded instead of seeking", cr.read, length, offset)
	}

	cr = &countingReader{Reader: bytes.NewReader(data)}
	rows, more, err := HexRows(cr, int64(len(data)), offset, length)
	if err != nil {
		t.Fatalf("HexRows: %v", err)
	}
	if len(rows) != 4 || rows[0].Offset != offset || !more {
		t.Errorf("HexRows window = %+v, more=%v", rows, more)
	}
	if cr.seeks == 0 || cr.read > length {
		t.Errorf("HexRows read %d bytes with %d seeks: it discarded instead of seeking", cr.read, cr.seeks)
	}

	// Past-EOF windows still come back empty and clean after a Seek.
	cr = &countingReader{Reader: bytes.NewReader(data)}
	res, err = Decode(cr, int64(len(data)), Auto, int64(len(data))+10, length)
	if err != nil || res.Text != "" || res.Truncated {
		t.Errorf("Decode past EOF via Seek = %+v, %v", res, err)
	}
	rows, more, err = HexRows(cr, -1, int64(len(data))+10, length)
	if err != nil || len(rows) != 0 || more {
		t.Errorf("HexRows past EOF via Seek = %+v, %v, %v", rows, more, err)
	}
}

// A reader that cannot seek (an archive stream) still gets its window by
// discarding the prefix, exactly as before.
func TestDecodeDiscardsOnNonSeekableStream(t *testing.T) {
	data := []byte(strings.Repeat("abcdefghij", 1000))
	const offset, length = 5000, 20

	cr := &countingReader{Reader: bytes.NewReader(data)}
	res, err := Decode(streamOnly{cr}, int64(len(data)), Auto, offset, length)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Text != string(data[offset:offset+length]) {
		t.Errorf("Decode window = %q", res.Text)
	}
	if cr.seeks != 0 || cr.read < offset+length {
		t.Errorf("non-seekable stream: %d seeks, %d bytes read; want a discarding read", cr.seeks, cr.read)
	}

	cr = &countingReader{Reader: bytes.NewReader(data)}
	rows, _, err := HexRows(streamOnly{cr}, -1, offset, length)
	if err != nil || len(rows) != 2 || rows[0].Offset != offset {
		t.Errorf("HexRows on stream = %+v, %v", rows, err)
	}
	if cr.seeks != 0 {
		t.Errorf("HexRows seeked a stream")
	}

	// Past EOF on a stream: empty, not an error.
	res, err = Decode(streamOnly{bytes.NewReader(data)}, -1, Auto, int64(len(data))+1, length)
	if err != nil || res.Text != "" {
		t.Errorf("Decode past EOF on stream = %+v, %v", res, err)
	}
}
