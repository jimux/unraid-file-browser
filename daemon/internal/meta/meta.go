// Package meta extracts searchable metadata embedded in files: EXIF from
// photos, stream layout from videos (via ffprobe), tags from music, and the
// control fields of .deb/.rpm packages. It is pure extraction — no database,
// no filesystem walking. The index package calls it during a crawl and stores
// the resulting fields.
//
// Every input is hostile by default: any SMB user can plant a file. All
// parsers read bounded windows through a budgeted reader, never allocate on
// an attacker-supplied length without a cap, run under a per-file wall-clock
// timeout, and are wrapped in a recover() so a third-party parser panic is
// reported as an error instead of taking the daemon down.
package meta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind is a metadata category; it doubles as the key prefix ("video.hdr").
type Kind string

const (
	KindImage   Kind = "image"
	KindVideo   Kind = "video"
	KindAudio   Kind = "audio"
	KindPackage Kind = "package"
)

// Field is one extracted key/value. Num is set when the value is numerically
// comparable (sizes, counts, seconds, unix timestamps for dates); Value is
// always the canonical string form the UI displays and "=" matches against.
type Field struct {
	Key   string
	Value string
	Num   *float64
}

// Extractor reads the metadata of one category of file.
type Extractor interface {
	Kind() Kind
	// Extensions are lowercase without a dot. Case-insensitive matching is
	// the caller's job (the index stores lowercased extensions already).
	Extensions() []string
	// Extract reads path's metadata through r (size bytes long). It must
	// return an error, never panic, on malformed input; an empty field list
	// with a nil error means "well-formed but nothing to say".
	Extract(ctx context.Context, path string, r io.ReaderAt, size int64) ([]Field, error)
}

// Limits shared by the extractors.
const (
	// FileTimeout is the wall-clock budget for one file, including any
	// ffprobe subprocess. Extract callers should apply it; Run does.
	FileTimeout = 30 * time.Second

	// maxValueLen bounds every stored string; longer values are cut on a
	// rune boundary. Metadata strings are labels, not documents.
	maxValueLen = 512

	// maxFieldsPerFile bounds the row count one file can produce (a package
	// with 10 000 dependencies is not worth 10 000 rows).
	maxFieldsPerFile = 256
)

// ErrUnsupported is returned when a file's extension maps to a category but
// its content is not something the extractor handles (e.g. a .png with no
// eXIf chunk). It is not a corruption signal.
var ErrUnsupported = errors.New("meta: no extractable metadata")

// Extractors returns the extractor set. ffprobe-backed extraction (video
// entirely; audio bitrate/duration fallback) is omitted when ffprobePath is
// empty, so the video category disappears from ExtensionKind.
func Extractors(ffprobePath string) []Extractor {
	var ff *ffprobe
	if ffprobePath != "" {
		ff = newFFprobe(ffprobePath)
	}
	out := []Extractor{
		&imageExtractor{},
		&audioExtractor{ff: ff},
		&debExtractor{},
		&rpmExtractor{},
	}
	if ff != nil {
		out = append(out, &videoExtractor{ff: ff})
	}
	return out
}

// Run applies the matching extractor to one file with the recover guard,
// the per-file timeout and field normalisation (length caps, NUL stripping,
// count cap). Callers that only need the raw Extractor interface may call
// Extract directly, but the crawler should always use Run.
func Run(ctx context.Context, ex Extractor, path string, r io.ReaderAt, size int64) (fields []Field, err error) {
	ctx, cancel := context.WithTimeout(ctx, FileTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			log.Printf("meta: %s extractor panicked on %s: %v\n%s", ex.Kind(), path, p, debug.Stack())
			fields, err = nil, fmt.Errorf("meta: %s extractor panicked: %v", ex.Kind(), p)
		}
	}()
	fields, err = ex.Extract(ctx, path, r, size)
	if err != nil {
		return nil, err
	}
	return normalize(fields), nil
}

// ByExtension indexes extractors by extension. Later extractors win a clash
// (there are none in the shipped set).
func ByExtension(exs []Extractor) map[string]Extractor {
	m := make(map[string]Extractor)
	for _, ex := range exs {
		for _, e := range ex.Extensions() {
			m[strings.ToLower(e)] = ex
		}
	}
	return m
}

// ExtensionKind maps a lowercase extension (no dot) to its category. It
// reflects the full extractor set, ffprobe included; a daemon without ffprobe
// simply extracts nothing for video files.
func ExtensionKind(ext string) (Kind, bool) {
	k, ok := extKinds[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return k, ok
}

// AllExtensions returns every extension the given extractors cover, sorted
// and deduplicated — the crawler's candidate filter.
func AllExtensions(exs []Extractor) []string {
	seen := map[string]bool{}
	var out []string
	for _, ex := range exs {
		for _, e := range ex.Extensions() {
			e = strings.ToLower(e)
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	sort.Strings(out)
	return out
}

var extKinds = func() map[string]Kind {
	m := map[string]Kind{}
	for _, e := range imageExts {
		m[e] = KindImage
	}
	for _, e := range videoExts {
		m[e] = KindVideo
	}
	for _, e := range audioExts {
		m[e] = KindAudio
	}
	m["deb"] = KindPackage
	m["rpm"] = KindPackage
	return m
}()

// --- field helpers ----------------------------------------------------------

// normalize applies the per-field caps: drop empty values, strip NUL and
// control characters, cut long strings, reject non-finite numbers, and cap
// the count.
func normalize(in []Field) []Field {
	out := make([]Field, 0, len(in))
	for _, f := range in {
		if f.Key == "" {
			continue
		}
		f.Value = cleanString(f.Value)
		if f.Num != nil && (math.IsNaN(*f.Num) || math.IsInf(*f.Num, 0)) {
			f.Num = nil
		}
		if f.Value == "" {
			if f.Num == nil {
				continue
			}
			f.Value = fmtNum(*f.Num)
		}
		out = append(out, f)
		if len(out) >= maxFieldsPerFile {
			break
		}
	}
	return out
}

// cleanString trims, drops NUL/control characters (tab and newline become a
// space) and bounds the length on a rune boundary.
func cleanString(s string) string {
	if s == "" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.TrimSpace(s)
	if len(s) > maxValueLen {
		cut := maxValueLen
		for cut > 0 && !isRuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimSpace(s[:cut])
	}
	return s
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// fmtNum renders a number canonically: integers without a decimal point,
// everything else with up to 4 significant decimals trimmed.
func fmtNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// text builds a string field; empty values are dropped by normalize.
func text(key, value string) Field { return Field{Key: key, Value: value} }

// num builds a numeric field with the canonical string form.
func num(key string, v float64) Field {
	return Field{Key: key, Value: fmtNum(v), Num: &v}
}

// numInt is num for integer inputs.
func numInt(key string, v int64) Field { return num(key, float64(v)) }

// date builds a date field: Value RFC3339 (UTC), Num unix seconds.
func date(key string, t time.Time) Field {
	if t.IsZero() {
		return Field{}
	}
	v := float64(t.Unix())
	return Field{Key: key, Value: t.UTC().Format(time.RFC3339), Num: &v}
}

// --- bounded reading --------------------------------------------------------

// budgetReader is an io.ReadSeeker/ReaderAt over a fixed-size section with a
// cumulative read budget: once a parser has pulled more than budget bytes in
// total the reads fail with errBudget. Header parsers legitimately read a few
// hundred KB (a JPEG with a large embedded preview, an MP4 moov box, an ID3
// tag with cover art); a hostile file that keeps a parser reading forever
// hits the budget instead of the daemon's memory or the disk's patience.
type budgetReader struct {
	sr     *io.SectionReader
	remain int64
}

var errBudget = errors.New("meta: read budget exhausted")

func newBudgetReader(r io.ReaderAt, size, budget int64) *budgetReader {
	if size < 0 {
		size = 0
	}
	return &budgetReader{sr: io.NewSectionReader(r, 0, size), remain: budget}
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.remain <= 0 {
		return 0, errBudget
	}
	if int64(len(p)) > b.remain {
		p = p[:b.remain]
	}
	n, err := b.sr.Read(p)
	b.remain -= int64(n)
	return n, err
}

func (b *budgetReader) ReadAt(p []byte, off int64) (int, error) {
	if b.remain <= 0 {
		return 0, errBudget
	}
	if int64(len(p)) > b.remain {
		p = p[:b.remain]
	}
	n, err := b.sr.ReadAt(p, off)
	b.remain -= int64(n)
	return n, err
}

func (b *budgetReader) Seek(offset int64, whence int) (int64, error) {
	return b.sr.Seek(offset, whence)
}

func (b *budgetReader) Size() int64 { return b.sr.Size() }

// readAtFull reads exactly len(p) bytes at off or fails.
func readAtFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// catalogExts lists the extensions of one kind for Catalog.
func catalogExts(k Kind) []string {
	var out []string
	for e, kk := range extKinds {
		if kk == k {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}
