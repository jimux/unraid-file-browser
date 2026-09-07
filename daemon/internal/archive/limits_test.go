package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"unraid-filebrowser/internal/types"
)

// --- Finding 1: entry-count cap and byte-bounded table cache ------------------

// emptyZip builds a zip with n zero-length entries named by nameFn (Store
// method, so building a quarter-million entries takes well under a second).
func emptyZip(t *testing.T, n int, nameFn func(i int) string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for i := 0; i < n; i++ {
		if _, err := w.CreateHeader(&zip.FileHeader{Name: nameFn(i), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// emptyTar builds a tar with n zero-length regular entries.
func emptyTar(t *testing.T, n int, nameFn func(i int) string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for i := 0; i < n; i++ {
		if err := w.WriteHeader(&tar.Header{Name: nameFn(i), Typeflag: tar.TypeReg, Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func flatName(i int) string { return fmt.Sprintf("f%07d", i) }

func TestMaxEntriesZipDefault(t *testing.T) {
	r, root := newTestResolver(t, Options{})
	if r.opts.MaxEntries != defaultMaxEntries {
		t.Fatalf("default MaxEntries = %d", r.opts.MaxEntries)
	}
	over := filepath.Join(root, "over.zip")
	writeFile(t, over, emptyZip(t, defaultMaxEntries+1, flatName))
	_, err := r.List(ctxT(t), over)
	wantCode(t, err, types.ErrArchive)
	if !strings.Contains(err.Error(), "too many entries") {
		t.Fatalf("err = %v", err)
	}
	r.cache.mu.Lock()
	cached := len(r.cache.vals)
	r.cache.mu.Unlock()
	if cached != 0 {
		t.Fatalf("rejected archive was cached (%d tables)", cached)
	}

	under := filepath.Join(root, "under.zip")
	writeFile(t, under, emptyZip(t, defaultMaxEntries-1, flatName))
	entries, err := r.List(ctxT(t), under)
	if err != nil {
		t.Fatalf("List under cap: %v", err)
	}
	if len(entries) != defaultMaxEntries-1 {
		t.Fatalf("got %d entries", len(entries))
	}
}

func TestMaxEntriesTar(t *testing.T) {
	const cap = 100
	r, root := newTestResolver(t, Options{MaxEntries: cap})
	over := filepath.Join(root, "over.tar")
	writeFile(t, over, emptyTar(t, cap+1, flatName))
	_, err := r.List(ctxT(t), over)
	wantCode(t, err, types.ErrArchive)

	exact := filepath.Join(root, "exact.tar")
	writeFile(t, exact, emptyTar(t, cap, flatName))
	entries, err := r.List(ctxT(t), exact)
	if err != nil || len(entries) != cap {
		t.Fatalf("List at cap: %d entries, %v", len(entries), err)
	}
	// The compressed variant goes through the same lister.
	overGz := filepath.Join(root, "over.tar.gz")
	writeFile(t, overGz, gzBytes(t, emptyTar(t, cap+1, flatName)))
	_, err = r.List(ctxT(t), overGz)
	wantCode(t, err, types.ErrArchive)
}

func TestMaxEntriesCountsImplicitDirs(t *testing.T) {
	// 10 files, each under its own 3-deep implicit directory chain: 40
	// table nodes. A cap of 39 must refuse, a cap of 40 must accept.
	deep := func(i int) string { return fmt.Sprintf("a%d/b%d/c%d/f", i, i, i) }
	r, root := newTestResolver(t, Options{MaxEntries: 39})
	zp := filepath.Join(root, "deep.zip")
	writeFile(t, zp, emptyZip(t, 10, deep))
	_, err := r.List(ctxT(t), zp)
	wantCode(t, err, types.ErrArchive)

	r2, root2 := newTestResolver(t, Options{MaxEntries: 40})
	zp2 := filepath.Join(root2, "deep.zip")
	writeFile(t, zp2, emptyZip(t, 10, deep))
	entries, err := r2.List(ctxT(t), zp2)
	if err != nil || len(entries) != 10 {
		t.Fatalf("List at exact node cap: %d, %v", len(entries), err)
	}
}

func TestParseSLTMaxEntries(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&sb, "Path = f%d\nSize = 1\n\n", i)
	}
	if recs, err := parseSLT(strings.NewReader(sb.String()), 5); err != nil || len(recs) != 5 {
		t.Fatalf("at cap: %d, %v", len(recs), err)
	}
	_, err := parseSLT(strings.NewReader(sb.String()), 4)
	wantCode(t, err, types.ErrArchive)
}

func TestSevenZipMaxEntries(t *testing.T) {
	bin := sevenZip(t)
	src := t.TempDir()
	for i := 0; i < 6; i++ {
		writeFile(t, filepath.Join(src, "d", fmt.Sprintf("f%d.txt", i)), []byte("x"))
	}
	// 6 files + 1 explicit dir = 7 records.
	r, root := newTestResolver(t, Options{SevenZipPath: bin, MaxEntries: 6})
	ap := filepath.Join(root, "many.7z")
	run7zz(t, bin, src, "a", "-bd", ap, "d")
	_, err := r.List(ctxT(t), ap)
	wantCode(t, err, types.ErrArchive)

	r2, root2 := newTestResolver(t, Options{SevenZipPath: bin, MaxEntries: 7})
	ap2 := filepath.Join(root2, "many.7z")
	run7zz(t, bin, src, "a", "-bd", ap2, "d")
	entries, err := r2.List(ctxT(t), ap2+sep+"d")
	if err != nil || len(entries) != 6 {
		t.Fatalf("List at cap: %d, %v", len(entries), err)
	}
}

func TestTableEstimatedBytesAndCompactRaw(t *testing.T) {
	tbl := mustTable(t, []entryRecord{
		{raw: "a/b.txt", size: 1},
		{raw: "dir/", isDir: true},
		{raw: "x/./y.txt", size: 1},
	})
	// Clean names do not keep a second copy of the stored name.
	rec, _ := tbl.lookup("a/b.txt")
	if rec.raw != "" || rec.storedName() != "a/b.txt" {
		t.Fatalf("clean record raw = %q", rec.raw)
	}
	// Unclean names keep the original for extraction.
	rec, _ = tbl.lookup("x/y.txt")
	if rec.storedName() != "x/./y.txt" {
		t.Fatalf("stored name = %q", rec.storedName())
	}
	rec, _ = tbl.lookup("dir")
	if !rec.isDir || rec.storedName() != "dir/" {
		t.Fatalf("dir record = %+v", rec)
	}
	// Nodes: a, a/b.txt, dir, x, x/y.txt.
	if tbl.count() != 5 {
		t.Fatalf("count = %d", tbl.count())
	}
	if tbl.bytes < 5*tableNodeOverhead || tbl.bytes > 5*tableNodeOverhead+200 {
		t.Fatalf("estimated bytes = %d", tbl.bytes)
	}
}

func TestTableCacheByteBudget(t *testing.T) {
	big := mustTable(t, []entryRecord{{raw: "a", size: 1}, {raw: "b", size: 1}, {raw: "c", size: 1}})
	small := mustTable(t, []entryRecord{{raw: "a", size: 1}})
	c := newTableCache(8, big.bytes+small.bytes) // room for exactly one of each

	c.put("small1", small)
	c.put("big", big)
	if _, ok := c.get("small1"); !ok {
		t.Fatal("within budget, small1 evicted")
	}
	// Adding another small table overflows the budget: LRU (big, since
	// small1 was just touched) goes.
	c.put("small2", small)
	if _, ok := c.get("big"); ok {
		t.Fatal("big should have been evicted by byte budget")
	}
	for _, k := range []string{"small1", "small2"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("%s missing", k)
		}
	}
	c.mu.Lock()
	got := c.bytes
	c.mu.Unlock()
	if got != 2*small.bytes {
		t.Fatalf("accounted bytes = %d, want %d", got, 2*small.bytes)
	}
	// A single table over the whole budget is still retained (alone).
	c2 := newTableCache(8, 1)
	c2.put("small", small)
	c2.put("big", big)
	if _, ok := c2.get("small"); ok {
		t.Fatal("small should be evicted")
	}
	if _, ok := c2.get("big"); !ok {
		t.Fatal("newest table must survive even when oversized")
	}
}

func TestResolverCacheEvictsByBytes(t *testing.T) {
	// Budget fits one ~10-entry table but not two.
	r, root := newTestResolver(t, Options{MaxCacheBytes: 15 * tableNodeOverhead})
	for _, n := range []string{"a.zip", "b.zip"} {
		writeFile(t, filepath.Join(root, n), emptyZip(t, 10, flatName))
		if _, err := r.List(ctxT(t), filepath.Join(root, n)); err != nil {
			t.Fatal(err)
		}
	}
	r.cache.mu.Lock()
	n, b := len(r.cache.vals), r.cache.bytes
	r.cache.mu.Unlock()
	if n != 1 || b > 15*tableNodeOverhead {
		t.Fatalf("cache holds %d tables / %d bytes, want 1 within budget", n, b)
	}
}

// --- Finding 2: concurrency slots and aggregate temp budget --------------------

func TestMaxConcurrentBlocksUntilDeadline(t *testing.T) {
	r, root := newTestResolver(t, Options{MaxConcurrent: 1})
	zp := filepath.Join(root, "x.zip")
	writeFile(t, zp, zipBytes(t, zEntry{name: "one.txt", data: "1"}))

	// Occupy the only slot.
	r.sem <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := r.List(ctx, zp)
	wantCode(t, err, types.ErrTimeout)
	<-r.sem

	// Slot free again: the same request succeeds and populates the cache.
	if _, err := r.List(ctxT(t), zp); err != nil {
		t.Fatalf("after release: %v", err)
	}
	// Cache-hit-only operations never wait for a slot.
	r.sem <- struct{}{}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if _, err := r.Stat(ctx2, zp+sep+"one.txt"); err != nil {
		t.Fatalf("cached Stat blocked on slot: %v", err)
	}
	if _, err := r.List(ctx2, zp); err != nil {
		t.Fatalf("cached List blocked on slot: %v", err)
	}
	<-r.sem
	if len(r.sem) != 0 {
		t.Fatalf("slot leaked: %d held", len(r.sem))
	}
}

func TestSingleSlotNestedResolutionDoesNotDeadlock(t *testing.T) {
	r, root := newTestResolver(t, Options{MaxConcurrent: 1})
	inner := zipBytes(t, zEntry{name: "deep/leaf.txt", data: "leaf"})
	tp := filepath.Join(root, "outer.tar.gz")
	writeFile(t, tp, gzBytes(t, tarBytes(t, tEntry{name: "inner.zip", data: string(inner)})))

	vp := tp + sep + "inner.zip" + sep + "deep/leaf.txt"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rc, _, err := r.Open(ctx, vp)
	if err != nil {
		t.Fatalf("Open nested with one slot: %v", err)
	}
	data, err := readAllAndClose(t, rc)
	if err != nil || string(data) != "leaf" {
		t.Fatalf("read = %q, %v", data, err)
	}
	if _, err := r.List(ctx, tp+sep+"inner.zip"+sep+"deep"); err != nil {
		t.Fatalf("List nested with one slot: %v", err)
	}
	if len(r.sem) != 0 {
		t.Fatalf("slot leaked: %d held", len(r.sem))
	}
}

func TestMaxTempBytesBudget(t *testing.T) {
	old := memSpillBytes
	memSpillBytes = 1024
	t.Cleanup(func() { memSpillBytes = old })

	tmp := t.TempDir()
	payload := incompressible(8 << 10)
	inner := zipBytes(t, zEntry{name: "blob.bin", data: string(payload)}) // ~8KB layer

	// Budget smaller than one layer: spilling fails with TOO_LARGE, the
	// temp file is removed and the ledger returns to zero.
	r, root := newTestResolver(t, Options{TempDir: tmp, MaxTempBytes: 4096})
	tp := filepath.Join(root, "outer.tar")
	writeFile(t, tp, tarBytes(t, tEntry{name: "inner.zip", data: string(inner)}))
	_, err := r.List(ctxT(t), tp+sep+"inner.zip")
	wantCode(t, err, types.ErrTooLarge)
	if left := tempLayerFiles(t, tmp); len(left) != 0 {
		t.Fatalf("temp layer files left after budget rejection: %v", left)
	}
	if used := r.temp.used.Load(); used != 0 {
		t.Fatalf("ledger = %d after rejection", used)
	}

	// Budget that fits one layer: the request succeeds and releases its
	// bytes when done, so a second request also succeeds.
	r2, root2 := newTestResolver(t, Options{TempDir: tmp, MaxTempBytes: 16 << 10})
	tp2 := filepath.Join(root2, "outer.tar")
	writeFile(t, tp2, tarBytes(t, tEntry{name: "inner.zip", data: string(inner)}))
	for i := 0; i < 2; i++ {
		rc, _, err := r2.Open(ctxT(t), tp2+sep+"inner.zip"+sep+"blob.bin")
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if used := r2.temp.used.Load(); used <= 0 {
			t.Fatalf("ledger = %d while layer is live", used)
		}
		data, err := readAllAndClose(t, rc)
		if err != nil || !bytes.Equal(data, payload) {
			t.Fatalf("payload mismatch, %v", err)
		}
		if used := r2.temp.used.Load(); used != 0 {
			t.Fatalf("ledger = %d after close", used)
		}
	}
	if left := tempLayerFiles(t, tmp); len(left) != 0 {
		t.Fatalf("temp layer files left: %v", left)
	}
}

func TestMaxTempBytesSevenZipLayer(t *testing.T) {
	bin := sevenZip(t)
	tmp := t.TempDir()
	// A 7z nested in a tar is always materialized to a temp file (7zz
	// needs a path), regardless of size: a tiny budget must reject it.
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "hello.txt"), []byte("hello"))
	inner := filepath.Join(t.TempDir(), "inner.7z")
	run7zz(t, bin, src, "a", "-bd", inner, "hello.txt")
	innerBytes, err := os.ReadFile(inner)
	if err != nil {
		t.Fatal(err)
	}
	r, root := newTestResolver(t, Options{SevenZipPath: bin, TempDir: tmp, MaxTempBytes: 8})
	tp := filepath.Join(root, "outer.tar")
	writeFile(t, tp, tarBytes(t, tEntry{name: "inner.7z", data: string(innerBytes)}))
	_, err = r.List(ctxT(t), tp+sep+"inner.7z")
	wantCode(t, err, types.ErrTooLarge)
	if left := tempLayerFiles(t, tmp); len(left) != 0 {
		t.Fatalf("temp layer files left: %v", left)
	}
	if used := r.temp.used.Load(); used != 0 {
		t.Fatalf("ledger = %d", used)
	}
}

// --- Finding 3: stale temp-file sweep -------------------------------------------

func TestSweepTemp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, tempLayerPrefix+"111"), []byte("stale"))
	writeFile(t, filepath.Join(dir, tempLayerPrefix+"222"), []byte("stale"))
	writeFile(t, filepath.Join(dir, "unrelated.txt"), []byte("keep"))
	if err := os.Mkdir(filepath.Join(dir, tempLayerPrefix+"dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := SweepTemp(dir)
	if err != nil || removed != 2 {
		t.Fatalf("SweepTemp = %d, %v", removed, err)
	}
	if left := tempLayerFiles(t, dir); len(left) != 1 || !strings.HasSuffix(left[0], "dir") {
		t.Fatalf("leftovers = %v (only the directory should remain)", left)
	}
	if _, err := os.Stat(filepath.Join(dir, "unrelated.txt")); err != nil {
		t.Fatal("unrelated file was removed")
	}
	// Missing directory is an error, not a panic.
	if _, err := SweepTemp(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("expected error for missing dir")
	}
}

func TestNewSweepsTempDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, tempLayerPrefix+"stale"), []byte("stale"))
	newTestResolver(t, Options{TempDir: dir})
	if left := tempLayerFiles(t, dir); len(left) != 0 {
		t.Fatalf("New did not sweep: %v", left)
	}
}

// --- Finding 4: separator flood rejected before syscalls ------------------------

// countingFS wraps a RealFS and counts calls.
type countingFS struct {
	RealFS
	stats, confines, opens atomic.Int64
}

func (c *countingFS) Stat(ctx context.Context, p string) (types.Entry, error) {
	c.stats.Add(1)
	return c.RealFS.Stat(ctx, p)
}

func (c *countingFS) Confine(p string) (string, error) {
	c.confines.Add(1)
	return c.RealFS.Confine(p)
}

func (c *countingFS) Open(ctx context.Context, p string) (io.ReadCloser, types.Entry, error) {
	c.opens.Add(1)
	return c.RealFS.Open(ctx, p)
}

// --- real-file reads never bypass RealFS.Open ------------------------------------

func TestRealFilesOpenedThroughRealFS(t *testing.T) {
	root := t.TempDir()
	fs := &countingFS{RealFS: &stubFS{root: root}}
	r := New(fs, Options{TempDir: t.TempDir()})
	zp := filepath.Join(root, "x.zip")
	writeFile(t, zp, zipBytes(t, zEntry{name: "one.txt", data: "1"}))

	// Listing (random access) and streaming an entry both go via Open.
	if _, err := r.List(ctxT(t), zp); err != nil {
		t.Fatal(err)
	}
	if fs.opens.Load() == 0 {
		t.Fatal("zip listing did not use RealFS.Open")
	}
	before := fs.opens.Load()
	rc, _, err := r.Open(ctxT(t), zp+sep+"one.txt")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := readAllAndClose(t, rc); err != nil || string(data) != "1" {
		t.Fatalf("read = %q, %v", data, err)
	}
	if fs.opens.Load() == before {
		t.Fatal("zip entry open did not use RealFS.Open")
	}
	// Downloading the archive file itself goes via Open too.
	before = fs.opens.Load()
	rc, _, err = r.Open(ctxT(t), zp)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if fs.opens.Load() == before {
		t.Fatal("whole-file open did not use RealFS.Open")
	}

	// A RealFS whose reader is not seekable cannot serve zip (random
	// access) and is refused cleanly rather than falling back to a path.
	r2 := New(&pipeFS{RealFS: &stubFS{root: root}}, Options{TempDir: t.TempDir()})
	_, err = r2.List(ctxT(t), zp)
	wantCode(t, err, types.ErrArchive)
}

func TestSevenZipRealFileViaDescriptor(t *testing.T) {
	bin := sevenZip(t)
	root := t.TempDir()
	fs := &countingFS{RealFS: &stubFS{root: root}}
	r := New(fs, Options{SevenZipPath: bin, TempDir: t.TempDir()})
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "docs", "a.txt"), []byte("hello"))
	ap := filepath.Join(root, "t.7z")
	run7zz(t, bin, src, "a", "-bd", ap, "docs")

	entries, err := r.List(ctxT(t), ap+sep+"docs")
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, names(entries), []string{"a.txt"})
	rc, _, err := r.Open(ctxT(t), ap+sep+"docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := readAllAndClose(t, rc); err != nil || string(data) != "hello" {
		t.Fatalf("read = %q, %v", data, err)
	}
	// Both the listing and the extraction handed 7zz a descriptor obtained
	// from RealFS.Open (the path itself is never given to the subprocess).
	if fs.opens.Load() < 2 {
		t.Fatalf("RealFS.Open calls = %d, want >= 2", fs.opens.Load())
	}
	af, cleanup, err := (&realBlob{fs: fs, path: ap}).file(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if af.path != childFDPath || af.fd == nil {
		t.Fatalf("realBlob.file = %+v, want /dev/fd/3 with descriptor", af)
	}
	// A RealFS that does not return *os.File cannot feed 7zz.
	r2 := New(&pipeFS{RealFS: &stubFS{root: root}}, Options{SevenZipPath: bin, TempDir: t.TempDir()})
	_, err = r2.List(ctxT(t), ap)
	wantCode(t, err, types.ErrArchive)
}

// pipeFS wraps readers so they are neither *os.File nor io.ReaderAt.
type pipeFS struct{ RealFS }

func (p *pipeFS) Open(ctx context.Context, path string) (io.ReadCloser, types.Entry, error) {
	rc, e, err := p.RealFS.Open(ctx, path)
	if err != nil {
		return nil, e, err
	}
	return struct{ io.ReadCloser }{rc}, e, nil
}

func TestSeparatorFloodRejectedBeforeSyscalls(t *testing.T) {
	root := t.TempDir()
	fs := &countingFS{RealFS: &stubFS{root: root}}
	r := New(fs, Options{TempDir: t.TempDir()})
	zp := filepath.Join(root, "x.zip")
	writeFile(t, zp, zipBytes(t, zEntry{name: "one.txt", data: "1"}))

	flood := zp + strings.Repeat(sep+"a", 200_000)
	start := time.Now()
	_, err := r.List(ctxT(t), flood)
	wantCode(t, err, types.ErrArchive)
	if !strings.Contains(err.Error(), "nesting too deep") {
		t.Fatalf("err = %v", err)
	}
	_, err = r.Stat(ctxT(t), flood)
	wantCode(t, err, types.ErrArchive)
	_, _, err = r.Open(ctxT(t), flood)
	wantCode(t, err, types.ErrArchive)
	if !r.IsVirtual(flood) {
		t.Fatal("flood path should be reported virtual (and rejected)")
	}
	if n := fs.stats.Load() + fs.confines.Load(); n != 0 {
		t.Fatalf("%d filesystem calls made for a flood path, want 0", n)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("rejection took %v", el)
	}

	// Exactly MaxDepth separators is still allowed through to resolution.
	ok := zp + sep + "a.zip" + sep + "b.zip" + sep + "c"
	_, err = r.Stat(ctxT(t), ok)
	wantCode(t, err, types.ErrNotFound) // "a.zip" is not in x.zip
	if fs.stats.Load() == 0 {
		t.Fatal("in-range path never reached the filesystem")
	}
}

// --- 7zz: zero-byte extraction of a non-empty entry is an error ----------------

func TestSevenZipEmptyOutputForNonEmptyEntry(t *testing.T) {
	bin := sevenZip(t)
	r, root := newTestResolver(t, Options{SevenZipPath: bin})
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "hello.txt"), []byte("hello"))
	ap := filepath.Join(root, "one.7z")
	run7zz(t, bin, src, "a", "-bd", ap, "hello.txt")

	// Bypass the table gate: ask the driver for a name the archive lacks
	// but which the (hypothetical) listing said had 5 bytes.
	rc, err := r.sevenZipOpenBlob(ctxT(t), &realBlob{fs: r.real, path: ap}, entryRecord{path: "missing.txt", size: 5})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	data, err := readAllAndClose(t, rc)
	if len(data) != 0 {
		t.Fatalf("unexpected data %q", data)
	}
	wantCode(t, err, types.ErrArchive)

	// A genuinely empty entry still streams cleanly.
	writeFile(t, filepath.Join(src, "empty.txt"), nil)
	ap2 := filepath.Join(root, "two.7z")
	run7zz(t, bin, src, "a", "-bd", ap2, "empty.txt", "hello.txt")
	rc, _, err = r.Open(ctxT(t), ap2+sep+"empty.txt")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := readAllAndClose(t, rc); err != nil || len(data) != 0 {
		t.Fatalf("empty entry read = %q, %v", data, err)
	}
}

func TestNewLimitDefaults(t *testing.T) {
	r, _ := newTestResolver(t, Options{})
	if r.opts.MaxEntries != 250_000 || r.opts.MaxCacheBytes != 64<<20 ||
		r.opts.MaxConcurrent != 4 || r.opts.MaxTempBytes != 2<<30 {
		t.Fatalf("defaults: %+v", r.opts)
	}
	if cap(r.sem) != 4 || r.temp.max != 2<<30 || r.cache.cap != 8 || r.cache.maxBytes != 64<<20 {
		t.Fatal("defaults not wired into resolver state")
	}
}
