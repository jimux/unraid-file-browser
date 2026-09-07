// Package fsops is the only code in the daemon that touches the real
// filesystem. The daemon runs as root, so every path that arrives from HTTP
// passes through Confine first: cleaned, checked for traversal, symlink
// resolved, and required to land inside a configured root. Confine is a
// pre-check on a path string, though, and a path string can change meaning
// between the check and the open (rename a directory component into a symlink
// and the kernel happily follows it). So nothing here opens a confined path by
// name either: List, Stat and Open walk the path one element at a time with
// openat(O_NOFOLLOW) relative to the already-opened parent, and any symlink
// met on the way is read and re-confined before it is followed. The object
// that is finally opened is exactly the object that was checked.
package fsops

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"unraid-filebrowser/internal/types"
	"unraid-filebrowser/internal/view"
)

// sniffBytes is how much of a file's head is read for MIME detection.
const sniffBytes = 512

// maxSymlinkHops bounds symlink following during a walk. It matches the
// kernel's own limit so a link loop inside a root is reported the same way
// Confine reports it (ELOOP → BAD_REQUEST).
const maxSymlinkHops = 40

const (
	// dirFlags opens a path element that must be a directory. O_NOFOLLOW is
	// the whole point: a symlink swapped in by a concurrent rename is refused
	// by the kernel instead of silently followed.
	dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	// leafFlags opens the final element of a path for reading. O_NONBLOCK
	// keeps open(2) itself from blocking on a FIFO with no writer; it is
	// cleared again once the fd is known to be a regular file.
	leafFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
)

// FS serves a fixed set of browse roots.
type FS struct {
	roots []root
}

// root keeps both spellings of a configured root: the cleaned path as
// configured (what users see) and its symlink-resolved form (what boundary
// checks compare against and what gets opened).
type root struct {
	clean    string
	resolved string
}

// New builds an FS confined to roots. Roots are cleaned and symlink-resolved
// once, here, so a root that itself sits behind a symlink (/mnt/user on some
// setups, /var on macOS) still matches. An FS with no roots denies everything.
func New(roots []string) *FS {
	f := &FS{}
	for _, r := range roots {
		if r == "" {
			continue
		}
		c := filepath.Clean(r)
		res, err := filepath.EvalSymlinks(c)
		if err != nil {
			res = c // missing root (array not started yet): check literally
		}
		f.roots = append(f.roots, root{clean: c, resolved: res})
	}
	return f
}

// Roots returns the configured roots as cleaned paths.
func (f *FS) Roots() []string {
	out := make([]string, 0, len(f.roots))
	for _, r := range f.roots {
		out = append(out, r.clean)
	}
	return out
}

// Confine is the first security check. It cleans path, rejects traversal,
// resolves symlinks against the deepest existing ancestor and requires the
// result to lie inside a configured root. The returned path is the cleaned
// *logical* path (not the symlink-resolved one) so that user-visible paths
// stay in the namespace the user asked about and lstat semantics are
// preserved for symlink entries.
//
// Confine gives good errors early (a static escape is FORBIDDEN before any fd
// is opened) but it is not what makes the open safe: the filesystem can
// change between this check and the open. The walk in List/Stat/Open is the
// guard that holds; Confine's result must still only be opened through it.
func (f *FS) Confine(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", types.Errf(types.ErrBadRequest, "path is required")
	}
	if strings.ContainsRune(path, 0) {
		return "", types.Errf(types.ErrBadRequest, "path contains a NUL byte")
	}
	if !filepath.IsAbs(path) {
		return "", types.Errf(types.ErrBadRequest, "path must be absolute")
	}
	clean := filepath.Clean(path)
	if hasDotDot(clean) {
		return "", types.Errf(types.ErrForbidden, "path escapes the browse roots")
	}
	if len(f.roots) == 0 {
		return "", types.Errf(types.ErrForbidden, "no browse roots configured")
	}

	resolved, err := resolveExisting(clean)
	if err != nil {
		return "", toAPIError(err, clean)
	}
	if !f.inRoots(resolved) {
		return "", types.Errf(types.ErrForbidden, "path is outside the browse roots")
	}
	// A dangling symlink resolves to itself above; make sure it does not point
	// out of the roots either, in case its target appears later.
	if target, ok := danglingTarget(clean); ok && !f.inRoots(target) {
		return "", types.Errf(types.ErrForbidden, "symlink points outside the browse roots")
	}
	return clean, nil
}

func (f *FS) inRoots(p string) bool {
	for _, r := range f.roots {
		if within(r.resolved, p) || within(r.clean, p) {
			return true
		}
	}
	return false
}

// within reports whether p is root or lives beneath it, comparing whole path
// elements so /mnt/userX never matches the root /mnt/user.
func within(rootPath, p string) bool {
	if rootPath == "" {
		return false
	}
	if p == rootPath {
		return true
	}
	sep := string(filepath.Separator)
	if rootPath == sep {
		return strings.HasPrefix(p, sep)
	}
	return strings.HasPrefix(p, rootPath+sep)
}

// hasDotDot reports whether any element of an already-cleaned path is "..".
func hasDotDot(p string) bool {
	for _, e := range strings.Split(p, string(filepath.Separator)) {
		if e == ".." {
			return true
		}
	}
	return false
}

// resolveExisting resolves symlinks in p. When p (or a leading part of it)
// does not exist yet, the deepest existing ancestor is resolved and the
// remainder appended literally, so a non-existent path still gets a
// boundary check before it is reported as missing.
func resolveExisting(p string) (string, error) {
	cur := p
	rest := ""
	for {
		res, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return res, nil
			}
			return filepath.Join(res, rest), nil
		}
		if errors.Is(err, syscall.ELOOP) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// danglingTarget returns the target of p when p is a symlink that does not
// resolve (EvalSymlinks failed on it), so the boundary check can still see
// where it points.
func danglingTarget(p string) (string, bool) {
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	if _, err := filepath.EvalSymlinks(p); err == nil {
		return "", false // resolved fine; already checked
	}
	target, err := os.Readlink(p)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(p), target)
	}
	return filepath.Clean(target), true
}

// ------------------------------------------------------------ safe walk

// matchRoot picks the root a confined logical path is spelled under and
// splits off the path elements below it. Either spelling of a root (as
// configured, or symlink-resolved) matches; the longest match wins so nested
// roots resolve to the deepest one.
func (f *FS) matchRoot(p string) (root, []string, bool) {
	best := -1
	prefix := ""
	for i, r := range f.roots {
		for _, cand := range []string{r.clean, r.resolved} {
			if within(cand, p) && len(cand) > len(prefix) {
				best, prefix = i, cand
			}
		}
	}
	if best < 0 {
		return root{}, nil, false
	}
	rest := strings.Trim(strings.TrimPrefix(p, prefix), string(filepath.Separator))
	if rest == "" {
		return f.roots[best], nil, true
	}
	return f.roots[best], strings.Split(rest, string(filepath.Separator)), true
}

// locate is matchRoot with one fallback: a path spelled through a symlink
// above the root (/var/... for /private/var/... on macOS) is accepted by
// Confine's resolution but matches neither spelling of the root lexically,
// so it is re-spelled through its resolved form. That re-spelling is only a
// choice of starting point — the walk verifies every element regardless.
func (f *FS) locate(p string) (root, []string, bool) {
	if r, elems, ok := f.matchRoot(p); ok {
		return r, elems, true
	}
	if res, err := resolveExisting(p); err == nil {
		return f.matchRoot(res)
	}
	return root{}, nil, false
}

// openRoot opens the directory a root resolves to. The root path itself is
// opened by name and symlinks in it are followed: roots are operator
// configuration resolved once at New, not request input.
func openRoot(r root) (int, error) {
	return openat(unix.AT_FDCWD, r.resolved, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
}

// walk opens the confined logical path p one element at a time and returns
// the fd of the final element, opened with leaf flags (dirFlags for a
// directory, leafFlags for a file). Every element below the root is opened
// with O_NOFOLLOW relative to the fd of its already-opened parent, so a
// rename that swaps a symlink into the path after Confine looked at it
// cannot redirect the open: the kernel refuses the link, walk reads the link
// text itself, and starts over from the target — which must in turn lie
// inside a root. Symlinks therefore still work (a link inside a root to
// another place inside a root behaves as before) but only ever through this
// re-confinement, never through kernel path resolution.
//
// The returned fd is the caller's to close.
func (f *FS) walk(p string, leaf int) (int, error) {
	for hops := 0; ; hops++ {
		if hops > maxSymlinkHops {
			return -1, types.Errf(types.ErrBadRequest, "too many levels of symbolic links: "+p)
		}
		r, elems, ok := f.locate(p)
		if !ok {
			return -1, types.Errf(types.ErrForbidden, "symlink points outside the browse roots")
		}
		fd, err := openRoot(r)
		if err != nil {
			return -1, toAPIError(err, p)
		}
		// phys is the directory currently held open, spelled from the
		// resolved root, so relative link targets resolve against the
		// directory they actually live in.
		phys := r.resolved
		followed := false
		for i, elem := range elems {
			flags := dirFlags
			if i == len(elems)-1 {
				flags = leaf
			}
			nfd, link, isLink, err := step(fd, elem, flags, p)
			unix.Close(fd)
			if err != nil {
				return -1, err
			}
			if isLink {
				if !filepath.IsAbs(link) {
					link = filepath.Join(phys, link)
				}
				p = filepath.Join(append([]string{link}, elems[i+1:]...)...)
				followed = true
				break
			}
			fd = nfd
			phys = filepath.Join(phys, elem)
		}
		if !followed {
			return fd, nil
		}
	}
}

// step opens elem relative to dirfd. When elem is a symlink it is not
// followed: step returns its link text with isLink set and no fd. For a
// non-directory leaf the entry is inspected before it is opened so that
// devices are never opened at all (opening some character devices as root
// has side effects) — the fd is then verified again after the open.
//
// A failed open is re-examined with lstat: a symlink is reported to the
// caller to re-confine; an object that has visibly changed since the open
// was attempted (ELOOP with no link there now, ENOTDIR on what is now a
// directory) is a rename in flight, retried a few times and then refused.
func step(dirfd int, elem string, flags int, p string) (fd int, link string, isLink bool, err error) {
	if elem == "" || elem == "." || elem == ".." || strings.ContainsRune(elem, filepath.Separator) {
		return -1, "", false, types.Errf(types.ErrForbidden, "path escapes the browse roots")
	}
	var st unix.Stat_t
	for attempt := 0; attempt < 4; attempt++ {
		if flags&unix.O_DIRECTORY == 0 {
			if err := fstatat(dirfd, elem, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return -1, "", false, walkError(err, p)
			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFLNK:
				return readLinkAt(dirfd, elem)
			case unix.S_IFREG, unix.S_IFDIR:
			default:
				return -1, "", false, notRegular(p)
			}
		}
		fd, err = openat(dirfd, elem, flags)
		if err == nil {
			return fd, "", false, nil
		}
		if lerr := fstatat(dirfd, elem, &st, unix.AT_SYMLINK_NOFOLLOW); lerr != nil {
			return -1, "", false, walkError(lerr, p)
		}
		typ := st.Mode & unix.S_IFMT
		switch {
		case typ == unix.S_IFLNK:
			return readLinkAt(dirfd, elem)
		case errors.Is(err, unix.ELOOP),
			errors.Is(err, unix.ENOTDIR) && typ == unix.S_IFDIR:
			continue // changed between open and lstat: look again
		}
		return -1, "", false, walkError(err, p)
	}
	return -1, "", false, changedUnderfoot(err)
}

// readLinkAt returns the text of the symlink elem in dirfd in step's result
// shape. A link that stops being a link under our feet is a change of the
// path mid-open and is refused rather than retried.
func readLinkAt(dirfd int, elem string) (int, string, bool, error) {
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(dirfd, elem, buf)
		if err == unix.EINTR {
			size /= 2
			continue
		}
		if err != nil {
			return -1, "", false, changedUnderfoot(err)
		}
		if n < size {
			if n == 0 {
				return -1, "", false, changedUnderfoot(unix.EINVAL)
			}
			return -1, string(buf[:n]), true, nil
		}
		if size >= 1<<16 {
			return -1, "", false, types.Errf(types.ErrBadRequest, "symlink target is too long")
		}
	}
}

// regularFile takes ownership of an fd opened with leafFlags, checks that it
// really is a regular file and wraps it as an *os.File. O_NONBLOCK was set
// only so open(2) could not block on a FIFO; it is cleared here so the
// returned file is indistinguishable from one opened by os.Open (a plain,
// blocking, seekable file that http.ServeContent is happy with). Should the
// clear fail the file still reads fine — O_NONBLOCK has no effect on regular
// file I/O — it would merely take the runtime's non-blocking code path.
func regularFile(fd int, p string) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, st, toAPIError(err, p)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFDIR:
		unix.Close(fd)
		return nil, st, types.Errf(types.ErrBadRequest, "is a directory: "+p)
	default:
		unix.Close(fd)
		return nil, st, notRegular(p)
	}
	_ = unix.SetNonblock(fd, false)
	return os.NewFile(uintptr(fd), p), st, nil
}

func openat(dirfd int, name string, flags int) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flags, 0)
		if err == unix.EINTR {
			continue
		}
		return fd, err
	}
}

func fstatat(dirfd int, name string, st *unix.Stat_t, flags int) error {
	for {
		err := unix.Fstatat(dirfd, name, st, flags)
		if err == unix.EINTR {
			continue
		}
		return err
	}
}

func notRegular(p string) error {
	return types.Errf(types.ErrBadRequest, "not a regular file: "+p)
}

func changedUnderfoot(err error) error {
	return types.Errf(types.ErrForbidden, "path changed while it was being opened: "+err.Error())
}

// walkError maps an openat/fstatat failure from the walk. ELOOP here means a
// symlink appeared where step's own check saw none an instant earlier — the
// path is being changed underneath us, and that is refused like any escape.
func walkError(err error, p string) error {
	switch {
	case errors.Is(err, unix.ELOOP):
		return changedUnderfoot(err)
	case errors.Is(err, unix.ENXIO), errors.Is(err, unix.EOPNOTSUPP):
		return notRegular(p) // a socket
	default:
		return toAPIError(err, p)
	}
}

// ---------------------------------------------------------- operations

// List returns the complete contents of a directory. Pagination is the API
// layer's job; this call is one readdir with no file opens, so a 100k-entry
// share stays fast.
func (f *FS) List(ctx context.Context, path string) ([]types.Entry, error) {
	dir, err := f.Confine(path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fd, err := f.walk(dir, dirFlags)
	if err != nil {
		return nil, err
	}
	df := os.NewFile(uintptr(fd), dir)
	defer df.Close()

	// Names only: (*os.File).ReadDir would lstat entries by parent+name (a
	// fresh path lookup that could be redirected), so per-entry metadata is
	// fetched relative to the directory fd instead.
	names, err := df.Readdirnames(-1)
	if err != nil {
		return nil, toAPIError(err, dir)
	}
	entries := make([]types.Entry, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries = append(entries, entryAt(fd, dir, name))
	}
	return entries, nil
}

// Stat describes one path. Symlinks are described as symlinks (lstat), not as
// whatever they point at.
func (f *FS) Stat(ctx context.Context, path string) (types.Entry, error) {
	p, err := f.Confine(path)
	if err != nil {
		return types.Entry{}, err
	}
	if err := ctx.Err(); err != nil {
		return types.Entry{}, err
	}

	if _, elems, ok := f.locate(p); ok && len(elems) == 0 {
		// The root itself: nothing above it to hold open, so fstat the
		// directory.
		fd, err := f.walk(p, dirFlags)
		if err != nil {
			return types.Entry{}, err
		}
		defer unix.Close(fd)
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return types.Entry{}, toAPIError(err, p)
		}
		return entryFromStat(p, &st), nil
	}

	// Open the parent safely, then look at the leaf relative to it without
	// following it — the parent fd is what keeps the lstat honest.
	dirfd, err := f.walk(filepath.Dir(p), dirFlags)
	if err != nil {
		return types.Entry{}, err
	}
	defer unix.Close(dirfd)
	base := filepath.Base(p)
	var st unix.Stat_t
	if err := fstatat(dirfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return types.Entry{}, toAPIError(err, p)
	}
	e := entryFromStat(p, &st)
	if e.Type == types.TypeSymlink {
		if _, t, ok, err := readLinkAt(dirfd, base); err == nil && ok {
			e.Target = t
		}
	}
	if e.Type == types.TypeFile && e.Mime == "" && st.Mode&unix.S_IFMT == unix.S_IFREG {
		e.Mime = sniffAt(dirfd, base, p)
	}
	return e, nil
}

// Open returns a reader over a file's bytes. The returned ReadCloser is an
// *os.File, so the API layer can use http.ServeContent for Range support. The
// entry describes the opened file (symlinks are followed here — you asked for
// the content — but only via walk's re-confinement of each link target).
// Anything that is not a regular file is refused: directories, and FIFOs,
// sockets and devices, which a plain open could block on or disturb.
func (f *FS) Open(ctx context.Context, path string) (io.ReadCloser, types.Entry, error) {
	p, err := f.Confine(path)
	if err != nil {
		return nil, types.Entry{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, types.Entry{}, err
	}

	fd, err := f.walk(p, leafFlags)
	if err != nil {
		return nil, types.Entry{}, err
	}
	file, st, err := regularFile(fd, p)
	if err != nil {
		return nil, types.Entry{}, err
	}

	e := entryFromStat(p, &st)
	if e.Mime == "" {
		head := make([]byte, sniffBytes)
		n, _ := io.ReadFull(file, head)
		e.Mime = view.SniffMime(head[:n], "")
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			file.Close()
			return nil, types.Entry{}, toAPIError(err, p)
		}
	}
	return file, e, nil
}

// entryAt builds a listing Entry for name inside the directory held open as
// dirfd, without following symlinks and without opening the file: MIME comes
// from the extension only.
func entryAt(dirfd int, dir, name string) types.Entry {
	p := filepath.Join(dir, name)
	var st unix.Stat_t
	if err := fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		// Gone between readdir and lstat (or unreadable): report what the
		// name says and no metadata, as before.
		e := types.Entry{Name: name, Path: p, Size: -1, Type: types.TypeFile}
		if types.IsArchiveName(name) {
			e.Type = types.TypeArchive
		}
		e.Mime = view.MimeByName(name)
		return e
	}
	e := entryFromStat(p, &st)
	if e.Type == types.TypeSymlink {
		if _, t, ok, err := readLinkAt(dirfd, name); err == nil && ok {
			e.Target = t
		}
	}
	return e
}

// entryFromStat builds an Entry for path from a stat result. Non-regular,
// non-directory objects (FIFOs, sockets, devices) are reported as files —
// the API has no other word for them — and are refused at Open.
func entryFromStat(path string, st *unix.Stat_t) types.Entry {
	name := filepath.Base(path)
	e := types.Entry{
		Name:  name,
		Path:  path,
		Size:  st.Size,
		Mtime: int64(st.Mtim.Sec),
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		e.Type = types.TypeSymlink
	case unix.S_IFDIR:
		e.Type = types.TypeDir
	default:
		if types.IsArchiveName(name) {
			e.Type = types.TypeArchive
		} else {
			e.Type = types.TypeFile
		}
	}
	if e.Type != types.TypeDir {
		e.Mime = view.MimeByName(name)
	}
	return e
}

// sniffAt reads the head of the regular file name in dirfd for MIME
// detection. Only ever called from Stat, never from a listing. The open is
// non-blocking and re-verified, so a FIFO swapped in meanwhile yields "".
func sniffAt(dirfd int, name, p string) string {
	fd, err := openat(dirfd, name, leafFlags)
	if err != nil {
		return ""
	}
	file, _, err := regularFile(fd, p)
	if err != nil {
		return ""
	}
	defer file.Close()
	head := make([]byte, sniffBytes)
	n, _ := io.ReadFull(file, head)
	return view.SniffMime(head[:n], "")
}

// toAPIError maps filesystem errors onto the API's error vocabulary.
func toAPIError(err error, path string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return types.Errf(types.ErrNotFound, "no such file or directory: "+path)
	case errors.Is(err, fs.ErrPermission):
		return types.Errf(types.ErrForbidden, "permission denied: "+path)
	case errors.Is(err, syscall.ELOOP):
		return types.Errf(types.ErrBadRequest, "too many levels of symbolic links: "+path)
	case errors.Is(err, syscall.ENOTDIR):
		return types.Errf(types.ErrBadRequest, "not a directory: "+path)
	case errors.Is(err, syscall.ENAMETOOLONG):
		return types.Errf(types.ErrBadRequest, "path is too long")
	default:
		return err
	}
}
