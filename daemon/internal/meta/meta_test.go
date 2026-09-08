package meta

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestCatalogAndExtensionKind(t *testing.T) {
	cat := Catalog()
	ids := map[string]bool{}
	defs := FieldDefs()
	for _, c := range cat {
		ids[c.ID] = true
		if c.ID == "common" && c.Extensions != nil {
			t.Error("common category must have nil Extensions")
		}
		if c.ID != "common" && len(c.Extensions) == 0 {
			t.Errorf("%s: no extensions", c.ID)
		}
		for _, e := range c.Extensions {
			k, ok := ExtensionKind(e)
			if !ok || string(k) != c.ID {
				t.Errorf("ExtensionKind(%q) = %q,%v; want %s", e, k, ok, c.ID)
			}
		}
		for _, f := range c.Fields {
			switch f.Type {
			case "text", "number", "bytes", "date", "enum", "bool":
			default:
				t.Errorf("%s: bad type %q", f.Key, f.Type)
			}
			if (f.Type == "enum" || f.Type == "bool") && len(f.Values) == 0 {
				t.Errorf("%s: enum without values", f.Key)
			}
			if f.Label == "" {
				t.Errorf("%s: no label", f.Key)
			}
			if c.ID != "common" && !strings.HasPrefix(f.Key, c.ID+".") {
				t.Errorf("%s: key not prefixed by %s", f.Key, c.ID)
			}
		}
	}
	for _, id := range []string{"common", "image", "video", "audio", "package"} {
		if !ids[id] {
			t.Errorf("missing category %s", id)
		}
	}
	// Every key the extractors emit is in the catalog.
	emitted := []string{"image.cameraMake", "image.gpsLat", "video.hdr", "video.audioCodec", "video.subtitleLang",
		"audio.albumArtist", "audio.durationSec", "package.depends", "package.installedSize", "package.format", "package.release"}
	for _, k := range emitted {
		if _, ok := defs[k]; !ok {
			t.Errorf("emitted key %s missing from catalog", k)
		}
	}
	if _, ok := ExtensionKind("ISO"); ok {
		t.Error("iso must not map to a category")
	}
	if k, ok := ExtensionKind(".RW2"); !ok || k != KindImage {
		t.Error("RW2 must map to image case-insensitively")
	}
	// Enum labels the UI shows verbatim.
	find := func(key, val string) string {
		for _, v := range defs[key].Values {
			if v.Value == val {
				return v.Label
			}
		}
		return ""
	}
	if find("video.audioCodec", "ac3") != "Dolby Digital (AC-3)" || find("video.hdr", "hdr10") != "HDR10 (PQ)" {
		t.Error("enum labels missing")
	}
}

func TestExtractorsWithAndWithoutFFprobe(t *testing.T) {
	without := Extractors("")
	with := Extractors("/usr/bin/ffprobe")
	kinds := func(exs []Extractor) string {
		var ks []string
		for _, e := range exs {
			ks = append(ks, string(e.Kind()))
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	if kinds(without) != "audio,image,package,package" {
		t.Errorf("without ffprobe: %s", kinds(without))
	}
	if kinds(with) != "audio,image,package,package,video" {
		t.Errorf("with ffprobe: %s", kinds(with))
	}
	all := AllExtensions(with)
	if len(all) == 0 || !sort.StringsAreSorted(all) {
		t.Error("AllExtensions not sorted/non-empty")
	}
	by := ByExtension(with)
	if by["rw2"].Kind() != KindImage || by["mkv"].Kind() != KindVideo || by["deb"].Kind() != KindPackage {
		t.Error("ByExtension mapping wrong")
	}
}

func TestNormalize(t *testing.T) {
	long := strings.Repeat("é", 600)
	nan := 0.0
	nan = nan / nan
	in := []Field{
		{Key: "a", Value: ""},
		{Key: "", Value: "x"},
		{Key: "b", Value: "  tab\tbed\x00nul\x01  "},
		{Key: "c", Value: long},
		{Key: "d", Value: "", Num: &nan},
		{Key: "e", Value: "", Num: ptr(3.5)},
	}
	out := normalize(in)
	if len(out) != 3 {
		t.Fatalf("normalize kept %d fields: %+v", len(out), out)
	}
	if out[0].Value != "tab bednul" {
		t.Errorf("clean = %q", out[0].Value)
	}
	if len(out[1].Value) > maxValueLen || !strings.HasSuffix(out[1].Value, "é") {
		t.Errorf("long value not cut on a rune boundary: len %d", len(out[1].Value))
	}
	if out[2].Value != "3.5" {
		t.Errorf("num without value = %q", out[2].Value)
	}
	many := make([]Field, 1000)
	for i := range many {
		many[i] = Field{Key: "k", Value: "v"}
	}
	if len(normalize(many)) != maxFieldsPerFile {
		t.Error("field count not capped")
	}
	if fmtNum(1920) != "1920" || fmtNum(23.976) != "23.976" || fmtNum(0.004) != "0.004" || fmtNum(1.0/3) != "0.3333" {
		t.Errorf("fmtNum: %s %s %s %s", fmtNum(1920), fmtNum(23.976), fmtNum(0.004), fmtNum(1.0/3))
	}
	d := date("k", time.Date(2024, 6, 15, 14, 3, 22, 0, time.FixedZone("x", 3600)))
	if d.Value != "2024-06-15T13:03:22Z" || *d.Num != 1718456602 {
		t.Errorf("date = %+v", d)
	}
}

func ptr(v float64) *float64 { return &v }

func TestBudgetReader(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1000)
	br := newBudgetReader(bytes.NewReader(data), 1000, 100)
	buf := make([]byte, 64)
	if n, _ := br.Read(buf); n != 64 {
		t.Fatal("first read short")
	}
	if n, _ := br.Read(buf); n != 36 {
		t.Fatalf("second read = %d, want 36 (budget clip)", n)
	}
	if _, err := br.Read(buf); err != errBudget {
		t.Errorf("third read err = %v", err)
	}
	if _, err := br.ReadAt(buf, 0); err != errBudget {
		t.Errorf("ReadAt past budget err = %v", err)
	}
	if _, err := br.Seek(10, io.SeekStart); err != nil {
		t.Error("seek must still work")
	}
}

// panicky is an extractor that panics; Run must convert that to an error.
type panicky struct{}

func (panicky) Kind() Kind           { return "boom" }
func (panicky) Extensions() []string { return []string{"boom"} }
func (panicky) Extract(context.Context, string, io.ReaderAt, int64) ([]Field, error) {
	var m map[string]int
	m["x"] = 1
	return nil, nil
}

func TestRunRecoversPanics(t *testing.T) {
	fs, err := Run(context.Background(), panicky{}, "/x/a.boom", bytes.NewReader(nil), 0)
	if err == nil || fs != nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("Run = %v, %v", fs, err)
	}
}

// --- corruption: every fixture, truncated and byte-flipped, must yield an
// error or a (possibly empty) result — never a panic, never a hang. -------

type corruptCase struct {
	name string
	ex   Extractor
	data []byte
}

func corruptionCorpus(t *testing.T) []corruptCase {
	var cases []corruptCase
	img := &imageExtractor{}
	for name, b := range imageVariants() {
		cases = append(cases, corruptCase{name, img, b})
	}
	deb := &debExtractor{}
	for _, comp := range []string{"gz", "xz", "zst", ""} {
		cases = append(cases, corruptCase{"pkg-" + comp + ".deb", deb, buildDeb(comp, sampleControl)})
	}
	cases = append(cases, corruptCase{"htop.rpm", &rpmExtractor{}, buildRPM(sampleRPMTags)})
	if fixDir != "" {
		aud := &audioExtractor{ff: newFFprobe(ffprobeBin)}
		for _, fx := range []string{fxMP3, fxFLAC, fxM4A, fxOGG, fxOpus, fxWAV} {
			b, err := os.ReadFile(filepath.Join(fixDir, fx))
			if err != nil {
				t.Fatal(err)
			}
			cases = append(cases, corruptCase{fx, aud, b})
		}
		vid := &videoExtractor{ff: newFFprobe(ffprobeBin)}
		for _, fx := range []string{fxSDR, fxMulti} {
			b, err := os.ReadFile(filepath.Join(fixDir, fx))
			if err != nil {
				t.Fatal(err)
			}
			cases = append(cases, corruptCase{fx, vid, b})
		}
	}
	return cases
}

// mustNotPanic calls Extract directly (bypassing Run's recover) so a library
// panic is a test failure we can see, not a logged recovery.
func mustNotPanic(t *testing.T, c corruptCase, label string, data []byte) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("%s/%s: PANIC %v", c.name, label, p)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), FileTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("%s/%s: PANIC %v", c.name, label, p)
			}
		}()
		c.ex.Extract(ctx, "/x/"+c.name, bytes.NewReader(data), int64(len(data))) //nolint:errcheck
	}()
	select {
	case <-done:
	case <-time.After(FileTimeout + 5*time.Second):
		t.Fatalf("%s/%s: extractor hung", c.name, label)
	}
}

func TestCorruptInputsNeverPanic(t *testing.T) {
	cases := corruptionCorpus(t)
	rng := rand.New(rand.NewSource(42))
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ffprobeBacked := false
			switch e := c.ex.(type) {
			case *videoExtractor:
				ffprobeBacked = true
			case *audioExtractor:
				ffprobeBacked = e.ff != nil
			}
			// ffprobe-backed cases spawn a process per variant: keep them few.
			truncs := []int{0, 1, 3, 7, 8, 12, 16, 31, 64, len(c.data) / 3, len(c.data) / 2, len(c.data) - 1}
			flips := 40
			if ffprobeBacked {
				truncs = []int{7, 64, len(c.data) / 2}
				flips = 4
			}
			for _, n := range truncs {
				if n > len(c.data) {
					continue
				}
				mustNotPanic(t, c, "trunc", c.data[:n])
			}
			for i := 0; i < flips; i++ {
				b := append([]byte(nil), c.data...)
				// Flip a few bytes, biased towards the header where the
				// structure lives.
				for j := 0; j < 1+rng.Intn(4); j++ {
					pos := rng.Intn(len(b))
					if rng.Intn(2) == 0 {
						pos = rng.Intn(min(len(b), 512))
					}
					b[pos] ^= byte(1 + rng.Intn(255))
				}
				mustNotPanic(t, c, "flip", b)
			}
			// Length fields set to extremes.
			b := append([]byte(nil), c.data...)
			for _, pos := range []int{4, 8, 12, 16, 20, 24} {
				if pos+4 <= len(b) {
					binary.BigEndian.PutUint32(b[pos:], 0xffffffff)
				}
			}
			mustNotPanic(t, c, "max-lengths", b)
			b = append([]byte(nil), c.data...)
			for _, pos := range []int{4, 8, 12, 16, 20, 24} {
				if pos+4 <= len(b) {
					binary.LittleEndian.PutUint32(b[pos:], 0x7fffffff)
				}
			}
			mustNotPanic(t, c, "max-lengths-le", b)
		})
	}
}

func TestErrUnsupportedIsSentinel(t *testing.T) {
	_, err := Run(context.Background(), &imageExtractor{}, "/x/a.jpg", bytes.NewReader([]byte("short")), 5)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("tiny file: %v", err)
	}
}
