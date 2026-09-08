package meta

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/evanoberholster/imagemeta"
	"github.com/evanoberholster/imagemeta/meta/exif"
)

// imageBudget bounds the bytes imagemeta may read from one file. EXIF lives
// in the first few hundred KB; an MP4-style HEIC may need the meta box which
// can sit after a preview. Anything past this is not metadata.
const imageBudget = 8 << 20

// rafHeaderLen is the fixed Fujifilm RAF header; the embedded JPEG's offset
// and length sit at bytes 84 and 88 (big-endian).
const rafHeaderLen = 92

// imageExtractor reads EXIF through github.com/evanoberholster/imagemeta:
// JPEG, TIFF, PNG (eXIf chunk), HEIC/HEIF/AVIF, CR2, CR3, CRW, DNG, NEF, ARW
// and RW2 natively; ORF (TIFF with an Olympus magic) and PEF (plain TIFF)
// through the TIFF path; RAF through its embedded JPEG.
type imageExtractor struct{}

func (*imageExtractor) Kind() Kind           { return KindImage }
func (*imageExtractor) Extensions() []string { return imageExts }

func (*imageExtractor) Extract(ctx context.Context, path string, r io.ReaderAt, size int64) ([]Field, error) {
	if size < 16 {
		return nil, ErrUnsupported
	}
	var magic [16]byte
	if err := readAtFull(r, magic[:], 0); err != nil {
		return nil, err
	}
	src := r
	sectionSize := size
	decode := imagemeta.Decode
	switch {
	case string(magic[:4]) == "IIU\x00", string(magic[:4]) == "MM\x00U":
		// Panasonic RW2: imagemeta.Decode's generic TIFF scan rejects the
		// 0x55 magic in v1.0.0; the exif package's own entry point carries
		// the RW2 special case (and Panasonic MakerNote handling).
		decode = func(rs io.ReadSeeker) (exif.Exif, error) { return exif.ParseWithReaderOptions(rs) }
	case string(magic[:15]) == "FUJIFILMCCD-RAW":
		// Fujifilm RAF: the EXIF is in the embedded JPEG.
		var hdr [rafHeaderLen]byte
		if err := readAtFull(r, hdr[:], 0); err != nil {
			return nil, err
		}
		off := int64(binary.BigEndian.Uint32(hdr[84:88]))
		ln := int64(binary.BigEndian.Uint32(hdr[88:92]))
		if off < rafHeaderLen || ln < 16 || off > size || ln > size-off {
			return nil, errors.New("raf: embedded jpeg out of range")
		}
		src = io.NewSectionReader(r, off, ln)
		sectionSize = ln
	case string(magic[:4]) == "IIRO", string(magic[:4]) == "IIRS":
		// Olympus ORF: little-endian TIFF with a vendor magic.
		src = &patchedReaderAt{r: r, patch: []byte{'I', 'I', 0x2a, 0x00}}
	case string(magic[:4]) == "MMOR":
		src = &patchedReaderAt{r: r, patch: []byte{'M', 'M', 0x00, 0x2a}}
	case string(magic[:8]) == pngSignature:
		// PNG: find the eXIf chunk ourselves and hand its TIFF payload to
		// the TIFF decoder (the library's PNG path assumes big-endian).
		off, ln, err := pngExifChunk(r, size)
		if err != nil {
			return nil, err
		}
		src = io.NewSectionReader(r, off, ln)
		sectionSize = ln
	}
	br := newBudgetReader(src, sectionSize, imageBudget)
	ex, err := decode(br)
	if err != nil {
		if errors.Is(err, imagemeta.ErrNoExif) || errors.Is(err, imagemeta.ErrImageTypeNotFound) ||
			errors.Is(err, imagemeta.ErrMetadataNotSupported) {
			return nil, ErrUnsupported
		}
		return nil, fmt.Errorf("exif: %w", err)
	}
	fields := exifFields(&ex)
	if !hasSubstance(fields) {
		return nil, ErrUnsupported
	}
	return fields, nil
}

// hasSubstance reports whether a decode produced anything beyond the
// always-present derived flags (image.gps=no): a JPEG with an empty or
// unreadable APP1 decodes "successfully" to nothing.
func hasSubstance(fields []Field) bool {
	for _, f := range fields {
		if f.Key != "image.gps" && f.Value != "" {
			return true
		}
	}
	return false
}

const pngSignature = "\x89PNG\r\n\x1a\n"

// pngMaxChunks bounds the chunk walk; eXIf sits before IDAT in every writer
// that emits it, so a long walk means there is none.
const pngMaxChunks = 64

// pngExifChunk walks PNG chunks (8-byte header: length, type; 4-byte CRC)
// and returns the eXIf payload's offset and length.
func pngExifChunk(r io.ReaderAt, size int64) (off, ln int64, err error) {
	pos := int64(len(pngSignature))
	var hdr [8]byte
	for i := 0; i < pngMaxChunks && pos+8 <= size; i++ {
		if err := readAtFull(r, hdr[:], pos); err != nil {
			return 0, 0, err
		}
		clen := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		if clen > size-pos-12 {
			return 0, 0, errors.New("png: chunk runs past end of file")
		}
		switch typ {
		case "eXIf":
			if clen < 8 {
				return 0, 0, ErrUnsupported
			}
			return pos + 8, clen, nil
		case "IDAT", "IEND":
			return 0, 0, ErrUnsupported
		}
		pos += 8 + clen + 4
	}
	return 0, 0, ErrUnsupported
}

// exifFields maps the decoded EXIF onto the catalog keys.
func exifFields(ex *exif.Exif) []Field {
	var out []Field
	out = append(out,
		text("image.cameraMake", ex.CameraMake()),
		text("image.cameraModel", ex.IFD0.Model),
		text("image.lens", ex.ExifIFD.LensModel),
		text("image.software", ex.IFD0.Software),
	)
	if iso := ex.ExifIFD.ISOSpeedRatings; iso > 0 && iso < 10_000_000 {
		out = append(out, numInt("image.iso", int64(iso)))
	}
	if f := float64(ex.ExifIFD.FNumber); f > 0 && f < 1000 {
		out = append(out, num("image.fNumber", round(f, 2)))
	}
	if et := float64(ex.ExifIFD.ExposureTime); et > 0 && et < 1e6 {
		out = append(out, num("image.exposureTime", round(et, 6)))
	}
	if fl := float64(ex.ExifIFD.FocalLength); fl > 0 && fl < 100_000 {
		out = append(out, num("image.focalLength", round(fl, 2)))
	}
	if d := ex.SelectedDate(); !d.IsZero() && d.Year() > 1900 && d.Year() < 2200 {
		out = append(out, date("image.dateTaken", d))
	}
	w, h := ex.ExifIFD.PixelXDimension, ex.ExifIFD.PixelYDimension
	if w == 0 || h == 0 {
		w, h = ex.IFD0.ImageWidth, ex.IFD0.ImageHeight
	}
	if w > 0 && h > 0 && w <= 1<<17 && h <= 1<<17 {
		out = append(out, numInt("image.width", int64(w)), numInt("image.height", int64(h)))
	}
	if o := orientationName(uint16(ex.IFD0.Orientation)); o != "" {
		out = append(out, text("image.orientation", o))
	}
	lat, lon := ex.GPS.Latitude(), ex.GPS.Longitude()
	if (lat != 0 || lon != 0) && math.Abs(lat) <= 90 && math.Abs(lon) <= 180 {
		out = append(out, text("image.gps", "yes"),
			num("image.gpsLat", round(lat, 6)), num("image.gpsLon", round(lon, 6)))
	} else {
		out = append(out, text("image.gps", "no"))
	}
	return out
}

// orientationName renders the EXIF orientation code (1..8) as a stable,
// searchable name independent of the library's wording.
func orientationName(o uint16) string {
	switch o {
	case 1:
		return "normal"
	case 2:
		return "mirror-horizontal"
	case 3:
		return "rotate-180"
	case 4:
		return "mirror-vertical"
	case 5:
		return "mirror-horizontal-rotate-270"
	case 6:
		return "rotate-90"
	case 7:
		return "mirror-horizontal-rotate-90"
	case 8:
		return "rotate-270"
	}
	return ""
}

func round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}

// patchedReaderAt overlays the first len(patch) bytes of r. Olympus ORF is a
// TIFF whose magic says "IIRO" instead of "II*\0"; presenting the standard
// magic lets the TIFF parser read it unchanged.
type patchedReaderAt struct {
	r     io.ReaderAt
	patch []byte
}

func (p *patchedReaderAt) ReadAt(b []byte, off int64) (int, error) {
	n, err := p.r.ReadAt(b, off)
	for i := 0; i < n; i++ {
		if pos := off + int64(i); pos < int64(len(p.patch)) {
			b[i] = p.patch[pos]
		} else {
			break
		}
	}
	return n, err
}
