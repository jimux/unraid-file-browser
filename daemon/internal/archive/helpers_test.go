package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unraid-filebrowser/internal/types"
)

// stubFS is a temp-root-backed RealFS: Confine = clean + prefix check,
// Stat = lstat. Mirrors the fsops contract closely enough for the resolver.
type stubFS struct {
	root string
}

func (s *stubFS) Confine(p string) (string, error) {
	c := filepath.Clean(p)
	if c != s.root && !strings.HasPrefix(c, s.root+string(filepath.Separator)) {
		return "", types.Errf(types.ErrForbidden, "path escapes root: "+p)
	}
	return c, nil
}

func (s *stubFS) Stat(_ context.Context, p string) (types.Entry, error) {
	c, err := s.Confine(p)
	if err != nil {
		return types.Entry{}, err
	}
	fi, err := os.Lstat(c)
	if err != nil {
		return types.Entry{}, err
	}
	e := types.Entry{
		Name:  fi.Name(),
		Path:  p,
		Size:  fi.Size(),
		Mtime: fi.ModTime().Unix(),
	}
	switch {
	case fi.IsDir():
		e.Type = types.TypeDir
	case fi.Mode()&os.ModeSymlink != 0:
		e.Type = types.TypeSymlink
	default:
		e.Type = types.TypeFile
	}
	return e, nil
}

func (s *stubFS) Open(ctx context.Context, p string) (io.ReadCloser, types.Entry, error) {
	e, err := s.Stat(ctx, p)
	if err != nil {
		return nil, types.Entry{}, err
	}
	c, _ := s.Confine(p)
	f, err := os.Open(c)
	if err != nil {
		return nil, types.Entry{}, err
	}
	return f, e, nil
}

// newTestResolver builds a Resolver over a fresh temp root. TempDir defaults
// to a dedicated temp dir so tests can assert layer-file cleanup.
func newTestResolver(t *testing.T, opts Options) (*Resolver, string) {
	t.Helper()
	root := t.TempDir()
	if opts.TempDir == "" {
		opts.TempDir = t.TempDir()
	}
	return New(&stubFS{root: root}, opts), root
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- archive builders --------------------------------------------------------

type zEntry struct {
	name string
	data string
	dir  bool
}

var fixedTime = time.Unix(1_700_000_000, 0)

func zipBytes(t *testing.T, entries ...zEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		name := e.name
		if e.dir && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		fw, err := w.CreateHeader(&zip.FileHeader{
			Name:     name,
			Method:   zip.Deflate,
			Modified: fixedTime,
		})
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if !e.dir {
			if _, err := fw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type tEntry struct {
	name    string
	data    string
	dir     bool
	symlink string
}

func tarBytes(t *testing.T, entries ...tEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, ModTime: fixedTime}
		switch {
		case e.dir:
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		case e.symlink != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.symlink
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.data))
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := w.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// --- assertion helpers -------------------------------------------------------

// apiCode extracts the APIError code, failing the test if err is not one.
func apiCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var ae *types.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *types.APIError, got %T: %v", err, err)
	}
	return ae.Code
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := apiCode(t, err); got != code {
		t.Fatalf("error code = %s, want %s (err: %v)", got, code, err)
	}
}

// names collapses entries to their Name column for compact assertions.
func names(entries []types.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func eqStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// findEntry returns the entry with the given name, fatal if absent.
func findEntry(t *testing.T, entries []types.Entry, name string) types.Entry {
	t.Helper()
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("entry %q not found in %v", name, names(entries))
	return types.Entry{}
}

// readAllAndClose fully drains rc and closes it.
func readAllAndClose(t *testing.T, rc io.ReadCloser) ([]byte, error) {
	t.Helper()
	data, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return data, err
}

// tempLayerFiles lists leftover filebrowserd-layer-* files in dir.
func tempLayerFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "filebrowserd-layer-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// mustTable builds an uncapped table from records, failing the test on error.
func mustTable(t *testing.T, recs []entryRecord) *table {
	t.Helper()
	tbl, err := newTable(recs, 0)
	if err != nil {
		t.Fatalf("newTable: %v", err)
	}
	return tbl
}
