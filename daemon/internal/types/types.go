// Package types holds the shared data structures of the REST API contract.
// This file is the Go mirror of API.md — keep the two in sync. All subsystem
// packages (api, fsops, view, archive, index) depend on this package and it
// depends on nothing, so it is safe ground for parallel development.
package types

// EntryType classifies a directory entry.
type EntryType string

const (
	TypeFile    EntryType = "file"
	TypeDir     EntryType = "dir"
	TypeSymlink EntryType = "symlink"
	// TypeArchive marks a regular file that can be browsed as a directory
	// (zip, tar.*, 7z, iso, ...). The UI drills into these via virtual paths.
	TypeArchive EntryType = "archive"
)

// Entry is one row in a directory listing (real or inside an archive).
type Entry struct {
	Name   string    `json:"name"`
	Path   string    `json:"path"` // full, possibly virtual ("a.zip!/b.txt")
	Type   EntryType `json:"type"`
	Size   int64     `json:"size"`  // -1 when unknown
	Mtime  int64     `json:"mtime"` // unix seconds, 0 when unknown
	Mime   string    `json:"mime"`
	Target string    `json:"target,omitempty"` // symlink target
}

// ViewResult is a decoded text window over any file (fs/view).
type ViewResult struct {
	Text      string `json:"text"`
	Encoding  string `json:"encoding"` // encoding actually applied
	Sniffed   string `json:"sniffed"`  // what auto-detection suggested
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"` // source bytes consumed by this window
	Truncated bool   `json:"truncated"`
	Lossy     bool   `json:"lossy"` // replacement characters were produced
}

// HexRow is one 16-byte row of a hex view (fs/hex).
type HexRow struct {
	Offset int64  `json:"offset"`
	Hex    string `json:"hex"`   // space-separated byte pairs
	ASCII  string `json:"ascii"` // printable ASCII, '.' otherwise
}

// SearchHit is one search result.
type SearchHit struct {
	Entry     Entry   `json:"entry"`
	Score     float64 `json:"score"`
	Snippet   string  `json:"snippet,omitempty"` // HTML with <mark>, content hits
	MatchedIn string  `json:"matchedIn"`         // "name" | "content"
}

// SearchQuery carries the parsed parameters of GET /search.
type SearchQuery struct {
	Q       string // FTS-style query text; optional when at least one filter is set
	Mode    string // "name" | "content" | "both"
	Path    string // scope prefix, "" = everywhere
	Exts    []string
	MinSize int64 // -1 = unset
	MaxSize int64 // -1 = unset
	After   int64 // mtime lower bound, 0 = unset
	Before  int64 // mtime upper bound, 0 = unset
	Limit   int
	Offset  int
	// Meta filters are ANDed; see MetaFilter. Keys come from the catalog
	// (GET /meta/fields): "video.hdr", "image.cameraModel", ... plus the
	// common keys name, ext, size, mtime, mime.
	Meta []MetaFilter
	// Sort is "relevance" | "name" | "size" | "mtime"; "" = relevance when Q
	// is set, else name. Dir is "asc" | "desc" ("" = asc, except relevance
	// which is always best-first).
	Sort string
	Dir  string
}

// MetaFilter is one embedded-metadata predicate of a search. Op is one of
// "=", "!=", "~" (case-insensitive substring), "<", "<=", ">", ">=". The
// ordering operators apply to numeric fields only (number, bytes, date —
// dates accept RFC3339 or unix seconds). "!=" means "no value of this key
// equals Value", so a file with two audio tracks, one AC-3 and one AAC, does
// NOT match video.audioCodec != ac3.
type MetaFilter struct {
	Key   string `json:"key"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// MetaValue is one distinct value of a metadata key with its file count
// (GET /meta/values).
type MetaValue struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// MetaCategory groups the searchable fields of one kind of file. Extensions
// are lowercase without a dot; nil for the "common" category, whose fields
// exist for every indexed file.
type MetaCategory struct {
	ID         string         `json:"id"`    // "common" | "image" | "video" | "audio" | "package"
	Label      string         `json:"label"` // UI heading, e.g. "Video"
	Extensions []string       `json:"extensions"`
	Fields     []MetaFieldDef `json:"fields"`
}

// MetaFieldDef describes one searchable metadata key for the UI. Type is
// "text" | "number" | "bytes" | "date" | "enum" | "bool". Unit is a display
// hint for numbers ("mm", "px", "s", "bit/s", "Hz", ...). Values lists the
// enum members (enum and bool types) with friendly labels to show verbatim.
type MetaFieldDef struct {
	Key    string          `json:"key"`
	Label  string          `json:"label"`
	Type   string          `json:"type"`
	Unit   string          `json:"unit,omitempty"`
	Values []MetaEnumValue `json:"values,omitempty"`
}

// MetaEnumValue is one member of an enum field: the stored value and its
// display label.
type MetaEnumValue struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// IndexStatus is the shape of GET /index/status and each SSE event.
type IndexStatus struct {
	State          string  `json:"state"` // "idle" | "crawling" | "extracting" | "metadata"
	FilesIndexed   int64   `json:"filesIndexed"`
	ContentIndexed int64   `json:"contentIndexed"`
	MetaIndexed    int64   `json:"metaIndexed"` // files with at least one extracted metadata field
	DBBytes        int64   `json:"dbBytes"`
	LastFullScan   int64   `json:"lastFullScan"` // unix seconds, 0 = never
	Current        string  `json:"current,omitempty"`
	Progress       float64 `json:"progress,omitempty"` // 0..1 when estimable
}

// ContentRules controls which files get full-text content indexing.
type ContentRules struct {
	Enabled      bool     `json:"enabled"`
	IncludePaths []string `json:"includePaths"`
	Extensions   []string `json:"extensions"` // lowercase, no dot
	MaxFileBytes int64    `json:"maxFileBytes"`
}

// IndexConfig is the persisted indexer configuration (GET/PUT /index/config).
type IndexConfig struct {
	Roots       []string     `json:"roots"`
	Schedule    string       `json:"schedule"` // cron expression
	Parallelism int          `json:"parallelism"`
	Content     ContentRules `json:"content"`
}

// Error codes of the API envelope (API.md). Each maps to one HTTP status.
const (
	ErrBadRequest = "BAD_REQUEST"    // 400
	ErrNotFound   = "NOT_FOUND"      // 404
	ErrForbidden  = "FORBIDDEN"      // 403
	ErrTooLarge   = "TOO_LARGE"      // 413
	ErrTimeout    = "TIMEOUT"        // 504
	ErrArchive    = "ARCHIVE_ERROR"  // 422
	ErrEncoding   = "ENCODING_ERROR" // 422
	ErrIndexing   = "INDEXING"       // 503
	// ErrUnavailable marks an optional subsystem that is not present on this
	// host (media transcoding without ffmpeg). 503, like INDEXING, but the
	// client should not retry: the situation is static until reinstall.
	ErrUnavailable = "UNAVAILABLE" // 503
	ErrInternal    = "INTERNAL"    // 500
)

// APIError is an error carrying an API error code; the api package renders it
// into the envelope. Subsystems should return these for expected failures.
type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// Errf builds an *APIError.
func Errf(code, message string) *APIError { return &APIError{Code: code, Message: message} }

// archiveExts are the file suffixes browsable as directories via virtual
// paths. Both fsops (to mark Type=archive in listings) and the archive
// resolver (to validate layers) must use IsArchiveName so they never disagree.
var archiveExts = []string{
	".zip", ".jar", ".war", ".cbz", ".epub",
	".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tar.xz", ".txz", ".tar.zst",
	".gz", ".bz2", ".xz", ".zst",
	".7z", ".rar", ".cbr", ".iso", ".img", ".cab", ".wim", ".dmg", ".deb", ".rpm",
}

// IsArchiveName reports whether a file name looks like a browsable archive.
func IsArchiveName(name string) bool {
	n := toLowerASCII(name)
	for _, ext := range archiveExts {
		if len(n) > len(ext) && n[len(n)-len(ext):] == ext {
			return true
		}
	}
	return false
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
