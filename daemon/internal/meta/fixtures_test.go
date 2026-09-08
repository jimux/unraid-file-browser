package meta

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// ---------------------------------------------------------------------------
// TIFF / EXIF builder. Produces byte-exact little/big-endian TIFF structures
// with IFD0, an Exif sub-IFD and a GPS sub-IFD, which every camera format in
// the image category wraps one way or another.
// ---------------------------------------------------------------------------

const (
	tBYTE      = 1
	tASCII     = 2
	tSHORT     = 3
	tLONG      = 4
	tRATIONAL  = 5
	tUNDEFINED = 7
	tSRATIONAL = 10
)

type tiffEntry struct {
	tag   uint16
	typ   uint16
	count uint32
	data  []byte // raw value bytes in the chosen byte order
}

type tiffIFD struct {
	entries []tiffEntry
}

func (ifd *tiffIFD) ascii(tag uint16, s string) {
	b := append([]byte(s), 0)
	ifd.entries = append(ifd.entries, tiffEntry{tag, tASCII, uint32(len(b)), b})
}

func (ifd *tiffIFD) short(order binary.ByteOrder, tag uint16, v uint16) {
	b := make([]byte, 2)
	order.PutUint16(b, v)
	ifd.entries = append(ifd.entries, tiffEntry{tag, tSHORT, 1, b})
}

func (ifd *tiffIFD) long(order binary.ByteOrder, tag uint16, v uint32) {
	b := make([]byte, 4)
	order.PutUint32(b, v)
	ifd.entries = append(ifd.entries, tiffEntry{tag, tLONG, 1, b})
}

func (ifd *tiffIFD) rational(order binary.ByteOrder, tag uint16, pairs ...[2]uint32) {
	b := make([]byte, 8*len(pairs))
	for i, p := range pairs {
		order.PutUint32(b[i*8:], p[0])
		order.PutUint32(b[i*8+4:], p[1])
	}
	ifd.entries = append(ifd.entries, tiffEntry{tag, tRATIONAL, uint32(len(pairs)), b})
}

func (ifd *tiffIFD) undefined(tag uint16, b []byte) {
	ifd.entries = append(ifd.entries, tiffEntry{tag, tUNDEFINED, uint32(len(b)), b})
}

func (ifd *tiffIFD) bytes(tag uint16, b []byte) {
	ifd.entries = append(ifd.entries, tiffEntry{tag, tBYTE, uint32(len(b)), b})
}

// size is the serialized length: count + entries + next pointer + overflow
// data (each value padded to even length).
func (ifd *tiffIFD) size() int {
	n := 2 + 12*len(ifd.entries) + 4
	for _, e := range ifd.entries {
		if len(e.data) > 4 {
			n += len(e.data) + len(e.data)&1
		}
	}
	return n
}

// serialize writes the IFD at absolute offset base (needed for overflow
// pointers). Entries are sorted by tag as the spec requires.
func (ifd *tiffIFD) serialize(order binary.ByteOrder, base int) []byte {
	ents := append([]tiffEntry(nil), ifd.entries...)
	for i := 1; i < len(ents); i++ {
		for j := i; j > 0 && ents[j].tag < ents[j-1].tag; j-- {
			ents[j], ents[j-1] = ents[j-1], ents[j]
		}
	}
	var buf bytes.Buffer
	b2 := make([]byte, 2)
	b4 := make([]byte, 4)
	order.PutUint16(b2, uint16(len(ents)))
	buf.Write(b2)
	dataOff := base + 2 + 12*len(ents) + 4
	var overflow bytes.Buffer
	for _, e := range ents {
		order.PutUint16(b2, e.tag)
		buf.Write(b2)
		order.PutUint16(b2, e.typ)
		buf.Write(b2)
		order.PutUint32(b4, e.count)
		buf.Write(b4)
		if len(e.data) <= 4 {
			v := make([]byte, 4)
			copy(v, e.data)
			buf.Write(v)
		} else {
			order.PutUint32(b4, uint32(dataOff+overflow.Len()))
			buf.Write(b4)
			overflow.Write(e.data)
			if len(e.data)&1 == 1 {
				overflow.WriteByte(0)
			}
		}
	}
	buf.Write([]byte{0, 0, 0, 0}) // next IFD
	buf.Write(overflow.Bytes())
	return buf.Bytes()
}

// exifSample is the canonical camera fixture; tests assert on these values.
type exifSample struct {
	make, model, lens, software string
	iso                         uint16
	fnum, expo, focal           [2]uint32
	dateTaken                   string // EXIF "YYYY:MM:DD HH:MM:SS"
	width, height               uint32
	orientation                 uint16
	gps                         bool
}

var gh5 = exifSample{
	make: "Panasonic", model: "DC-GH5", lens: "LEICA DG 12-60/F2.8-4.0", software: "Ver.2.7",
	iso: 800, fnum: [2]uint32{28, 10}, expo: [2]uint32{1, 250}, focal: [2]uint32{350, 10},
	dateTaken: "2024:06:15 14:03:22", width: 5184, height: 3888, orientation: 8, gps: true,
}

// buildTIFF returns a TIFF holding the sample's EXIF. magic overrides the
// bytes 2..3 (0x2a for TIFF, 0x55 for RW2, "RO" for ORF); firstIFD moves
// IFD0 (RW2 uses 0x18). extra adds IFD0 tags (DNGVersion etc.).
func buildTIFF(order binary.ByteOrder, s exifSample, magic uint16, firstIFD int, extra func(*tiffIFD)) []byte {
	ifd0 := &tiffIFD{}
	ifd0.ascii(0x010f, s.make)
	ifd0.ascii(0x0110, s.model)
	ifd0.short(order, 0x0112, s.orientation)
	ifd0.ascii(0x0131, s.software)
	ifd0.ascii(0x0132, s.dateTaken)
	ifd0.long(order, 0x0100, s.width) // ImageWidth/Length on IFD0 (raws do this)
	ifd0.long(order, 0x0101, s.height)
	if extra != nil {
		extra(ifd0)
	}
	// Placeholders so size() is right; values patched below.
	ifd0.long(order, 0x8769, 0)
	if s.gps {
		ifd0.long(order, 0x8825, 0)
	}

	exifIFD := &tiffIFD{}
	exifIFD.rational(order, 0x829a, s.expo)
	exifIFD.rational(order, 0x829d, s.fnum)
	exifIFD.short(order, 0x8827, s.iso)
	exifIFD.undefined(0x9000, []byte("0230"))
	exifIFD.ascii(0x9003, s.dateTaken)
	exifIFD.rational(order, 0x920a, s.focal)
	exifIFD.long(order, 0xa002, s.width)
	exifIFD.long(order, 0xa003, s.height)
	exifIFD.ascii(0xa434, s.lens)

	gpsIFD := &tiffIFD{}
	if s.gps {
		gpsIFD.bytes(0x0000, []byte{2, 3, 0, 0})
		gpsIFD.ascii(0x0001, "N")
		gpsIFD.rational(order, 0x0002, [2]uint32{51, 1}, [2]uint32{30, 1}, [2]uint32{0, 1}) // 51.5
		gpsIFD.ascii(0x0003, "W")
		gpsIFD.rational(order, 0x0004, [2]uint32{0, 1}, [2]uint32{7, 1}, [2]uint32{30, 1}) // -0.125
	}

	ifd0Off := firstIFD
	exifOff := ifd0Off + ifd0.size()
	gpsOff := exifOff + exifIFD.size()
	for i := range ifd0.entries {
		switch ifd0.entries[i].tag {
		case 0x8769:
			order.PutUint32(ifd0.entries[i].data, uint32(exifOff))
		case 0x8825:
			order.PutUint32(ifd0.entries[i].data, uint32(gpsOff))
		}
	}

	var out bytes.Buffer
	if order == binary.LittleEndian {
		out.WriteString("II")
	} else {
		out.WriteString("MM")
	}
	b2 := make([]byte, 2)
	order.PutUint16(b2, magic)
	out.Write(b2)
	b4 := make([]byte, 4)
	order.PutUint32(b4, uint32(firstIFD))
	out.Write(b4)
	for out.Len() < firstIFD {
		out.WriteByte(0)
	}
	out.Write(ifd0.serialize(order, ifd0Off))
	out.Write(exifIFD.serialize(order, exifOff))
	if s.gps {
		out.Write(gpsIFD.serialize(order, gpsOff))
	}
	return out.Bytes()
}

// tinyJPEG encodes an 8x8 image with the standard library.
func tinyJPEG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 50})
	return buf.Bytes()
}

// buildJPEG splices an APP1 Exif segment right after SOI.
func buildJPEG(tiff []byte) []byte {
	body := tinyJPEG()
	seg := append([]byte("Exif\x00\x00"), tiff...)
	var out bytes.Buffer
	out.Write(body[:2]) // SOI
	out.Write([]byte{0xff, 0xe1})
	ln := make([]byte, 2)
	binary.BigEndian.PutUint16(ln, uint16(len(seg)+2))
	out.Write(ln)
	out.Write(seg)
	out.Write(body[2:])
	return out.Bytes()
}

// buildPNG inserts an eXIf chunk after IHDR.
func buildPNG(tiff []byte) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var raw bytes.Buffer
	png.Encode(&raw, img)
	b := raw.Bytes()
	// signature(8) + IHDR chunk (8 + 13 + 4)
	split := 8 + 8 + 13 + 4
	var out bytes.Buffer
	out.Write(b[:split])
	ln := make([]byte, 4)
	binary.BigEndian.PutUint32(ln, uint32(len(tiff)))
	out.Write(ln)
	chunk := append([]byte("eXIf"), tiff...)
	out.Write(chunk)
	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(chunk))
	out.Write(crc)
	out.Write(b[split:])
	return out.Bytes()
}

// buildRAF wraps a JPEG in a Fujifilm RAF header (JPEG offset/length at
// bytes 84/88, big-endian).
func buildRAF(jpg []byte) []byte {
	hdr := make([]byte, 160)
	copy(hdr, "FUJIFILMCCD-RAW 0201FF129502")
	copy(hdr[28:], "X-T5")
	binary.BigEndian.PutUint32(hdr[84:], 160)
	binary.BigEndian.PutUint32(hdr[88:], uint32(len(jpg)))
	return append(hdr, jpg...)
}

// buildHEIC assembles a minimal HEIF: ftyp, meta{hdlr,pitm,iinf,iloc}, mdat
// with an Exif item (4-byte TIFF-header offset + "Exif\0\0" + TIFF).
func buildHEIC(tiff []byte) []byte {
	box := func(typ string, payload ...[]byte) []byte {
		n := 8
		for _, p := range payload {
			n += len(p)
		}
		b := make([]byte, 0, n)
		b = binary.BigEndian.AppendUint32(b, uint32(n))
		b = append(b, typ...)
		for _, p := range payload {
			b = append(b, p...)
		}
		return b
	}
	fullbox := func(typ string, payload ...[]byte) []byte {
		return box(typ, append([][]byte{{0, 0, 0, 0}}, payload...)...)
	}
	u16 := func(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
	u32 := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

	ftyp := box("ftyp", []byte("heic"), u32(0), []byte("mif1"), []byte("heic"))
	hdlr := fullbox("hdlr", u32(0), []byte("pict"), make([]byte, 12), []byte{0})
	pitm := fullbox("pitm", u16(1))
	infe1 := box("infe", []byte{2, 0, 0, 0}, u16(1), u16(0), []byte("hvc1"), []byte{0})
	infe2 := box("infe", []byte{2, 0, 0, 0}, u16(2), u16(0), []byte("Exif"), []byte{0})
	iinf := fullbox("iinf", u16(2), infe1, infe2)

	// ExifDataBlock: offset from the end of this field to the TIFF header
	// (6, past the "Exif\0\0" APP1 prefix), as iPhones write it.
	exifItem := append(u32(6), []byte("Exif\x00\x00")...)
	exifItem = append(exifItem, tiff...)
	hvcItem := []byte{0xde, 0xad, 0xbe, 0xef}

	// iloc with placeholder offsets; sizes are known so fill after layout.
	ilocBody := func(off1, off2 uint32) []byte {
		b := []byte{0x44, 0x00} // offset_size 4, length_size 4, base_offset_size 0
		b = append(b, u16(2)...)
		b = append(b, u16(1)...) // item 1
		b = append(b, u16(0)...) // data ref
		b = append(b, u16(1)...) // extent count
		b = append(b, u32(off1)...)
		b = append(b, u32(uint32(len(hvcItem)))...)
		b = append(b, u16(2)...)
		b = append(b, u16(0)...)
		b = append(b, u16(1)...)
		b = append(b, u32(off2)...)
		b = append(b, u32(uint32(len(exifItem)))...)
		return b
	}
	iloc := fullbox("iloc", ilocBody(0, 0))
	meta := fullbox("meta", hdlr, pitm, iinf, iloc)
	mdatStart := len(ftyp) + len(meta) + 8
	iloc = fullbox("iloc", ilocBody(uint32(mdatStart), uint32(mdatStart+len(hvcItem))))
	meta = fullbox("meta", hdlr, pitm, iinf, iloc)
	mdat := box("mdat", hvcItem, exifItem)
	out := append(ftyp, meta...)
	return append(out, mdat...)
}

// ---------------------------------------------------------------------------
// Package builders.
// ---------------------------------------------------------------------------

const sampleControl = `Package: filebrowserd
Version: 1.2.3-1
Architecture: amd64
Maintainer: Example Maintainer <maint@example.com>
Installed-Size: 4096
Depends: libc6 (>= 2.34), libssl3 | libssl1.1, ffmpeg:any
Pre-Depends: dpkg (>= 1.17)
Section: utils
Priority: optional
Description: Browse Unraid shares from the webGUI
 Long description, second paragraph line one.
 Line two.
`

// arArchive writes a GNU ar archive of the given members.
func arArchive(members ...struct {
	name string
	data []byte
}) []byte {
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	for _, m := range members {
		hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.data))
		out.WriteString(hdr)
		out.Write(m.data)
		if len(m.data)&1 == 1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func tarOf(entries map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range entries {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	return buf.Bytes()
}

func gzipOf(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func xzOf(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := xz.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func zstdOf(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

type arMember = struct {
	name string
	data []byte
}

// buildDeb assembles a .deb whose control.tar uses the given compression
// ("gz", "xz", "zst", "").
func buildDeb(comp string, control string) []byte {
	ctl := tarOf(map[string]string{"./control": control, "./md5sums": "abc  usr/bin/x\n"})
	name := "control.tar"
	switch comp {
	case "gz":
		ctl, name = gzipOf(ctl), "control.tar.gz"
	case "xz":
		ctl, name = xzOf(ctl), "control.tar.xz"
	case "zst":
		ctl, name = zstdOf(ctl), "control.tar.zst"
	}
	data := gzipOf(tarOf(map[string]string{"./usr/bin/x": "#!/bin/sh\n"}))
	return arArchive(
		arMember{"debian-binary", []byte("2.0\n")},
		arMember{name, ctl},
		arMember{"data.tar.gz", data},
	)
}

// rpmTag is one header value for buildRPM.
type rpmTag struct {
	tag  uint32
	typ  uint32
	strs []string // for STRING / I18NSTRING / STRING_ARRAY
	i32  uint32   // for INT32
}

// rpmHeaderBytes serializes one header structure (intro + index + store).
func rpmHeaderBytes(tags []rpmTag) []byte {
	var store bytes.Buffer
	var index bytes.Buffer
	for _, t := range tags {
		// INT32 must be 4-aligned in the store.
		if t.typ == rpmTypeInt32 {
			for store.Len()%4 != 0 {
				store.WriteByte(0)
			}
		}
		off := uint32(store.Len())
		count := uint32(1)
		switch t.typ {
		case rpmTypeInt32:
			store.Write(binary.BigEndian.AppendUint32(nil, t.i32))
		default:
			count = uint32(len(t.strs))
			for _, s := range t.strs {
				store.WriteString(s)
				store.WriteByte(0)
			}
		}
		index.Write(binary.BigEndian.AppendUint32(nil, t.tag))
		index.Write(binary.BigEndian.AppendUint32(nil, t.typ))
		index.Write(binary.BigEndian.AppendUint32(nil, off))
		index.Write(binary.BigEndian.AppendUint32(nil, count))
	}
	var out bytes.Buffer
	out.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0})
	out.Write(binary.BigEndian.AppendUint32(nil, uint32(len(tags))))
	out.Write(binary.BigEndian.AppendUint32(nil, uint32(store.Len())))
	out.Write(index.Bytes())
	out.Write(store.Bytes())
	return out.Bytes()
}

var sampleRPMTags = []rpmTag{
	{tag: rpmTagName, typ: rpmTypeString, strs: []string{"htop"}},
	{tag: rpmTagVersion, typ: rpmTypeString, strs: []string{"3.3.0"}},
	{tag: rpmTagRelease, typ: rpmTypeString, strs: []string{"2.fc40"}},
	{tag: rpmTagSummary, typ: rpmTypeI18NString, strs: []string{"Interactive process viewer", "Visualiseur de processus"}},
	{tag: rpmTagSize, typ: rpmTypeInt32, i32: 409600},
	{tag: rpmTagVendor, typ: rpmTypeString, strs: []string{"Fedora Project"}},
	{tag: rpmTagLicense, typ: rpmTypeString, strs: []string{"GPL-2.0-or-later"}},
	{tag: rpmTagPackager, typ: rpmTypeString, strs: []string{"Fedora Project <packager@fedoraproject.org>"}},
	{tag: rpmTagGroup, typ: rpmTypeI18NString, strs: []string{"Applications/System"}},
	{tag: rpmTagArch, typ: rpmTypeString, strs: []string{"x86_64"}},
	{tag: rpmTagRequireName, typ: rpmTypeStringArray, strs: []string{"libc.so.6()(64bit)", "libncursesw.so.6()(64bit)", "rpmlib(CompressedFileNames)", "/bin/sh"}},
}

// buildRPM assembles lead + signature header (padded to 8) + header + a fake
// payload.
func buildRPM(tags []rpmTag) []byte {
	lead := make([]byte, rpmLeadLen)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb, 3, 0, 0, 0})
	copy(lead[10:], "htop-3.3.0-2.fc40")
	sig := rpmHeaderBytes([]rpmTag{{tag: 1000, typ: rpmTypeInt32, i32: 12345}})
	for len(sig)%8 != 0 {
		sig = append(sig, 0)
	}
	hdr := rpmHeaderBytes(tags)
	out := append(lead, sig...)
	out = append(out, hdr...)
	return append(out, []byte("payload-would-be-cpio")...)
}

// ---------------------------------------------------------------------------
// ffmpeg fixtures (skipped when ffmpeg is not installed).
// ---------------------------------------------------------------------------

var (
	ffmpegBin  string
	ffprobeBin string
	fixDir     string
)

func lookTool(name string) string {
	for _, p := range []string{"/opt/homebrew/bin/" + name, "/usr/local/bin/" + name} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard) // Run's recover path logs stack traces
	ffmpegBin, ffprobeBin = lookTool("ffmpeg"), lookTool("ffprobe")
	if ffmpegBin != "" && ffprobeBin != "" {
		dir, err := os.MkdirTemp("", "meta-fixtures-")
		if err != nil {
			panic(err)
		}
		fixDir = dir
		if err := makeMediaFixtures(dir); err != nil {
			fmt.Fprintln(os.Stderr, "fixtures:", err)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	code := m.Run()
	if fixDir != "" {
		os.RemoveAll(fixDir)
	}
	os.Exit(code)
}

const (
	fxHDR10  = "hdr10.mkv"
	fxHLG    = "hlg.mp4"
	fxAC3    = "ac3.mkv"
	fxMulti  = "multi.mkv"
	fxSDR    = "sdr.mp4"
	fxMP3    = "song.mp3"
	fxFLAC   = "song.flac"
	fxM4A    = "song.m4a"
	fxOGG    = "song.ogg"
	fxWAV    = "song.wav"
	fxOpus   = "song.opus"
	fxNoTags = "notags.mp3"
)

func makeMediaFixtures(dir string) error {
	run := func(name string, args ...string) error {
		full := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, args...)
		full = append(full, filepath.Join(dir, name))
		out, err := exec.Command(ffmpegBin, full...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v: %s", name, err, out)
		}
		return nil
	}
	const bars = "smptebars=size=64x64:rate=25"
	const tone = "sine=frequency=440:sample_rate=48000"
	const tone2 = "sine=frequency=880:sample_rate=48000"
	vid := func(src string) []string { return []string{"-f", "lavfi", "-i", src} }
	steps := []func() error{
		func() error {
			// HDR10: 10-bit HEVC with PQ transfer and BT.2020 primaries.
			return run(fxHDR10, append(append(vid(bars), vid(tone)...),
				"-t", "0.4", "-vf", "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc",
				"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
				"-x265-params", "log-level=none",
				"-c:a", "eac3", "-metadata:s:a:0", "language=eng")...)
		},
		func() error {
			return run(fxHLG, append(vid(bars),
				"-t", "0.4", "-vf", "setparams=color_primaries=bt2020:color_trc=arib-std-b67:colorspace=bt2020nc",
				"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
				"-x265-params", "log-level=none", "-tag:v", "hvc1", "-an")...)
		},
		func() error {
			return run(fxAC3, append(append(vid(bars), vid(tone)...),
				"-t", "0.4", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
				"-c:a", "ac3", "-ac", "6", "-metadata:s:a:0", "language=fre")...)
		},
		func() error {
			// Multi-track: AAC stereo eng + AC-3 5.1 deu + DTS jpn + two subtitles.
			sub := filepath.Join(dir, "sub.srt")
			if err := os.WriteFile(sub, []byte("1\n00:00:00,000 --> 00:00:00,300\nhi\n"), 0o644); err != nil {
				return err
			}
			args := append(vid(bars), vid(tone)...)
			args = append(args, vid(tone2)...)
			args = append(args, "-i", sub, "-i", sub)
			args = append(args,
				"-t", "0.4", "-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3", "-map", "4",
				"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
				"-c:a:0", "aac", "-ac:a:0", "2", "-metadata:s:a:0", "language=eng",
				"-c:a:1", "ac3", "-ac:a:1", "6", "-metadata:s:a:1", "language=deu",
				"-c:s", "srt", "-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=spa")
			return run(fxMulti, args...)
		},
		func() error {
			return run(fxSDR, append(append(vid(bars), vid(tone)...),
				"-t", "0.4", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac")...)
		},
		func() error {
			return run(fxMP3, append(vid(tone), "-t", "0.5", "-c:a", "libmp3lame", "-b:a", "128k",
				"-metadata", "artist=The Crawlers", "-metadata", "album=Index Sessions",
				"-metadata", "title=Header Only", "-metadata", "date=2021", "-metadata", "genre=Electronic",
				"-metadata", "track=7/12", "-metadata", "album_artist=Various Crawlers")...)
		},
		func() error {
			return run(fxFLAC, append(vid(tone), "-t", "0.5", "-c:a", "flac", "-sample_fmt", "s16",
				"-metadata", "artist=Lossless Larry", "-metadata", "album=Bits",
				"-metadata", "title=Flat", "-metadata", "date=1999-05-01", "-metadata", "track=2")...)
		},
		func() error {
			return run(fxM4A, append(vid(tone), "-t", "0.5", "-c:a", "aac", "-b:a", "96k",
				"-metadata", "artist=Apple Alice", "-metadata", "album=Atoms", "-metadata", "title=Moov",
				"-metadata", "date=2010", "-metadata", "track=3")...)
		},
		func() error {
			return run(fxOGG, append(vid(tone), "-t", "0.5", "-c:a", "vorbis", "-strict", "-2", "-ac", "2", "-b:a", "64k",
				"-metadata", "artist=Ogg Olga", "-metadata", "title=Comment", "-metadata", "date=2005")...)
		},
		func() error {
			return run(fxOpus, append(vid(tone), "-t", "0.5", "-c:a", "libopus", "-b:a", "48k",
				"-metadata", "artist=Opus Otto", "-metadata", "title=Packets")...)
		},
		func() error {
			return run(fxWAV, append(vid(tone), "-t", "0.5", "-c:a", "pcm_s16le",
				"-metadata", "artist=Wave Wanda", "-metadata", "title=Riff")...)
		},
		func() error {
			return run(fxNoTags, append(vid(tone), "-t", "0.5", "-c:a", "libmp3lame", "-b:a", "64k",
				"-map_metadata", "-1", "-write_xing", "0", "-id3v2_version", "0", "-write_id3v1", "0")...)
		},
	}
	for _, st := range steps {
		if err := st(); err != nil {
			return err
		}
	}
	return nil
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	if fixDir == "" {
		t.Skip("ffmpeg not available; skipping media fixture test")
	}
	p := filepath.Join(fixDir, name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s missing: %v", name, err)
	}
	return p
}

// fieldMap indexes fields by key; multi-valued keys keep every value.
type fieldMap map[string][]Field

func toMap(fs []Field) fieldMap {
	m := fieldMap{}
	for _, f := range fs {
		m[f.Key] = append(m[f.Key], f)
	}
	return m
}

func (m fieldMap) value(key string) string {
	if fs := m[key]; len(fs) > 0 {
		return fs[0].Value
	}
	return ""
}

func (m fieldMap) values(key string) []string {
	var out []string
	for _, f := range m[key] {
		out = append(out, f.Value)
	}
	return out
}

func (m fieldMap) num(key string) float64 {
	if fs := m[key]; len(fs) > 0 && fs[0].Num != nil {
		return *fs[0].Num
	}
	return -1
}

func (m fieldMap) has(key, value string) bool {
	for _, f := range m[key] {
		if f.Value == value {
			return true
		}
	}
	return false
}

func keysOf(m fieldMap) string {
	var ks []string
	for k, fs := range m {
		for _, f := range fs {
			ks = append(ks, k+"="+f.Value)
		}
	}
	return strings.Join(ks, ", ")
}
