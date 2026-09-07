package archive

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unraid-filebrowser/internal/types"
)

// --- -slt parser unit tests (no binary needed) -------------------------------

// sltFixture is a captured-shape `7zz l -slt -ba` output (blocks separated by
// blank lines, "Key = Value" pairs; dirs carry a leading D attribute token).
const sltFixture = `Path = docs
Size = 0
Packed Size = 0
Modified = 2024-05-01 10:00:00.1234567
Attributes = D drwxr-xr-x
CRC =
Encrypted = -
Method =
Block =

Path = docs/readme.txt
Size = 13
Packed Size = 17
Modified = 2024-05-01 10:30:00
Attributes = A -rw-r--r--
CRC = 8B44D746
Encrypted = -
Method = LZMA2:12
Block = 0

Path = docs/link
Size = 10
Packed Size =
Modified = 2024-05-01 10:30:00
Attributes = A -rwxr-xr-x
Symbolic Link = readme.txt
Encrypted = -
Block = 0

Path = folderflag
Folder = +
Size = 0
Modified =
Attributes =
Block =

Path = has space and = sign.txt
Size = 5
Modified = 2024-05-01 10:30:00
Attributes = A -rw-r--r--
Block = 1
`

func TestParseSLTFixture(t *testing.T) {
	recs, err := parseSLT(strings.NewReader(sltFixture), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Fatalf("parsed %d records, want 5: %+v", len(recs), recs)
	}

	docs := recs[0]
	if docs.raw != "docs" || !docs.isDir || docs.size != 0 {
		t.Fatalf("docs: %+v", docs)
	}
	wantMtime := time.Date(2024, 5, 1, 10, 0, 0, 0, time.Local).Unix()
	if docs.mtime != wantMtime {
		t.Fatalf("docs mtime = %d, want %d (fractional seconds must be tolerated)", docs.mtime, wantMtime)
	}

	readme := recs[1]
	if readme.raw != "docs/readme.txt" || readme.isDir || readme.size != 13 {
		t.Fatalf("readme: %+v", readme)
	}

	link := recs[2]
	if link.symlink != "readme.txt" {
		t.Fatalf("symlink not captured: %+v", link)
	}

	// "Folder = +" marks a dir even without a D attribute.
	folder := recs[3]
	if folder.raw != "folderflag" || !folder.isDir || folder.size != 0 || folder.mtime != 0 {
		t.Fatalf("folderflag: %+v", folder)
	}

	// Values containing " = " must survive the first-cut split.
	weird := recs[4]
	if weird.raw != "has space and = sign.txt" || weird.size != 5 {
		t.Fatalf("weird name: %+v", weird)
	}
}

func TestParseSLTEmptyAndGarbage(t *testing.T) {
	recs, err := parseSLT(strings.NewReader(""), 0)
	if err != nil || len(recs) != 0 {
		t.Fatalf("empty: %v, %v", recs, err)
	}
	// Non "Key = Value" lines are skipped, incomplete blocks without Path drop.
	recs, err = parseSLT(strings.NewReader("random noise\nSize = 5\n\nPath = ok\nSize = 1\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].raw != "ok" || recs[0].size != 1 {
		t.Fatalf("garbage tolerance: %+v", recs)
	}
}

func TestAttrHasD(t *testing.T) {
	cases := map[string]bool{
		"D drwxr-xr-x":  true,
		"D_ drwxr-xr-x": true,
		"DA":            true,
		"A -rw-r--r--":  false,
		"A_ -rw-r--r--": false,
		"":              false,
		"RA hidden":     false,
		"A -rwD-r--r--": false, // D beyond the first token is ignored
	}
	for in, want := range cases {
		if got := attrHasD(in); got != want {
			t.Errorf("attrHasD(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseSevenZipTime(t *testing.T) {
	want := time.Date(2024, 5, 1, 10, 0, 0, 0, time.Local).Unix()
	for _, s := range []string{
		"2024-05-01 10:00:00",
		"2024-05-01 10:00:00.1234567",
		"  2024-05-01 10:00:00.9  ",
	} {
		if got := parseSevenZipTime(s); got != want {
			t.Errorf("parseSevenZipTime(%q) = %d, want %d", s, got, want)
		}
	}
	for _, s := range []string{"", "garbage", "2024-99-99 10:00:00"} {
		if got := parseSevenZipTime(s); got != 0 {
			t.Errorf("parseSevenZipTime(%q) = %d, want 0", s, got)
		}
	}
}

func TestBoundedBuf(t *testing.T) {
	var b boundedBuf
	big := bytes.Repeat([]byte("x"), stderrCap+1000)
	n, err := b.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := len(b.b); got != stderrCap {
		t.Fatalf("retained %d bytes, want %d", got, stderrCap)
	}
	// summary condenses lines and falls back when empty.
	var empty boundedBuf
	if s := empty.summary("fallback"); s != "fallback" {
		t.Fatalf("empty summary = %q", s)
	}
	var multi boundedBuf
	multi.Write([]byte("\nERROR: one\n\n  two  \n")) //nolint:errcheck
	if s := multi.summary("alt"); s != "ERROR: one; two" {
		t.Fatalf("summary = %q", s)
	}
}

// --- integration with the real 7zz binary ------------------------------------

// sevenZip returns the 7zz path or skips the test.
func sevenZip(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("7zz")
	if err != nil {
		t.Skip("7zz not installed; skipping 7-Zip driver integration tests")
	}
	return p
}

// run7zz runs 7zz with args in dir, failing the test on error.
func run7zz(t *testing.T, bin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("7zz %v: %v\n%s", args, err, out)
	}
}

func TestSevenZipListStatOpen(t *testing.T) {
	bin := sevenZip(t)
	r, root := newTestResolver(t, Options{SevenZipPath: bin})

	// Build source tree and archive it with the real binary.
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "docs", "readme.txt"), []byte("hello readme\n"))
	writeFile(t, filepath.Join(src, "docs", "sub", "data.bin"), []byte{0, 1, 2, 3, 4, 255})
	ap := filepath.Join(root, "test.7z")
	run7zz(t, bin, src, "a", "-bd", ap, "docs")

	// Root listing.
	entries, err := r.List(ctxT(t), ap)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	eqStrings(t, names(entries), []string{"docs"})
	if entries[0].Type != types.TypeDir {
		t.Fatalf("docs type = %s", entries[0].Type)
	}

	// Subdir listing.
	sub, err := r.List(ctxT(t), ap+sep+"docs")
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, names(sub), []string{"readme.txt", "sub"})
	readme := findEntry(t, sub, "readme.txt")
	if readme.Type != types.TypeFile || readme.Size != int64(len("hello readme\n")) {
		t.Fatalf("readme entry: %+v", readme)
	}
	if readme.Mtime == 0 {
		t.Error("readme mtime not parsed")
	}

	// Stat + Open a nested file.
	vp := ap + sep + "docs/sub/data.bin"
	e, err := r.Stat(ctxT(t), vp)
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != 6 || e.Type != types.TypeFile {
		t.Fatalf("data.bin entry: %+v", e)
	}
	rc, _, err := r.Open(ctxT(t), vp)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || !bytes.Equal(data, []byte{0, 1, 2, 3, 4, 255}) {
		t.Fatalf("read = %v, %v", data, err)
	}

	// Missing entry inside the 7z.
	_, err = r.Stat(ctxT(t), ap+sep+"docs/absent.txt")
	wantCode(t, err, types.ErrNotFound)
}

func TestSevenZipNestedBothDirections(t *testing.T) {
	bin := sevenZip(t)
	tmp := t.TempDir()
	r, root := newTestResolver(t, Options{SevenZipPath: bin, TempDir: tmp})

	// Direction 1: zip inside a .7z (nested layer streamed OUT of 7zz).
	src := t.TempDir()
	innerZip := zipBytes(t, zEntry{name: "hello.txt", data: "zip in 7z"})
	writeFile(t, filepath.Join(src, "inner.zip"), innerZip)
	sevenPath := filepath.Join(root, "outer.7z")
	run7zz(t, bin, src, "a", "-bd", sevenPath, "inner.zip")

	entries, err := r.List(ctxT(t), sevenPath)
	if err != nil {
		t.Fatal(err)
	}
	if findEntry(t, entries, "inner.zip").Type != types.TypeArchive {
		t.Fatal("inner.zip not marked archive in 7z listing")
	}
	rc, _, err := r.Open(ctxT(t), sevenPath+sep+"inner.zip"+sep+"hello.txt")
	if err != nil {
		t.Fatalf("open zip-in-7z: %v", err)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || string(data) != "zip in 7z" {
		t.Fatalf("read = %q, %v", data, err)
	}

	// Direction 2: .7z inside a zip (7zz needs the layer materialized to a
	// temp file; it must be cleaned up afterwards).
	src2 := t.TempDir()
	writeFile(t, filepath.Join(src2, "f.txt"), []byte("7z in zip"))
	inner7z := filepath.Join(t.TempDir(), "inner.7z")
	run7zz(t, bin, src2, "a", "-bd", inner7z, "f.txt")
	sevenBytes, err := os.ReadFile(inner7z)
	if err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(root, "carrier.zip")
	writeFile(t, zipPath, zipBytes(t, zEntry{name: "inner.7z", data: string(sevenBytes)}))

	inner, err := r.List(ctxT(t), zipPath+sep+"inner.7z")
	if err != nil {
		t.Fatalf("list 7z-in-zip: %v", err)
	}
	eqStrings(t, names(inner), []string{"f.txt"})
	rc2, _, err := r.Open(ctxT(t), zipPath+sep+"inner.7z"+sep+"f.txt")
	if err != nil {
		t.Fatalf("open 7z-in-zip: %v", err)
	}
	data2, err := readAllAndClose(t, rc2)
	if err != nil || string(data2) != "7z in zip" {
		t.Fatalf("read = %q, %v", data2, err)
	}
	if left := tempLayerFiles(t, tmp); len(left) != 0 {
		t.Fatalf("temp layer files not cleaned up: %v", left)
	}
}

func TestSevenZipEncrypted(t *testing.T) {
	bin := sevenZip(t)
	r, root := newTestResolver(t, Options{SevenZipPath: bin})
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "secret.txt"), []byte("classified"))

	// Header-encrypted: even listing must fail cleanly (never prompt).
	he := filepath.Join(root, "enc-header.7z")
	run7zz(t, bin, src, "a", "-bd", "-pSecret", "-mhe=on", he, "secret.txt")
	_, err := r.List(ctxT(t), he)
	wantCode(t, err, types.ErrArchive)

	// Content-encrypted only: listing works, extraction fails cleanly.
	ce := filepath.Join(root, "enc-content.7z")
	run7zz(t, bin, src, "a", "-bd", "-pSecret", ce, "secret.txt")
	entries, err := r.List(ctxT(t), ce)
	if err != nil {
		t.Fatalf("list content-encrypted: %v", err)
	}
	eqStrings(t, names(entries), []string{"secret.txt"})
	rc, _, err := r.Open(ctxT(t), ce+sep+"secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, err = readAllAndClose(t, rc)
	wantCode(t, err, types.ErrArchive)
}

func TestSevenZipCorruptArchive(t *testing.T) {
	bin := sevenZip(t)
	r, root := newTestResolver(t, Options{SevenZipPath: bin})
	bp := filepath.Join(root, "bad.7z")
	writeFile(t, bp, []byte("definitely not a 7z archive"))
	_, err := r.List(ctxT(t), bp)
	wantCode(t, err, types.ErrArchive)
}

func TestSevenZipHostileName(t *testing.T) {
	bin := sevenZip(t)
	r, root := newTestResolver(t, Options{SevenZipPath: bin})
	// A file whose name looks like a 7zz switch must not be interpreted as
	// one ("--" and -spd in the fixed argv protect us).
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "-not-a-flag.txt"), []byte("tricky"))
	ap := filepath.Join(root, "tricky.7z")
	run7zz(t, bin, src, "a", "-bd", ap, "--", "-not-a-flag.txt")

	entries, err := r.List(ctxT(t), ap)
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, names(entries), []string{"-not-a-flag.txt"})
	rc, _, err := r.Open(ctxT(t), ap+sep+"-not-a-flag.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || string(data) != "tricky" {
		t.Fatalf("read = %q, %v", data, err)
	}
}
