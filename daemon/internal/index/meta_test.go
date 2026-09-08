package index

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unraid-filebrowser/internal/types"
)

// --- compact fixture builders (the meta package has the full versions) -----

// exifJPEG returns a JPEG whose APP1 carries Make, Model, DateTimeOriginal
// and ISO, big-endian. Enough for the crawl/query tests here; format
// coverage lives in internal/meta.
func exifJPEG(maker, model string, iso uint16, date string) []byte {
	be := binary.BigEndian
	type ent struct {
		tag, typ uint16
		count    uint32
		data     []byte
	}
	ifd := func(ents []ent, base int) []byte {
		var b bytes.Buffer
		b.Write(be.AppendUint16(nil, uint16(len(ents))))
		dataOff := base + 2 + 12*len(ents) + 4
		var over bytes.Buffer
		for _, e := range ents {
			b.Write(be.AppendUint16(nil, e.tag))
			b.Write(be.AppendUint16(nil, e.typ))
			b.Write(be.AppendUint32(nil, e.count))
			if len(e.data) <= 4 {
				v := make([]byte, 4)
				copy(v, e.data)
				b.Write(v)
			} else {
				b.Write(be.AppendUint32(nil, uint32(dataOff+over.Len())))
				over.Write(e.data)
				if len(e.data)&1 == 1 {
					over.WriteByte(0)
				}
			}
		}
		b.Write([]byte{0, 0, 0, 0})
		b.Write(over.Bytes())
		return b.Bytes()
	}
	ascii := func(tag uint16, s string) ent { d := append([]byte(s), 0); return ent{tag, 2, uint32(len(d)), d} }
	short := func(tag uint16, v uint16) ent { return ent{tag, 3, 1, be.AppendUint16(nil, v)} }
	long := func(tag uint16, v uint32) ent { return ent{tag, 4, 1, be.AppendUint32(nil, v)} }

	exifEnts := []ent{short(0x8827, iso), ascii(0x9003, date)}
	ifd0Ents := []ent{ascii(0x010f, maker), ascii(0x0110, model), long(0x8769, 0)}
	ifd0Size := len(ifd(ifd0Ents, 8))
	ifd0Ents[2] = long(0x8769, uint32(8+ifd0Size))
	tiff := append([]byte{'M', 'M', 0, 0x2a, 0, 0, 0, 8}, ifd(ifd0Ents, 8)...)
	tiff = append(tiff, ifd(exifEnts, 8+ifd0Size)...)

	img := image.NewGray(image.Rect(0, 0, 4, 4))
	var body bytes.Buffer
	jpeg.Encode(&body, img, nil)
	seg := append([]byte("Exif\x00\x00"), tiff...)
	out := append([]byte{0xff, 0xd8, 0xff, 0xe1}, be.AppendUint16(nil, uint16(len(seg)+2))...)
	out = append(out, seg...)
	return append(out, body.Bytes()[2:]...)
}

func plainJPEG() []byte {
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	var body bytes.Buffer
	jpeg.Encode(&body, img, nil)
	return body.Bytes()
}

// debPackage builds a minimal .deb (ar: debian-binary, control.tar.gz).
func debPackage(control string) []byte {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	tw.WriteHeader(&tar.Header{Name: "./control", Mode: 0o644, Size: int64(len(control)), Typeflag: tar.TypeReg})
	tw.Write([]byte(control))
	tw.Close()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(tarBuf.Bytes())
	zw.Close()
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	for _, m := range []struct {
		name string
		data []byte
	}{{"debian-binary", []byte("2.0\n")}, {"control.tar.gz", gz.Bytes()}} {
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.data))
		out.Write(m.data)
		if len(m.data)&1 == 1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func writeBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// metaRoot lays out the standard fixture tree and returns its path.
//
//	photos/gh5.jpg     Panasonic DC-GH5, ISO 800,  2024-06-15
//	photos/r5.jpg      Canon EOS R5,     ISO 3200, 2023-01-02
//	photos/plain.jpg   no EXIF
//	pkgs/tool.deb      package tool 1.0, depends libc6, zlib1g
//	notes/readme.txt   not a metadata candidate
func metaRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "photos", "gh5.jpg"), exifJPEG("Panasonic", "DC-GH5", 800, "2024:06:15 14:03:22"))
	writeBytes(t, filepath.Join(root, "photos", "r5.jpg"), exifJPEG("Canon", "Canon EOS R5", 3200, "2023:01:02 03:04:05"))
	writeBytes(t, filepath.Join(root, "photos", "plain.jpg"), plainJPEG())
	writeBytes(t, filepath.Join(root, "pkgs", "tool.deb"), debPackage("Package: tool\nVersion: 1.0\nArchitecture: all\nInstalled-Size: 10\nDepends: libc6, zlib1g (>= 1.2)\nDescription: a tool\n"))
	writeFile(t, filepath.Join(root, "notes", "readme.txt"), "hello world")
	return root
}

func mf(key, op, value string) types.MetaFilter {
	return types.MetaFilter{Key: key, Op: op, Value: value}
}

// fq builds a filter-only query.
func fq(filters ...types.MetaFilter) types.SearchQuery {
	return types.SearchQuery{MinSize: -1, MaxSize: -1, Meta: filters}
}

func names(hits []types.SearchHit) string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Entry.Name)
	}
	return strings.Join(out, ",")
}

func metaRowCount(t *testing.T, s *Service, where string, args ...any) int64 {
	t.Helper()
	var n int64
	q := `SELECT COUNT(*) FROM file_meta m JOIN files f ON f.id = m.file_id`
	if where != "" {
		q += ` WHERE ` + where
	}
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// --- crawl -------------------------------------------------------------------

func TestMetaCrawlExtracts(t *testing.T) {
	root := metaRoot(t)
	s := newService(t, testConfig(root))
	crawl(t, s)

	st := s.Status()
	if st.MetaIndexed != 3 {
		t.Errorf("MetaIndexed = %d, want 3 (gh5, r5, tool.deb)", st.MetaIndexed)
	}
	if got := s.metaExtracted.Load(); got != 4 {
		t.Errorf("extraction attempts = %d, want 4 (three jpgs + deb; txt is not a candidate)", got)
	}
	// plain.jpg was processed (marked) but produced no rows.
	var marked int64
	s.db.QueryRow(`SELECT meta_extracted FROM files WHERE name = 'plain.jpg'`).Scan(&marked)
	if marked != 1 {
		t.Error("plain.jpg not marked processed")
	}
	if n := metaRowCount(t, s, `f.name = 'plain.jpg'`); n != 0 {
		t.Errorf("plain.jpg has %d rows", n)
	}
	// Multi-valued key: two depends rows for one file.
	if n := metaRowCount(t, s, `f.name = 'tool.deb' AND m.key = 'package.depends'`); n != 2 {
		t.Errorf("tool.deb depends rows = %d, want 2", n)
	}
	// Numeric column populated.
	var num sql.NullFloat64
	s.db.QueryRow(`SELECT m.num FROM file_meta m JOIN files f ON f.id = m.file_id WHERE f.name = 'gh5.jpg' AND m.key = 'image.iso'`).Scan(&num)
	if !num.Valid || num.Float64 != 800 {
		t.Errorf("image.iso num = %v", num)
	}
}

func TestMetaReextractOnlyChanged(t *testing.T) {
	root := metaRoot(t)
	s := newService(t, testConfig(root))
	crawl(t, s)
	before := s.metaExtracted.Load()

	crawl(t, s)
	if got := s.metaExtracted.Load(); got != before {
		t.Errorf("unchanged files re-extracted: %d → %d", before, got)
	}

	// Rewrite gh5.jpg with different EXIF; the crawl must re-read only it.
	// The index keys change detection on (size, mtime-in-seconds), so give
	// the rewrite a distinct mtime rather than racing the clock.
	p := filepath.Join(root, "photos", "gh5.jpg")
	writeBytes(t, p, exifJPEG("Panasonic", "DC-GH6", 6400, "2025:01:01 00:00:00"))
	later := time.Now().Add(time.Hour)
	os.Chtimes(p, later, later)
	crawl(t, s)
	if got := s.metaExtracted.Load(); got != before+1 {
		t.Errorf("after change: attempts %d → %d, want exactly one more", before, got)
	}
	hits, _ := mustSearch(t, s, fq(mf("image.cameraModel", "=", "DC-GH6")))
	if names(hits) != "gh5.jpg" {
		t.Errorf("new model not searchable: %q", names(hits))
	}
	hits, _ = mustSearch(t, s, fq(mf("image.cameraModel", "=", "DC-GH5")))
	if len(hits) != 0 {
		t.Errorf("stale model still searchable: %q", names(hits))
	}
	if n := metaRowCount(t, s, `f.name = 'gh5.jpg' AND m.key = 'image.iso'`); n != 1 {
		t.Errorf("iso rows after re-extract = %d, want 1 (old rows replaced, not appended)", n)
	}

	// Same bytes, new mtime: also re-extracted (mtime is part of the
	// change test), still exactly one row set.
	future := time.Now().Add(2 * time.Hour)
	os.Chtimes(p, future, future)
	crawl(t, s)
	if got := s.metaExtracted.Load(); got != before+2 {
		t.Errorf("after touch: attempts = %d, want %d", got, before+2)
	}
}

func TestMetaSweeps(t *testing.T) {
	root := metaRoot(t)
	s := newService(t, testConfig(root))
	crawl(t, s)
	if n := metaRowCount(t, s, ""); n == 0 {
		t.Fatal("no rows")
	}
	// Deleted file: rows go with it.
	os.Remove(filepath.Join(root, "photos", "r5.jpg"))
	crawl(t, s)
	if n := metaRowCount(t, s, `f.name = 'r5.jpg'`); n != 0 {
		t.Errorf("r5.jpg rows survive deletion: %d", n)
	}
	var orphans int64
	s.db.QueryRow(`SELECT COUNT(*) FROM file_meta WHERE file_id NOT IN (SELECT id FROM files)`).Scan(&orphans)
	if orphans != 0 {
		t.Errorf("%d orphan file_meta rows", orphans)
	}
	if s.Status().MetaIndexed != 2 {
		t.Errorf("MetaIndexed after delete = %d, want 2", s.Status().MetaIndexed)
	}

	// Scoped rescan of pkgs/ must not touch photos' rows.
	if err := s.Rescan(filepath.Join(root, "photos")); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	if n := metaRowCount(t, s, `f.name = 'tool.deb'`); n == 0 {
		t.Error("scoped rescan of photos/ swept pkgs/ rows")
	}

	// Narrowing the roots sweeps rows outside them in the same transaction
	// as the files.
	cfg := s.Config()
	cfg.Roots = []string{filepath.Join(root, "pkgs")}
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if n := metaRowCount(t, s, `f.name = 'gh5.jpg'`); n != 0 {
		t.Errorf("rows outside the narrowed roots survive: %d", n)
	}
	if n := metaRowCount(t, s, `f.name = 'tool.deb'`); n == 0 {
		t.Error("rows inside the narrowed roots were lost")
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM file_meta WHERE file_id NOT IN (SELECT id FROM files)`).Scan(&orphans)
	if orphans != 0 {
		t.Errorf("%d orphan rows after narrowing", orphans)
	}
	if s.Status().MetaIndexed != 1 {
		t.Errorf("MetaIndexed after narrowing = %d, want 1", s.Status().MetaIndexed)
	}
}

func TestMetaPersistsAcrossReopen(t *testing.T) {
	root := metaRoot(t)
	dbPath := filepath.Join(t.TempDir(), "index.db")
	s, err := New(dbPath, testConfig(root), Options{AllowedRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	crawl(t, s)
	s.Close()
	s2, err := New(dbPath, testConfig(root), Options{AllowedRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Status().MetaIndexed != 3 {
		t.Errorf("MetaIndexed after reopen = %d", s2.Status().MetaIndexed)
	}
	hits, _ := mustSearch(t, s2, fq(mf("image.cameraMake", "=", "canon")))
	if names(hits) != "r5.jpg" {
		t.Errorf("after reopen: %q", names(hits))
	}
}

// --- filters -------------------------------------------------------------------

func TestMetaFiltersEveryOperator(t *testing.T) {
	root := metaRoot(t)
	s := newService(t, testConfig(root))
	crawl(t, s)
	jpg := types.MetaFilter{Key: "ext", Op: "=", Value: "jpg"}

	cases := []struct {
		name string
		q    types.SearchQuery
		want string
	}{
		{"= case-insensitive", fq(mf("image.cameraModel", "=", "dc-gh5")), "gh5.jpg"},
		{"= make", fq(mf("image.cameraMake", "=", "Canon")), "r5.jpg"},
		{"~ substring", fq(mf("image.cameraModel", "~", "eos")), "r5.jpg"},
		{"~ both", fq(mf("image.cameraModel", "~", "5")), "gh5.jpg,r5.jpg"},
		{"!= excludes value, keeps files without the key", fq(jpg, mf("image.cameraMake", "!=", "panasonic")), "plain.jpg,r5.jpg"},
		{"> number", fq(mf("image.iso", ">", "1000")), "r5.jpg"},
		{">= number", fq(mf("image.iso", ">=", "800")), "gh5.jpg,r5.jpg"},
		{"< number", fq(mf("image.iso", "<", "800")), ""},
		{"<= number", fq(mf("image.iso", "<=", "800")), "gh5.jpg"},
		{"= number matches num", fq(mf("image.iso", "=", "800.0")), "gh5.jpg"},
		{"!= number", fq(jpg, mf("image.iso", "!=", "800")), "plain.jpg,r5.jpg"},
		{"date >= RFC3339", fq(mf("image.dateTaken", ">=", "2024-01-01T00:00:00Z")), "gh5.jpg"},
		{"date < YYYY-MM-DD", fq(mf("image.dateTaken", "<", "2024-01-01")), "r5.jpg"},
		{"date > unix", fq(mf("image.dateTaken", ">", "1700000000")), "gh5.jpg"},
		{"multi-valued = first", fq(mf("package.depends", "=", "libc6")), "tool.deb"},
		{"multi-valued = second", fq(mf("package.depends", "=", "zlib1g")), "tool.deb"},
		{"multi-valued != one of them excludes", fq(mf("ext", "=", "deb"), mf("package.depends", "!=", "zlib1g")), ""},
		{"bytes field", fq(mf("package.installedSize", ">=", "10240")), "tool.deb"},
		{"AND of two filters", fq(mf("image.cameraMake", "=", "Panasonic"), mf("image.iso", "<", "1000")), "gh5.jpg"},
		{"AND that contradicts", fq(mf("image.cameraMake", "=", "Panasonic"), mf("image.iso", ">", "1000")), ""},
		{"common name ~", fq(mf("name", "~", "5.jpg")), "gh5.jpg,r5.jpg"},
		{"common name =", fq(mf("name", "=", "README.TXT")), "readme.txt"},
		{"common ext = with dot", fq(mf("ext", "=", ".DEB")), "tool.deb"},
		{"common ext !=", fq(mf("ext", "!=", "jpg"), mf("ext", "!=", "txt")), "tool.deb"},
		{"common mime =", fq(mf("mime", "=", "image")), "gh5.jpg,plain.jpg,r5.jpg"},
		{"common size <", fq(mf("size", "<", "100")), "readme.txt"},
		{"common mtime > past", fq(mf("mtime", ">", "946684800"), mf("ext", "=", "txt")), "readme.txt"},
		{"legacy ext filter + meta", types.SearchQuery{MinSize: -1, MaxSize: -1, Exts: []string{"jpg"}, Meta: []types.MetaFilter{mf("image.gps", "=", "no")}}, "gh5.jpg,r5.jpg"},
		{"legacy path filter only", types.SearchQuery{MinSize: -1, MaxSize: -1, Path: filepath.Join(root, "pkgs")}, "tool.deb"},
		{"q plus meta filter", types.SearchQuery{Q: "jpg", MinSize: -1, MaxSize: -1, Meta: []types.MetaFilter{mf("image.iso", ">", "1000")}}, "r5.jpg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits, total := mustSearch(t, s, c.q)
			if got := names(hits); got != c.want {
				t.Errorf("hits = %q, want %q", got, c.want)
			}
			if total != len(hits) {
				t.Errorf("total %d != len(hits) %d", total, len(hits))
			}
			for _, h := range hits {
				if h.MatchedIn != "name" || h.Entry.Path == "" || h.Entry.Size < 0 {
					t.Errorf("hit shape: %+v", h)
				}
			}
		})
	}
}

func TestMetaFilterValidation(t *testing.T) {
	root := metaRoot(t)
	s := newService(t, testConfig(root))
	crawl(t, s)
	bad := map[string]types.SearchQuery{
		"no q no filters":             {MinSize: -1, MaxSize: -1},
		"q of only operators":         {Q: "AND OR", MinSize: -1, MaxSize: -1},
		"unknown key":                 fq(mf("video.nope", "=", "x")),
		"unknown op":                  fq(mf("image.iso", "==", "800")),
		"ordering on text":            fq(mf("image.cameraMake", ">", "a")),
		"ordering on enum":            fq(mf("video.hdr", "<", "hdr10")),
		"non-numeric value":           fq(mf("image.iso", ">", "eight hundred")),
		"non-date value":              fq(mf("image.dateTaken", ">=", "yesterday")),
		"~ on common numeric":         fq(mf("size", "~", "1")),
		"bad sort":                    {Q: "x", MinSize: -1, MaxSize: -1, Sort: "color"},
		"bad dir":                     {Q: "x", MinSize: -1, MaxSize: -1, Sort: "name", Dir: "sideways"},
		"too many filters":            fq(append(make([]types.MetaFilter, 0), repeatFilter(17)...)...),
		"injection in value is inert": fq(mf("image.cameraMake", "=", "x' OR 1=1 --")),
	}
	for name, q := range bad {
		t.Run(name, func(t *testing.T) {
			hits, _, err := s.Search(context.Background(), q)
			if name == "injection in value is inert" {
				if err != nil || len(hits) != 0 {
					t.Errorf("injection: hits=%d err=%v", len(hits), err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error, got %d hits", len(hits))
			}
			if apiCode(t, err) != types.ErrBadRequest {
				t.Errorf("code = %s", apiCode(t, err))
			}
		})
	}
}

func repeatFilter(n int) []types.MetaFilter {
	out := make([]types.MetaFilter, n)
	for i := range out {
		out[i] = mf("image.iso", ">", "0")
	}
	return out
}

// --- sorting --------------------------------------------------------------------

func TestSearchSortOrders(t *testing.T) {
	root := t.TempDir()
	// Distinct sizes and mtimes, names that sort differently from both.
	files := []struct {
		name string
		size int
		age  time.Duration
	}{
		{"bravo.log", 300, 3 * time.Hour},
		{"Alpha.log", 100, 1 * time.Hour},
		{"charlie.log", 200, 2 * time.Hour},
	}
	for _, f := range files {
		p := filepath.Join(root, f.name)
		writeFile(t, p, strings.Repeat("x", f.size))
		tm := time.Now().Add(-f.age)
		os.Chtimes(p, tm, tm)
	}
	s := newService(t, testConfig(root))
	crawl(t, s)

	cases := []struct {
		sort, dir, want string
		q               string
	}{
		{"", "", "Alpha.log,bravo.log,charlie.log", ""},          // filter-only default: name asc, case-insensitive
		{"relevance", "", "Alpha.log,bravo.log,charlie.log", ""}, // relevance without q → name
		{"name", "desc", "charlie.log,bravo.log,Alpha.log", ""},
		{"size", "asc", "Alpha.log,charlie.log,bravo.log", ""},
		{"size", "desc", "bravo.log,charlie.log,Alpha.log", ""},
		{"mtime", "asc", "bravo.log,charlie.log,Alpha.log", ""},
		{"mtime", "desc", "Alpha.log,charlie.log,bravo.log", ""},
		{"size", "desc", "bravo.log,charlie.log,Alpha.log", "log"}, // with q: explicit sort wins over relevance
		{"name", "", "Alpha.log,bravo.log,charlie.log", "log"},
	}
	for _, c := range cases {
		q := types.SearchQuery{Q: c.q, MinSize: -1, MaxSize: -1, Sort: c.sort, Dir: c.dir, Exts: []string{"log"}}
		hits, _ := mustSearch(t, s, q)
		if got := names(hits); got != c.want {
			t.Errorf("sort=%q dir=%q q=%q: %q, want %q", c.sort, c.dir, c.q, got, c.want)
		}
	}
	// Paging under an explicit sort is stable.
	q := types.SearchQuery{MinSize: -1, MaxSize: -1, Sort: "size", Dir: "asc", Exts: []string{"log"}, Limit: 1, Offset: 1}
	hits, total := mustSearch(t, s, q)
	if names(hits) != "charlie.log" || total != 3 {
		t.Errorf("page 2 of size asc = %q total %d", names(hits), total)
	}
}

func TestSearchSortBothModeMerge(t *testing.T) {
	// Content indexing on: "needle" appears in file names and in bodies, so
	// both branches contribute and the merge must honour the sort.
	root := t.TempDir()
	cfg := contentConfig(root)
	writeFile(t, filepath.Join(root, "needle-a.txt"), "nothing here")            // name hit only
	writeFile(t, filepath.Join(root, "zzz.txt"), "a needle in the body")         // content hit only
	writeFile(t, filepath.Join(root, "needle-b.txt"), "needle in name and body") // both
	writeFile(t, filepath.Join(root, "mmm.txt"), "another needle body")          // content hit only
	s := newService(t, cfg)
	crawl(t, s)

	q := types.SearchQuery{Q: "needle", Mode: "both", MinSize: -1, MaxSize: -1, Sort: "name", Dir: "asc"}
	hits, total := mustSearch(t, s, q)
	if names(hits) != "mmm.txt,needle-a.txt,needle-b.txt,zzz.txt" || total != 4 {
		t.Errorf("both/name asc: %q total %d", names(hits), total)
	}
	q.Dir = "desc"
	hits, _ = mustSearch(t, s, q)
	if names(hits) != "zzz.txt,needle-b.txt,needle-a.txt,mmm.txt" {
		t.Errorf("both/name desc: %q", names(hits))
	}
	// Paging through the merged, sorted result is exact.
	q.Dir, q.Limit, q.Offset = "asc", 2, 2
	hits, _ = mustSearch(t, s, q)
	if names(hits) != "needle-b.txt,zzz.txt" {
		t.Errorf("both/name page 2: %q", names(hits))
	}
	// The dedupe keeps the snippet of a name hit that also matched content.
	q.Limit, q.Offset = 10, 0
	hits, _ = mustSearch(t, s, q)
	for _, h := range hits {
		if h.Entry.Name == "needle-b.txt" && !strings.Contains(h.Snippet, "<mark>") {
			t.Error("merged name hit lost its content snippet")
		}
	}
}

// --- MetaValues -----------------------------------------------------------------

func TestMetaValues(t *testing.T) {
	root := metaRoot(t)
	writeBytes(t, filepath.Join(root, "more", "p2.jpg"), exifJPEG("Panasonic", "DC-G9", 200, "2022:02:02 02:02:02"))
	s := newService(t, testConfig(root))
	crawl(t, s)
	ctx := context.Background()

	vals, err := s.MetaValues(ctx, "image.cameraMake", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(vals) != "[{Panasonic 2} {Canon 1}]" {
		t.Errorf("cameraMake values = %v", vals)
	}
	vals, _ = s.MetaValues(ctx, "image.cameraMake", "pan", "", 10)
	if fmt.Sprint(vals) != "[{Panasonic 2}]" {
		t.Errorf("prefix narrowing = %v", vals)
	}
	vals, _ = s.MetaValues(ctx, "image.cameraMake", "", filepath.Join(root, "photos"), 10)
	if fmt.Sprint(vals) != "[{Canon 1} {Panasonic 1}]" {
		t.Errorf("path scope narrowing = %v (ties break by value asc)", vals)
	}
	vals, _ = s.MetaValues(ctx, "image.cameraModel", "", "", 1)
	if len(vals) != 1 {
		t.Errorf("limit not applied: %v", vals)
	}
	vals, _ = s.MetaValues(ctx, "package.depends", "", "", 0)
	if fmt.Sprint(vals) != "[{libc6 1} {zlib1g 1}]" {
		t.Errorf("multi-valued key values = %v", vals)
	}
	vals, _ = s.MetaValues(ctx, "ext", "", "", 0)
	if fmt.Sprint(vals) != "[{jpg 4} {deb 1} {txt 1}]" {
		t.Errorf("ext values = %v", vals)
	}
	vals, _ = s.MetaValues(ctx, "ext", ".J", "", 0)
	if fmt.Sprint(vals) != "[{jpg 4}]" {
		t.Errorf("ext prefix = %v", vals)
	}
	vals, _ = s.MetaValues(ctx, "mime", "", "", 0)
	if fmt.Sprint(vals) != "[{image 4} {archive 1} {text 1}]" {
		t.Errorf("mime values = %v", vals)
	}
	if _, err := s.MetaValues(ctx, "video.nope", "", "", 0); apiCode(t, err) != types.ErrBadRequest {
		t.Error("unknown key must be BAD_REQUEST")
	}
	if _, err := s.MetaValues(ctx, "size", "", "", 0); apiCode(t, err) != types.ErrBadRequest {
		t.Error("non-enumerable common key must be BAD_REQUEST")
	}
	// LIKE metacharacters in the prefix are literal.
	vals, _ = s.MetaValues(ctx, "image.cameraMake", "%", "", 0)
	if len(vals) != 0 {
		t.Errorf("%% prefix matched: %v", vals)
	}
	// Root confinement: narrowing the roots hides values outside them.
	cfg := s.Config()
	cfg.Roots = []string{filepath.Join(root, "more")}
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	vals, _ = s.MetaValues(ctx, "image.cameraMake", "", "", 0)
	if fmt.Sprint(vals) != "[{Panasonic 1}]" {
		t.Errorf("after narrowing = %v", vals)
	}
	// The catalog is served as-is.
	if cats := s.MetaFields(); len(cats) < 5 || cats[0].ID != "common" {
		t.Errorf("MetaFields = %d categories", len(cats))
	}
}

// --- migration --------------------------------------------------------------------

// schemaV1DDL is the shipped v1 schema, verbatim, so the migration test
// starts from what real installations have on disk.
var schemaV1DDL = []string{
	`CREATE TABLE files (
		id              INTEGER PRIMARY KEY,
		path            TEXT NOT NULL UNIQUE,
		dir             TEXT NOT NULL,
		name            TEXT NOT NULL,
		ext             TEXT NOT NULL DEFAULT '',
		size            INTEGER NOT NULL DEFAULT 0,
		mtime           INTEGER NOT NULL DEFAULT 0,
		mimeclass       TEXT NOT NULL DEFAULT '',
		content_indexed INTEGER NOT NULL DEFAULT 0,
		gen             INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX files_dir_idx ON files(dir)`,
	`CREATE INDEX files_ext_idx ON files(ext)`,
	`CREATE INDEX files_size_idx ON files(size)`,
	`CREATE INDEX files_mtime_idx ON files(mtime)`,
	`CREATE INDEX files_gen_idx ON files(gen)`,
	`CREATE INDEX files_ci_idx ON files(content_indexed)`,
	`CREATE VIRTUAL TABLE names_fts USING fts5(name, dir, content='files', content_rowid='id', tokenize="unicode61 tokenchars '._-'")`,
	`CREATE TRIGGER files_fts_ai AFTER INSERT ON files BEGIN
		INSERT INTO names_fts(rowid, name, dir) VALUES (new.id, new.name, new.dir);
	END`,
	`CREATE TRIGGER files_fts_ad AFTER DELETE ON files BEGIN
		INSERT INTO names_fts(names_fts, rowid, name, dir) VALUES ('delete', old.id, old.name, old.dir);
	END`,
	`CREATE TRIGGER files_fts_au AFTER UPDATE OF name, dir ON files BEGIN
		INSERT INTO names_fts(names_fts, rowid, name, dir) VALUES ('delete', old.id, old.name, old.dir);
		INSERT INTO names_fts(rowid, name, dir) VALUES (new.id, new.name, new.dir);
	END`,
	`CREATE VIRTUAL TABLE content_fts USING fts5(path UNINDEXED, body, tokenize='unicode61')`,
	`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`PRAGMA user_version = 1`,
}

func TestMigrateV1ToV2(t *testing.T) {
	root := metaRoot(t)
	dbPath := filepath.Join(t.TempDir(), "index.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range schemaV1DDL {
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatalf("v1 ddl: %v", err)
		}
	}
	// Pre-existing rows: one real file (so the crawl keeps it) and one
	// content document, plus the generation counter.
	gh5 := filepath.Join(root, "photos", "gh5.jpg")
	st, _ := os.Stat(gh5)
	raw.Exec(`INSERT INTO files(path, dir, name, ext, size, mtime, mimeclass, content_indexed, gen) VALUES (?,?,?,?,?,?,?,1,5)`,
		gh5, filepath.Dir(gh5), "gh5.jpg", "jpg", st.Size(), st.ModTime().Unix(), "image")
	raw.Exec(`INSERT INTO content_fts(path, body) VALUES (?, 'legacy body text')`, gh5)
	raw.Exec(`INSERT INTO meta(key, value) VALUES ('generation', '5'), ('last_full_scan', '1700000000')`)
	raw.Close()

	s, err := New(dbPath, testConfig(root), Options{AllowedRoots: []string{root}})
	if err != nil {
		t.Fatalf("New on v1 db: %v", err)
	}
	defer s.Close()
	var v int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&n)
	if n != 1 {
		t.Errorf("files rows after migration = %d", n)
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&n)
	if n != 1 {
		t.Errorf("content rows after migration = %d", n)
	}
	var me int
	if err := s.db.QueryRow(`SELECT meta_extracted FROM files`).Scan(&me); err != nil || me != 0 {
		t.Errorf("meta_extracted column: %v %d", err, me)
	}
	if s.Status().LastFullScan != 1700000000 || s.Status().MetaIndexed != 0 {
		t.Errorf("status after migration: %+v", s.Status())
	}
	// A crawl then extracts the pre-existing file (meta_extracted was 0)
	// without re-reading its content (content_indexed stayed 1).
	crawl(t, s)
	if s.Status().MetaIndexed != 3 || s.extracted.Load() != 0 {
		t.Errorf("post-migration crawl: %+v extracted=%d", s.Status(), s.extracted.Load())
	}
	hits, _ := mustSearch(t, s, fq(mf("image.cameraModel", "=", "DC-GH5")))
	if names(hits) != "gh5.jpg" {
		t.Errorf("migrated file not searchable by metadata: %q", names(hits))
	}
	// Reopening a v2 database is a no-op migration.
	s.Close()
	s2, err := New(dbPath, testConfig(root), Options{AllowedRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	// A database from the future is refused.
	raw, _ = sql.Open("sqlite", "file:"+dbPath)
	raw.Exec(`PRAGMA user_version = 99`)
	raw.Close()
	if _, err := New(dbPath, testConfig(root), Options{AllowedRoots: []string{root}}); err == nil {
		t.Error("newer schema must be refused")
	}
}

// --- ffprobe-backed (skipped without ffmpeg) ----------------------------------------

func ffTools() (ffmpeg, ffprobe string) {
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		f, p := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "ffprobe")
		if _, err := os.Stat(f); err == nil {
			if _, err := os.Stat(p); err == nil {
				return f, p
			}
		}
	}
	f, err1 := exec.LookPath("ffmpeg")
	p, err2 := exec.LookPath("ffprobe")
	if err1 == nil && err2 == nil {
		return f, p
	}
	return "", ""
}

func TestMetaVideoThroughFFprobe(t *testing.T) {
	ffmpeg, ffprobe := ffTools()
	if ffmpeg == "" {
		t.Skip("ffmpeg not available")
	}
	root := t.TempDir()
	gen := func(name string, args ...string) {
		full := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "smptebars=size=64x64:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000", "-t", "0.4"}, args...)
		full = append(full, filepath.Join(root, name))
		if out, err := exec.Command(ffmpeg, full...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
	}
	gen("dd.mkv", "-map", "0:v", "-map", "1:a", "-map", "2:a", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a:0", "ac3", "-ac:a:0", "6", "-metadata:s:a:0", "language=eng", "-c:a:1", "aac", "-ac:a:1", "2", "-metadata:s:a:1", "language=fre")
	gen("aac.mp4", "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac")
	gen("song.mp3", "-map", "1:a", "-c:a", "libmp3lame", "-b:a", "64k", "-metadata", "artist=Crawler")

	// Without ffprobe: video files are not candidates; the mp3 still gets
	// its tags but no stream facts.
	s0 := newService(t, testConfig(root))
	crawl(t, s0)
	if got := s0.metaExtracted.Load(); got != 1 {
		t.Errorf("without ffprobe: %d extraction attempts, want 1 (song.mp3)", got)
	}
	if hits, _ := mustSearch(t, s0, fq(mf("audio.artist", "=", "crawler"))); names(hits) != "song.mp3" {
		t.Errorf("mp3 tags without ffprobe: %q", names(hits))
	}
	if hits, _ := mustSearch(t, s0, fq(mf("audio.durationSec", ">", "0"))); len(hits) != 0 {
		t.Errorf("duration without ffprobe: %q", names(hits))
	}
	s0.Close()

	s, err := New(filepath.Join(t.TempDir(), "index.db"), testConfig(root), Options{AllowedRoots: []string{root}, FFprobePath: ffprobe})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	crawl(t, s)
	if s.Status().MetaIndexed != 3 {
		t.Errorf("MetaIndexed = %d", s.Status().MetaIndexed)
	}
	check := func(want string, filters ...types.MetaFilter) {
		t.Helper()
		hits, _ := mustSearch(t, s, fq(filters...))
		if names(hits) != want {
			t.Errorf("%v: %q, want %q", filters, names(hits), want)
		}
	}
	check("dd.mkv", mf("video.audioCodec", "=", "ac3"))
	check("aac.mp4,dd.mkv", mf("video.audioCodec", "=", "aac"))                           // multi-valued: dd.mkv has both
	check("aac.mp4", mf("video.codec", "=", "h264"), mf("video.audioCodec", "!=", "ac3")) // != excludes the file with an ac3 track
	check("dd.mkv", mf("video.audioChannels", ">=", "6"))
	check("dd.mkv", mf("video.audioLang", "=", "fre"))
	check("aac.mp4,dd.mkv", mf("video.hdr", "=", "none"))
	check("aac.mp4,dd.mkv", mf("video.codec", "=", "h264"), mf("video.width", "=", "64"))
	check("dd.mkv", mf("video.container", "=", "matroska"))
	check("song.mp3", mf("audio.durationSec", ">", "0.3"), mf("audio.codec", "=", "mp3"))
	vals, _ := s.MetaValues(context.Background(), "video.audioCodec", "", "", 0)
	if fmt.Sprint(vals) != "[{aac 2} {ac3 1}]" {
		t.Errorf("audioCodec values = %v", vals)
	}
}
