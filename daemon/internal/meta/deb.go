package meta

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// .deb limits.
const (
	debMaxMembers      = 64        // ar members scanned before giving up
	debMaxControlComp  = 8 << 20   // compressed control.tar member
	debMaxControlTar   = 16 << 20  // decompressed bytes read from it
	debMaxControlFile  = 256 << 10 // the control file itself
	debMaxDepends      = 64        // package.depends rows
	arHeaderLen        = 60
	arMagic            = "!<arch>\n"
	debControlBasename = "control"
)

// debExtractor reads the control file of a Debian package: the .deb is an ar
// archive holding debian-binary, control.tar.{gz,xz,zst,bz2,} and data.tar.*;
// only the control member is decompressed, and only as far as the control
// file, under fixed caps.
type debExtractor struct{}

func (*debExtractor) Kind() Kind           { return KindPackage }
func (*debExtractor) Extensions() []string { return []string{"deb", "udeb"} }

func (*debExtractor) Extract(ctx context.Context, path string, r io.ReaderAt, size int64) ([]Field, error) {
	var magic [len(arMagic)]byte
	if err := readAtFull(r, magic[:], 0); err != nil {
		return nil, fmt.Errorf("deb: %w", err)
	}
	if string(magic[:]) != arMagic {
		return nil, errors.New("deb: not an ar archive")
	}
	off := int64(len(arMagic))
	for i := 0; i < debMaxMembers && off+arHeaderLen <= size; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var hdr [arHeaderLen]byte
		if err := readAtFull(r, hdr[:], off); err != nil {
			return nil, fmt.Errorf("deb: member header: %w", err)
		}
		if string(hdr[58:60]) != "`\n" {
			return nil, errors.New("deb: bad ar member magic")
		}
		name := strings.TrimRight(string(hdr[0:16]), " ")
		name = strings.TrimSuffix(name, "/")
		msize, ok := parseDecimal(string(hdr[48:58]))
		if !ok || msize < 0 || msize > size-off-arHeaderLen {
			return nil, errors.New("deb: member size out of range")
		}
		data := off + arHeaderLen
		if strings.HasPrefix(name, "control.tar") {
			if msize > debMaxControlComp {
				return nil, fmt.Errorf("deb: control member is %d bytes (cap %d)", msize, debMaxControlComp)
			}
			ctl, err := readDebControl(name, io.NewSectionReader(r, data, msize))
			if err != nil {
				return nil, err
			}
			return debFields(ctl), nil
		}
		off = data + msize + msize&1 // members are padded to even offsets
	}
	return nil, errors.New("deb: no control.tar member")
}

// readDebControl decompresses the control member (by suffix) and streams
// the tar until the control file appears.
func readDebControl(member string, r io.Reader) ([]byte, error) {
	var dec io.Reader
	switch {
	case strings.HasSuffix(member, ".gz"):
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("deb: control gzip: %w", err)
		}
		gz.Multistream(false)
		dec = gz
	case strings.HasSuffix(member, ".xz"):
		xr, err := xz.NewReader(bufio.NewReader(r))
		if err != nil {
			return nil, fmt.Errorf("deb: control xz: %w", err)
		}
		dec = xr
	case strings.HasSuffix(member, ".zst"):
		zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			return nil, fmt.Errorf("deb: control zstd: %w", err)
		}
		defer zr.Close()
		dec = zr
	case strings.HasSuffix(member, ".bz2"):
		dec = bzip2.NewReader(r)
	case member == "control.tar":
		dec = r
	default:
		return nil, fmt.Errorf("deb: unsupported control compression %q", member)
	}
	tr := tar.NewReader(io.LimitReader(dec, debMaxControlTar))
	for i := 0; i < 256; i++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("deb: control tar: %w", err)
		}
		base := strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), "/")
		if base != debControlBasename || h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Size > debMaxControlFile {
			return nil, fmt.Errorf("deb: control file is %d bytes (cap %d)", h.Size, debMaxControlFile)
		}
		buf, err := io.ReadAll(io.LimitReader(tr, debMaxControlFile))
		if err != nil {
			return nil, fmt.Errorf("deb: control file: %w", err)
		}
		return buf, nil
	}
	return nil, errors.New("deb: control file not found")
}

// parseControl reads an RFC822-style paragraph: "Field: value" with
// continuation lines starting with whitespace. Only the first paragraph is
// read. Field names are matched case-insensitively (stored lowercased).
func parseControl(b []byte) map[string]string {
	out := map[string]string{}
	var cur string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), debMaxControlFile+1)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			if len(out) > 0 {
				break // paragraph end
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if cur != "" {
				out[cur] += "\n" + strings.TrimSpace(line)
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		cur = strings.ToLower(strings.TrimSpace(k))
		out[cur] = strings.TrimSpace(v)
	}
	return out
}

func debFields(control []byte) []Field {
	c := parseControl(control)
	out := []Field{
		text("package.format", "deb"),
		text("package.name", c["package"]),
		text("package.version", c["version"]),
		text("package.arch", c["architecture"]),
		text("package.section", c["section"]),
		text("package.maintainer", c["maintainer"]),
	}
	if d := c["description"]; d != "" {
		first, _, _ := strings.Cut(d, "\n")
		out = append(out, text("package.summary", first))
	}
	if kb, ok := parseDecimal(c["installed-size"]); ok && kb >= 0 && kb < 1<<40 {
		out = append(out, numInt("package.installedSize", kb*1024))
	}
	for _, dep := range splitDepends(c["depends"] + "," + c["pre-depends"]) {
		out = append(out, text("package.depends", dep))
	}
	return dedupe(out)
}

// splitDepends turns "libc6 (>= 2.34), libssl3 | libssl1.1, foo:any [amd64]"
// into bare package names, capped.
func splitDepends(s string) []string {
	var out []string
	for _, group := range strings.Split(s, ",") {
		for _, alt := range strings.Split(group, "|") {
			name := alt
			if i := strings.IndexAny(name, "([<"); i >= 0 {
				name = name[:i]
			}
			name = strings.TrimSpace(name)
			if i := strings.IndexByte(name, ':'); i > 0 {
				name = name[:i] // architecture qualifier
			}
			if name == "" {
				continue
			}
			out = append(out, name)
			if len(out) >= debMaxDepends {
				return out
			}
		}
	}
	return out
}

// parseDecimal parses a non-negative decimal with surrounding spaces.
func parseDecimal(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 18 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}
