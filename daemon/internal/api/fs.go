package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"unraid-filebrowser/internal/types"
	"unraid-filebrowser/internal/view"
)

// maxOffset keeps offsets in a range where offset+length cannot overflow.
const maxOffset = int64(1) << 62

type listData struct {
	Entries []types.Entry `json:"entries"`
	Total   int           `json:"total"`
	Path    string        `json:"path"`
}

type entryData struct {
	Entry types.Entry `json:"entry"`
}

type viewData struct {
	View types.ViewResult `json:"view"`
}

type hexData struct {
	Rows      []types.HexRow `json:"rows"`
	Size      int64          `json:"size"`
	Truncated bool           `json:"truncated"`
}

type encodingsData struct {
	Encodings []view.EncodingInfo `json:"encodings"`
}

func (s *server) fsList(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	path, err := pathParam(q)
	if err != nil {
		return nil, err
	}
	offset, err := intParam(q, "offset", 0, 0, maxOffset)
	if err != nil {
		return nil, err
	}
	limit, err := intParam(q, "limit", listLimitDefault, 0, listLimitMax)
	if err != nil {
		return nil, err
	}
	sortKey, err := enumParam(q, "sort", "name", "name", "size", "mtime", "type")
	if err != nil {
		return nil, err
	}
	dir, err := enumParam(q, "dir", "asc", "asc", "desc")
	if err != nil {
		return nil, err
	}
	dirsFirst, err := boolParam(q, "dirsFirst", true)
	if err != nil {
		return nil, err
	}

	entries, err := s.list(r.Context(), path)
	if err != nil {
		return nil, err
	}
	sortEntries(entries, sortKey, dir, dirsFirst)
	return listData{Entries: page(entries, offset, limit), Total: len(entries), Path: path}, nil
}

func (s *server) fsStat(w http.ResponseWriter, r *http.Request) (any, error) {
	path, err := pathParam(r.URL.Query())
	if err != nil {
		return nil, err
	}
	entry, err := s.stat(r.Context(), path)
	if err != nil {
		return nil, err
	}
	return entryData{Entry: entry}, nil
}

func (s *server) fsView(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	path, err := pathParam(q)
	if err != nil {
		return nil, err
	}
	offset, err := intParam(q, "offset", 0, 0, maxOffset)
	if err != nil {
		return nil, err
	}
	length, err := intParam(q, "length", viewLengthDefault, 1, viewLengthMax)
	if err != nil {
		return nil, err
	}
	encoding := q.Get("encoding")

	// Decoding an entry out of an archive can cost an extraction, so a view
	// inside a virtual path counts against the expensive-request budget. A
	// view of a real file is a bounded pread and is left alone.
	if s.isVirtual(path) {
		release, err := s.busy.acquire(r.Context())
		if err != nil {
			return nil, err
		}
		defer release()
	}

	rc, entry, err := s.open(r.Context(), path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	res, err := view.Decode(rc, entry.Size, encoding, offset, length)
	if err != nil {
		return nil, err
	}
	if entry.Mime != "" {
		res.Mime = entry.Mime
	}
	return viewData{View: res}, nil
}

func (s *server) fsHex(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	path, err := pathParam(q)
	if err != nil {
		return nil, err
	}
	offset, err := intParam(q, "offset", 0, 0, maxOffset)
	if err != nil {
		return nil, err
	}
	length, err := intParam(q, "length", hexLengthDefault, 1, hexLengthMax)
	if err != nil {
		return nil, err
	}

	rc, entry, err := s.open(r.Context(), path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	rows, trunc, err := view.HexRows(rc, entry.Size, offset, length)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []types.HexRow{}
	}
	return hexData{Rows: rows, Size: entry.Size, Truncated: trunc}, nil
}

// rawInlineTypes are the media types fs/raw is willing to hand the browser
// with their real Content-Type. Nothing here can execute script, so nothing
// here can be turned into stored XSS on the webGUI origin (the SPA shares it,
// and the webGUI session is root). text/plain is on the list only because
// nosniff plus the sandbox CSP stop a browser re-interpreting it as HTML.
//
// Everything absent from this list — text/html, image/svg+xml, XML of any
// flavour, JavaScript, and every unknown or empty type — is served as
// application/octet-stream with an attachment disposition.
var rawInlineTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"image/bmp":       true,
	"image/avif":      true,
	"application/pdf": true,
	"text/plain":      true,
}

// rawInlinePrefixes are whole families that render but cannot script.
var rawInlinePrefixes = []string{"audio/", "video/"}

// rawContentType decides what fs/raw actually advertises for an entry and
// whether the body may render inline. mime is the sniffed/derived type from
// the filesystem layer, possibly with parameters ("text/plain; charset=utf-8")
// and possibly empty.
func rawContentType(mime string) (ct string, inline bool) {
	essence := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(essence, ';'); i >= 0 {
		essence = strings.TrimSpace(essence[:i])
	}
	if rawInlineTypes[essence] {
		return mime, true
	}
	for _, p := range rawInlinePrefixes {
		if strings.HasPrefix(essence, p) && len(essence) > len(p) {
			return mime, true
		}
	}
	return "application/octet-stream", false
}

// fsRaw streams bytes. It is deliberately not wrapped in the JSON envelope or
// the request timeout: downloads of array-sized files must not be cut off.
func (s *server) fsRaw(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path, err := pathParam(q)
	if err != nil {
		writeError(w, r, err)
		return
	}
	download, err := boolParam(q, "download", false)
	if err != nil {
		writeError(w, r, err)
		return
	}

	rc, entry, err := s.open(r.Context(), path)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer rc.Close()

	// Defence in depth for every raw byte we serve, inline or not: no MIME
	// sniffing, no script/plugin/same-origin privileges if it renders at all,
	// and nothing left in a shared cache.
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")

	ct, inline := rawContentType(entry.Mime)
	h.Set("Content-Type", ct)
	if download || !inline {
		h.Set("Content-Disposition", disposition(entry.Name))
	}

	var modtime time.Time
	if entry.Mtime > 0 {
		modtime = time.Unix(entry.Mtime, 0)
	}

	// Real files are seekable, so Range works; archive entries are streams.
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, entry.Name, modtime, rs)
		return
	}
	h.Set("Accept-Ranges", "none")
	if entry.Size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, rc)
}

func (s *server) encodings(w http.ResponseWriter, r *http.Request) (any, error) {
	return encodingsData{Encodings: view.Encodings()}, nil
}

// disposition builds a Content-Disposition value that survives odd file names:
// a sanitised ASCII filename for old clients plus the RFC 5987 UTF-8 form
// (which every current browser prefers) when the name needs it.
func disposition(name string) string {
	if name == "" {
		name = "download"
	}
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' || r > 0x7e {
			return '_'
		}
		return r
	}, name)
	d := `attachment; filename="` + ascii + `"`
	if ascii != name {
		d += "; filename*=UTF-8''" + escapeExtValue(name)
	}
	return d
}

// escapeExtValue percent-encodes everything outside RFC 5987's attr-char set.
func escapeExtValue(s string) string {
	const safe = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&0x0F])
		}
	}
	return b.String()
}
