package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"unraid-filebrowser/internal/types"
)

func TestMain(m *testing.M) {
	// writeError logs INTERNAL errors; several tests provoke them on purpose.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// ------------------------------------------------------------------ fakes

// seekReadCloser is what a real file looks like to the api layer: seekable, so
// fs/raw can serve Range requests through http.ServeContent.
type seekReadCloser struct {
	*bytes.Reader
	closed bool
}

func (s *seekReadCloser) Close() error { s.closed = true; return nil }

// streamReadCloser is what an archive entry looks like: a forward-only stream.
type streamReadCloser struct {
	r      io.Reader
	closed bool
}

func (s *streamReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *streamReadCloser) Close() error               { s.closed = true; return nil }

type fakeFS struct {
	entries []types.Entry
	body    []byte
	entry   types.Entry
	err     error

	stream bool // hand back a non-seekable reader

	mu        sync.Mutex
	listPaths []string
	openPaths []string
	statPaths []string
	lastOpen  io.ReadCloser
}

func (f *fakeFS) record(dst *[]string, p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*dst = append(*dst, p)
}

func (f *fakeFS) calls(get func(*fakeFS) *[]string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), *get(f)...)
}

func (f *fakeFS) List(ctx context.Context, path string) ([]types.Entry, error) {
	f.record(&f.listPaths, path)
	if f.err != nil {
		return nil, f.err
	}
	return append([]types.Entry(nil), f.entries...), nil
}

func (f *fakeFS) Stat(ctx context.Context, path string) (types.Entry, error) {
	f.record(&f.statPaths, path)
	if f.err != nil {
		return types.Entry{}, f.err
	}
	return f.entry, nil
}

func (f *fakeFS) Open(ctx context.Context, path string) (io.ReadCloser, types.Entry, error) {
	f.record(&f.openPaths, path)
	if f.err != nil {
		return nil, types.Entry{}, f.err
	}
	e := f.entry
	if e.Size == 0 {
		e.Size = int64(len(f.body))
	}
	var rc io.ReadCloser
	if f.stream {
		rc = &streamReadCloser{r: bytes.NewReader(f.body)}
	} else {
		rc = &seekReadCloser{Reader: bytes.NewReader(f.body)}
	}
	f.mu.Lock()
	f.lastOpen = rc
	f.mu.Unlock()
	return rc, e, nil
}

// fakeArchive routes on a prefix so tests can assert virtual-vs-real dispatch
// without depending on the real resolver.
type fakeArchive struct {
	fakeFS
	virtual func(string) bool
}

func (a *fakeArchive) IsVirtual(p string) bool {
	if a.virtual != nil {
		return a.virtual(p)
	}
	return strings.Contains(p, "!/")
}

type fakeIndex struct {
	hits   []types.SearchHit
	total  int
	err    error
	status types.IndexStatus
	cfg    types.IndexConfig
	setErr error

	allowedRoots []string

	categories []types.MetaCategory
	values     []types.MetaValue
	valuesErr  error

	mu          sync.Mutex
	lastQuery   types.SearchQuery
	lastValues  metaValuesCall
	valuesCalls int
	rescanPaths []string
	paused      int
	resumed     int
	events      chan types.IndexStatus
	unsubscribe int
}

// metaValuesCall records the arguments of the last MetaValues call.
type metaValuesCall struct {
	key    string
	prefix string
	path   string
	limit  int
}

func (i *fakeIndex) MetaFields() []types.MetaCategory { return i.categories }

func (i *fakeIndex) MetaValues(ctx context.Context, key, prefix, pathScope string, limit int) ([]types.MetaValue, error) {
	i.mu.Lock()
	i.lastValues = metaValuesCall{key: key, prefix: prefix, path: pathScope, limit: limit}
	i.valuesCalls++
	i.mu.Unlock()
	if i.valuesErr != nil {
		return nil, i.valuesErr
	}
	return i.values, nil
}

func (i *fakeIndex) valuesCall() metaValuesCall {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastValues
}

func (i *fakeIndex) Search(ctx context.Context, q types.SearchQuery) ([]types.SearchHit, int, error) {
	i.mu.Lock()
	i.lastQuery = q
	i.mu.Unlock()
	if i.err != nil {
		return nil, 0, i.err
	}
	return i.hits, i.total, nil
}

func (i *fakeIndex) query() types.SearchQuery {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastQuery
}

func (i *fakeIndex) Status() types.IndexStatus { return i.status }

func (i *fakeIndex) Subscribe() (<-chan types.IndexStatus, func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.events == nil {
		i.events = make(chan types.IndexStatus, 8)
	}
	ch := i.events
	return ch, func() {
		i.mu.Lock()
		i.unsubscribe++
		i.mu.Unlock()
	}
}

func (i *fakeIndex) unsubscribes() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.unsubscribe
}

func (i *fakeIndex) Config() types.IndexConfig { return i.cfg }

func (i *fakeIndex) AllowedRoots() []string { return i.allowedRoots }

func (i *fakeIndex) SetConfig(cfg types.IndexConfig) error {
	if i.setErr != nil {
		return i.setErr
	}
	i.cfg = cfg
	return nil
}

func (i *fakeIndex) Rescan(path string) error {
	i.mu.Lock()
	i.rescanPaths = append(i.rescanPaths, path)
	i.mu.Unlock()
	return i.err
}

func (i *fakeIndex) Pause()  { i.mu.Lock(); i.paused++; i.mu.Unlock() }
func (i *fakeIndex) Resume() { i.mu.Lock(); i.resumed++; i.mu.Unlock() }

// ---------------------------------------------------------------- helpers

type envelopeOut struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *errorBody      `json:"error"`
}

func do(t *testing.T, h http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// getJSON performs a request and asserts the success envelope, decoding data.
func getJSON(t *testing.T, h http.Handler, target string, data any) {
	t.Helper()
	rec := do(t, h, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body %s)", target, rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var env envelopeOut
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("GET %s: body is not JSON: %v (%s)", target, err, rec.Body)
	}
	if !env.OK {
		t.Fatalf("GET %s: ok=false %+v", target, env.Error)
	}
	if env.Error != nil {
		t.Errorf("GET %s: success envelope carries an error %+v", target, env.Error)
	}
	if data != nil {
		if err := json.Unmarshal(env.Data, data); err != nil {
			t.Fatalf("GET %s: decode data: %v (%s)", target, err, env.Data)
		}
	}
}

// expectErr performs a request and asserts the error envelope and status.
func expectErr(t *testing.T, h http.Handler, method, target string, status int, code string) errorBody {
	t.Helper()
	rec := do(t, h, method, target, nil)
	if rec.Code != status {
		t.Fatalf("%s %s = %d, want %d (body %s)", method, target, rec.Code, status, rec.Body)
	}
	var env envelopeOut
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: body is not JSON: %v (%s)", method, target, err, rec.Body)
	}
	if env.OK {
		t.Fatalf("%s %s: ok=true on an error response", method, target)
	}
	if env.Error == nil {
		t.Fatalf("%s %s: error envelope has no error object", method, target)
	}
	if env.Error.Code != code {
		t.Fatalf("%s %s: code = %q, want %q", method, target, env.Error.Code, code)
	}
	if env.Error.Message == "" {
		t.Errorf("%s %s: error message is empty", method, target)
	}
	if len(env.Data) != 0 && string(env.Data) != "null" {
		t.Errorf("%s %s: error envelope also carries data %s", method, target, env.Data)
	}
	return *env.Error
}

func listingFixture() []types.Entry {
	return []types.Entry{
		{Name: "beta", Path: "/r/beta", Type: types.TypeDir, Size: 100, Mtime: 300},
		{Name: "Alpha", Path: "/r/Alpha", Type: types.TypeDir, Size: 200, Mtime: 100},
		{Name: "zeta.txt", Path: "/r/zeta.txt", Type: types.TypeFile, Size: 50, Mtime: 500},
		{Name: "gamma.zip", Path: "/r/gamma.zip", Type: types.TypeArchive, Size: 10, Mtime: 200},
		{Name: "link", Path: "/r/link", Type: types.TypeSymlink, Size: 5, Mtime: 400},
	}
}

func names(entries []types.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------- the router

// The concrete subsystems must keep satisfying the consumer interfaces; this is
// the compile-time half of the integration contract.
func TestInterfaceContract(t *testing.T) {
	var (
		_ FileSystem   = (*fakeFS)(nil)
		_ ArchiveFS    = (*fakeArchive)(nil)
		_ IndexService = (*fakeIndex)(nil)
	)
}

func TestEnvelopeShapeOnSuccess(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{entries: listingFixture()}, Version: "1.2.3"})
	rec := do(t, h, http.MethodGet, "/api/v1/fs/list?path=/r", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if string(raw["ok"]) != "true" {
		t.Errorf(`"ok" = %s, want true`, raw["ok"])
	}
	if _, ok := raw["data"]; !ok {
		t.Errorf("success envelope has no data key: %s", rec.Body)
	}
	if _, ok := raw["error"]; ok {
		t.Errorf("success envelope carries an error key: %s", rec.Body)
	}
}

func TestErrorCodeToStatusMapping(t *testing.T) {
	cases := []struct {
		code   string
		status int
	}{
		{types.ErrBadRequest, http.StatusBadRequest},
		{types.ErrNotFound, http.StatusNotFound},
		{types.ErrForbidden, http.StatusForbidden},
		{types.ErrTooLarge, http.StatusRequestEntityTooLarge},
		{types.ErrTimeout, http.StatusGatewayTimeout},
		{types.ErrArchive, http.StatusUnprocessableEntity},
		{types.ErrEncoding, http.StatusUnprocessableEntity},
		{types.ErrIndexing, http.StatusServiceUnavailable},
		{types.ErrInternal, http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			fs := &fakeFS{err: types.Errf(c.code, "boom")}
			h := NewRouter(Deps{FS: fs})
			got := expectErr(t, h, http.MethodGet, "/api/v1/fs/list?path=/r", c.status, c.code)
			if got.Message != "boom" {
				t.Errorf("message = %q, want the subsystem's own message", got.Message)
			}
		})
	}
}

// Errors that are not part of the vocabulary become INTERNAL and must not leak
// their text to the client.
func TestUnknownErrorsBecomeInternal(t *testing.T) {
	fs := &fakeFS{err: errors.New("postgres://user:hunter2@db/secret is down")}
	h := NewRouter(Deps{FS: fs})
	got := expectErr(t, h, http.MethodGet, "/api/v1/fs/list?path=/r",
		http.StatusInternalServerError, types.ErrInternal)
	if strings.Contains(got.Message, "hunter2") {
		t.Errorf("internal error leaked its message: %q", got.Message)
	}
}

func TestTimeoutAndPermissionMapping(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{err: context.DeadlineExceeded}})
	expectErr(t, h, http.MethodGet, "/api/v1/fs/list?path=/r", http.StatusGatewayTimeout, types.ErrTimeout)

	h = NewRouter(Deps{FS: &fakeFS{err: os.ErrNotExist}})
	expectErr(t, h, http.MethodGet, "/api/v1/fs/list?path=/r", http.StatusNotFound, types.ErrNotFound)

	h = NewRouter(Deps{FS: &fakeFS{err: os.ErrPermission}})
	expectErr(t, h, http.MethodGet, "/api/v1/fs/list?path=/r", http.StatusForbidden, types.ErrForbidden)
}

func TestUnknownRouteAndMethod(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{}})
	expectErr(t, h, http.MethodGet, "/api/v1/nope", http.StatusNotFound, types.ErrNotFound)
	expectErr(t, h, http.MethodGet, "/", http.StatusNotFound, types.ErrNotFound)

	rec := do(t, h, http.MethodPost, "/api/v1/fs/list", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /fs/list = %d, want 405 (body %s)", rec.Code, rec.Body)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q, want it to list GET", allow)
	}
	var env envelopeOut
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.OK {
		t.Errorf("405 body is not an error envelope: %s", rec.Body)
	}
}

// -------------------------------------------------------------- fs/list

func TestFsListPagination(t *testing.T) {
	fs := &fakeFS{entries: listingFixture()}
	h := NewRouter(Deps{FS: fs})

	var got listData
	getJSON(t, h, "/api/v1/fs/list?path=/r&offset=2&limit=2", &got)
	if got.Total != 5 {
		t.Errorf("Total = %d, want the full directory count 5", got.Total)
	}
	if got.Path != "/r" {
		t.Errorf("Path = %q", got.Path)
	}
	if want := []string{"gamma.zip", "link"}; !eq(names(got.Entries), want) {
		t.Errorf("page = %v, want %v", names(got.Entries), want)
	}

	// Offset past the end: an empty page, not an error and not null.
	getJSON(t, h, "/api/v1/fs/list?path=/r&offset=99", &got)
	if got.Entries == nil {
		t.Errorf("entries must serialise as [] not null")
	}
	if len(got.Entries) != 0 || got.Total != 5 {
		t.Errorf("past-the-end page = %v total=%d", names(got.Entries), got.Total)
	}

	// Limit larger than the directory returns everything.
	getJSON(t, h, "/api/v1/fs/list?path=/r&limit=100", &got)
	if len(got.Entries) != 5 {
		t.Errorf("got %d entries, want 5", len(got.Entries))
	}
}

func TestFsListSorting(t *testing.T) {
	fs := &fakeFS{entries: listingFixture()}
	h := NewRouter(Deps{FS: fs})

	cases := []struct {
		query string
		want  []string
	}{
		// Default: name ascending, directories first.
		{"", []string{"Alpha", "beta", "gamma.zip", "link", "zeta.txt"}},
		{"&sort=name&dir=asc&dirsFirst=false", []string{"Alpha", "beta", "gamma.zip", "link", "zeta.txt"}},
		{"&sort=name&dir=desc&dirsFirst=false", []string{"zeta.txt", "link", "gamma.zip", "beta", "Alpha"}},
		{"&sort=size&dir=asc&dirsFirst=false", []string{"link", "gamma.zip", "zeta.txt", "beta", "Alpha"}},
		{"&sort=size&dir=desc&dirsFirst=false", []string{"Alpha", "beta", "zeta.txt", "gamma.zip", "link"}},
		{"&sort=mtime&dir=asc&dirsFirst=false", []string{"Alpha", "gamma.zip", "beta", "link", "zeta.txt"}},
		{"&sort=mtime&dir=desc&dirsFirst=false", []string{"zeta.txt", "link", "beta", "gamma.zip", "Alpha"}},
		{"&sort=type&dir=asc&dirsFirst=false", []string{"Alpha", "beta", "gamma.zip", "zeta.txt", "link"}},
		// dirsFirst survives a descending sort — a file manager keeps folders up top.
		{"&sort=size&dir=desc&dirsFirst=true", []string{"Alpha", "beta", "zeta.txt", "gamma.zip", "link"}},
		{"&sort=name&dir=desc&dirsFirst=true", []string{"beta", "Alpha", "zeta.txt", "link", "gamma.zip"}},
	}
	for _, c := range cases {
		t.Run("sort"+c.query, func(t *testing.T) {
			var got listData
			getJSON(t, h, "/api/v1/fs/list?path=/r"+c.query, &got)
			if !eq(names(got.Entries), c.want) {
				t.Errorf("order = %v\n want %v", names(got.Entries), c.want)
			}
		})
	}
}

func TestFsListParamValidation(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{entries: listingFixture()}})
	cases := []string{
		"/api/v1/fs/list",                         // no path
		"/api/v1/fs/list?path=",                   // blank path
		"/api/v1/fs/list?path=/r&offset=-1",       // negative offset
		"/api/v1/fs/list?path=/r&limit=abc",       // non-numeric
		"/api/v1/fs/list?path=/r&sort=colour",     // unknown sort key
		"/api/v1/fs/list?path=/r&dir=sideways",    // unknown direction
		"/api/v1/fs/list?path=/r&dirsFirst=maybe", // not a boolean
	}
	for _, target := range cases {
		t.Run(target, func(t *testing.T) {
			expectErr(t, h, http.MethodGet, target, http.StatusBadRequest, types.ErrBadRequest)
		})
	}
}

// Over-large window/page requests are clamped to the contract's maximum rather
// than rejected: the client gets the biggest legal answer.
func TestListLimitCap(t *testing.T) {
	entries := make([]types.Entry, 12000)
	for i := range entries {
		entries[i] = types.Entry{Name: string(rune('a'+i%26)) + string(rune(i)), Type: types.TypeFile}
	}
	h := NewRouter(Deps{FS: &fakeFS{entries: entries}})

	var got listData
	getJSON(t, h, "/api/v1/fs/list?path=/r", &got)
	if len(got.Entries) != listLimitDefault {
		t.Errorf("default page = %d entries, want %d", len(got.Entries), listLimitDefault)
	}
	if got.Total != 12000 {
		t.Errorf("Total = %d, want the whole directory", got.Total)
	}

	getJSON(t, h, "/api/v1/fs/list?path=/r&limit=999999", &got)
	if len(got.Entries) != listLimitMax {
		t.Errorf("clamped page = %d entries, want %d", len(got.Entries), listLimitMax)
	}
}

// -------------------------------------------------------- fs/stat, view, hex

func TestFsStat(t *testing.T) {
	want := types.Entry{Name: "notes.txt", Path: "/r/notes.txt", Type: types.TypeFile, Size: 11, Mtime: 42, Mime: "text/plain"}
	h := NewRouter(Deps{FS: &fakeFS{entry: want}})

	var got entryData
	getJSON(t, h, "/api/v1/fs/stat?path=/r/notes.txt", &got)
	if got.Entry != want {
		t.Errorf("entry = %+v, want %+v", got.Entry, want)
	}
	expectErr(t, h, http.MethodGet, "/api/v1/fs/stat", http.StatusBadRequest, types.ErrBadRequest)
}

func TestFsView(t *testing.T) {
	fs := &fakeFS{
		body:  []byte("hello world"),
		entry: types.Entry{Name: "a.txt", Size: 11, Mime: "text/plain"},
	}
	h := NewRouter(Deps{FS: fs})

	var got viewData
	getJSON(t, h, "/api/v1/fs/view?path=/r/a.txt", &got)
	if got.View.Text != "hello world" {
		t.Errorf("text = %q", got.View.Text)
	}
	if got.View.Encoding != "utf-8" || got.View.Sniffed != "utf-8" {
		t.Errorf("encoding/sniffed = %q/%q", got.View.Encoding, got.View.Sniffed)
	}
	if got.View.Size != 11 || got.View.Offset != 0 || got.View.Length != 11 {
		t.Errorf("size/offset/length = %d/%d/%d", got.View.Size, got.View.Offset, got.View.Length)
	}
	if got.View.Truncated || got.View.Lossy {
		t.Errorf("whole small file should be neither truncated nor lossy")
	}
	if got.View.Mime != "text/plain" {
		t.Errorf("mime = %q, want the entry's", got.View.Mime)
	}

	// Windowing.
	getJSON(t, h, "/api/v1/fs/view?path=/r/a.txt&offset=6&length=3", &got)
	if got.View.Text != "wor" || got.View.Offset != 6 || got.View.Length != 3 {
		t.Errorf("window = %q offset=%d length=%d", got.View.Text, got.View.Offset, got.View.Length)
	}
	if !got.View.Truncated {
		t.Errorf("a window with bytes after it must report truncated")
	}

	// Any file may be viewed with any supported encoding ("binary as text").
	getJSON(t, h, "/api/v1/fs/view?path=/r/a.txt&encoding=iso-8859-1", &got)
	if got.View.Encoding != "iso-8859-1" {
		t.Errorf("encoding = %q, want the one the client forced", got.View.Encoding)
	}

	expectErr(t, h, http.MethodGet, "/api/v1/fs/view?path=/r/a.txt&encoding=klingon",
		http.StatusUnprocessableEntity, types.ErrEncoding)
	expectErr(t, h, http.MethodGet, "/api/v1/fs/view?path=/r/a.txt&length=0",
		http.StatusBadRequest, types.ErrBadRequest)
	expectErr(t, h, http.MethodGet, "/api/v1/fs/view?path=/r/a.txt&offset=-3",
		http.StatusBadRequest, types.ErrBadRequest)
}

func TestViewLengthCaps(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 2*viewLengthMax)
	fs := &fakeFS{body: body, entry: types.Entry{Name: "big.txt", Size: int64(len(body)), Mime: "text/plain"}}
	h := NewRouter(Deps{FS: fs})

	var got viewData
	getJSON(t, h, "/api/v1/fs/view?path=/r/big.txt", &got)
	if got.View.Length != viewLengthDefault {
		t.Errorf("default window = %d bytes, want %d", got.View.Length, viewLengthDefault)
	}
	if !got.View.Truncated {
		t.Errorf("a windowed read of a huge file must report truncated")
	}

	getJSON(t, h, "/api/v1/fs/view?path=/r/big.txt&length=99999999", &got)
	if got.View.Length != viewLengthMax {
		t.Errorf("clamped window = %d bytes, want %d", got.View.Length, viewLengthMax)
	}
}

func TestFsHex(t *testing.T) {
	fs := &fakeFS{body: []byte("Hello, hex!"), entry: types.Entry{Name: "a.bin", Size: 11}}
	h := NewRouter(Deps{FS: fs})

	var got hexData
	getJSON(t, h, "/api/v1/fs/hex?path=/r/a.bin", &got)
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(got.Rows))
	}
	if got.Rows[0].Offset != 0 || got.Rows[0].ASCII != "Hello, hex!" {
		t.Errorf("row = %+v", got.Rows[0])
	}
	if got.Rows[0].Hex != "48 65 6c 6c 6f 2c 20 68 65 78 21" {
		t.Errorf("hex = %q", got.Rows[0].Hex)
	}
	if got.Size != 11 || got.Truncated {
		t.Errorf("size/truncated = %d/%v", got.Size, got.Truncated)
	}

	// An empty window still serialises as [] so the SPA can iterate it.
	getJSON(t, h, "/api/v1/fs/hex?path=/r/a.bin&offset=500", &got)
	if got.Rows == nil {
		t.Errorf("rows must serialise as [] not null")
	}
}

func TestHexLengthCaps(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 4*hexLengthMax)
	fs := &fakeFS{body: body, entry: types.Entry{Name: "big.bin", Size: int64(len(body))}}
	h := NewRouter(Deps{FS: fs})

	var got hexData
	getJSON(t, h, "/api/v1/fs/hex?path=/r/big.bin", &got)
	if len(got.Rows) != hexLengthDefault/16 {
		t.Errorf("default hex window = %d rows, want %d", len(got.Rows), hexLengthDefault/16)
	}
	if !got.Truncated {
		t.Errorf("truncated must be true")
	}

	getJSON(t, h, "/api/v1/fs/hex?path=/r/big.bin&length=99999999", &got)
	if len(got.Rows) != hexLengthMax/16 {
		t.Errorf("clamped hex window = %d rows, want %d", len(got.Rows), hexLengthMax/16)
	}
}

// -------------------------------------------------------------- fs/raw

func TestFsRawStreamsWithRange(t *testing.T) {
	body := []byte("0123456789abcdefghij")
	fs := &fakeFS{body: body, entry: types.Entry{Name: "f.bin", Size: int64(len(body)), Mime: "application/octet-stream", Mtime: 1700000000}}
	h := NewRouter(Deps{FS: fs})

	rec := do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/f.bin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Errorf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	// application/octet-stream is not on the inline allowlist, so it is served
	// as an attachment even without download=1.
	if d := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(d, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment for a non-inline type", d)
	}
	// http.ServeContent advertises range support for seekable readers.
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes for a real file", ar)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fs/raw?path=/r/f.bin", nil)
	req.Header.Set("Range", "bytes=4-8")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("ranged status = %d, want 206", rec.Code)
	}
	if rec.Body.String() != "45678" {
		t.Errorf("ranged body = %q, want 45678", rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 4-8/20" {
		t.Errorf("Content-Range = %q", cr)
	}
}

func TestFsRawNonSeekableStream(t *testing.T) {
	body := []byte("inside the archive")
	arc := &fakeArchive{fakeFS: fakeFS{
		body:   body,
		stream: true,
		entry:  types.Entry{Name: "inner.txt", Size: int64(len(body)), Mime: "text/plain"},
	}}
	h := NewRouter(Deps{FS: &fakeFS{}, Archive: arc})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fs/raw?path=/r/a.zip!/inner.txt", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Archive entries are streams: the Range header is ignored, not honoured.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ranges are not supported here)", rec.Code)
	}
	if rec.Header().Get("Accept-Ranges") != "none" {
		t.Errorf("Accept-Ranges = %q, want none", rec.Header().Get("Accept-Ranges"))
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body = %q, want the whole entry", rec.Body.String())
	}
}

func TestFsRawDownloadDisposition(t *testing.T) {
	fs := &fakeFS{body: []byte("x"), entry: types.Entry{Name: "réport final.pdf", Size: 1, Mime: "application/pdf"}}
	h := NewRouter(Deps{FS: fs})

	rec := do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/x&download=1", nil)
	d := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(d, "attachment;") {
		t.Fatalf("Content-Disposition = %q, want an attachment", d)
	}
	if !strings.Contains(d, `filename="r_port final.pdf"`) {
		t.Errorf("Content-Disposition = %q, want an ASCII-safe filename", d)
	}
	if !strings.Contains(d, "filename*=UTF-8''r%C3%A9port%20final.pdf") {
		t.Errorf("Content-Disposition = %q, want the RFC 5987 form for the real name", d)
	}
}

// Raw errors are still envelopes (the SPA fetches this endpoint too).
func TestFsRawErrors(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{err: types.Errf(types.ErrForbidden, "outside roots")}})
	expectErr(t, h, http.MethodGet, "/api/v1/fs/raw?path=/etc/passwd", http.StatusForbidden, types.ErrForbidden)

	h = NewRouter(Deps{FS: &fakeFS{}})
	expectErr(t, h, http.MethodGet, "/api/v1/fs/raw", http.StatusBadRequest, types.ErrBadRequest)
	expectErr(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/x&download=perhaps",
		http.StatusBadRequest, types.ErrBadRequest)
}

func TestFsRawClosesTheReader(t *testing.T) {
	fs := &fakeFS{body: []byte("bytes"), entry: types.Entry{Name: "f", Size: 5}}
	h := NewRouter(Deps{FS: fs})
	do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/f", nil)

	fs.mu.Lock()
	rc := fs.lastOpen
	fs.mu.Unlock()
	if src, ok := rc.(*seekReadCloser); !ok || !src.closed {
		t.Errorf("fs/raw leaked the open file handle")
	}
}

// ------------------------------------------------- virtual vs real routing

func TestVirtualVersusRealRouting(t *testing.T) {
	real := &fakeFS{entries: []types.Entry{{Name: "real", Type: types.TypeFile}}, entry: types.Entry{Name: "real"}, body: []byte("real")}
	arc := &fakeArchive{fakeFS: fakeFS{
		entries: []types.Entry{{Name: "virtual", Type: types.TypeFile}},
		entry:   types.Entry{Name: "virtual"},
		body:    []byte("virtual"),
	}}
	h := NewRouter(Deps{FS: real, Archive: arc})

	var list listData
	getJSON(t, h, "/api/v1/fs/list?path=/mnt/user/a.zip!/dir", &list)
	if len(list.Entries) != 1 || list.Entries[0].Name != "virtual" {
		t.Errorf("virtual list did not reach the archive resolver: %v", names(list.Entries))
	}
	getJSON(t, h, "/api/v1/fs/list?path=/mnt/user/dir", &list)
	if len(list.Entries) != 1 || list.Entries[0].Name != "real" {
		t.Errorf("real list did not reach the filesystem: %v", names(list.Entries))
	}

	var st entryData
	getJSON(t, h, "/api/v1/fs/stat?path=/mnt/user/a.zip!/f", &st)
	if st.Entry.Name != "virtual" {
		t.Errorf("virtual stat routed to the filesystem")
	}
	getJSON(t, h, "/api/v1/fs/stat?path=/mnt/user/f", &st)
	if st.Entry.Name != "real" {
		t.Errorf("real stat routed to the archive resolver")
	}

	rec := do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/mnt/user/a.zip!/f", nil)
	if rec.Body.String() != "virtual" {
		t.Errorf("virtual raw body = %q", rec.Body.String())
	}

	if got := arc.calls(func(f *fakeFS) *[]string { return &f.listPaths }); len(got) != 1 {
		t.Errorf("archive List calls = %v, want exactly the virtual one", got)
	}
	if got := real.calls(func(f *fakeFS) *[]string { return &f.listPaths }); len(got) != 1 {
		t.Errorf("filesystem List calls = %v, want exactly the real one", got)
	}
	// The full virtual path is handed through untouched.
	if got := arc.calls(func(f *fakeFS) *[]string { return &f.statPaths }); len(got) != 1 || got[0] != "/mnt/user/a.zip!/f" {
		t.Errorf("archive received %v, want the verbatim virtual path", got)
	}
}

// Without an archive resolver wired, virtual paths simply go to the filesystem
// (which will reject them) rather than panicking.
func TestNoArchiveResolver(t *testing.T) {
	real := &fakeFS{err: types.Errf(types.ErrNotFound, "no such file")}
	h := NewRouter(Deps{FS: real})
	expectErr(t, h, http.MethodGet, "/api/v1/fs/list?path=/mnt/user/a.zip!/x",
		http.StatusNotFound, types.ErrNotFound)
}

// ------------------------------------------------------------- encodings

func TestEncodings(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{}})
	var got encodingsData
	getJSON(t, h, "/api/v1/encodings", &got)
	if len(got.Encodings) < 25 {
		t.Fatalf("got %d encodings, want the full picker list", len(got.Encodings))
	}
	if got.Encodings[0].ID != "auto" {
		t.Errorf("first picker entry = %q, want auto", got.Encodings[0].ID)
	}
	for _, e := range got.Encodings {
		if e.ID == "" || e.Label == "" {
			t.Errorf("encoding entry %+v is incomplete", e)
		}
	}
}

// ---------------------------------------------------------------- search

func TestSearch(t *testing.T) {
	idx := &fakeIndex{
		hits: []types.SearchHit{{
			Entry:     types.Entry{Name: "a.go", Path: "/r/a.go", Type: types.TypeFile},
			Score:     1.5,
			Snippet:   "func <mark>main</mark>()",
			MatchedIn: "content",
		}},
		total:  1,
		status: types.IndexStatus{State: "idle", LastFullScan: 1700000000},
	}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	var got searchData
	getJSON(t, h, "/api/v1/search?q=main&mode=content&path=/r&ext=go,.md&minSize=10&maxSize=99&after=5&before=9&limit=7&offset=3", &got)
	if len(got.Hits) != 1 || got.Total != 1 {
		t.Fatalf("hits/total = %d/%d", len(got.Hits), got.Total)
	}
	if got.Hits[0].Snippet != "func <mark>main</mark>()" {
		t.Errorf("snippet = %q", got.Hits[0].Snippet)
	}
	if !got.IndexFresh {
		t.Errorf("an idle index with a completed scan is fresh")
	}
	if got.TookMs < 0 {
		t.Errorf("tookMs = %d", got.TookMs)
	}

	q := idx.query()
	if q.Q != "main" || q.Mode != "content" || q.Path != "/r" {
		t.Errorf("query = %+v", q)
	}
	if !eq(q.Exts, []string{"go", "md"}) {
		t.Errorf("Exts = %v, want [go md] (dots and spaces stripped)", q.Exts)
	}
	if q.MinSize != 10 || q.MaxSize != 99 || q.After != 5 || q.Before != 9 {
		t.Errorf("filters = %+v", q)
	}
	if q.Limit != 7 || q.Offset != 3 {
		t.Errorf("limit/offset = %d/%d", q.Limit, q.Offset)
	}

	// Defaults.
	getJSON(t, h, "/api/v1/search?q=x", &got)
	q = idx.query()
	if q.Mode != "both" || q.Limit != searchLimitDefault || q.Offset != 0 {
		t.Errorf("defaults = mode %q limit %d offset %d", q.Mode, q.Limit, q.Offset)
	}
	if q.MinSize != -1 || q.MaxSize != -1 {
		t.Errorf("unset size filters must be -1, got %d/%d", q.MinSize, q.MaxSize)
	}

	// Cap.
	getJSON(t, h, "/api/v1/search?q=x&limit=50000", &got)
	if q := idx.query(); q.Limit != searchLimitMax {
		t.Errorf("clamped limit = %d, want %d", q.Limit, searchLimitMax)
	}

	// hits is never null.
	idx.hits = nil
	idx.total = 0
	getJSON(t, h, "/api/v1/search?q=nothing", &got)
	if got.Hits == nil {
		t.Errorf("hits must serialise as [] not null")
	}
}

func TestSearchValidation(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})
	for _, target := range []string{
		"/api/v1/search",
		"/api/v1/search?q=",
		"/api/v1/search?q=%20%20",
		"/api/v1/search?q=x&mode=telepathy",
		"/api/v1/search?q=x&limit=-1",
	} {
		t.Run(target, func(t *testing.T) {
			expectErr(t, h, http.MethodGet, target, http.StatusBadRequest, types.ErrBadRequest)
		})
	}
}

func TestSearchIndexErrorPropagates(t *testing.T) {
	idx := &fakeIndex{err: types.Errf(types.ErrIndexing, "still crawling")}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})
	expectErr(t, h, http.MethodGet, "/api/v1/search?q=x", http.StatusServiceUnavailable, types.ErrIndexing)
}

// ------------------------------------------------------------------ index

func TestIndexEndpointsWithoutAnIndex(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{}})
	cases := []struct{ method, target string }{
		{http.MethodGet, "/api/v1/search?q=x"},
		{http.MethodGet, "/api/v1/index/status"},
		{http.MethodGet, "/api/v1/index/config"},
		{http.MethodGet, "/api/v1/index/events"},
		{http.MethodPut, "/api/v1/index/config"},
		{http.MethodPost, "/api/v1/index/rescan"},
		{http.MethodPost, "/api/v1/index/pause"},
		{http.MethodPost, "/api/v1/index/resume"},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			expectErr(t, h, c.method, c.target, http.StatusServiceUnavailable, types.ErrIndexing)
		})
	}
}

func TestIndexStatus(t *testing.T) {
	idx := &fakeIndex{status: types.IndexStatus{
		State: "crawling", FilesIndexed: 42, ContentIndexed: 7,
		DBBytes: 1024, LastFullScan: 1700000000, Current: "/mnt/user/x", Progress: 0.5,
	}}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	var got types.IndexStatus
	getJSON(t, h, "/api/v1/index/status", &got)
	if got != idx.status {
		t.Errorf("status = %+v, want %+v", got, idx.status)
	}
}

func TestIndexConfigGetAndPut(t *testing.T) {
	idx := &fakeIndex{
		cfg:          types.IndexConfig{Roots: []string{"/mnt/user"}, Schedule: "0 3 * * *", Parallelism: 2},
		allowedRoots: []string{"/mnt/user", "/mnt/disks"},
	}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	var got configData
	getJSON(t, h, "/api/v1/index/config", &got)
	if len(got.Config.Roots) != 1 || got.Config.Roots[0] != "/mnt/user" {
		t.Errorf("config = %+v", got.Config)
	}
	if !eq(got.AllowedRoots, []string{"/mnt/user", "/mnt/disks"}) {
		t.Errorf("allowedRoots = %v, want the browse roots", got.AllowedRoots)
	}

	// Both the wrapped and the bare shape are accepted.
	for _, body := range []string{
		`{"config":{"roots":["/mnt/disk1"],"schedule":"@daily","parallelism":3,"content":{"enabled":true,"maxFileBytes":123}}}`,
		`{"roots":["/mnt/disk1"],"schedule":"@daily","parallelism":3,"content":{"enabled":true,"maxFileBytes":123}}`,
	} {
		idx.cfg = types.IndexConfig{}
		rec := do(t, h, http.MethodPut, "/api/v1/index/config", strings.NewReader(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT config = %d (%s)", rec.Code, rec.Body)
		}
		if len(idx.cfg.Roots) != 1 || idx.cfg.Roots[0] != "/mnt/disk1" || idx.cfg.Parallelism != 3 {
			t.Errorf("stored config = %+v (body %s)", idx.cfg, body)
		}
		if !idx.cfg.Content.Enabled || idx.cfg.Content.MaxFileBytes != 123 {
			t.Errorf("content rules = %+v", idx.cfg.Content)
		}
		// PUT answers with the same shape as GET, allowedRoots included.
		var put envelopeOut
		if err := json.Unmarshal(rec.Body.Bytes(), &put); err != nil {
			t.Fatalf("PUT body is not JSON: %v", err)
		}
		var back configData
		if err := json.Unmarshal(put.Data, &back); err != nil {
			t.Fatalf("PUT data: %v", err)
		}
		if !eq(back.AllowedRoots, []string{"/mnt/user", "/mnt/disks"}) {
			t.Errorf("PUT allowedRoots = %v", back.AllowedRoots)
		}
	}

	// Bad bodies.
	for _, body := range []string{"", "not json", "[1,2,3]"} {
		rec := do(t, h, http.MethodPut, "/api/v1/index/config", strings.NewReader(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT config %q = %d, want 400", body, rec.Code)
		}
	}

	// A rejected config surfaces the index's own error code.
	idx.setErr = types.Errf(types.ErrBadRequest, "root must be absolute")
	rec := do(t, h, http.MethodPut, "/api/v1/index/config", strings.NewReader(`{"roots":["relative"]}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("rejected config = %d, want 400", rec.Code)
	}
}

func TestIndexRescanPauseResume(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	if rec := do(t, h, http.MethodPost, "/api/v1/index/rescan", strings.NewReader(`{"path":"/mnt/user/sub"}`)); rec.Code != http.StatusOK {
		t.Fatalf("rescan = %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/index/rescan?path=/mnt/user/query", nil); rec.Code != http.StatusOK {
		t.Fatalf("rescan via query = %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/index/rescan", nil); rec.Code != http.StatusOK {
		t.Fatalf("full rescan = %d (%s)", rec.Code, rec.Body)
	}
	idx.mu.Lock()
	got := append([]string(nil), idx.rescanPaths...)
	idx.mu.Unlock()
	if !eq(got, []string{"/mnt/user/sub", "/mnt/user/query", ""}) {
		t.Errorf("rescan paths = %v", got)
	}

	if rec := do(t, h, http.MethodPost, "/api/v1/index/pause", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause = %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/index/resume", nil); rec.Code != http.StatusOK {
		t.Fatalf("resume = %d", rec.Code)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.paused != 1 || idx.resumed != 1 {
		t.Errorf("pause/resume = %d/%d", idx.paused, idx.resumed)
	}
}

// -------------------------------------------------------------------- SSE

func TestIndexEventsStream(t *testing.T) {
	idx := &fakeIndex{status: types.IndexStatus{State: "crawling", FilesIndexed: 1}}
	srv := httptest.NewServer(NewRouter(Deps{FS: &fakeFS{}, Index: idx}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/index/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("SSE must not be cached")
	}

	// The stream opens with the current status, flushed immediately — a client
	// that connects to an idle daemon must not block waiting for a change.
	first := readSSEEvent(t, resp.Body)
	var st types.IndexStatus
	if err := json.Unmarshal([]byte(first), &st); err != nil {
		t.Fatalf("event %q is not an IndexStatus: %v", first, err)
	}
	if st.State != "crawling" || st.FilesIndexed != 1 {
		t.Errorf("first event = %+v, want the current status", st)
	}

	// Disconnecting must release the subscription.
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for idx.unsubscribes() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if idx.unsubscribes() == 0 {
		t.Errorf("client disconnect did not unsubscribe from the index")
	}
}

// readSSEEvent reads one "data: ...\n\n" frame and returns its payload.
func readSSEEvent(t *testing.T, r io.Reader) string {
	t.Helper()
	buf := make([]byte, 1)
	var line strings.Builder
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				s := line.String()
				if strings.HasPrefix(s, "data: ") {
					return strings.TrimPrefix(s, "data: ")
				}
				line.Reset()
				continue
			}
			line.WriteByte(buf[0])
		}
		if err != nil {
			t.Fatalf("reading SSE stream: %v (partial %q)", err, line.String())
		}
	}
}

// ---------------------------------------------------------------- healthz

func TestHealthz(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{}, Version: "1.2.3", Roots: []string{"/mnt/user", "/mnt/disks"}})
	var got healthData
	getJSON(t, h, "/api/v1/healthz", &got)
	if !eq(got.Roots, []string{"/mnt/user", "/mnt/disks"}) {
		t.Errorf("roots = %v, want the daemon's browse roots", got.Roots)
	}
	if got.Version != "1.2.3" {
		t.Errorf("version = %q", got.Version)
	}
	if got.IndexDB != "missing" {
		t.Errorf("indexDb = %q, want missing when no index is wired", got.IndexDB)
	}
	if got.UptimeSec < 0 {
		t.Errorf("uptimeSec = %d", got.UptimeSec)
	}

	h = NewRouter(Deps{FS: &fakeFS{}, Index: &fakeIndex{status: types.IndexStatus{DBBytes: 4096}}, Version: "9"})
	getJSON(t, h, "/api/v1/healthz", &got)
	if got.IndexDB != "ok" {
		t.Errorf("indexDb = %q, want ok when the database has content", got.IndexDB)
	}
}

// ------------------------------------------------------------------- CORS

func TestDevModeCORS(t *testing.T) {
	dev := NewRouter(Deps{FS: &fakeFS{entries: listingFixture()}, DevMode: true})
	rec := do(t, dev, http.MethodGet, "/api/v1/fs/list?path=/r", nil)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("dev mode Allow-Origin = %q, want *", got)
	}

	rec = do(t, dev, http.MethodOptions, "/api/v1/fs/list", nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight = %d, want 204", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "PUT") {
		t.Errorf("preflight Allow-Methods = %q", rec.Header().Get("Access-Control-Allow-Methods"))
	}

	prod := NewRouter(Deps{FS: &fakeFS{entries: listingFixture()}})
	rec = do(t, prod, http.MethodGet, "/api/v1/fs/list?path=/r", nil)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("production must not send CORS headers, got %q", got)
	}
}

// -------------------------------------------------------------- misc guards

// A missing filesystem must not panic the daemon.
func TestNoFileSystemWired(t *testing.T) {
	h := NewRouter(Deps{})
	expectErr(t, h, http.MethodGet, "/api/v1/fs/list?path=/r",
		http.StatusInternalServerError, types.ErrInternal)
	expectErr(t, h, http.MethodGet, "/api/v1/fs/stat?path=/r",
		http.StatusInternalServerError, types.ErrInternal)
	expectErr(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r",
		http.StatusInternalServerError, types.ErrInternal)
}

// Every endpoint lives under the contract's base path.
func TestAllEndpointsAreUnderApiV1(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{entries: listingFixture()}, Index: &fakeIndex{}})
	for _, p := range []string{"/fs/list", "/fs/stat", "/encodings", "/healthz", "/index/status"} {
		expectErr(t, h, http.MethodGet, p, http.StatusNotFound, types.ErrNotFound)
	}
	getJSON(t, h, "/api/v1/encodings", nil)
}

// ------------------------------------------------- fs/raw content policy

// rawSecurityHeaders must be on every raw response, inline or attachment: the
// SPA shares the Unraid webGUI origin, so anything that renders here renders
// with the admin (root) session.
func assertRawSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Errorf("Content-Security-Policy = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q, want private, no-store", got)
	}
}

func TestFsRawInlineAllowlist(t *testing.T) {
	inline := []struct{ name, mime string }{
		{"a.png", "image/png"},
		{"a.jpg", "image/jpeg"},
		{"a.gif", "image/gif"},
		{"a.webp", "image/webp"},
		{"a.bmp", "image/bmp"},
		{"a.avif", "image/avif"},
		{"a.pdf", "application/pdf"},
		{"a.mp3", "audio/mpeg"},
		{"a.mp4", "video/mp4"},
		{"a.txt", "text/plain; charset=utf-8"},
		{"a.txt", "TEXT/PLAIN"}, // matching is case-insensitive
	}
	for _, tc := range inline {
		fs := &fakeFS{body: []byte("payload"), entry: types.Entry{Name: tc.name, Size: 7, Mime: tc.mime}}
		h := NewRouter(Deps{FS: fs})
		rec := do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/"+tc.name, nil)
		if ct := rec.Header().Get("Content-Type"); ct != tc.mime {
			t.Errorf("%s: Content-Type = %q, want the real type %q", tc.mime, ct, tc.mime)
		}
		if d := rec.Header().Get("Content-Disposition"); d != "" {
			t.Errorf("%s: renderable types stay inline, got %q", tc.mime, d)
		}
		assertRawSecurityHeaders(t, rec)
	}
}

// Anything that a browser could execute on this origin — or anything we cannot
// classify — is neutralised: octet-stream plus an attachment disposition.
func TestFsRawNeutralisesScriptableTypes(t *testing.T) {
	dangerous := []struct{ name, mime string }{
		{"evil.html", "text/html; charset=utf-8"},
		{"evil.htm", "text/html"},
		{"evil.svg", "image/svg+xml"},
		{"evil.xml", "application/xml"},
		{"evil.xml", "text/xml"},
		{"evil.xhtml", "application/xhtml+xml"},
		{"evil.js", "text/javascript"},
		{"evil.js", "application/javascript"},
		{"evil.css", "text/css"},
		{"evil.json", "application/json"},
		// Extensionless file whose bytes sniffed as HTML in fsops.
		{"README", "text/html; charset=utf-8"},
		// Unknown / missing type.
		{"mystery", ""},
		{"mystery", "application/x-whatever"},
	}
	for _, tc := range dangerous {
		fs := &fakeFS{body: []byte("<script>alert(1)</script>"), entry: types.Entry{Name: tc.name, Size: 25, Mime: tc.mime}}
		h := NewRouter(Deps{FS: fs})
		rec := do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/"+tc.name, nil)
		if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("%s (%s): Content-Type = %q, want application/octet-stream", tc.name, tc.mime, ct)
		}
		d := rec.Header().Get("Content-Disposition")
		if !strings.HasPrefix(d, "attachment;") {
			t.Errorf("%s (%s): Content-Disposition = %q, want an attachment", tc.name, tc.mime, d)
		}
		if !strings.Contains(d, `filename="`+tc.name+`"`) {
			t.Errorf("%s: disposition %q lost the sanitised filename", tc.name, d)
		}
		assertRawSecurityHeaders(t, rec)
		if rec.Body.String() != "<script>alert(1)</script>" {
			t.Errorf("%s: body was altered", tc.name)
		}
	}
}

// The same policy applies to files reached through a virtual (archive) path —
// planting an .html inside a zip must not buy an attacker an inline render.
func TestFsRawPolicyAppliesInsideArchives(t *testing.T) {
	arc := &fakeArchive{fakeFS: fakeFS{
		body:   []byte("<html>x</html>"),
		stream: true,
		entry:  types.Entry{Name: "payload.html", Size: 14, Mime: "text/html"},
	}}
	h := NewRouter(Deps{FS: &fakeFS{}, Archive: arc})
	rec := do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/a.zip!/payload.html", nil)
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("virtual Content-Type = %q", ct)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment;") {
		t.Errorf("virtual html was not forced to attachment")
	}
	assertRawSecurityHeaders(t, rec)
}

func TestFsRawDownloadForcesAttachmentOnInlineTypes(t *testing.T) {
	fs := &fakeFS{body: []byte("\x89PNG"), entry: types.Entry{Name: "photo.png", Size: 4, Mime: "image/png"}}
	h := NewRouter(Deps{FS: fs})

	rec := do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/photo.png", nil)
	if rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("png without download=1 must stay inline")
	}

	rec = do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/photo.png&download=1", nil)
	if d := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(d, "attachment;") {
		t.Errorf("download=1 on a png: Content-Disposition = %q, want an attachment", d)
	}
	// The type stays honest — the attachment disposition is what stops render.
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("download=1 Content-Type = %q, want image/png", ct)
	}
	assertRawSecurityHeaders(t, rec)
}

// Range serving must survive the header policy.
func TestFsRawRangeStillWorksUnderThePolicy(t *testing.T) {
	body := []byte("0123456789")
	fs := &fakeFS{body: body, entry: types.Entry{Name: "a.png", Size: 10, Mime: "image/png"}}
	h := NewRouter(Deps{FS: fs})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fs/raw?path=/r/a.png", nil)
	req.Header.Set("Range", "bytes=2-4")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "234" {
		t.Fatalf("range = %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("ranged Content-Type = %q", ct)
	}
	assertRawSecurityHeaders(t, rec)
}

func TestRawContentTypeUnit(t *testing.T) {
	cases := []struct {
		mime   string
		wantCT string
		inline bool
	}{
		{"image/png", "image/png", true},
		{" Image/PNG ", " Image/PNG ", true},
		{"audio/", "application/octet-stream", false}, // bare prefix is not a type
		{"video/x-matroska", "video/x-matroska", true},
		{"text/html", "application/octet-stream", false},
		{"image/svg+xml", "application/octet-stream", false},
		{"", "application/octet-stream", false},
	}
	for _, c := range cases {
		ct, inline := rawContentType(c.mime)
		if ct != c.wantCT || inline != c.inline {
			t.Errorf("rawContentType(%q) = (%q, %v), want (%q, %v)", c.mime, ct, inline, c.wantCT, c.inline)
		}
	}
}

// ----------------------------------------------------- listing size cap

func TestFsListRefusesOversizedDirectories(t *testing.T) {
	huge := make([]types.Entry, maxListEntries+1)
	for i := range huge {
		huge[i] = types.Entry{Name: "f", Type: types.TypeFile}
	}
	h := NewRouter(Deps{FS: &fakeFS{entries: huge}})
	e := expectErr(t, h, http.MethodGet, "/api/v1/fs/list?path=/r&limit=1",
		http.StatusRequestEntityTooLarge, types.ErrTooLarge)
	if !strings.Contains(e.Message, "narrow with search") {
		t.Errorf("message = %q, want actionable advice", e.Message)
	}

	// One under the cap is still served.
	ok := NewRouter(Deps{FS: &fakeFS{entries: huge[:maxListEntries]}})
	var list listData
	getJSON(t, ok, "/api/v1/fs/list?path=/r&limit=1", &list)
	if list.Total != maxListEntries || len(list.Entries) != 1 {
		t.Errorf("total = %d, page = %d", list.Total, len(list.Entries))
	}
}

// ------------------------------------------------- concurrency limiter

// blockingFS holds every List until the test releases it.
type blockingFS struct {
	fakeFS
	entered chan struct{}
	release chan struct{}
}

func (b *blockingFS) List(ctx context.Context, path string) ([]types.Entry, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}

func TestExpensiveEndpointsAreConcurrencyLimited(t *testing.T) {
	fs := &blockingFS{entered: make(chan struct{}, 1), release: make(chan struct{})}
	s := newServer(Deps{FS: fs, Index: &fakeIndex{}})
	s.busy = newLimiter(1, 20*time.Millisecond)
	h := newRouter(s)

	done := make(chan struct{})
	go func() {
		defer close(done)
		do(t, h, http.MethodGet, "/api/v1/fs/list?path=/r", nil)
	}()
	<-fs.entered // the single slot is now held

	// Everything expensive queues behind it and gives up honestly.
	for _, target := range []string{"/api/v1/fs/list?path=/r2", "/api/v1/search?q=x"} {
		e := expectErr(t, h, http.MethodGet, target, http.StatusGatewayTimeout, types.ErrTimeout)
		if e.Message != "server busy" {
			t.Errorf("%s: message = %q, want %q", target, e.Message, "server busy")
		}
	}

	close(fs.release)
	<-done

	// With the slot free again the endpoint works.
	var list listData
	getJSON(t, h, "/api/v1/fs/list?path=/r", &list)
}

// fs/view inside an archive costs an extraction, so it shares the budget; a
// view of a real file does not.
func TestVirtualViewIsLimitedButRealViewIsNot(t *testing.T) {
	arc := &fakeArchive{fakeFS: fakeFS{body: []byte("hello"), entry: types.Entry{Name: "in.txt", Size: 5}}}
	s := newServer(Deps{FS: &fakeFS{body: []byte("hello"), entry: types.Entry{Name: "f.txt", Size: 5}}, Archive: arc})
	s.busy = newLimiter(1, 20*time.Millisecond)
	h := newRouter(s)

	// Occupy the only slot for the duration of the checks.
	release, err := s.busy.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expectErr(t, h, http.MethodGet, "/api/v1/fs/view?path=/r/a.zip!/in.txt",
		http.StatusGatewayTimeout, types.ErrTimeout)

	var v viewData
	getJSON(t, h, "/api/v1/fs/view?path=/r/f.txt", &v)
	release()
}

// ------------------------------------------------------- request deadlines

// deadlineFS records whether the context it was handed carries a deadline.
type deadlineFS struct {
	fakeFS
	mu       sync.Mutex
	listDone bool
	openDone bool
}

func (d *deadlineFS) List(ctx context.Context, path string) ([]types.Entry, error) {
	_, ok := ctx.Deadline()
	d.mu.Lock()
	d.listDone = ok
	d.mu.Unlock()
	return nil, nil
}

func (d *deadlineFS) Open(ctx context.Context, path string) (io.ReadCloser, types.Entry, error) {
	_, ok := ctx.Deadline()
	d.mu.Lock()
	d.openDone = ok
	d.mu.Unlock()
	return d.fakeFS.Open(ctx, path)
}

// A multi-gigabyte download must not be killed by the 30s request timeout, so
// fs/raw is routed without wrap(). Everything else keeps the deadline.
func TestStreamingEndpointsSkipTheRequestTimeout(t *testing.T) {
	fs := &deadlineFS{fakeFS: fakeFS{body: []byte("x"), entry: types.Entry{Name: "big.bin", Size: 1}}}
	h := NewRouter(Deps{FS: fs})

	do(t, h, http.MethodGet, "/api/v1/fs/list?path=/r", nil)
	do(t, h, http.MethodGet, "/api/v1/fs/raw?path=/r/big.bin", nil)

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.listDone {
		t.Errorf("fs/list lost the %s request timeout", requestTimeout)
	}
	if fs.openDone {
		t.Errorf("fs/raw inherited a request deadline; big downloads would be cut off")
	}
}

// ------------------------------------------------------------ body limits

func TestOversizedRequestBodiesAreTooLarge(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	big := strings.NewReader(`{"schedule":"` + strings.Repeat("a", maxBodyBytes+64) + `"}`)
	rec := do(t, h, http.MethodPut, "/api/v1/index/config", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT oversized config = %d, want 413 (body %s)", rec.Code, rec.Body)
	}
	var env envelopeOut
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error == nil {
		t.Fatalf("oversized body did not produce an error envelope: %s", rec.Body)
	}
	if env.Error.Code != types.ErrTooLarge {
		t.Errorf("code = %q, want %q", env.Error.Code, types.ErrTooLarge)
	}

	rec = do(t, h, http.MethodPost, "/api/v1/index/rescan",
		strings.NewReader(`{"path":"`+strings.Repeat("b", maxBodyBytes)+`"}`))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("POST oversized rescan = %d, want 413", rec.Code)
	}
}

// ------------------------------------------------------------------ SSE

// sseWriter is a ResponseWriter that supports the streaming controls the SSE
// handler relies on, and records that they were used.
type sseWriter struct {
	mu        sync.Mutex
	hdr       http.Header
	buf       bytes.Buffer
	code      int
	deadlines int
	flushes   int
	writeErr  error
}

func newSSEWriter() *sseWriter { return &sseWriter{hdr: make(http.Header)} }

func (s *sseWriter) Header() http.Header { return s.hdr }

func (s *sseWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.buf.Write(p)
}

func (s *sseWriter) WriteHeader(code int) {
	s.mu.Lock()
	s.code = code
	s.mu.Unlock()
}

func (s *sseWriter) Flush() {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
}

func (s *sseWriter) SetWriteDeadline(time.Time) error {
	s.mu.Lock()
	s.deadlines++
	s.mu.Unlock()
	return nil
}

func (s *sseWriter) snapshot() (body string, deadlines int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String(), s.deadlines
}

func (s *sseWriter) fail(err error) {
	s.mu.Lock()
	s.writeErr = err
	s.mu.Unlock()
}

// The stream must put bytes on the wire even when the index status never
// changes: that is how a closed browser becomes visible to the daemon and to
// the PHP bridge. Each write arms a deadline first, so a client that stops
// reading cannot pin this goroutine (the server has no WriteTimeout).
func TestSSEHeartbeatAndWriteDeadline(t *testing.T) {
	idx := &fakeIndex{status: types.IndexStatus{State: "idle"}}
	s := newServer(Deps{FS: &fakeFS{}, Index: idx})
	s.sseHeartbeat = 10 * time.Millisecond
	w := newSSEWriter()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/index/events", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() { defer close(done); s.indexEvents(w, req) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if body, _ := w.snapshot(); strings.Contains(body, ": keepalive\n\n") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	body, deadlines := w.snapshot()
	if !strings.Contains(body, ": keepalive\n\n") {
		t.Fatalf("no heartbeat comment on a silent stream: %q", body)
	}
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("stream must open with the current status: %q", body)
	}
	if deadlines < 2 {
		t.Errorf("SetWriteDeadline called %d times, want one per write", deadlines)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client went away")
	}
	if idx.unsubscribes() != 1 {
		t.Errorf("unsubscribes = %d, want 1", idx.unsubscribes())
	}
}

// A write error (which is what a blown write deadline looks like) must end the
// stream rather than spin.
func TestSSEStopsOnWriteError(t *testing.T) {
	idx := &fakeIndex{status: types.IndexStatus{State: "idle"}}
	s := newServer(Deps{FS: &fakeFS{}, Index: idx})
	s.sseHeartbeat = 5 * time.Millisecond
	w := newSSEWriter()
	w.fail(errors.New("i/o timeout"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/index/events", nil)
	done := make(chan struct{})
	go func() { defer close(done); s.indexEvents(w, req) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a wedged client did not release the SSE goroutine")
	}
	if got := s.sseSubs.Load(); got != 0 {
		t.Errorf("subscriber slot leaked: %d", got)
	}
}

func TestSSESubscriberCap(t *testing.T) {
	idx := &fakeIndex{status: types.IndexStatus{State: "idle"}}
	s := newServer(Deps{FS: &fakeFS{}, Index: idx})
	s.sseMax = 2
	s.sseHeartbeat = time.Hour // no heartbeats during this test
	h := newRouter(s)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < s.sseMax; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/index/events", nil).WithContext(ctx)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.sseSubs.Load() < int64(s.sseMax) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	e := expectErr(t, h, http.MethodGet, "/api/v1/index/events",
		http.StatusServiceUnavailable, types.ErrIndexing)
	if !strings.Contains(e.Message, "too many") {
		t.Errorf("message = %q, want it to name the cap", e.Message)
	}

	cancel()
	wg.Wait()
	if got := s.sseSubs.Load(); got != 0 {
		t.Errorf("subscriber slots leaked: %d", got)
	}

	// Slots are reusable once the streams end.
	if !s.sseAdmit() {
		t.Errorf("cap did not release after disconnects")
	}
	s.sseRelease()
}

// A value json cannot represent used to be written after WriteHeader(200),
// leaving the client with an empty 200 body and nothing in the log — the
// "unexpected response shape (HTTP 200)" a user reported from the field.
func TestWriteOKUnencodableBecomesLoggedError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOK(rec, map[string]any{"fps": math.NaN()})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not an envelope: %v (%q)", err, rec.Body.String())
	}
	if env.OK || env.Error.Code != types.ErrInternal {
		t.Fatalf("envelope = %+v, want ok:false INTERNAL", env)
	}
}
