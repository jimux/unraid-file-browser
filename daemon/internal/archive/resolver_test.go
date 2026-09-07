package archive

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unraid-filebrowser/internal/types"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// --- filesystem-first resolution --------------------------------------------

func TestFilesystemFirstWeirdDir(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	weird := filepath.Join(root, "weird!")
	writeFile(t, filepath.Join(weird, "child.txt"), []byte("real file"))

	p := weird + "/child.txt" // literally contains "!/" but exists on disk
	if !strings.Contains(p, sep) {
		t.Fatal("test path must contain the separator")
	}
	if r.IsVirtual(p) {
		t.Fatalf("IsVirtual(%q) = true, want false (filesystem-first)", p)
	}
	e, err := r.Stat(ctxT(t), p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if e.Type != types.TypeFile || e.Name != "child.txt" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	rc, _, err := r.Open(ctxT(t), p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || string(data) != "real file" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

func TestIsVirtual(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	writeFile(t, filepath.Join(root, "a.zip"), zipBytes(t, zEntry{name: "x.txt", data: "x"}))

	if r.IsVirtual(filepath.Join(root, "a.zip")) {
		t.Error("plain real path reported virtual")
	}
	if !r.IsVirtual(filepath.Join(root, "a.zip") + sep + "x.txt") {
		t.Error("archive-entry path not reported virtual")
	}
	if r.IsVirtual(filepath.Join(root, "no-separator.txt")) {
		t.Error("path without separator reported virtual")
	}
}

func TestRealDirectoryIsNotAnArchive(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := r.List(ctxT(t), filepath.Join(root, "plain"))
	wantCode(t, err, types.ErrArchive)
	// "plain!/x": literal does not exist, left resolves to a real directory,
	// which is not an archive.
	_, err = r.List(ctxT(t), filepath.Join(root, "plain")+sep+"x")
	wantCode(t, err, types.ErrArchive)
	_, err = r.Stat(ctxT(t), filepath.Join(root, "plain")+sep+"x")
	wantCode(t, err, types.ErrArchive)
}

func TestForbiddenPassthrough(t *testing.T) {
	r, _ := newTestResolver(t, Options{})
	_, err := r.List(ctxT(t), "/definitely/outside/root.zip"+sep+"x")
	wantCode(t, err, types.ErrForbidden)
	_, err = r.Stat(ctxT(t), "/definitely/outside/root.txt")
	wantCode(t, err, types.ErrForbidden)
}

// --- zip ---------------------------------------------------------------------

func TestZipListRootAndImplicitDirs(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	zp := filepath.Join(root, "x.zip")
	// No explicit directory headers: "a" and "a/b" must be synthesized.
	writeFile(t, zp, zipBytes(t,
		zEntry{name: "a/b/c.txt", data: "ccc"},
		zEntry{name: "a/d.txt", data: "ddd"},
		zEntry{name: "top.txt", data: "top"},
	))

	entries, err := r.List(ctxT(t), zp)
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	eqStrings(t, names(entries), []string{"a", "top.txt"})
	a := findEntry(t, entries, "a")
	if a.Type != types.TypeDir {
		t.Fatalf("implicit dir type = %s", a.Type)
	}
	if a.Path != zp+sep+"a" {
		t.Fatalf("dir path = %q", a.Path)
	}
	top := findEntry(t, entries, "top.txt")
	if top.Type != types.TypeFile || top.Size != 3 {
		t.Fatalf("top.txt entry: %+v", top)
	}
	if !strings.HasPrefix(top.Mime, "text/plain") {
		t.Fatalf("top.txt mime = %q", top.Mime)
	}
	if top.Mtime != fixedTime.Unix() {
		t.Fatalf("top.txt mtime = %d, want %d", top.Mtime, fixedTime.Unix())
	}

	// Trailing-separator root form must list identically.
	entries2, err := r.List(ctxT(t), zp+sep)
	if err != nil {
		t.Fatalf("List root with trailing sep: %v", err)
	}
	eqStrings(t, names(entries2), []string{"a", "top.txt"})

	// One-level subdir listing.
	sub, err := r.List(ctxT(t), zp+sep+"a")
	if err != nil {
		t.Fatalf("List subdir: %v", err)
	}
	eqStrings(t, names(sub), []string{"b", "d.txt"})
	if findEntry(t, sub, "b").Type != types.TypeDir {
		t.Fatal("a/b should be a dir")
	}
	deep, err := r.List(ctxT(t), zp+sep+"a/b")
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, names(deep), []string{"c.txt"})
	if got := deep[0].Path; got != zp+sep+"a/b/c.txt" {
		t.Fatalf("deep path = %q", got)
	}
}

func TestZipStatAndOpenEntry(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	zp := filepath.Join(root, "x.zip")
	writeFile(t, zp, zipBytes(t, zEntry{name: "a/b/c.txt", data: "hello from zip"}))

	vp := zp + sep + "a/b/c.txt"
	e, err := r.Stat(ctxT(t), vp)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if e.Type != types.TypeFile || e.Size != int64(len("hello from zip")) || e.Path != vp {
		t.Fatalf("entry: %+v", e)
	}
	rc, oe, err := r.Open(ctxT(t), vp)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if oe.Path != vp {
		t.Fatalf("open entry path = %q", oe.Path)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || string(data) != "hello from zip" {
		t.Fatalf("read = %q, %v", data, err)
	}

	// Root of the archive stats as a directory.
	re, err := r.Stat(ctxT(t), zp+sep)
	if err != nil {
		t.Fatal(err)
	}
	if re.Type != types.TypeDir {
		t.Fatalf("archive root type = %s", re.Type)
	}

	// A file entry is not listable, a dir entry is not openable.
	_, err = r.List(ctxT(t), vp)
	wantCode(t, err, types.ErrArchive)
	_, _, err = r.Open(ctxT(t), zp+sep+"a/b")
	wantCode(t, err, types.ErrArchive)
}

func TestZipMissingEntry(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	zp := filepath.Join(root, "x.zip")
	writeFile(t, zp, zipBytes(t, zEntry{name: "there.txt", data: "x"}))
	_, err := r.Stat(ctxT(t), zp+sep+"missing.txt")
	wantCode(t, err, types.ErrNotFound)
	_, err = r.List(ctxT(t), zp+sep+"missing-dir")
	wantCode(t, err, types.ErrNotFound)
}

// --- zip-slip ----------------------------------------------------------------

// hostileZipBytes builds a zip whose stored names try to escape: the names
// are written with safe placeholders, then byte-patched (same length) so the
// zip writer cannot sanitize them.
func hostileZipBytes(t *testing.T) []byte {
	t.Helper()
	raw := zipBytes(t,
		zEntry{name: "AB/evil.txt", data: "evil"}, // → ../evil.txt
		zEntry{name: "Xabs.txt", data: "abs"},     // → /abs.txt
		zEntry{name: "ok.txt", data: "fine"},
	)
	raw = bytes.ReplaceAll(raw, []byte("AB/evil.txt"), []byte("../evil.txt"))
	raw = bytes.ReplaceAll(raw, []byte("Xabs.txt"), []byte("/abs.txt"))
	return raw
}

func TestZipSlipNamesRejected(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	zp := filepath.Join(root, "evil.zip")
	writeFile(t, zp, hostileZipBytes(t))

	entries, err := r.List(ctxT(t), zp)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	eqStrings(t, names(entries), []string{"ok.txt"})

	// Traversal in the request path is rejected too.
	_, err = r.Stat(ctxT(t), zp+sep+"../evil.txt")
	wantCode(t, err, types.ErrArchive)
	_, _, err = r.Open(ctxT(t), zp+sep+"..")
	wantCode(t, err, types.ErrArchive)
	// The patched names must not be reachable under their cleaned forms.
	_, err = r.Stat(ctxT(t), zp+sep+"evil.txt")
	wantCode(t, err, types.ErrNotFound)
	_, err = r.Stat(ctxT(t), zp+sep+"abs.txt")
	wantCode(t, err, types.ErrNotFound)
}

func TestTarSlipNamesRejected(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	tp := filepath.Join(root, "evil.tar")
	writeFile(t, tp, tarBytes(t,
		tEntry{name: "../escape.txt", data: "evil"},
		tEntry{name: "/abs.txt", data: "evil"},
		tEntry{name: "nested/../../up.txt", data: "evil"},
		tEntry{name: "good.txt", data: "fine"},
	))
	entries, err := r.List(ctxT(t), tp)
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, names(entries), []string{"good.txt"})
}

// --- tar family --------------------------------------------------------------

func TestTarGzWithSymlinkAndExplicitDirs(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	tp := filepath.Join(root, "site.tar.gz")
	writeFile(t, tp, gzBytes(t, tarBytes(t,
		tEntry{name: "www", dir: true},
		tEntry{name: "www/index.html", data: "<html>"},
		tEntry{name: "www/link", symlink: "index.html"},
		tEntry{name: "www/js/app.js", data: "app"}, // implicit "www/js"
	)))

	entries, err := r.List(ctxT(t), tp)
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, names(entries), []string{"www"})

	sub, err := r.List(ctxT(t), tp+sep+"www")
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, names(sub), []string{"index.html", "js", "link"})
	link := findEntry(t, sub, "link")
	if link.Type != types.TypeSymlink || link.Target != "index.html" {
		t.Fatalf("symlink entry: %+v", link)
	}
	if findEntry(t, sub, "js").Type != types.TypeDir {
		t.Fatal("implicit tar dir missing")
	}

	// Symlinks inside archives cannot be streamed.
	_, _, err = r.Open(ctxT(t), tp+sep+"www/link")
	wantCode(t, err, types.ErrArchive)

	rc, _, err := r.Open(ctxT(t), tp+sep+"www/index.html")
	if err != nil {
		t.Fatal(err)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || string(data) != "<html>" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

// --- nested archives ---------------------------------------------------------

func TestNestedZipInTarGz(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	inner := zipBytes(t,
		zEntry{name: "deep/file.txt", data: "nested content"},
		zEntry{name: "deep/inner2.tar", data: "not really a tar"},
	)
	tp := filepath.Join(root, "bk.tar.gz")
	writeFile(t, tp, gzBytes(t, tarBytes(t,
		tEntry{name: "sub/inner.zip", data: string(inner)},
		tEntry{name: "sub/readme.txt", data: "readme"},
	)))

	// The nested zip is marked browsable in its parent listing.
	sub, err := r.List(ctxT(t), tp+sep+"sub")
	if err != nil {
		t.Fatal(err)
	}
	iz := findEntry(t, sub, "inner.zip")
	if iz.Type != types.TypeArchive {
		t.Fatalf("inner.zip type = %s, want archive", iz.Type)
	}
	if iz.Size != int64(len(inner)) {
		t.Fatalf("inner.zip size = %d, want %d", iz.Size, len(inner))
	}

	// Stat of the nested archive file itself.
	se, err := r.Stat(ctxT(t), tp+sep+"sub/inner.zip")
	if err != nil {
		t.Fatal(err)
	}
	if se.Type != types.TypeArchive {
		t.Fatalf("nested stat type = %s", se.Type)
	}

	// Listing drills through the tar.gz layer into the zip.
	entries, err := r.List(ctxT(t), tp+sep+"sub/inner.zip")
	if err != nil {
		t.Fatalf("List nested: %v", err)
	}
	eqStrings(t, names(entries), []string{"deep"})

	deep, err := r.List(ctxT(t), tp+sep+"sub/inner.zip"+sep+"deep")
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, names(deep), []string{"file.txt", "inner2.tar"})
	// An archive-by-extension entry inside the nested zip is marked too.
	if findEntry(t, deep, "inner2.tar").Type != types.TypeArchive {
		t.Fatal("inner2.tar not marked archive")
	}

	vp := tp + sep + "sub/inner.zip" + sep + "deep/file.txt"
	e, err := r.Stat(ctxT(t), vp)
	if err != nil {
		t.Fatal(err)
	}
	if e.Path != vp || e.Size != int64(len("nested content")) {
		t.Fatalf("nested entry: %+v", e)
	}
	rc, _, err := r.Open(ctxT(t), vp)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || string(data) != "nested content" {
		t.Fatalf("read = %q, %v", data, err)
	}

	// Opening the nested archive file itself yields its raw bytes.
	rc2, _, err := r.Open(ctxT(t), tp+sep+"sub/inner.zip")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readAllAndClose(t, rc2)
	if err != nil || !bytes.Equal(raw, inner) {
		t.Fatalf("nested raw read mismatch (len %d vs %d), err %v", len(raw), len(inner), err)
	}
}

func TestDepthCap(t *testing.T) {
	r, root := newTestResolver(t, Options{MaxDepth: 2})
	z3 := zipBytes(t, zEntry{name: "f.txt", data: "bottom"})
	z2 := zipBytes(t, zEntry{name: "c.zip", data: string(z3)})
	z1 := zipBytes(t, zEntry{name: "b.zip", data: string(z2)})
	ap := filepath.Join(root, "a.zip")
	writeFile(t, ap, z1)

	// Depth 2 works: a.zip!/b.zip!/c.zip is a chain of 2 entries.
	e, err := r.Stat(ctxT(t), ap+sep+"b.zip"+sep+"c.zip")
	if err != nil {
		t.Fatalf("depth-2 stat: %v", err)
	}
	if e.Type != types.TypeArchive {
		t.Fatalf("c.zip type = %s", e.Type)
	}

	// Listing c.zip would open a third layer → rejected.
	_, err = r.List(ctxT(t), ap+sep+"b.zip"+sep+"c.zip")
	wantCode(t, err, types.ErrArchive)

	// Resolving an entry inside the third layer is rejected too.
	_, err = r.Stat(ctxT(t), ap+sep+"b.zip"+sep+"c.zip"+sep+"f.txt")
	wantCode(t, err, types.ErrArchive)
	_, _, err = r.Open(ctxT(t), ap+sep+"b.zip"+sep+"c.zip"+sep+"f.txt")
	wantCode(t, err, types.ErrArchive)

	// Default depth (3) reaches the bottom file.
	r3, root3 := newTestResolver(t, Options{})
	ap3 := filepath.Join(root3, "a.zip")
	writeFile(t, ap3, z1)
	rc, _, err := r3.Open(ctxT(t), ap3+sep+"b.zip"+sep+"c.zip"+sep+"f.txt")
	if err != nil {
		t.Fatalf("depth-3 open: %v", err)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || string(data) != "bottom" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

// --- bare single-file compression --------------------------------------------

func TestBareGzSingleEntry(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	gp := filepath.Join(root, "notes.txt.gz")
	writeFile(t, gp, gzBytes(t, []byte("plain text notes")))

	entries, err := r.List(ctxT(t), gp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %v", names(entries))
	}
	e := entries[0]
	if e.Name != "notes.txt" || e.Type != types.TypeFile || e.Size != -1 {
		t.Fatalf("bare entry: %+v", e)
	}
	if e.Path != gp+sep+"notes.txt" {
		t.Fatalf("bare entry path = %q", e.Path)
	}

	rc, oe, err := r.Open(ctxT(t), gp+sep+"notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if oe.Size != -1 {
		t.Fatalf("open entry size = %d, want -1", oe.Size)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || string(data) != "plain text notes" {
		t.Fatalf("read = %q, %v", data, err)
	}

	_, err = r.Stat(ctxT(t), gp+sep+"other.txt")
	wantCode(t, err, types.ErrNotFound)
}

// --- error surface -----------------------------------------------------------

func TestUnsupportedAndCorrupt(t *testing.T) {
	r, root := newTestResolver(t, Options{})

	// Plain file: not an archive at all.
	np := filepath.Join(root, "notes.txt")
	writeFile(t, np, []byte("hello"))
	_, err := r.List(ctxT(t), np)
	wantCode(t, err, types.ErrArchive)
	_, err = r.Stat(ctxT(t), np+sep+"x")
	wantCode(t, err, types.ErrArchive)

	// Archive extension, garbage bytes.
	bp := filepath.Join(root, "bad.zip")
	writeFile(t, bp, []byte("this is not a zip file"))
	_, err = r.List(ctxT(t), bp)
	wantCode(t, err, types.ErrArchive)

	bt := filepath.Join(root, "bad.tar.gz")
	writeFile(t, bt, []byte("this is not gzip"))
	_, err = r.List(ctxT(t), bt)
	wantCode(t, err, types.ErrArchive)

	// Missing real path.
	_, err = r.Stat(ctxT(t), filepath.Join(root, "nope.zip"))
	wantCode(t, err, types.ErrNotFound)
}

func TestSevenZipUnavailable(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	r.seven = "" // simulate no 7zz/7z on PATH
	sp := filepath.Join(root, "x.7z")
	writeFile(t, sp, []byte("irrelevant"))
	_, err := r.List(ctxT(t), sp)
	wantCode(t, err, types.ErrArchive)
	if !strings.Contains(err.Error(), "7") {
		t.Fatalf("error should mention 7-Zip: %v", err)
	}
}

func TestCanceledContextIsTimeout(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	zp := filepath.Join(root, "x.zip")
	writeFile(t, zp, zipBytes(t, zEntry{name: "a.txt", data: "x"}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // request already dead → deterministic ctx.Err() everywhere
	_, err := r.List(ctx, zp)
	wantCode(t, err, types.ErrTimeout)
	_, err = r.Stat(ctx, zp+sep+"a.txt")
	wantCode(t, err, types.ErrTimeout)
	_, _, err = r.Open(ctx, zp+sep+"a.txt")
	wantCode(t, err, types.ErrTimeout)
}

func TestMaxEntryBytesOnOpen(t *testing.T) {
	r, root := newTestResolver(t, Options{MaxEntryBytes: 4})
	zp := filepath.Join(root, "x.zip")
	writeFile(t, zp, zipBytes(t,
		zEntry{name: "big.txt", data: "0123456789"},
		zEntry{name: "exact.txt", data: "1234"},
	))

	rc, _, err := r.Open(ctxT(t), zp+sep+"big.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, err = readAllAndClose(t, rc)
	wantCode(t, err, types.ErrTooLarge)

	// A file of exactly the cap size reads fully.
	rc2, _, err := r.Open(ctxT(t), zp+sep+"exact.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := readAllAndClose(t, rc2)
	if err != nil || string(data) != "1234" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

// --- nested-layer materialization limits -------------------------------------

// incompressible returns n pseudo-random (deflate-resistant) bytes.
func incompressible(n int) []byte {
	buf := make([]byte, n)
	rnd := rand.New(rand.NewSource(42))
	rnd.Read(buf)
	return buf
}

func TestNestedLayerSpillsToTempFile(t *testing.T) {
	old := memSpillBytes
	memSpillBytes = 1024
	t.Cleanup(func() { memSpillBytes = old })

	tmp := t.TempDir()
	r, root := newTestResolver(t, Options{TempDir: tmp})
	payload := incompressible(8 << 10)
	inner := zipBytes(t, zEntry{name: "blob.bin", data: string(payload)}) // ~8KB > 1KB spill point
	tp := filepath.Join(root, "outer.tar")
	writeFile(t, tp, tarBytes(t, tEntry{name: "inner.zip", data: string(inner)}))

	vp := tp + sep + "inner.zip" + sep + "blob.bin"
	rc, _, err := r.Open(ctxT(t), vp)
	if err != nil {
		t.Fatalf("Open through spilled layer: %v", err)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("payload mismatch (len %d), err %v", len(data), err)
	}
	if left := tempLayerFiles(t, tmp); len(left) != 0 {
		t.Fatalf("temp layer files not cleaned up: %v", left)
	}

	// Listing (which also materializes the layer) must clean up too.
	if _, err := r.List(ctxT(t), tp+sep+"inner.zip"); err != nil {
		t.Fatal(err)
	}
	if left := tempLayerFiles(t, tmp); len(left) != 0 {
		t.Fatalf("temp layer files not cleaned up after List: %v", left)
	}
}

func TestNestedLayerOverCapRejected(t *testing.T) {
	oldSpill, oldCap := memSpillBytes, maxLayerBytes
	memSpillBytes = 512
	maxLayerBytes = 2048
	t.Cleanup(func() { memSpillBytes, maxLayerBytes = oldSpill, oldCap })

	tmp := t.TempDir()
	r, root := newTestResolver(t, Options{TempDir: tmp})
	inner := zipBytes(t, zEntry{name: "blob.bin", data: string(incompressible(8 << 10))})
	tp := filepath.Join(root, "outer.tar")
	writeFile(t, tp, tarBytes(t, tEntry{name: "inner.zip", data: string(inner)}))

	_, err := r.List(ctxT(t), tp+sep+"inner.zip")
	wantCode(t, err, types.ErrTooLarge)
	if left := tempLayerFiles(t, tmp); len(left) != 0 {
		t.Fatalf("temp layer files not cleaned up after cap rejection: %v", left)
	}
}

// --- caching -----------------------------------------------------------------

func TestTableCacheReuseAndInvalidation(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	zp := filepath.Join(root, "x.zip")
	writeFile(t, zp, zipBytes(t, zEntry{name: "one.txt", data: "1"}))

	if _, err := r.List(ctxT(t), zp); err != nil {
		t.Fatal(err)
	}
	r.cache.mu.Lock()
	cached := len(r.cache.vals)
	r.cache.mu.Unlock()
	if cached != 1 {
		t.Fatalf("cache size after first List = %d, want 1", cached)
	}
	// Second List must serve from cache (same single slot).
	if _, err := r.List(ctxT(t), zp); err != nil {
		t.Fatal(err)
	}
	r.cache.mu.Lock()
	cached = len(r.cache.vals)
	r.cache.mu.Unlock()
	if cached != 1 {
		t.Fatalf("cache size after second List = %d, want 1", cached)
	}

	// Rewrite the archive with different content and a different mtime:
	// the key changes, and the new content must be served.
	writeFile(t, zp, zipBytes(t, zEntry{name: "two.txt", data: "2"}))
	if err := os.Chtimes(zp, time.Now(), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	entries, err := r.List(ctxT(t), zp)
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, names(entries), []string{"two.txt"})
}

// --- defaults ----------------------------------------------------------------

func TestNewDefaults(t *testing.T) {
	r, _ := newTestResolver(t, Options{TempDir: t.TempDir()})
	if r.opts.MaxDepth != 3 {
		t.Errorf("MaxDepth = %d", r.opts.MaxDepth)
	}
	if r.opts.MaxEntryBytes != 256<<20 {
		t.Errorf("MaxEntryBytes = %d", r.opts.MaxEntryBytes)
	}
	if r.opts.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v", r.opts.Timeout)
	}
}

// Interface satisfaction of the public contract types.
var (
	_ RealFS        = (*stubFS)(nil)
	_ io.ReadCloser = (*compositeRC)(nil)
	_ io.ReadCloser = (*procReader)(nil)
)
