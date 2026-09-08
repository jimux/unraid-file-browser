package meta

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// RPM v3/v4 file layout: a 96-byte lead, a signature header, then the main
// header; the payload (cpio) follows and is never read. A header is
//
//	magic 8E AD E8 | version 01 | reserved[4] | nindex u32 | hsize u32
//	nindex × { tag u32, type u32, offset u32, count u32 }
//	store[hsize]
//
// with the signature header padded to 8 bytes. Only the index entries and
// the individual values this extractor wants are read — the store of a
// large package (file lists, changelogs) can run to many MB and is skipped
// over by offset arithmetic.
const (
	rpmLeadLen        = 96
	rpmHeaderIntroLen = 16
	rpmIndexEntryLen  = 16
	rpmMaxIndex       = 8192     // index entries per header
	rpmMaxStore       = 64 << 20 // header store size accepted (never read whole)
	rpmMaxString      = 4096     // bytes read for one string value
	rpmMaxArrayBytes  = 256 << 10
	rpmMaxRequires    = 64
)

var (
	rpmLeadMagic   = []byte{0xed, 0xab, 0xee, 0xdb}
	rpmHeaderMagic = []byte{0x8e, 0xad, 0xe8}
)

// RPM tag numbers (rpmtag.h) and value types.
const (
	rpmTagName        = 1000
	rpmTagVersion     = 1001
	rpmTagRelease     = 1002
	rpmTagSummary     = 1004
	rpmTagSize        = 1009
	rpmTagVendor      = 1011
	rpmTagLicense     = 1014
	rpmTagPackager    = 1015
	rpmTagGroup       = 1016
	rpmTagArch        = 1022
	rpmTagRequireName = 1049

	rpmTypeInt32       = 4
	rpmTypeInt64       = 5
	rpmTypeString      = 6
	rpmTypeStringArray = 8
	rpmTypeI18NString  = 9
)

type rpmIndexEntry struct {
	tag, typ, offset, count uint32
}

type rpmHeader struct {
	r       io.ReaderAt
	store   int64 // absolute offset of the store
	size    int64 // store size
	entries []rpmIndexEntry
}

// rpmExtractor parses the RPM header directly (see the layout above).
type rpmExtractor struct{}

func (*rpmExtractor) Kind() Kind           { return KindPackage }
func (*rpmExtractor) Extensions() []string { return []string{"rpm"} }

func (*rpmExtractor) Extract(ctx context.Context, path string, r io.ReaderAt, size int64) ([]Field, error) {
	var lead [rpmLeadLen]byte
	if err := readAtFull(r, lead[:], 0); err != nil {
		return nil, fmt.Errorf("rpm: lead: %w", err)
	}
	if !bytes.Equal(lead[:4], rpmLeadMagic) {
		return nil, errors.New("rpm: not an rpm (bad lead magic)")
	}
	// Signature header: parse only for its length.
	sig, err := readRPMHeader(r, rpmLeadLen, size)
	if err != nil {
		return nil, fmt.Errorf("rpm: signature header: %w", err)
	}
	next := sig.store + sig.size
	next += (8 - next%8) % 8 // padded to 8
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	h, err := readRPMHeader(r, next, size)
	if err != nil {
		return nil, fmt.Errorf("rpm: header: %w", err)
	}
	return rpmFields(h), nil
}

func readRPMHeader(r io.ReaderAt, off, size int64) (*rpmHeader, error) {
	if off < 0 || off+rpmHeaderIntroLen > size {
		return nil, io.ErrUnexpectedEOF
	}
	var intro [rpmHeaderIntroLen]byte
	if err := readAtFull(r, intro[:], off); err != nil {
		return nil, err
	}
	if !bytes.Equal(intro[:3], rpmHeaderMagic) {
		return nil, errors.New("bad header magic")
	}
	nindex := int64(binary.BigEndian.Uint32(intro[8:12]))
	hsize := int64(binary.BigEndian.Uint32(intro[12:16]))
	if nindex == 0 || nindex > rpmMaxIndex {
		return nil, fmt.Errorf("index count %d out of range", nindex)
	}
	if hsize > rpmMaxStore {
		return nil, fmt.Errorf("store size %d exceeds cap", hsize)
	}
	idxOff := off + rpmHeaderIntroLen
	storeOff := idxOff + nindex*rpmIndexEntryLen
	if storeOff+hsize > size {
		return nil, errors.New("header runs past end of file")
	}
	raw := make([]byte, nindex*rpmIndexEntryLen)
	if err := readAtFull(r, raw, idxOff); err != nil {
		return nil, err
	}
	h := &rpmHeader{r: r, store: storeOff, size: hsize, entries: make([]rpmIndexEntry, nindex)}
	for i := range h.entries {
		e := raw[i*rpmIndexEntryLen:]
		h.entries[i] = rpmIndexEntry{
			tag:    binary.BigEndian.Uint32(e[0:4]),
			typ:    binary.BigEndian.Uint32(e[4:8]),
			offset: binary.BigEndian.Uint32(e[8:12]),
			count:  binary.BigEndian.Uint32(e[12:16]),
		}
		if int64(h.entries[i].offset) > hsize {
			return nil, fmt.Errorf("index %d offset out of range", i)
		}
	}
	return h, nil
}

func (h *rpmHeader) find(tag uint32) (rpmIndexEntry, bool) {
	for _, e := range h.entries {
		if e.tag == tag {
			return e, true
		}
	}
	return rpmIndexEntry{}, false
}

// readStore reads up to n bytes of the store at offset, clipped to the store.
func (h *rpmHeader) readStore(offset uint32, n int64) []byte {
	start := int64(offset)
	if start >= h.size {
		return nil
	}
	if remain := h.size - start; n > remain {
		n = remain
	}
	buf := make([]byte, n)
	got, err := h.r.ReadAt(buf, h.store+start)
	if err != nil && err != io.EOF {
		return nil
	}
	return buf[:got]
}

// str returns a STRING or the first (C locale) entry of an I18NSTRING.
func (h *rpmHeader) str(tag uint32) string {
	e, ok := h.find(tag)
	if !ok || (e.typ != rpmTypeString && e.typ != rpmTypeI18NString) {
		return ""
	}
	b := h.readStore(e.offset, rpmMaxString)
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// strs returns up to max entries of a STRING_ARRAY.
func (h *rpmHeader) strs(tag uint32, max int) []string {
	e, ok := h.find(tag)
	if !ok || e.typ != rpmTypeStringArray || e.count == 0 {
		return nil
	}
	b := h.readStore(e.offset, rpmMaxArrayBytes)
	var out []string
	for len(b) > 0 && len(out) < max && uint32(len(out)) < e.count {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			break // truncated by the read cap: keep what we have
		}
		if s := string(b[:i]); s != "" {
			out = append(out, s)
		}
		b = b[i+1:]
	}
	return out
}

// integer returns an INT32/INT64 scalar.
func (h *rpmHeader) integer(tag uint32) (int64, bool) {
	e, ok := h.find(tag)
	if !ok {
		return 0, false
	}
	switch e.typ {
	case rpmTypeInt32:
		b := h.readStore(e.offset, 4)
		if len(b) < 4 {
			return 0, false
		}
		return int64(binary.BigEndian.Uint32(b)), true
	case rpmTypeInt64:
		b := h.readStore(e.offset, 8)
		if len(b) < 8 {
			return 0, false
		}
		v := binary.BigEndian.Uint64(b)
		if v > 1<<62 {
			return 0, false
		}
		return int64(v), true
	}
	return 0, false
}

func rpmFields(h *rpmHeader) []Field {
	out := []Field{
		text("package.format", "rpm"),
		text("package.name", h.str(rpmTagName)),
		text("package.version", h.str(rpmTagVersion)),
		text("package.release", h.str(rpmTagRelease)),
		text("package.arch", h.str(rpmTagArch)),
		text("package.summary", h.str(rpmTagSummary)),
		text("package.section", h.str(rpmTagGroup)),
		text("package.maintainer", h.str(rpmTagPackager)),
		text("package.license", h.str(rpmTagLicense)),
		text("package.vendor", h.str(rpmTagVendor)),
	}
	if sz, ok := h.integer(rpmTagSize); ok && sz >= 0 {
		out = append(out, numInt("package.installedSize", sz))
	}
	for _, dep := range h.strs(rpmTagRequireName, rpmMaxRequires) {
		// rpmlib(...) pseudo-dependencies and file paths are noise.
		if strings.HasPrefix(dep, "rpmlib(") || strings.HasPrefix(dep, "/") {
			continue
		}
		out = append(out, text("package.depends", dep))
	}
	return dedupe(out)
}
