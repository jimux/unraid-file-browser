package fsops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"unraid-filebrowser/internal/types"
)

// codeOf extracts the API error code from err, or "" when err is not an
// *types.APIError. Every expected failure in this package must carry one.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var apiErr *types.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v (%T) is not an *types.APIError", err, err)
	}
	return apiErr.Code
}

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func symlink(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// sandbox builds
//
//	<tmp>/root/            <- the single browse root
//	<tmp>/root/notes.txt
//	<tmp>/root/sub/deep.txt
//	<tmp>/root/backup.tar.gz, photos.zip, disk.iso  (archives by name)
//	<tmp>/root/link.txt        -> notes.txt          (inside)
//	<tmp>/root/escape          -> ../outside         (relative escape)
//	<tmp>/root/absescape       -> /etc               (absolute escape)
//	<tmp>/root/dangling        -> /no-such-dir-xyz/secret (dangling escape)
//	<tmp>/root/dangling-inside -> missing.txt        (dangling, in root)
//	<tmp>/outside/secret.txt   <- must never be reachable
//	<tmp>/rootX/secret.txt     <- root-boundary trap (prefix of "root")
func sandbox(t *testing.T) (fs *FS, root string, tmp string) {
	t.Helper()
	tmp = t.TempDir()
	root = filepath.Join(tmp, "root")

	write(t, filepath.Join(root, "notes.txt"), "hello notes\n")
	write(t, filepath.Join(root, "sub", "deep.txt"), "deep\n")
	write(t, filepath.Join(root, "backup.tar.gz"), "not really gzip")
	write(t, filepath.Join(root, "photos.zip"), "not really zip")
	write(t, filepath.Join(root, "disk.iso"), "not really iso")
	write(t, filepath.Join(root, "binary.dat"), "\x89PNG\r\n\x1a\n\x00\x00\x00\x0d")
	write(t, filepath.Join(tmp, "outside", "secret.txt"), "TOP SECRET\n")
	write(t, filepath.Join(tmp, "rootX", "secret.txt"), "SIBLING SECRET\n")

	symlink(t, filepath.Join(root, "notes.txt"), filepath.Join(root, "link.txt"))
	symlink(t, "../outside", filepath.Join(root, "escape"))
	symlink(t, "/etc", filepath.Join(root, "absescape"))
	symlink(t, "/no-such-dir-xyz/secret", filepath.Join(root, "dangling"))
	symlink(t, "missing.txt", filepath.Join(root, "dangling-inside"))

	return New([]string{root}), root, tmp
}

// ---------------------------------------------------------------- Confine

func TestConfineAcceptsPathsInsideRoot(t *testing.T) {
	fs, root, _ := sandbox(t)
	for _, p := range []string{
		root,
		root + "/",
		filepath.Join(root, "notes.txt"),
		filepath.Join(root, "sub"),
		filepath.Join(root, "sub", "deep.txt"),
		filepath.Join(root, "sub", ".", "deep.txt"),
		filepath.Join(root, "sub", "..", "notes.txt"),
		filepath.Join(root, "does-not-exist-yet.txt"), // missing but confined
	} {
		got, err := fs.Confine(p)
		if err != nil {
			t.Errorf("Confine(%q) = %v, want success", p, err)
			continue
		}
		if got != filepath.Clean(p) {
			t.Errorf("Confine(%q) = %q, want the cleaned path %q", p, got, filepath.Clean(p))
		}
	}
}

func TestConfineRejectsTraversal(t *testing.T) {
	fs, root, tmp := sandbox(t)
	cases := []struct {
		name string
		path string
		code string
	}{
		{"parent of root", filepath.Dir(root), types.ErrForbidden},
		{"dotdot out of root", root + "/../outside/secret.txt", types.ErrForbidden},
		{"dotdot chain", root + "/sub/../../outside/secret.txt", types.ErrForbidden},
		{"deep dotdot", root + "/sub/../../../../../../etc/passwd", types.ErrForbidden},
		{"uncleaned dotdot survives", root + "/./sub/./../../outside", types.ErrForbidden},
		{"absolute elsewhere", "/etc/passwd", types.ErrForbidden},
		{"outside sibling", filepath.Join(tmp, "outside", "secret.txt"), types.ErrForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := fs.Confine(c.path)
			if err == nil {
				t.Fatalf("Confine(%q) = %q, want %s", c.path, got, c.code)
			}
			if code := codeOf(t, err); code != c.code {
				t.Fatalf("Confine(%q) code = %s, want %s", c.path, code, c.code)
			}
		})
	}
}

// A root-name prefix must not be a path prefix: /mnt/userX is not /mnt/user.
func TestConfineRootBoundaryIsElementWise(t *testing.T) {
	_, root, tmp := sandbox(t)
	fs := New([]string{root})

	for _, p := range []string{
		filepath.Join(tmp, "rootX"),
		filepath.Join(tmp, "rootX", "secret.txt"),
		root + "X",
		root + "X/secret.txt",
	} {
		if got, err := fs.Confine(p); err == nil {
			t.Errorf("Confine(%q) = %q, want FORBIDDEN — %q is not under root %q", p, got, p, root)
		} else if code := codeOf(t, err); code != types.ErrForbidden {
			t.Errorf("Confine(%q) code = %s, want FORBIDDEN", p, code)
		}
	}
	// ...while the root itself and its children still pass.
	if _, err := fs.Confine(root); err != nil {
		t.Errorf("Confine(root) = %v", err)
	}
}

func TestConfineRejectsSymlinkEscapes(t *testing.T) {
	fs, root, _ := sandbox(t)
	cases := []struct{ name, path string }{
		{"relative symlink dir", filepath.Join(root, "escape")},
		{"through relative symlink", filepath.Join(root, "escape", "secret.txt")},
		{"absolute symlink", filepath.Join(root, "absescape")},
		{"through absolute symlink", filepath.Join(root, "absescape", "passwd")},
		{"dangling symlink out of root", filepath.Join(root, "dangling")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := fs.Confine(c.path)
			if err == nil {
				t.Fatalf("Confine(%q) = %q, want FORBIDDEN", c.path, got)
			}
			if code := codeOf(t, err); code != types.ErrForbidden {
				t.Fatalf("Confine(%q) code = %s, want FORBIDDEN", c.path, code)
			}
		})
	}
}

// Symlinks that stay inside the roots keep working, including dangling ones
// (the SPA still has to be able to show them as broken links).
func TestConfineAllowsSymlinksInsideRoot(t *testing.T) {
	fs, root, _ := sandbox(t)
	for _, p := range []string{
		filepath.Join(root, "link.txt"),
		filepath.Join(root, "dangling-inside"),
	} {
		if _, err := fs.Confine(p); err != nil {
			t.Errorf("Confine(%q) = %v, want success", p, err)
		}
	}
}

// An escaping symlink must not become reachable by dressing it up with "..".
func TestConfineSymlinkPlusDotDot(t *testing.T) {
	fs, root, _ := sandbox(t)
	// escape -> ../outside, so escape/../outside/secret.txt is a lexical
	// no-op that Clean turns into root/outside/... — it must not resolve to
	// the real outside dir either way.
	for _, p := range []string{
		filepath.Join(root, "escape", "..", "outside", "secret.txt"),
		filepath.Join(root, "escape", "."),
	} {
		if got, err := fs.Confine(p); err == nil {
			// Cleaning may have removed the symlink component entirely; if so
			// the result must still be inside the root.
			if !strings.HasPrefix(got, root+string(filepath.Separator)) && got != root {
				t.Errorf("Confine(%q) = %q, which is outside %q", p, got, root)
			}
		}
	}
}

func TestConfineBadInput(t *testing.T) {
	fs, root, _ := sandbox(t)
	cases := []struct{ name, path, code string }{
		{"empty", "", types.ErrBadRequest},
		{"blank", "   ", types.ErrBadRequest},
		{"relative", "root/notes.txt", types.ErrBadRequest},
		{"bare relative dotdot", "../../etc/passwd", types.ErrBadRequest},
		{"nul byte", root + "/notes.txt\x00.png", types.ErrBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := fs.Confine(c.path); codeOf(t, err) != c.code {
				t.Fatalf("Confine(%q) = %v, want %s", c.path, err, c.code)
			}
		})
	}
}

func TestConfineNoRootsDeniesEverything(t *testing.T) {
	fs := New(nil)
	if _, err := fs.Confine("/etc/passwd"); codeOf(t, err) != types.ErrForbidden {
		t.Fatalf("empty FS allowed /etc/passwd: %v", err)
	}
	if got := fs.Roots(); len(got) != 0 {
		t.Fatalf("Roots() = %v, want empty", got)
	}
	// Empty strings in the root list are dropped rather than becoming "." .
	if got := New([]string{"", "/tmp"}).Roots(); len(got) != 1 || got[0] != "/tmp" {
		t.Fatalf("Roots() = %v, want [/tmp]", got)
	}
}

func TestConfineMultipleRoots(t *testing.T) {
	tmp := t.TempDir()
	a := write(t, filepath.Join(tmp, "a", "one.txt"), "1")
	b := write(t, filepath.Join(tmp, "b", "two.txt"), "2")
	c := write(t, filepath.Join(tmp, "c", "three.txt"), "3")
	fs := New([]string{filepath.Join(tmp, "a"), filepath.Join(tmp, "b")})

	for _, p := range []string{a, b} {
		if _, err := fs.Confine(p); err != nil {
			t.Errorf("Confine(%q) = %v", p, err)
		}
	}
	if _, err := fs.Confine(c); codeOf(t, err) != types.ErrForbidden {
		t.Errorf("Confine(%q) reached an unconfigured root: %v", c, err)
	}
	if got := fs.Roots(); len(got) != 2 {
		t.Errorf("Roots() = %v, want two", got)
	}
}

// A root that is itself a symlink (the /mnt/user case, and /var on macOS) must
// still match paths spelled through it.
func TestConfineRootBehindSymlink(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	write(t, filepath.Join(real, "f.txt"), "x")
	link := filepath.Join(tmp, "linkroot")
	symlink(t, real, link)

	fs := New([]string{link})
	for _, p := range []string{
		filepath.Join(link, "f.txt"),
		filepath.Join(real, "f.txt"),
	} {
		if _, err := fs.Confine(p); err != nil {
			t.Errorf("Confine(%q) = %v, want success", p, err)
		}
	}
	if _, err := fs.Confine(filepath.Join(tmp, "elsewhere.txt")); codeOf(t, err) != types.ErrForbidden {
		t.Errorf("symlinked root leaked its parent")
	}
}

// ------------------------------------------------------------------- List

func TestListEntries(t *testing.T) {
	fs, root, _ := sandbox(t)
	entries, err := fs.List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]types.Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	want := map[string]types.EntryType{
		"notes.txt":       types.TypeFile,
		"sub":             types.TypeDir,
		"backup.tar.gz":   types.TypeArchive,
		"photos.zip":      types.TypeArchive,
		"disk.iso":        types.TypeArchive,
		"binary.dat":      types.TypeFile,
		"link.txt":        types.TypeSymlink,
		"escape":          types.TypeSymlink,
		"absescape":       types.TypeSymlink,
		"dangling":        types.TypeSymlink,
		"dangling-inside": types.TypeSymlink,
	}
	if len(entries) != len(want) {
		t.Fatalf("List returned %d entries, want %d: %v", len(entries), len(want), byName)
	}
	for name, typ := range want {
		e, ok := byName[name]
		if !ok {
			t.Errorf("entry %q missing from listing", name)
			continue
		}
		if e.Type != typ {
			t.Errorf("%q type = %q, want %q", name, e.Type, typ)
		}
		if e.Path != filepath.Join(root, name) {
			t.Errorf("%q path = %q", name, e.Path)
		}
	}

	if e := byName["notes.txt"]; e.Size != int64(len("hello notes\n")) || e.Mtime <= 0 {
		t.Errorf("notes.txt size/mtime = %d/%d", e.Size, e.Mtime)
	}
	if e := byName["notes.txt"]; !strings.HasPrefix(e.Mime, "text/plain") {
		t.Errorf("notes.txt mime = %q, want text/plain", e.Mime)
	}
	if e := byName["sub"]; e.Mime != "" {
		t.Errorf("directories must not carry a MIME type, got %q", e.Mime)
	}
	if e := byName["link.txt"]; e.Target != filepath.Join(root, "notes.txt") {
		t.Errorf("link.txt target = %q", e.Target)
	}
	if e := byName["escape"]; e.Target != "../outside" {
		t.Errorf("escape target = %q, want the raw link text", e.Target)
	}
	// MIME in a listing comes from the extension alone — the file is never
	// opened, so PNG bytes under an unknown ".dat" extension yield "" rather
	// than the image/png a sniff would have found.
	if e := byName["binary.dat"]; e.Mime != "" {
		t.Errorf("binary.dat mime = %q, want \"\" — listings must not sniff content", e.Mime)
	}
}

func TestListIsCompleteForBigDirectories(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "big")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const n = 5000
	for i := range n {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%05d.txt", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := New([]string{root})
	entries, err := fs.List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// List returns the FULL directory; pagination belongs to the api layer.
	if len(entries) != n {
		t.Fatalf("List returned %d entries, want all %d", len(entries), n)
	}
	seen := make(map[string]bool, n)
	for _, e := range entries {
		if seen[e.Name] {
			t.Fatalf("duplicate entry %q", e.Name)
		}
		seen[e.Name] = true
	}
}

func TestListErrors(t *testing.T) {
	fs, root, _ := sandbox(t)
	cases := []struct{ name, path, code string }{
		{"missing", filepath.Join(root, "nope"), types.ErrNotFound},
		{"a file", filepath.Join(root, "notes.txt"), types.ErrBadRequest},
		{"escaping symlink", filepath.Join(root, "escape"), types.ErrForbidden},
		{"outside", "/etc", types.ErrForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := fs.List(context.Background(), c.path); codeOf(t, err) != c.code {
				t.Fatalf("List(%q) = %v, want %s", c.path, err, c.code)
			}
		})
	}
}

func TestListHonoursContextCancellation(t *testing.T) {
	fs, root, _ := sandbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fs.List(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("List with cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := fs.Stat(ctx, filepath.Join(root, "notes.txt")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stat with cancelled ctx = %v", err)
	}
	if _, _, err := fs.Open(ctx, filepath.Join(root, "notes.txt")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open with cancelled ctx = %v", err)
	}
}

// ------------------------------------------------------------------- Stat

func TestStat(t *testing.T) {
	fs, root, _ := sandbox(t)

	e, err := fs.Stat(context.Background(), filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if e.Name != "notes.txt" || e.Type != types.TypeFile || e.Size != 12 {
		t.Errorf("Stat file = %+v", e)
	}

	d, err := fs.Stat(context.Background(), filepath.Join(root, "sub"))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if d.Type != types.TypeDir {
		t.Errorf("Stat dir type = %q", d.Type)
	}

	a, err := fs.Stat(context.Background(), filepath.Join(root, "photos.zip"))
	if err != nil {
		t.Fatalf("Stat archive: %v", err)
	}
	if a.Type != types.TypeArchive {
		t.Errorf("Stat archive type = %q, want archive", a.Type)
	}

	// lstat semantics: a symlink describes itself, not its target.
	l, err := fs.Stat(context.Background(), filepath.Join(root, "link.txt"))
	if err != nil {
		t.Fatalf("Stat symlink: %v", err)
	}
	if l.Type != types.TypeSymlink {
		t.Errorf("Stat symlink type = %q", l.Type)
	}
	if l.Target != filepath.Join(root, "notes.txt") {
		t.Errorf("Stat symlink target = %q", l.Target)
	}

	// A broken link inside the roots still stats.
	b, err := fs.Stat(context.Background(), filepath.Join(root, "dangling-inside"))
	if err != nil {
		t.Fatalf("Stat dangling: %v", err)
	}
	if b.Type != types.TypeSymlink || b.Target != "missing.txt" {
		t.Errorf("Stat dangling = %+v", b)
	}

	if _, err := fs.Stat(context.Background(), filepath.Join(root, "nope")); codeOf(t, err) != types.ErrNotFound {
		t.Errorf("Stat missing = %v, want NOT_FOUND", err)
	}
	if _, err := fs.Stat(context.Background(), filepath.Join(root, "escape", "secret.txt")); codeOf(t, err) != types.ErrForbidden {
		t.Errorf("Stat through escaping symlink = %v, want FORBIDDEN", err)
	}
}

// Stat may sniff (it is one file, not a listing), so an extension-less file
// gets a content-derived type.
func TestStatSniffsUnknownExtensions(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	write(t, filepath.Join(root, "mystery"), "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	fs := New([]string{root})

	e, err := fs.Stat(context.Background(), filepath.Join(root, "mystery"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if e.Mime != "image/png" {
		t.Errorf("Mime = %q, want image/png from the sniff", e.Mime)
	}
}

// ------------------------------------------------------------------- Open

func TestOpen(t *testing.T) {
	fs, root, _ := sandbox(t)

	rc, e, err := fs.Open(context.Background(), filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(b) != "hello notes\n" {
		t.Errorf("content = %q", b)
	}
	if e.Size != 12 || e.Type != types.TypeFile {
		t.Errorf("entry = %+v", e)
	}
	// The api layer relies on this for Range support via http.ServeContent.
	if _, ok := rc.(io.ReadSeeker); !ok {
		t.Errorf("Open must return a seekable reader for real files")
	}
}

// Open follows a symlink (you asked for the bytes) but only after Confine has
// established the target is inside the roots.
func TestOpenFollowsConfinedSymlink(t *testing.T) {
	fs, root, _ := sandbox(t)
	rc, e, err := fs.Open(context.Background(), filepath.Join(root, "link.txt"))
	if err != nil {
		t.Fatalf("Open symlink: %v", err)
	}
	defer rc.Close()
	if e.Type != types.TypeFile {
		t.Errorf("Open on a symlink describes the target: type = %q", e.Type)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "hello notes\n" {
		t.Errorf("content through symlink = %q", b)
	}
}

// The head read for MIME sniffing must be rewound: callers get byte 0.
func TestOpenRewindsAfterSniffing(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	body := strings.Repeat("A", 2000)
	write(t, filepath.Join(root, "mystery"), body)
	fs := New([]string{root})

	rc, e, err := fs.Open(context.Background(), filepath.Join(root, "mystery"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	if !strings.HasPrefix(e.Mime, "text/plain") {
		t.Errorf("Mime = %q, want a sniffed text/plain", e.Mime)
	}
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(b) != body {
		t.Errorf("read %d bytes after sniffing, want the whole %d", len(b), len(body))
	}
}

func TestOpenErrors(t *testing.T) {
	fs, root, _ := sandbox(t)
	cases := []struct{ name, path, code string }{
		{"directory", filepath.Join(root, "sub"), types.ErrBadRequest},
		{"missing", filepath.Join(root, "nope"), types.ErrNotFound},
		{"broken link", filepath.Join(root, "dangling-inside"), types.ErrNotFound},
		{"escaping symlink", filepath.Join(root, "escape", "secret.txt"), types.ErrForbidden},
		{"outside root", "/etc/hosts", types.ErrForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc, _, err := fs.Open(context.Background(), c.path)
			if err == nil {
				rc.Close()
				t.Fatalf("Open(%q) succeeded, want %s", c.path, c.code)
			}
			if code := codeOf(t, err); code != c.code {
				t.Fatalf("Open(%q) code = %s, want %s", c.path, code, c.code)
			}
		})
	}
}

// Nothing in this package may reach the filesystem without Confine: exercising
// every entry point against a path outside the roots must fail identically.
func TestEveryEntryPointIsConfined(t *testing.T) {
	fs, _, tmp := sandbox(t)
	target := filepath.Join(tmp, "outside", "secret.txt")

	if _, err := fs.List(context.Background(), filepath.Dir(target)); codeOf(t, err) != types.ErrForbidden {
		t.Errorf("List escaped confinement: %v", err)
	}
	if _, err := fs.Stat(context.Background(), target); codeOf(t, err) != types.ErrForbidden {
		t.Errorf("Stat escaped confinement: %v", err)
	}
	if rc, _, err := fs.Open(context.Background(), target); err == nil {
		rc.Close()
		t.Errorf("Open escaped confinement")
	} else if codeOf(t, err) != types.ErrForbidden {
		t.Errorf("Open escaped confinement: %v", err)
	}
}

// ---------------------------------------------------------------- races

// The reviewer's harness: one goroutine keeps renaming a path component
// between a real directory (holding the innocent target) and a symlink to an
// outside-root directory (holding the secret), while readers hammer Open on
// the logical path. Confine-then-open-by-name loses this race within a
// second; the fd-relative walk must never hand out the outside bytes.
func TestOpenRaceNeverEscapesRoot(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	outside := filepath.Join(tmp, "outside")
	const inside, secret = "inside\n", "TOP SECRET\n"
	write(t, filepath.Join(root, "d.dir", "t"), inside)
	write(t, filepath.Join(outside, "t"), secret)
	symlink(t, outside, filepath.Join(root, "d.lnk"))
	fs := New([]string{root})

	cur := filepath.Join(root, "d")
	dir := filepath.Join(root, "d.dir")
	lnk := filepath.Join(root, "d.lnk")
	target := filepath.Join(cur, "t")

	duration := 2 * time.Second
	if testing.Short() {
		duration = 300 * time.Millisecond
	}
	stop := make(chan struct{})
	var flipper sync.WaitGroup
	flipper.Add(1)
	go func() {
		defer flipper.Done()
		isDir := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			if isDir {
				os.Rename(cur, dir)
				os.Rename(lnk, cur)
			} else {
				os.Rename(cur, lnk)
				os.Rename(dir, cur)
			}
			isDir = !isDir
		}
	}()

	var (
		mu       sync.Mutex
		attempts int
		opened   int
		codes    = map[string]int{}
		failure  string
	)
	const readers = 4
	var wg sync.WaitGroup
	deadline := time.Now().Add(duration)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for time.Now().Before(deadline) {
				rc, _, err := fs.Open(ctx, target)
				mu.Lock()
				attempts++
				mu.Unlock()
				if err != nil {
					var apiErr *types.APIError
					if !errors.As(err, &apiErr) {
						mu.Lock()
						failure = fmt.Sprintf("Open returned a non-API error %T: %v", err, err)
						mu.Unlock()
						return
					}
					mu.Lock()
					codes[apiErr.Code]++
					if apiErr.Code != types.ErrForbidden && apiErr.Code != types.ErrNotFound {
						failure = "unexpected error code from Open during race: " + err.Error()
					}
					mu.Unlock()
					continue
				}
				b, rerr := io.ReadAll(rc)
				rc.Close()
				mu.Lock()
				opened++
				if rerr != nil {
					failure = "read after Open: " + rerr.Error()
				} else if string(b) != inside {
					failure = fmt.Sprintf("ESCAPE: Open(%q) returned %q from outside the root", target, b)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(stop)
	flipper.Wait()

	if failure != "" {
		t.Fatal(failure)
	}
	t.Logf("%d attempts, %d successful in-root opens, errors by code: %v", attempts, opened, codes)
	if opened == 0 {
		t.Errorf("the harness never once opened the in-root file; the flipper is starving readers")
	}
	if codes[types.ErrForbidden] == 0 {
		t.Errorf("the harness never once saw the symlink; the race was not exercised")
	}
}

// ---------------------------------------------------- special files

// A FIFO with no writer makes a plain open(2) block forever. Open must come
// back promptly with an error; Stat and List must keep working and must not
// open it to sniff.
func TestFIFO(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	write(t, filepath.Join(root, "notes.txt"), "x")
	fifo := filepath.Join(root, "pipe")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	fs := New([]string{root})

	type result struct {
		rc  io.ReadCloser
		err error
	}
	done := make(chan result, 1)
	go func() {
		rc, _, err := fs.Open(context.Background(), fifo)
		done <- result{rc, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			r.rc.Close()
			t.Fatalf("Open(fifo) succeeded, want BAD_REQUEST")
		}
		if code := codeOf(t, r.err); code != types.ErrBadRequest {
			t.Fatalf("Open(fifo) code = %s (%v), want BAD_REQUEST", code, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Open(fifo) blocked: the open must be non-blocking")
	}

	done2 := make(chan error, 1)
	go func() {
		e, err := fs.Stat(context.Background(), fifo)
		if err == nil && e.Type != types.TypeFile {
			err = fmt.Errorf("Stat(fifo) type = %q, want file", e.Type)
		}
		done2 <- err
	}()
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("Stat(fifo): %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stat(fifo) blocked: sniffing must not open a FIFO")
	}

	entries, err := fs.List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "pipe" {
			found = true
		}
	}
	if !found {
		t.Errorf("List omitted the FIFO: %+v", entries)
	}
}

func TestOpenRefusesSocket(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "r")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot create unix socket: %v", err)
	}
	defer ln.Close()
	fs := New([]string{root})

	rc, _, err := fs.Open(context.Background(), sock)
	if err == nil {
		rc.Close()
		t.Fatal("Open(socket) succeeded, want BAD_REQUEST")
	}
	if code := codeOf(t, err); code != types.ErrBadRequest {
		t.Fatalf("Open(socket) code = %s (%v), want BAD_REQUEST", code, err)
	}
	if e, err := fs.Stat(context.Background(), sock); err != nil || e.Type != types.TypeFile {
		t.Errorf("Stat(socket) = %+v, %v", e, err)
	}
}

// The walk must keep symlinks that stay inside the roots usable, including
// as intermediate components and across roots, and still refuse ones that
// leave — for List, Stat and Open alike.
func TestWalkFollowsInRootSymlinksOnly(t *testing.T) {
	fs, root, tmp := sandbox(t)
	symlink(t, "sub", filepath.Join(root, "linkdir"))                      // relative, to a dir
	symlink(t, filepath.Join(root, "sub"), filepath.Join(root, "abslink")) // absolute, to a dir
	symlink(t, "../notes.txt", filepath.Join(root, "sub", "up"))           // relative with ..
	symlink(t, "loop-b", filepath.Join(root, "loop-a"))
	symlink(t, "loop-a", filepath.Join(root, "loop-b"))

	entries, err := fs.List(context.Background(), filepath.Join(root, "linkdir"))
	if err != nil {
		t.Fatalf("List through relative symlink dir: %v", err)
	}
	if len(entries) != 2 || entries[0].Path != filepath.Join(root, "linkdir", entries[0].Name) {
		t.Errorf("List through symlink dir = %+v", entries)
	}
	if _, err := fs.List(context.Background(), filepath.Join(root, "abslink")); err != nil {
		t.Errorf("List through absolute symlink dir: %v", err)
	}
	for _, p := range []string{
		filepath.Join(root, "linkdir", "deep.txt"),
		filepath.Join(root, "abslink", "deep.txt"),
		filepath.Join(root, "sub", "up"),
		filepath.Join(root, "linkdir", "up"),
	} {
		rc, e, err := fs.Open(context.Background(), p)
		if err != nil {
			t.Errorf("Open(%q) = %v", p, err)
			continue
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if len(b) == 0 || e.Type != types.TypeFile || e.Path != p {
			t.Errorf("Open(%q) = %q, %+v", p, b, e)
		}
	}
	e, err := fs.Stat(context.Background(), filepath.Join(root, "linkdir", "deep.txt"))
	if err != nil || e.Type != types.TypeFile || e.Size != 5 {
		t.Errorf("Stat through symlink dir = %+v, %v", e, err)
	}
	e, err = fs.Stat(context.Background(), filepath.Join(root, "linkdir"))
	if err != nil || e.Type != types.TypeSymlink || e.Target != "sub" {
		t.Errorf("Stat of the symlink itself = %+v, %v", e, err)
	}

	// Escapes via an intermediate symlink stay FORBIDDEN on every entry point.
	for _, p := range []string{
		filepath.Join(root, "escape", "secret.txt"),
		filepath.Join(root, "absescape", "passwd"),
	} {
		if _, err := fs.List(context.Background(), filepath.Dir(p)); codeOf(t, err) != types.ErrForbidden {
			t.Errorf("List(%q) = %v", filepath.Dir(p), err)
		}
		if _, err := fs.Stat(context.Background(), p); codeOf(t, err) != types.ErrForbidden {
			t.Errorf("Stat(%q) = %v", p, err)
		}
		if rc, _, err := fs.Open(context.Background(), p); err == nil {
			rc.Close()
			t.Errorf("Open(%q) succeeded", p)
		} else if codeOf(t, err) != types.ErrForbidden {
			t.Errorf("Open(%q) = %v", p, err)
		}
	}
	// A link loop is a bad path, not an escape.
	if rc, _, err := fs.Open(context.Background(), filepath.Join(root, "loop-a")); err == nil {
		rc.Close()
		t.Error("Open(loop) succeeded")
	} else if codeOf(t, err) != types.ErrBadRequest {
		t.Errorf("Open(loop) = %v, want BAD_REQUEST", err)
	}

	// Cross-root links work when both ends are roots.
	other := filepath.Join(tmp, "other")
	write(t, filepath.Join(other, "o.txt"), "other")
	symlink(t, other, filepath.Join(root, "to-other"))
	two := New([]string{root, other})
	if rc, _, err := two.Open(context.Background(), filepath.Join(root, "to-other", "o.txt")); err != nil {
		t.Errorf("cross-root link with both roots configured: %v", err)
	} else {
		rc.Close()
	}
	if _, _, err := fs.Open(context.Background(), filepath.Join(root, "to-other", "o.txt")); codeOf(t, err) != types.ErrForbidden {
		t.Errorf("cross-root link with one root configured = %v, want FORBIDDEN", err)
	}
}

// The root itself, and a root spelled through a symlink, stat and list.
func TestStatAndListRoot(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	write(t, filepath.Join(real, "f.txt"), "x")
	link := filepath.Join(tmp, "linkroot")
	symlink(t, real, link)
	fs := New([]string{link})

	for _, p := range []string{link, real} {
		e, err := fs.Stat(context.Background(), p)
		if err != nil || e.Type != types.TypeDir || e.Path != p {
			t.Errorf("Stat(root %q) = %+v, %v", p, e, err)
		}
		entries, err := fs.List(context.Background(), p)
		if err != nil || len(entries) != 1 || entries[0].Path != filepath.Join(p, "f.txt") {
			t.Errorf("List(root %q) = %+v, %v", p, entries, err)
		}
		if rc, _, err := fs.Open(context.Background(), filepath.Join(p, "f.txt")); err != nil {
			t.Errorf("Open under root %q: %v", p, err)
		} else {
			rc.Close()
		}
	}
	if _, _, err := fs.Open(context.Background(), link); codeOf(t, err) != types.ErrBadRequest {
		t.Errorf("Open(root dir) = %v, want BAD_REQUEST", err)
	}
}
