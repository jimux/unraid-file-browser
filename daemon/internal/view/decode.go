package view

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/transform"

	"unraid-filebrowser/internal/types"
)

// Byte-order marks, longest first so UTF-32 wins over its UTF-16 prefix.
var boms = []struct {
	id    string
	bytes []byte
}{
	{UTF8, []byte{0xEF, 0xBB, 0xBF}},
	{"utf-32le", []byte{0xFF, 0xFE, 0x00, 0x00}},
	{"utf-32be", []byte{0x00, 0x00, 0xFE, 0xFF}},
	{"utf-16le", []byte{0xFF, 0xFE}},
	{"utf-16be", []byte{0xFE, 0xFF}},
}

// Decode reads the window [offset, offset+length) out of r and converts it to
// UTF-8. r must be positioned at byte 0 of the stream. When r can seek (a
// file) the window is reached with one Seek; otherwise (an archive stream)
// the leading offset bytes are read and discarded.
//
// size is the total stream size (-1 when unknown) and is used only to report
// Truncated. The caller enforces the API's window caps; any positive length is
// accepted here.
func Decode(r io.Reader, size int64, encName string, offset, length int64) (types.ViewResult, error) {
	if offset < 0 {
		return types.ViewResult{}, types.Errf(types.ErrBadRequest, "offset must not be negative")
	}
	if length <= 0 {
		return types.ViewResult{}, types.Errf(types.ErrBadRequest, "length must be positive")
	}

	id := canonical(encName)
	if _, ok := lookup(id); !ok {
		return types.ViewResult{}, types.Errf(types.ErrEncoding, "unsupported encoding "+encName)
	}

	buf, err := readWindow(r, offset, length)
	if err != nil {
		return types.ViewResult{}, err
	}

	sniffed := detect(buf, offset)
	chosen := id
	if chosen == Auto {
		chosen = sniffed
	}

	body := buf
	consumed := int64(0)
	if n := bomLen(chosen, buf, offset); n > 0 {
		body = buf[n:]
		consumed = int64(n)
	}

	// Only treat the window as the end of the stream when it really is: at a
	// mid-file boundary an incomplete trailing character is left for the next
	// window instead of being turned into a replacement character.
	atEOF := int64(len(buf)) < length || (size >= 0 && offset+int64(len(buf)) >= size)

	text, used, lossy, err := decodeWith(chosen, body, atEOF)
	if err != nil {
		return types.ViewResult{}, err
	}
	// Forward-progress guarantee: a window too small to hold even one character
	// of a multi-byte encoding (length=1 on UTF-16, say) would otherwise consume
	// nothing, and a client paging by Length would loop forever on the same
	// offset. Decode it as final instead — the incomplete character becomes a
	// replacement char (so Lossy reports it) and the window advances.
	if used == 0 && len(body) > 0 && !atEOF {
		text, used, lossy, err = decodeWith(chosen, body, true)
		if err != nil {
			return types.ViewResult{}, err
		}
		if used == 0 { // paranoia: never hand back a zero-length window
			used = int64(len(body))
		}
	}
	consumed += used

	// Content sniffing only means anything at the head of the stream; magic
	// numbers found halfway through a file are coincidences. Callers that know
	// the name (the api layer does) overwrite this with the better answer.
	mime := ""
	if offset == 0 {
		mime = SniffMime(buf, "")
	}

	res := types.ViewResult{
		Text:      text,
		Encoding:  chosen,
		Sniffed:   sniffed,
		Mime:      mime,
		Size:      size,
		Offset:    offset,
		Length:    consumed,
		Truncated: truncated(size, offset, consumed, int64(len(buf)), length),
		Lossy:     lossy,
	}
	return res, nil
}

// readWindow positions r at offset and reads at most length bytes.
func readWindow(r io.Reader, offset, length int64) ([]byte, error) {
	if offset > 0 {
		if err := skipTo(r, offset); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil // window starts past EOF: empty, not an error
			}
			return nil, err
		}
	}
	buf, err := io.ReadAll(io.LimitReader(r, length))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

// skipTo moves r to byte offset. A seekable reader (a real file) gets there
// with one Seek — reading and discarding the prefix would cost O(offset) I/O,
// which for a window near the end of a 40 GB file is the whole file. Anything
// else (an archive member stream) is read forward and discarded; a reader
// that claims Seek but cannot (a pipe behind an *os.File) falls back to that
// too. Past-EOF offsets surface as io.EOF from the discard path and as an
// empty read after a successful Seek; callers treat both as an empty window.
func skipTo(r io.Reader, offset int64) error {
	if s, ok := r.(io.Seeker); ok {
		if _, err := s.Seek(offset, io.SeekStart); err == nil {
			return nil
		}
	}
	_, err := io.CopyN(io.Discard, r, offset)
	return err
}

// decodeWith converts body from the named encoding, returning the text, the
// number of source bytes consumed (a trailing partial character is left for
// the next window unless atEOF) and whether replacement characters were
// produced.
func decodeWith(id string, body []byte, atEOF bool) (string, int64, bool, error) {
	if id == UTF8 {
		text, used, lossy := decodeUTF8(body, atEOF)
		return text, int64(used), lossy, nil
	}
	enc, ok := lookup(id)
	if !ok || enc == nil {
		return "", 0, false, types.Errf(types.ErrEncoding, "unsupported encoding "+id)
	}
	out, n, err := transformAll(enc.NewDecoder(), body, atEOF)
	if err != nil && !errors.Is(err, transform.ErrShortSrc) {
		if n == 0 {
			return "", 0, false, types.Errf(types.ErrEncoding, "decode as "+id+": "+err.Error())
		}
		// Partial conversion: keep what decoded and stop the window there.
		return string(out), int64(n), true, nil
	}
	return string(out), int64(n), bytes.ContainsRune(out, utf8.RuneError), nil
}

// transformAll runs a transformer over src, growing the destination as needed.
// Unlike transform.Bytes it can be told the source is not final, which is what
// makes windowed decoding cut on character boundaries.
func transformAll(tr transform.Transformer, src []byte, atEOF bool) ([]byte, int, error) {
	tr.Reset()
	out := make([]byte, 0, len(src)+len(src)/2+16)
	dst := make([]byte, 8192)
	pos := 0
	for {
		nDst, nSrc, err := tr.Transform(dst, src[pos:], atEOF)
		out = append(out, dst[:nDst]...)
		pos += nSrc
		if errors.Is(err, transform.ErrShortDst) {
			if nDst == 0 && nSrc == 0 {
				dst = make([]byte, len(dst)*2)
			}
			continue
		}
		return out, pos, err
	}
}

// decodeUTF8 validates UTF-8 in place; x/text's UTF-8 encoding is a no-op
// transformer and would happily pass malformed bytes through.
func decodeUTF8(b []byte, atEOF bool) (string, int, bool) {
	if !atEOF {
		b = trimPartialRune(b)
	}
	if utf8.Valid(b) {
		return string(b), len(b), false
	}
	var sb strings.Builder
	sb.Grow(len(b))
	lossy := false
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			sb.WriteRune(utf8.RuneError)
			lossy = true
			i++
			continue
		}
		sb.Write(b[i : i+size])
		i += size
	}
	return sb.String(), len(b), lossy
}

// trimPartialRune drops a trailing byte sequence that is the start of a rune
// the window cut in half, so paging resumes on a character boundary.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		c := b[i]
		if c&0xC0 == 0x80 {
			continue // continuation byte, keep looking for the lead
		}
		if c < utf8.RuneSelf {
			return b // ASCII: whatever followed was complete
		}
		var n int
		switch {
		case c&0xE0 == 0xC0:
			n = 2
		case c&0xF0 == 0xE0:
			n = 3
		case c&0xF8 == 0xF0:
			n = 4
		default:
			return b // invalid lead byte, leave it to the replacement path
		}
		if len(b)-i < n {
			return b[:i]
		}
		return b
	}
	return b
}

// detect implements the "auto" rule: BOM, then UTF-8 validity, then the
// windows-1252 fallback. BOMs only count at the start of the stream.
func detect(buf []byte, offset int64) string {
	if offset == 0 {
		for _, b := range boms {
			if bytes.HasPrefix(buf, b.bytes) {
				return b.id
			}
		}
	}
	if utf8.Valid(trimPartialRune(buf)) {
		return UTF8
	}
	return Fixed
}

// bomLen reports how many leading bytes are a BOM for the chosen encoding.
func bomLen(id string, buf []byte, offset int64) int {
	if offset != 0 {
		return 0
	}
	for _, b := range boms {
		if b.id == id && bytes.HasPrefix(buf, b.bytes) {
			return len(b.bytes)
		}
	}
	return 0
}

// truncated reports whether bytes remain after this window.
func truncated(size, offset, consumed, read, length int64) bool {
	if size >= 0 {
		return offset+consumed < size
	}
	return read >= length
}
