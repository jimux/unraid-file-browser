// Package archive implements read-only browsing inside archive files via
// virtual paths ("/x/a.tar.gz!/www/b.zip!/c.txt", separator "!/").
//
// Resolution is filesystem-first with longest literal match (see API.md):
// a path is only treated as virtual when the whole literal string does not
// exist on disk, in which case it is split on the LAST "!/", the left side is
// resolved recursively under the same rule, required to be an archive file,
// and the right side is looked up as an entry path inside it.
//
// Two driver tiers: an in-process Go fast path (zip, tar and compressed tar
// variants, bare gz/bz2/xz/zst) and a 7-Zip ("7zz") subprocess fallback for
// everything else (7z, iso, rar, cab, wim, dmg, deb, rpm, ...).
package archive

import (
	"context"
	"errors"
	"io"
	"mime"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"unraid-filebrowser/internal/types"
)

// sep is the virtual-path separator between an archive and an entry path.
const sep = "!/"

const (
	defaultMaxDepth      = 3
	defaultMaxEntryBytes = 256 << 20
	defaultTimeout       = 30 * time.Second
	defaultMaxEntries    = 250_000
	defaultMaxCacheBytes = 64 << 20
	defaultMaxConcurrent = 4
	defaultMaxTempBytes  = 2 << 30
	tableCacheSlots      = 8

	// tempLayerPrefix names nested-layer spill files in Options.TempDir.
	tempLayerPrefix = "filebrowserd-layer-"
)

// Package-level so tests can shrink them; effectively constants in production.
var (
	// memSpillBytes is the largest nested layer buffered in memory when a
	// driver needs random access; larger layers spill to a temp file.
	memSpillBytes int64 = 32 << 20
	// maxLayerBytes caps the compressed size of a nested archive layer that
	// must be materialized (API.md: 512 MB).
	maxLayerBytes int64 = 512 << 20
)

// RealFS is the slice of the real-filesystem layer (fsops) the resolver
// needs: confined stat and path confinement.
type RealFS interface {
	Stat(ctx context.Context, path string) (types.Entry, error) // lstat-style, confined
	Confine(path string) (string, error)                        // clean+symlink-safe+root check
	// Open must open the file through a race-safe walk (fsops opens every
	// component O_NOFOLLOW relative to a verified root fd). The archive layer
	// never calls os.Open on a user-derived path itself: Confine checks a
	// logical name, and a rename-swapped symlink between that check and a
	// path-based open would escape the root. The returned reader is expected
	// to also implement io.ReaderAt and io.Seeker for regular files.
	Open(ctx context.Context, path string) (io.ReadCloser, types.Entry, error)
}

// Options tunes a Resolver. Zero values select the documented defaults.
type Options struct {
	SevenZipPath  string        // "" → exec.LookPath("7zz") then "7z"
	TempDir       string        // for large nested-layer extraction
	MaxDepth      int           // default 3
	MaxEntryBytes int64         // default 256 << 20
	Timeout       time.Duration // default 30s per operation

	// MaxEntries caps the number of entries a single archive listing may
	// hold (including directories synthesized for implicit parents);
	// larger archives fail with ARCHIVE_ERROR instead of being indexed.
	// Default 250_000.
	MaxEntries int
	// MaxCacheBytes bounds the summed estimated size of cached entry
	// tables (in addition to the fixed slot count). Default 64 << 20.
	MaxCacheBytes int64
	// MaxConcurrent bounds how many List/Stat/Open operations may be
	// reading archive bytes (parsing listings, materializing nested
	// layers) at once; others block until a slot frees or their context
	// deadline expires (TIMEOUT). Default 4.
	MaxConcurrent int
	// MaxTempBytes bounds the aggregate size of live nested-layer temp
	// files in TempDir; spilling past it fails with TOO_LARGE. Default
	// 2 << 30.
	MaxTempBytes int64
}

// Resolver answers List/Stat/Open for virtual (archive) paths.
type Resolver struct {
	real  RealFS
	opts  Options
	seven string // resolved 7zz binary path; "" if unavailable
	cache *tableCache
	sem   chan struct{} // MaxConcurrent slots
	temp  tempLedger    // live temp-file byte accounting
}

// New builds a Resolver over the given real filesystem. Stale nested-layer
// temp files left in TempDir by an earlier, abruptly terminated process are
// swept (best effort) as a side effect.
func New(real RealFS, opts Options) *Resolver {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = defaultMaxDepth
	}
	if opts.MaxEntryBytes <= 0 {
		opts.MaxEntryBytes = defaultMaxEntryBytes
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.TempDir == "" {
		opts.TempDir = os.TempDir()
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = defaultMaxEntries
	}
	if opts.MaxCacheBytes <= 0 {
		opts.MaxCacheBytes = defaultMaxCacheBytes
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = defaultMaxConcurrent
	}
	if opts.MaxTempBytes <= 0 {
		opts.MaxTempBytes = defaultMaxTempBytes
	}
	SweepTemp(opts.TempDir) //nolint:errcheck — best effort at startup
	seven := opts.SevenZipPath
	if seven == "" {
		for _, cand := range []string{"7zz", "7z"} {
			if p, err := exec.LookPath(cand); err == nil {
				seven = p
				break
			}
		}
	}
	return &Resolver{
		real:  real,
		opts:  opts,
		seven: seven,
		cache: newTableCache(tableCacheSlots, opts.MaxCacheBytes),
		sem:   make(chan struct{}, opts.MaxConcurrent),
		temp:  tempLedger{max: opts.MaxTempBytes},
	}
}

// SweepTemp removes stale nested-layer temp files ("filebrowserd-layer-*")
// from dir, as left behind when a previous process died without cleanup
// (SIGKILL, OOM). It returns the number of files removed and the first
// removal error, if any. Only regular files are touched.
func SweepTemp(dir string) (removed int, err error) {
	ents, rerr := os.ReadDir(dir)
	if rerr != nil {
		return 0, rerr
	}
	for _, de := range ents {
		if !strings.HasPrefix(de.Name(), tempLayerPrefix) || !de.Type().IsRegular() {
			continue
		}
		if e := os.Remove(filepath.Join(dir, de.Name())); e != nil {
			if err == nil {
				err = e
			}
			continue
		}
		removed++
	}
	return removed, err
}

// tooDeep reports whether p carries more "!/" separators than MaxDepth
// permits. Checked before any recursion or syscall so a path with an
// enormous number of separators is rejected in O(len) without a stat per
// separator.
func (r *Resolver) tooDeep(p string) bool {
	return strings.Count(p, sep) > r.opts.MaxDepth
}

// IsVirtual reports whether path must be handled by the archive resolver:
// it contains the "!/" separator and does not exist literally on disk
// (filesystem-first rule — a real directory named "weird!" wins). Paths
// nested past MaxDepth are reported virtual without touching the disk;
// the resolver then rejects them.
func (r *Resolver) IsVirtual(p string) bool {
	if !strings.Contains(p, sep) {
		return false
	}
	if r.tooDeep(p) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.Timeout)
	defer cancel()
	if _, err := r.real.Confine(p); err == nil {
		if _, err := r.real.Stat(ctx, p); err == nil {
			return false
		}
	}
	return true
}

// node is a resolved (possibly virtual) path.
type node struct {
	lit   string      // the literal vpath addressing this node
	real  string      // confined real path of the chain root file
	chain []string    // entry path per archive layer; empty → real node.
	entry types.Entry // stat of the node itself
}

// resolve applies the filesystem-first, longest-literal-match rule.
func (r *Resolver) resolve(ctx context.Context, vpath string) (*node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if vpath == "" {
		return nil, types.Errf(types.ErrNotFound, "empty path")
	}
	if r.tooDeep(vpath) {
		return nil, types.Errf(types.ErrArchive, "nesting too deep")
	}
	real, cerr := r.real.Confine(vpath)
	var fsErr error
	if cerr == nil {
		e, serr := r.real.Stat(ctx, vpath)
		if serr == nil {
			e.Path = vpath
			if e.Name == "" {
				e.Name = filepath.Base(real)
			}
			if e.Type == types.TypeFile && types.IsArchiveName(e.Name) {
				e.Type = types.TypeArchive
			}
			return &node{lit: vpath, real: real, entry: e}, nil
		}
		fsErr = serr
	} else {
		fsErr = cerr
	}
	i := strings.LastIndex(vpath, sep)
	if i < 0 {
		return nil, fsErr
	}
	left, right := vpath[:i], vpath[i+len(sep):]
	parent, err := r.resolve(ctx, left)
	if err != nil {
		return nil, err
	}
	if parent.entry.Type == types.TypeDir {
		return nil, types.Errf(types.ErrArchive, left+" is a directory, not an archive")
	}
	if !types.IsArchiveName(parent.entry.Name) {
		return nil, types.Errf(types.ErrArchive, left+" is not a supported archive")
	}
	if len(parent.chain)+1 > r.opts.MaxDepth {
		return nil, types.Errf(types.ErrArchive, "archive nesting deeper than limit")
	}
	ep, ok := cleanRequestPath(right)
	if !ok {
		return nil, types.Errf(types.ErrArchive, "invalid archive entry path: "+right)
	}
	chain := append(append([]string(nil), parent.chain...), ep)
	if ep == "" {
		// Root directory of the archive itself ("a.zip!/").
		return &node{
			lit:   vpath,
			real:  parent.real,
			chain: chain,
			entry: types.Entry{
				Name:  parent.entry.Name,
				Path:  vpath,
				Type:  types.TypeDir,
				Size:  0,
				Mtime: parent.entry.Mtime,
			},
		}, nil
	}
	container := r.blobFor(parent.real, parent.chain)
	tbl, err := r.tableForBlob(ctx, container)
	if err != nil {
		return nil, err
	}
	rec, found := tbl.lookup(ep)
	if !found {
		return nil, types.Errf(types.ErrNotFound, "no entry "+ep+" in "+left)
	}
	return &node{lit: vpath, real: parent.real, chain: chain, entry: entryFromRecord(rec, vpath)}, nil
}

// List returns the children of a virtual directory: the root of an archive
// file (real or nested) or a directory inside one.
func (r *Resolver) List(ctx context.Context, vpath string) ([]types.Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	ctx, done := r.beginOp(ctx)
	defer done()
	n, err := r.resolve(ctx, vpath)
	if err != nil {
		return nil, mapErr(err)
	}
	var (
		container    blob
		containerLit string
		dir          string
	)
	switch {
	case len(n.chain) == 0:
		// Real file: must be an archive; list its root.
		if n.entry.Type == types.TypeDir {
			return nil, types.Errf(types.ErrArchive, vpath+" is a real directory, not an archive")
		}
		if !types.IsArchiveName(n.entry.Name) {
			return nil, types.Errf(types.ErrArchive, vpath+" is not a supported archive")
		}
		container = &realBlob{fs: r.real, path: n.real}
		containerLit = n.lit
	case n.entry.Type == types.TypeDir:
		container = r.blobFor(n.real, n.chain[:len(n.chain)-1])
		containerLit = vpath[:strings.LastIndex(vpath, sep)]
		dir = n.chain[len(n.chain)-1]
	case n.entry.Type == types.TypeArchive:
		// Nested archive file: drill one layer deeper.
		if len(n.chain)+1 > r.opts.MaxDepth {
			return nil, types.Errf(types.ErrArchive, "archive nesting deeper than limit")
		}
		container = r.blobFor(n.real, n.chain)
		containerLit = n.lit
	default:
		return nil, types.Errf(types.ErrArchive, vpath+" is not a directory")
	}
	tbl, err := r.tableForBlob(ctx, container)
	if err != nil {
		return nil, mapErr(err)
	}
	recs, ok := tbl.listDir(dir)
	if !ok {
		return nil, types.Errf(types.ErrNotFound, "no such directory in archive: "+dir)
	}
	entries := make([]types.Entry, 0, len(recs))
	for _, rec := range recs {
		entries = append(entries, entryFromRecord(rec, containerLit+sep+rec.path))
	}
	return entries, nil
}

// Stat resolves a virtual path to its Entry.
func (r *Resolver) Stat(ctx context.Context, vpath string) (types.Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	ctx, done := r.beginOp(ctx)
	defer done()
	n, err := r.resolve(ctx, vpath)
	if err != nil {
		return types.Entry{}, mapErr(err)
	}
	return n.entry, nil
}

// Open streams the content of a file entry addressed by a virtual path. The
// returned reader enforces MaxEntryBytes (ErrTooLarge past the cap) and the
// operation timeout; Close releases all underlying layers and temp files.
// The concurrency slot is held only while resolving and opening (which is
// where nested layers get materialized), not while the caller streams.
func (r *Resolver) Open(ctx context.Context, vpath string) (io.ReadCloser, types.Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	ctx, done := r.beginOp(ctx)
	defer done()
	n, err := r.resolve(ctx, vpath)
	if err != nil {
		cancel()
		return nil, types.Entry{}, mapErr(err)
	}
	if n.entry.Type == types.TypeDir {
		cancel()
		return nil, types.Entry{}, types.Errf(types.ErrArchive, vpath+" is a directory")
	}
	if len(n.chain) == 0 {
		// Plain real file (an archive downloaded whole, typically).
		f, _, err := r.real.Open(ctx, n.real)
		cancel()
		if err != nil {
			return nil, types.Entry{}, mapErr(err)
		}
		return f, n.entry, nil
	}
	container := r.blobFor(n.real, n.chain[:len(n.chain)-1])
	rc, err := r.openEntryRaw(ctx, container, n.chain[len(n.chain)-1])
	if err != nil {
		cancel()
		return nil, types.Entry{}, mapErr(err)
	}
	wrapped := &compositeRC{
		Reader: &cappedReader{
			r:    &ctxReader{ctx: ctx, r: rc},
			left: r.opts.MaxEntryBytes,
		},
		closers: []func() error{rc.Close, func() error { cancel(); return nil }},
	}
	return wrapped, n.entry, nil
}

// blobFor builds the blob addressing the archive FILE at real+chain (chain
// elements are entry paths within successive layers; none may be "").
func (r *Resolver) blobFor(real string, chain []string) blob {
	var b blob = &realBlob{fs: r.real, path: real}
	for _, ep := range chain {
		b = &nestedBlob{r: r, parent: b, epath: ep}
	}
	return b
}

// tableForBlob returns the (cached) parsed entry table of an archive blob.
func (r *Resolver) tableForBlob(ctx context.Context, b blob) (*table, error) {
	key, kerr := b.cacheKey(ctx)
	if kerr == nil {
		if t, ok := r.cache.get(key); ok {
			return t, nil
		}
	}
	recs, err := r.listBlob(ctx, b)
	if err != nil {
		return nil, err
	}
	t, err := newTable(recs, r.opts.MaxEntries)
	if err != nil {
		return nil, err
	}
	if kerr == nil {
		r.cache.put(key, t)
	}
	return t, nil
}

// --- concurrency gate ---------------------------------------------------------

// opGate is the per-operation record of whether this List/Stat/Open already
// holds a MaxConcurrent slot. It travels in the context so that nested
// layer opens within one operation never take a second slot (which could
// deadlock under load) and so cache-hit-only operations never take one.
type opGate struct {
	held bool
	done bool
}

type gateKey struct{}

// beginOp attaches a fresh gate to ctx; the returned func releases the slot
// (if one was taken) and must be called when the operation completes.
func (r *Resolver) beginOp(ctx context.Context) (context.Context, func()) {
	g := &opGate{}
	return context.WithValue(ctx, gateKey{}, g), func() {
		if g.held {
			<-r.sem
			g.held = false
		}
		g.done = true
	}
}

// acquire takes a concurrency slot for archive I/O, blocking until one is
// free or ctx expires (surfacing as TIMEOUT via mapErr). The returned
// release is a no-op when the slot is owned by the enclosing operation.
func (r *Resolver) acquire(ctx context.Context) (release func(), err error) {
	g, _ := ctx.Value(gateKey{}).(*opGate)
	if g != nil && !g.done {
		if g.held {
			return func() {}, nil
		}
		select {
		case r.sem <- struct{}{}:
			g.held = true
			return func() {}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case r.sem <- struct{}{}:
		return func() { <-r.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// --- temp-file budget ---------------------------------------------------------

// tempLedger tracks the aggregate bytes of live nested-layer temp files.
type tempLedger struct {
	max  int64
	used atomic.Int64
}

// reserve claims n more bytes; false when that would exceed the budget.
func (l *tempLedger) reserve(n int64) bool {
	if l.used.Add(n) > l.max {
		l.used.Add(-n)
		return false
	}
	return true
}

func (l *tempLedger) release(n int64) { l.used.Add(-n) }

// budgetWriter charges every byte written to a tempLedger and fails with
// TOO_LARGE once the aggregate budget is exhausted. n is the running total
// this writer has reserved (to be released on cleanup).
type budgetWriter struct {
	w io.Writer
	l *tempLedger
	n int64
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	if !b.l.reserve(int64(len(p))) {
		return 0, types.Errf(types.ErrTooLarge, "aggregate nested-layer temp space exhausted")
	}
	b.n += int64(len(p))
	return b.w.Write(p)
}

// entryFromRecord converts a table record into an API Entry with the given
// full virtual path.
func entryFromRecord(rec entryRecord, full string) types.Entry {
	name := baseOf(rec.path)
	if name == "" {
		name = rec.path
	}
	e := types.Entry{Name: name, Path: full, Size: rec.size, Mtime: rec.mtime}
	switch {
	case rec.isDir:
		e.Type = types.TypeDir
		if e.Size < 0 {
			e.Size = 0
		}
	case rec.symlink != "":
		e.Type = types.TypeSymlink
		e.Target = rec.symlink
	case types.IsArchiveName(name):
		e.Type = types.TypeArchive
		e.Mime = mimeByExt(name)
	default:
		e.Type = types.TypeFile
		e.Mime = mimeByExt(name)
	}
	return e
}

func mimeByExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return ""
	}
	m := mime.TypeByExtension(ext)
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// cleanRequestPath normalizes a caller-supplied entry path (right side of
// "!/"). "" (or all slashes) addresses the archive root. Absolute paths and
// ".." traversal are rejected.
func cleanRequestPath(s string) (string, bool) {
	if strings.ContainsRune(s, 0) {
		return "", false
	}
	s = strings.Trim(s, "/")
	if s == "" {
		return "", true
	}
	c := path.Clean(s)
	if c == "." {
		return "", true
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// mapErr converts internal failures to *types.APIError (passing existing
// ones through untouched, including ErrForbidden from Confine).
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var ae *types.APIError
	if errors.As(err, &ae) {
		return ae
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return types.Errf(types.ErrTimeout, "archive operation timed out")
	}
	if errors.Is(err, os.ErrNotExist) {
		return types.Errf(types.ErrNotFound, err.Error())
	}
	return types.Errf(types.ErrArchive, err.Error())
}

// ctxReader fails reads once the operation context is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, types.Errf(types.ErrTimeout, "archive read timed out")
		}
		return 0, err
	}
	return c.r.Read(p)
}

// cappedReader allows up to left bytes, then returns ErrTooLarge if the
// source still has data (reading a file of exactly the cap size succeeds).
type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var probe [1]byte
		n, err := c.r.Read(probe[:])
		if n > 0 {
			return 0, types.Errf(types.ErrTooLarge, "archive entry exceeds decompressed size limit")
		}
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// compositeRC is a reader with a stack of close functions (run once, in
// order; first error wins).
type compositeRC struct {
	io.Reader
	closers []func() error
	closed  bool
}

func (c *compositeRC) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	var first error
	for _, fn := range c.closers {
		if err := fn(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func baseOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}
