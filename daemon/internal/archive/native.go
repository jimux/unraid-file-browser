package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"context"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"unraid-filebrowser/internal/types"
)

// format is the driver selector for one archive file, chosen by file name.
type format int

const (
	fmtUnknown format = iota
	fmtZip
	fmtTar
	fmtTarGz
	fmtTarBz2
	fmtTarXz
	fmtTarZst
	fmtGz
	fmtBz2
	fmtXz
	fmtZst
	fmt7z // 7zz subprocess fallback (7z, iso, rar, cab, wim, dmg, deb, rpm, ...)
)

func detectFormat(name string) format {
	n := strings.ToLower(name)
	suffix := func(exts ...string) bool {
		for _, e := range exts {
			if strings.HasSuffix(n, e) && len(n) > len(e) {
				return true
			}
		}
		return false
	}
	switch {
	case suffix(".tar.gz", ".tgz"):
		return fmtTarGz
	case suffix(".tar.bz2", ".tbz2"):
		return fmtTarBz2
	case suffix(".tar.xz", ".txz"):
		return fmtTarXz
	case suffix(".tar.zst"):
		return fmtTarZst
	case suffix(".tar"):
		return fmtTar
	case suffix(".zip", ".jar", ".war", ".cbz", ".epub"):
		return fmtZip
	case suffix(".gz"):
		return fmtGz
	case suffix(".bz2"):
		return fmtBz2
	case suffix(".xz"):
		return fmtXz
	case suffix(".zst"):
		return fmtZst
	case types.IsArchiveName(name):
		return fmt7z
	default:
		return fmtUnknown
	}
}

// blob abstracts "the bytes of one archive file", whether it sits on the
// real filesystem or is an entry nested inside other archives.
type blob interface {
	// name is the file name, used to pick the driver.
	name() string
	// cacheKey identifies the current content for the table LRU.
	cacheKey(ctx context.Context) (string, error)
	// stream opens a fresh sequential reader over the archive bytes.
	stream(ctx context.Context) (io.ReadCloser, error)
	// readerAt provides random access (memory or temp file for nested
	// layers). cleanup must be called when done.
	readerAt(ctx context.Context) (io.ReaderAt, int64, func() error, error)
	// file materializes the archive for a 7zz subprocess. cleanup must be
	// called when done.
	file(ctx context.Context) (archiveFile, func() error, error)
}

// archiveFile is how an archive is handed to 7zz: either a path on disk
// (our own temp layer file under Options.TempDir) or an already-open
// descriptor (real files, opened via RealFS.Open so the subprocess never
// re-resolves a user-derived path; it is inherited as ExtraFiles[0] and
// addressed as /dev/fd/3).
type archiveFile struct {
	path string
	fd   *os.File
}

// childFDPath is where ExtraFiles[0] appears inside the 7zz subprocess.
const childFDPath = "/dev/fd/3"

func noopCleanup() error { return nil }

// realBlob is an archive file on the real filesystem (already confined).
type realBlob struct {
	fs   RealFS
	path string
}

func (b *realBlob) name() string { return filepath.Base(b.path) }

func (b *realBlob) cacheKey(ctx context.Context) (string, error) {
	e, err := b.fs.Stat(ctx, b.path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\x00%d\x00%d", b.path, e.Mtime, e.Size), nil
}

func (b *realBlob) stream(ctx context.Context) (io.ReadCloser, error) {
	rc, _, err := b.fs.Open(ctx, b.path)
	return rc, err
}

func (b *realBlob) readerAt(ctx context.Context) (io.ReaderAt, int64, func() error, error) {
	rc, ent, err := b.fs.Open(ctx, b.path)
	if err != nil {
		return nil, 0, nil, err
	}
	ra, ok := rc.(io.ReaderAt)
	if !ok {
		rc.Close()
		return nil, 0, nil, types.Errf(types.ErrArchive, "archive file is not randomly accessible")
	}
	size := ent.Size
	if st, ok := rc.(interface{ Stat() (os.FileInfo, error) }); ok {
		if fi, err := st.Stat(); err == nil {
			size = fi.Size()
		}
	}
	if size < 0 {
		rc.Close()
		return nil, 0, nil, types.Errf(types.ErrArchive, "archive size unknown")
	}
	return ra, size, rc.Close, nil
}

func (b *realBlob) file(ctx context.Context) (archiveFile, func() error, error) {
	rc, _, err := b.fs.Open(ctx, b.path)
	if err != nil {
		return archiveFile{}, nil, err
	}
	f, ok := rc.(*os.File)
	if !ok {
		rc.Close()
		return archiveFile{}, nil, types.Errf(types.ErrArchive, b.name()+": archive file cannot be passed to 7zz")
	}
	return archiveFile{path: childFDPath, fd: f}, f.Close, nil
}

// nestedBlob is an archive that is itself an entry inside a parent archive.
type nestedBlob struct {
	r      *Resolver
	parent blob
	epath  string // cleaned entry path within parent
}

func (b *nestedBlob) name() string { return baseOf(b.epath) }

func (b *nestedBlob) cacheKey(ctx context.Context) (string, error) {
	k, err := b.parent.cacheKey(ctx)
	if err != nil {
		return "", err
	}
	return k + "\x00!" + b.epath, nil
}

func (b *nestedBlob) stream(ctx context.Context) (io.ReadCloser, error) {
	return b.r.openEntryRaw(ctx, b.parent, b.epath)
}

func (b *nestedBlob) readerAt(ctx context.Context) (io.ReaderAt, int64, func() error, error) {
	rc, err := b.stream(ctx)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rc.Close()
	head, err := io.ReadAll(io.LimitReader(rc, memSpillBytes+1))
	if err != nil {
		return nil, 0, nil, err
	}
	if int64(len(head)) <= memSpillBytes {
		return bytes.NewReader(head), int64(len(head)), noopCleanup, nil
	}
	// Too big for memory: spill to a temp file, capped at maxLayerBytes per
	// layer and MaxTempBytes in aggregate.
	tmp, err := os.CreateTemp(b.r.opts.TempDir, tempLayerPrefix+"*")
	if err != nil {
		return nil, 0, nil, err
	}
	bw := &budgetWriter{w: tmp, l: &b.r.temp}
	cleanup := func() error {
		tmp.Close()
		b.r.temp.release(bw.n)
		return os.Remove(tmp.Name())
	}
	if _, err := bw.Write(head); err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	n, err := io.Copy(bw, io.LimitReader(rc, maxLayerBytes-int64(len(head))+1))
	if err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	total := int64(len(head)) + n
	if total > maxLayerBytes {
		cleanup()
		return nil, 0, nil, types.Errf(types.ErrTooLarge, "nested archive layer exceeds 512 MB limit")
	}
	return tmp, total, cleanup, nil
}

func (b *nestedBlob) file(ctx context.Context) (archiveFile, func() error, error) {
	rc, err := b.stream(ctx)
	if err != nil {
		return archiveFile{}, nil, err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(b.r.opts.TempDir, tempLayerPrefix+"*")
	if err != nil {
		return archiveFile{}, nil, err
	}
	bw := &budgetWriter{w: tmp, l: &b.r.temp}
	cleanup := func() error {
		b.r.temp.release(bw.n)
		return os.Remove(tmp.Name())
	}
	n, err := io.Copy(bw, io.LimitReader(rc, maxLayerBytes+1))
	closeErr := tmp.Close()
	if err != nil {
		cleanup()
		return archiveFile{}, nil, err
	}
	if closeErr != nil {
		cleanup()
		return archiveFile{}, nil, closeErr
	}
	if n > maxLayerBytes {
		cleanup()
		return archiveFile{}, nil, types.Errf(types.ErrTooLarge, "nested archive layer exceeds 512 MB limit")
	}
	return archiveFile{path: tmp.Name()}, cleanup, nil
}

// listBlob parses the full entry list of an archive blob (at most
// MaxEntries records; more is ErrArchive) under a concurrency slot.
func (r *Resolver) listBlob(ctx context.Context, b blob) ([]entryRecord, error) {
	release, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	switch f := detectFormat(b.name()); f {
	case fmtZip:
		return listZip(ctx, b, r.opts.MaxEntries)
	case fmtTar, fmtTarGz, fmtTarBz2, fmtTarXz, fmtTarZst:
		return listTar(ctx, b, f, r.opts.MaxEntries)
	case fmtGz, fmtBz2, fmtXz, fmtZst:
		return []entryRecord{{raw: bareEntryName(b.name()), size: -1}}, nil
	case fmt7z:
		return r.sevenZipListBlob(ctx, b)
	default:
		return nil, types.Errf(types.ErrArchive, b.name()+" is not a supported archive format")
	}
}

// openEntryRaw opens one entry of an archive blob as an UNCAPPED stream
// (Resolver.Open applies MaxEntryBytes; layer materialization applies the
// 512 MB layer cap).
func (r *Resolver) openEntryRaw(ctx context.Context, b blob, epath string) (io.ReadCloser, error) {
	tbl, err := r.tableForBlob(ctx, b)
	if err != nil {
		return nil, err
	}
	rec, ok := tbl.lookup(epath)
	if !ok {
		return nil, types.Errf(types.ErrNotFound, "no entry "+epath+" in "+b.name())
	}
	if rec.isDir {
		return nil, types.Errf(types.ErrArchive, epath+" is a directory")
	}
	if rec.symlink != "" {
		return nil, types.Errf(types.ErrArchive, epath+" is a symlink inside the archive; cannot stream")
	}
	// Opening may materialize nested layers: gate it. The slot is released
	// once the driver has handed back its stream (the enclosing operation's
	// slot, if any, is released by the operation itself).
	release, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	switch f := detectFormat(b.name()); f {
	case fmtZip:
		return openZipEntry(ctx, b, rec)
	case fmtTar, fmtTarGz, fmtTarBz2, fmtTarXz, fmtTarZst:
		return openTarEntry(ctx, b, f, rec)
	case fmtGz, fmtBz2, fmtXz, fmtZst:
		return openBareEntry(ctx, b, f)
	case fmt7z:
		return r.sevenZipOpenBlob(ctx, b, rec)
	default:
		return nil, types.Errf(types.ErrArchive, b.name()+" is not a supported archive format")
	}
}

// --- zip -------------------------------------------------------------------

func listZip(ctx context.Context, b blob, maxEntries int) ([]entryRecord, error) {
	ra, size, cleanup, err := b.readerAt(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, types.Errf(types.ErrArchive, "invalid zip: "+err.Error())
	}
	// zip.NewReader has already parsed the central directory (a compact
	// struct slice); refuse before amplifying it into records and a table.
	if len(zr.File) > maxEntries {
		return nil, errTooManyEntries()
	}
	recs := make([]entryRecord, 0, len(zr.File))
	for _, f := range zr.File {
		isDir := strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir()
		var mtime int64
		if !f.Modified.IsZero() {
			mtime = f.Modified.Unix()
		}
		recs = append(recs, entryRecord{
			raw:   f.Name,
			size:  int64(f.UncompressedSize64),
			mtime: mtime,
			isDir: isDir,
		})
	}
	return recs, nil
}

func openZipEntry(ctx context.Context, b blob, rec entryRecord) (io.ReadCloser, error) {
	ra, size, cleanup, err := b.readerAt(ctx)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		cleanup()
		return nil, types.Errf(types.ErrArchive, "invalid zip: "+err.Error())
	}
	want := rec.storedName()
	for _, f := range zr.File {
		if f.Name != want || strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			cleanup()
			return nil, types.Errf(types.ErrArchive, "zip entry open: "+err.Error())
		}
		return &compositeRC{Reader: rc, closers: []func() error{rc.Close, cleanup}}, nil
	}
	cleanup()
	return nil, types.Errf(types.ErrNotFound, "no entry "+rec.path+" in "+b.name())
}

// --- tar (+ compressed variants) --------------------------------------------

// decompress wraps r according to the compression of format f. The returned
// close function releases decompressor resources (not the source reader).
func decompress(f format, r io.Reader) (io.Reader, func() error, error) {
	switch f {
	case fmtTar:
		return r, noopCleanup, nil
	case fmtTarGz, fmtGz:
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, types.Errf(types.ErrArchive, "invalid gzip: "+err.Error())
		}
		return gz, gz.Close, nil
	case fmtTarBz2, fmtBz2:
		return bzip2.NewReader(r), noopCleanup, nil
	case fmtTarXz, fmtXz:
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, types.Errf(types.ErrArchive, "invalid xz: "+err.Error())
		}
		return xr, noopCleanup, nil
	case fmtTarZst, fmtZst:
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, types.Errf(types.ErrArchive, "invalid zstd: "+err.Error())
		}
		return zr, func() error { zr.Close(); return nil }, nil
	default:
		return r, noopCleanup, nil
	}
}

func listTar(ctx context.Context, b blob, f format, maxEntries int) ([]entryRecord, error) {
	rc, err := b.stream(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	dr, dclose, err := decompress(f, rc)
	if err != nil {
		return nil, err
	}
	defer dclose()
	tr := tar.NewReader(dr)
	var recs []entryRecord
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(recs) >= maxEntries {
			// Stop scanning: one more header would exceed the cap.
			if _, err := tr.Next(); err != io.EOF {
				return nil, errTooManyEntries()
			}
			break
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, types.Errf(types.ErrArchive, "invalid tar: "+err.Error())
		}
		var mtime int64
		if !hdr.ModTime.IsZero() {
			mtime = hdr.ModTime.Unix()
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			recs = append(recs, entryRecord{raw: hdr.Name, mtime: mtime, isDir: true})
		case tar.TypeReg, tar.TypeLink:
			recs = append(recs, entryRecord{raw: hdr.Name, size: hdr.Size, mtime: mtime})
		case tar.TypeSymlink:
			recs = append(recs, entryRecord{raw: hdr.Name, mtime: mtime, symlink: hdr.Linkname})
		default:
			// char/block devices, fifos, etc. — not browsable content.
		}
	}
	return recs, nil
}

func openTarEntry(ctx context.Context, b blob, f format, rec entryRecord) (io.ReadCloser, error) {
	rc, err := b.stream(ctx)
	if err != nil {
		return nil, err
	}
	dr, dclose, err := decompress(f, rc)
	if err != nil {
		rc.Close()
		return nil, err
	}
	closers := []func() error{dclose, rc.Close}
	tr := tar.NewReader(dr)
	want := rec.storedName()
	for {
		if err := ctx.Err(); err != nil {
			closeAll(closers)
			return nil, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			closeAll(closers)
			return nil, types.Errf(types.ErrArchive, "invalid tar: "+err.Error())
		}
		if hdr.Name != want {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeLink {
			continue
		}
		// tar.Reader yields EOF at the end of the current entry.
		return &compositeRC{Reader: tr, closers: closers}, nil
	}
	closeAll(closers)
	return nil, types.Errf(types.ErrNotFound, "no entry "+rec.path+" in "+b.name())
}

func closeAll(fns []func() error) {
	for _, fn := range fns {
		fn() //nolint:errcheck — best-effort cleanup on error paths
	}
}

// --- bare single-file compression (.gz/.bz2/.xz/.zst) ------------------------

// bareEntryName is the synthetic entry name of a bare compressed file:
// the archive file name with its compression extension stripped.
func bareEntryName(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range []string{".gz", ".bz2", ".xz", ".zst"} {
		if strings.HasSuffix(lower, ext) && len(name) > len(ext) {
			s := name[:len(name)-len(ext)]
			if s != "" && s != "." && s != ".." {
				return path.Clean(s)
			}
		}
	}
	return "data"
}

func openBareEntry(ctx context.Context, b blob, f format) (io.ReadCloser, error) {
	rc, err := b.stream(ctx)
	if err != nil {
		return nil, err
	}
	dr, dclose, err := decompress(f, rc)
	if err != nil {
		rc.Close()
		return nil, err
	}
	return &compositeRC{Reader: dr, closers: []func() error{dclose, rc.Close}}, nil
}
