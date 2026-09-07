package archive

import (
	"strings"
	"testing"
)

func TestNewTableImplicitDirsAndLookup(t *testing.T) {
	tbl := mustTable(t, []entryRecord{
		{raw: "a/b/c.txt", size: 3},
		{raw: "a/d.txt", size: 1},
		{raw: "top.txt", size: 2},
		{raw: "x/", isDir: true}, // explicit dir marker with trailing slash
	})

	// Root listing: implicit "a", explicit "x", file "top.txt".
	recs, ok := tbl.listDir("")
	if !ok {
		t.Fatal("root not listable")
	}
	got := make([]string, len(recs))
	for i, r := range recs {
		got[i] = r.path
	}
	eqStrings(t, got, []string{"a", "top.txt", "x"})

	// Implicit intermediate dirs exist and are typed as dirs.
	for _, dir := range []string{"a", "a/b", "x"} {
		rec, found := tbl.lookup(dir)
		if !found || !rec.isDir {
			t.Errorf("dir %q: found=%v isDir=%v", dir, found, rec.isDir)
		}
	}
	// One-level listing only.
	recs, ok = tbl.listDir("a")
	if !ok {
		t.Fatal("a not listable")
	}
	got = got[:0]
	for _, r := range recs {
		got = append(got, r.path)
	}
	eqStrings(t, got, []string{"a/b", "a/d.txt"})

	// File lookup.
	rec, found := tbl.lookup("a/b/c.txt")
	if !found || rec.isDir || rec.size != 3 {
		t.Fatalf("file lookup: found=%v rec=%+v", found, rec)
	}
	// Root lookup.
	rec, found = tbl.lookup("")
	if !found || !rec.isDir {
		t.Fatalf("root lookup: %+v", rec)
	}
	// Missing paths.
	if _, found := tbl.lookup("nope"); found {
		t.Error("lookup(nope) found")
	}
	if _, ok := tbl.listDir("a/b/c.txt"); ok {
		t.Error("listDir on a file succeeded")
	}
	if _, ok := tbl.listDir("nope"); ok {
		t.Error("listDir on missing dir succeeded")
	}
}

func TestNewTableDropsHostileNames(t *testing.T) {
	tbl := mustTable(t, []entryRecord{
		{raw: "../evil.txt", size: 1},
		{raw: "/abs.txt", size: 1},
		{raw: "a/../../up.txt", size: 1},
		{raw: "..", isDir: true},
		{raw: ".", isDir: true},
		{raw: "", size: 1},
		{raw: "bad\x00null.txt", size: 1},
		{raw: "ok.txt", size: 1},
		{raw: "sub/./also-ok.txt", size: 1}, // cleans to sub/also-ok.txt
	})
	recs, ok := tbl.listDir("")
	if !ok {
		t.Fatal("root not listable")
	}
	got := make([]string, len(recs))
	for i, r := range recs {
		got[i] = r.path
	}
	eqStrings(t, got, []string{"ok.txt", "sub"})
	if _, found := tbl.lookup("sub/also-ok.txt"); !found {
		t.Error("cleaned nested name not reachable")
	}
	if _, found := tbl.lookup("evil.txt"); found {
		t.Error("hostile name leaked into table")
	}
}

func TestTableDirWinsOverSameNamedFile(t *testing.T) {
	tbl := mustTable(t, []entryRecord{
		{raw: "thing", size: 5},           // file "thing"
		{raw: "thing/child.txt", size: 1}, // implies dir "thing"
	})
	rec, found := tbl.lookup("thing")
	if !found || !rec.isDir {
		t.Fatalf("dir should win: %+v", rec)
	}
}

func TestCleanEntryName(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"a/b.txt", "a/b.txt", true},
		{"a//b.txt", "a/b.txt", true},
		{"./a", "a", true},
		{"a/./b", "a/b", true},
		{"dir/", "dir", true},
		{"", "", false},
		{"/abs", "", false},
		{"..", "", false},
		{".", "", false},
		{"../x", "", false},
		{"a/../../x", "", false},
		{"a/..", "", false},
		{"nul\x00l", "", false},
	}
	for _, c := range cases {
		got, ok := cleanEntryName(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("cleanEntryName(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCleanRequestPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"/", "", true},
		{"///", "", true},
		{".", "", true},
		{"a/..", "", true}, // resolves to the archive root
		{"a/b", "a/b", true},
		{"/a/b/", "a/b", true},
		{"a/../b", "b", true},
		{"..", "", false},
		{"../x", "", false},
		{"a/../../x", "", false},
		{"nul\x00l", "", false},
	}
	for _, c := range cases {
		got, ok := cleanRequestPath(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("cleanRequestPath(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTableCacheLRU(t *testing.T) {
	c := newTableCache(2, 1<<30)
	ta, tb, tc := mustTable(t, nil), mustTable(t, nil), mustTable(t, nil)

	c.put("a", ta)
	c.put("b", tb)
	if got, ok := c.get("a"); !ok || got != ta {
		t.Fatal("a missing")
	}
	// "a" was just touched, so adding "c" must evict "b".
	c.put("c", tc)
	if _, ok := c.get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("a should have survived")
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("c should be present")
	}
	// Re-putting an existing key replaces the value without growing.
	ta2 := mustTable(t, nil)
	c.put("a", ta2)
	if got, _ := c.get("a"); got != ta2 {
		t.Fatal("re-put did not replace value")
	}
	c.mu.Lock()
	n, k := len(c.vals), len(c.keys)
	c.mu.Unlock()
	if n != 2 || k != 2 {
		t.Fatalf("cache size = %d vals / %d keys, want 2/2", n, k)
	}
}

func TestBaseAndParentOf(t *testing.T) {
	if baseOf("a/b/c") != "c" || baseOf("c") != "c" {
		t.Error("baseOf")
	}
	if parentOf("a/b/c") != "a/b" || parentOf("c") != "" {
		t.Error("parentOf")
	}
	if joinEntry("", "x") != "x" || joinEntry("a", "x") != "a/x" {
		t.Error("joinEntry")
	}
}

func TestDetectFormat(t *testing.T) {
	cases := map[string]format{
		"a.zip":          fmtZip,
		"A.JAR":          fmtZip,
		"book.epub":      fmtZip,
		"comic.cbz":      fmtZip,
		"a.tar":          fmtTar,
		"a.tar.gz":       fmtTarGz,
		"a.tgz":          fmtTarGz,
		"a.tar.bz2":      fmtTarBz2,
		"a.tbz2":         fmtTarBz2,
		"a.tar.xz":       fmtTarXz,
		"a.txz":          fmtTarXz,
		"a.tar.zst":      fmtTarZst,
		"a.gz":           fmtGz,
		"a.bz2":          fmtBz2,
		"a.xz":           fmtXz,
		"a.zst":          fmtZst,
		"a.7z":           fmt7z,
		"a.rar":          fmt7z,
		"a.iso":          fmt7z,
		"a.deb":          fmt7z,
		"a.txt":          fmtUnknown,
		"tar":            fmtUnknown, // no dot prefix
		".gz":            fmtUnknown, // extension only, no stem
		"noextension":    fmtUnknown,
		"tricky.zip.txt": fmtUnknown,
	}
	for name, want := range cases {
		if got := detectFormat(name); got != want {
			t.Errorf("detectFormat(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestBareEntryName(t *testing.T) {
	cases := map[string]string{
		"notes.txt.gz": "notes.txt",
		"data.bz2":     "data",
		"a.xz":         "a",
		"b.zst":        "b",
		"UPPER.GZ":     "UPPER",
	}
	for in, want := range cases {
		if got := bareEntryName(in); got != want {
			t.Errorf("bareEntryName(%q) = %q, want %q", in, got, want)
		}
	}
	// Degenerate stems fall back to a safe synthetic name.
	if got := bareEntryName(".gz"); got != "data" && !strings.Contains(got, "data") {
		t.Errorf("bareEntryName(.gz) = %q", got)
	}
}
