package view

import (
	"io"
	"strings"

	"unraid-filebrowser/internal/types"
)

// hexBytesPerRow is fixed by the API contract.
const hexBytesPerRow = 16

const hexDigits = "0123456789abcdef"

// HexRows renders the window [offset, offset+length) as 16-byte hex rows. Like
// Decode it takes a reader positioned at byte 0 and moves to the window
// itself (one Seek on a file, a discarding read on a stream). The bool
// reports whether bytes remain after the window.
func HexRows(r io.Reader, size int64, offset, length int64) ([]types.HexRow, bool, error) {
	if offset < 0 {
		return nil, false, types.Errf(types.ErrBadRequest, "offset must not be negative")
	}
	if length <= 0 {
		return nil, false, types.Errf(types.ErrBadRequest, "length must be positive")
	}

	buf, err := readWindow(r, offset, length)
	if err != nil {
		return nil, false, err
	}

	rows := make([]types.HexRow, 0, (len(buf)+hexBytesPerRow-1)/hexBytesPerRow)
	for i := 0; i < len(buf); i += hexBytesPerRow {
		end := min(i+hexBytesPerRow, len(buf))
		rows = append(rows, hexRow(offset+int64(i), buf[i:end]))
	}
	return rows, truncated(size, offset, int64(len(buf)), int64(len(buf)), length), nil
}

func hexRow(off int64, chunk []byte) types.HexRow {
	var hx, as strings.Builder
	hx.Grow(len(chunk) * 3)
	as.Grow(len(chunk))
	for i, c := range chunk {
		if i > 0 {
			hx.WriteByte(' ')
		}
		hx.WriteByte(hexDigits[c>>4])
		hx.WriteByte(hexDigits[c&0x0F])
		if c >= 0x20 && c < 0x7F {
			as.WriteByte(c)
		} else {
			as.WriteByte('.')
		}
	}
	return types.HexRow{Offset: off, Hex: hx.String(), ASCII: as.String()}
}
