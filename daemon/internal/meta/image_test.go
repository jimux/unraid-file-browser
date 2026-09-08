package meta

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
)

// imageVariants are the containers the image extractor must read, all
// wrapping the same GH5 EXIF sample.
func imageVariants() map[string][]byte {
	le, be := binary.LittleEndian, binary.BigEndian
	tiffLE := buildTIFF(le, gh5, 0x2a, 8, nil)
	tiffBE := buildTIFF(be, gh5, 0x2a, 8, nil)
	jpg := buildJPEG(tiffBE)
	dng := buildTIFF(le, gh5, 0x2a, 8, func(ifd *tiffIFD) { ifd.bytes(0xc612, []byte{1, 4, 0, 0}) })
	rw2 := buildTIFF(le, gh5, 0x55, 0x18, nil)
	copy(rw2[8:], []byte{0x88, 0xe7, 0x74, 0xd8}) // Panasonic raw signature
	orf := buildTIFF(le, gh5, 0x2a, 8, nil)
	copy(orf[2:4], "RO") // Olympus magic: "IIRO"
	cr2 := buildTIFF(le, gh5, 0x2a, 16, nil)
	copy(cr2[8:], []byte{'C', 'R', 2, 0, 0, 0, 0, 0})
	return map[string][]byte{
		"shot.jpg":  jpg,
		"shot.tif":  tiffLE,
		"shot.png":  buildPNG(tiffLE),
		"shot.rw2":  rw2,
		"shot.orf":  orf,
		"shot.pef":  tiffBE,
		"shot.nef":  tiffBE,
		"shot.arw":  tiffLE,
		"shot.dng":  dng,
		"shot.cr2":  cr2,
		"shot.raf":  buildRAF(jpg),
		"shot.heic": buildHEIC(tiffBE),
	}
}

func extractBytes(t *testing.T, ex Extractor, name string, b []byte) fieldMap {
	t.Helper()
	fs, err := Run(context.Background(), ex, "/x/"+name, bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return toMap(fs)
}

func TestImageFormats(t *testing.T) {
	ex := &imageExtractor{}
	for name, b := range imageVariants() {
		t.Run(name, func(t *testing.T) {
			m := extractBytes(t, ex, name, b)
			want := map[string]string{
				"image.cameraMake":   "Panasonic",
				"image.cameraModel":  "DC-GH5",
				"image.lens":         "LEICA DG 12-60/F2.8-4.0",
				"image.software":     "Ver.2.7",
				"image.iso":          "800",
				"image.fNumber":      "2.8",
				"image.exposureTime": "0.004",
				"image.focalLength":  "35",
				"image.dateTaken":    "2024-06-15T14:03:22Z",
				"image.width":        "5184",
				"image.height":       "3888",
				"image.orientation":  "rotate-270",
				"image.gps":          "yes",
				"image.gpsLat":       "51.5",
				"image.gpsLon":       "-0.125",
			}
			for k, v := range want {
				if got := m.value(k); got != v {
					t.Errorf("%s: %s = %q, want %q (all: %s)", name, k, got, v, keysOf(m))
				}
			}
			if m.num("image.iso") != 800 || m.num("image.dateTaken") != 1718460202 {
				t.Errorf("%s: numeric iso/date wrong: %v %v", name, m.num("image.iso"), m.num("image.dateTaken"))
			}
		})
	}
}

func TestImageNoGPSAndNoExif(t *testing.T) {
	ex := &imageExtractor{}
	s := gh5
	s.gps = false
	m := extractBytes(t, ex, "nogps.jpg", buildJPEG(buildTIFF(binary.BigEndian, s, 0x2a, 8, nil)))
	if m.value("image.gps") != "no" || len(m["image.gpsLat"]) != 0 {
		t.Errorf("no-gps sample: %s", keysOf(m))
	}
	// A plain JPEG without EXIF is "unsupported", not an error class the
	// crawler should log about.
	plain := tinyJPEG()
	_, err := Run(context.Background(), ex, "/x/plain.jpg", bytes.NewReader(plain), int64(len(plain)))
	if err != ErrUnsupported {
		t.Errorf("plain jpeg: err = %v, want ErrUnsupported", err)
	}
}
