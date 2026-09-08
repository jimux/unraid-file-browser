package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"unraid-filebrowser/internal/types"
)

// ------------------------------------------------------ meta filter parsing

// Every operator must survive the trip from the URL to the SearchQuery
// unchanged — including the two-character ones, which a naive scanner splits
// into a one-character operator and a value beginning with "=".
func TestMetaFilterParsingOperators(t *testing.T) {
	cases := []struct {
		in            string
		key, op, want string
	}{
		{"image.cameraModel=DMC-GH5", "image.cameraModel", "=", "DMC-GH5"},
		{"image.iso>=800", "image.iso", ">=", "800"},
		{"image.iso<=800", "image.iso", "<=", "800"},
		{"image.iso!=800", "image.iso", "!=", "800"},
		{"image.iso>800", "image.iso", ">", "800"},
		{"image.iso<800", "image.iso", "<", "800"},
		{"audio.artist~davis", "audio.artist", "~", "davis"},
		{"video.hdr=hdr10", "video.hdr", "=", "hdr10"},
		// A value may contain any of the operator characters: only the first
		// operator after the key separates the two.
		{"audio.title=a=b", "audio.title", "=", "a=b"},
		{"audio.title=>=x", "audio.title", "=", ">=x"},
		{"audio.title~a~b", "audio.title", "~", "a~b"},
		{"image.iso>=>=1", "image.iso", ">=", ">=1"},
		// Surrounding whitespace is not part of the filter.
		{"  audio.artist = Miles Davis  ", "audio.artist", "=", "Miles Davis"},
		// Unicode values pass through byte-for-byte.
		{"audio.artist=Björk", "audio.artist", "=", "Björk"},
		{"image.cameraModel=キヤノン", "image.cameraModel", "=", "キヤノン"},
		{"audio.title~💾", "audio.title", "~", "💾"},
		// A single-letter category and field is still the convention.
		{"a.B=1", "a.B", "=", "1"},
		// The catalog's "common" category uses bare keys: the columns every
		// indexed file has rather than extracted metadata.
		{"size>=21474836480", "size", ">=", "21474836480"},
		{"ext=mkv", "ext", "=", "mkv"},
		{"mtime<1700000000", "mtime", "<", "1700000000"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseMetaFilter(c.in)
			if err != nil {
				t.Fatalf("parseMetaFilter(%q) = %v", c.in, err)
			}
			if got.Key != c.key || got.Op != c.op || got.Value != c.want {
				t.Errorf("parseMetaFilter(%q) = %+v, want {%s %s %s}", c.in, got, c.key, c.op, c.want)
			}
		})
	}
}

// Anything that is not a well-formed clause is a BAD_REQUEST that names the
// offending item — never a filter silently dropped or forwarded half-parsed.
func TestMetaFilterParsingRejections(t *testing.T) {
	for _, in := range []string{
		"",                       // meta=
		"   ",                    // whitespace only
		"foo",                    // no operator
		"a.b?c",                  // "?" is not an operator
		"image.iso",              // key alone
		"image.iso=",             // empty value
		"image.iso~   ",          // whitespace-only value
		"=800",                   // no key
		"Image.iso=1",            // uppercase category
		"image.cam era=1",        // space inside the key
		"image..iso=1",           // empty field
		"image.iso.speed=1",      // three segments
		"image.iso9=1",           // digit in the field
		"im age.iso=1",           // space inside the category
		"../../etc/passwd=1",     // path traversal shaped input
		"'; DROP TABLE files;--", // no operator, so no key either
	} {
		t.Run(strconv.Quote(in), func(t *testing.T) {
			if got, err := parseMetaFilter(in); err == nil {
				t.Fatalf("parseMetaFilter(%q) = %+v, want an error", in, got)
			} else if !strings.Contains(err.Error(), "meta filter") {
				t.Errorf("error does not name the parameter: %v", err)
			}
		})
	}
}

// The message quotes the offending item, but a pathological parameter must not
// produce a pathological response.
func TestMetaFilterErrorEchoIsBounded(t *testing.T) {
	long := strings.Repeat("x", 5000)
	_, err := parseMetaFilter(long)
	if err == nil {
		t.Fatal("want an error")
	}
	if len(err.Error()) > 300 {
		t.Errorf("error message is %d bytes; the echoed item must be truncated", len(err.Error()))
	}
}

func TestMetaFilterCount(t *testing.T) {
	q := url.Values{}
	for i := 0; i < maxMetaFilters; i++ {
		q.Add("meta", "image.iso="+strconv.Itoa(i))
	}
	got, err := parseMetaFilters(q)
	if err != nil {
		t.Fatalf("%d filters must be accepted: %v", maxMetaFilters, err)
	}
	if len(got) != maxMetaFilters {
		t.Fatalf("got %d filters, want %d", len(got), maxMetaFilters)
	}

	q.Add("meta", "image.iso=one-too-many")
	if _, err := parseMetaFilters(q); err == nil {
		t.Fatalf("%d filters must be refused", maxMetaFilters+1)
	}

	// No meta at all is not an error and is not an empty slice either.
	if got, err := parseMetaFilters(url.Values{}); err != nil || got != nil {
		t.Errorf("no meta = %v, %v; want nil, nil", got, err)
	}
}

// --------------------------------------------------------- /search plumbing

func TestSearchForwardsMetaSortAndDir(t *testing.T) {
	idx := &fakeIndex{status: types.IndexStatus{State: "idle", LastFullScan: 1}}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	target := "/api/v1/search?q=x" +
		"&meta=" + url.QueryEscape("image.cameraModel=DMC-GH5") +
		"&meta=" + url.QueryEscape("image.iso>=800") +
		"&meta=" + url.QueryEscape("video.hdr=hdr10") +
		"&meta=" + url.QueryEscape("audio.artist~Miles Davis") +
		"&sort=size&dir=desc"
	var got searchData
	getJSON(t, h, target, &got)

	q := idx.query()
	want := []types.MetaFilter{
		{Key: "image.cameraModel", Op: "=", Value: "DMC-GH5"},
		{Key: "image.iso", Op: ">=", Value: "800"},
		{Key: "video.hdr", Op: "=", Value: "hdr10"},
		{Key: "audio.artist", Op: "~", Value: "Miles Davis"},
	}
	if len(q.Meta) != len(want) {
		t.Fatalf("Meta = %+v, want %d filters", q.Meta, len(want))
	}
	for i := range want {
		if q.Meta[i] != want[i] {
			t.Errorf("Meta[%d] = %+v, want %+v", i, q.Meta[i], want[i])
		}
	}
	if q.Sort != "size" || q.Dir != "desc" {
		t.Errorf("sort/dir = %q/%q", q.Sort, q.Dir)
	}
}

// net/http has already percent-decoded the parameter; decoding again would
// turn a literal "%2F" in a filename into a slash.
func TestSearchDoesNotDoubleDecodeMetaValues(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	var got searchData
	// The escaped form on the wire is meta=audio.title%3D100%252Fmix%2B2: one
	// decode yields the literal "100%2Fmix+2", a second would yield "100/mix 2".
	getJSON(t, h, "/api/v1/search?q=x&meta="+url.QueryEscape("audio.title=100%2Fmix+2"), &got)
	if q := idx.query(); len(q.Meta) != 1 || q.Meta[0].Value != "100%2Fmix+2" {
		t.Errorf("Meta = %+v, want the value decoded exactly once", q.Meta)
	}

	// And the single decode that net/http does perform is not undone: a raw
	// "+" in the query string is a space by the time we see it.
	getJSON(t, h, "/api/v1/search?q=x&meta=audio.title%3Dmiles+davis", &got)
	if q := idx.query(); len(q.Meta) != 1 || q.Meta[0].Value != "miles davis" {
		t.Errorf("Meta = %+v, want the value decoded exactly once", q.Meta)
	}
}

// Unset sort/dir are passed through empty: the index owns the default.
func TestSearchLeavesSortUnsetByDefault(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	var got searchData
	getJSON(t, h, "/api/v1/search?q=x", &got)
	if q := idx.query(); q.Sort != "" || q.Dir != "" {
		t.Errorf("sort/dir = %q/%q, want both empty so the index applies its own default", q.Sort, q.Dir)
	}
}

func TestSearchSortAndDirValidation(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	for _, sortKey := range []string{"relevance", "name", "size", "mtime", "SIZE", " mtime "} {
		var got searchData
		getJSON(t, h, "/api/v1/search?q=x&sort="+url.QueryEscape(sortKey), &got)
		if q := idx.query(); q.Sort != strings.ToLower(strings.TrimSpace(sortKey)) {
			t.Errorf("sort=%q forwarded as %q", sortKey, q.Sort)
		}
	}
	for _, dir := range []string{"asc", "desc", "ASC"} {
		var got searchData
		getJSON(t, h, "/api/v1/search?q=x&dir="+dir, &got)
		if q := idx.query(); q.Dir != strings.ToLower(dir) {
			t.Errorf("dir=%q forwarded as %q", dir, q.Dir)
		}
	}
	for _, target := range []string{
		"/api/v1/search?q=x&sort=type",     // valid for fs/list, not for search
		"/api/v1/search?q=x&sort=dirsized", // nonsense
		"/api/v1/search?q=x&dir=sideways",
		"/api/v1/search?q=x&dir=descending",
	} {
		t.Run(target, func(t *testing.T) {
			expectErr(t, h, http.MethodGet, target, http.StatusBadRequest, types.ErrBadRequest)
		})
	}
}

// The owner's "files bigger than 20 GB" case: a filter is a query.
func TestSearchQueryIsOptionalWhenFiltered(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	cases := []struct {
		name, target string
		check        func(types.SearchQuery) bool
	}{
		{"minSize", "/api/v1/search?minSize=21474836480", func(q types.SearchQuery) bool { return q.MinSize == 21474836480 }},
		{"maxSize", "/api/v1/search?maxSize=1024", func(q types.SearchQuery) bool { return q.MaxSize == 1024 }},
		{"path", "/api/v1/search?path=/mnt/user/media", func(q types.SearchQuery) bool { return q.Path == "/mnt/user/media" }},
		{"ext", "/api/v1/search?ext=mkv,.mp4", func(q types.SearchQuery) bool { return len(q.Exts) == 2 }},
		{"after", "/api/v1/search?after=1700000000", func(q types.SearchQuery) bool { return q.After == 1700000000 }},
		{"before", "/api/v1/search?before=1700000000", func(q types.SearchQuery) bool { return q.Before == 1700000000 }},
		{"meta", "/api/v1/search?meta=" + url.QueryEscape("image.iso>=800"), func(q types.SearchQuery) bool { return len(q.Meta) == 1 }},
		{"blank q plus a filter", "/api/v1/search?q=%20&meta=" + url.QueryEscape("video.hdr=hdr10"), func(q types.SearchQuery) bool {
			return q.Q == "" && len(q.Meta) == 1
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got searchData
			getJSON(t, h, c.target, &got)
			q := idx.query()
			if q.Q != "" {
				t.Errorf("Q = %q, want empty", q.Q)
			}
			if !c.check(q) {
				t.Errorf("filter did not reach the index: %+v", q)
			}
		})
	}
}

// Neither text nor a filter is not a search.
func TestSearchWithNeitherQueryNorFilterIsRejected(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})
	for _, target := range []string{
		"/api/v1/search",
		"/api/v1/search?q=",
		"/api/v1/search?q=%20%20",
		"/api/v1/search?limit=10&offset=0",
		"/api/v1/search?q=&mode=name&sort=name&dir=asc",
		"/api/v1/search?path=%20", // a blank scope is not a filter
		"/api/v1/search?ext=",     // nor is an empty extension list
		"/api/v1/search?ext=,,",
	} {
		t.Run(target, func(t *testing.T) {
			e := expectErr(t, h, http.MethodGet, target, http.StatusBadRequest, types.ErrBadRequest)
			if !strings.Contains(e.Message, "filter") {
				t.Errorf("message = %q; it should explain that a filter can stand in for q", e.Message)
			}
		})
	}
}

func TestSearchMetaValidationOverTheWire(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	targets := []string{
		"/api/v1/search?q=x&meta=",
		"/api/v1/search?q=x&meta=foo",
		"/api/v1/search?q=x&meta=" + url.QueryEscape("a.b?c"),
		"/api/v1/search?q=x&meta=" + url.QueryEscape("image.iso="),
		"/api/v1/search?q=x&meta=" + url.QueryEscape("IMAGE.iso=1"),
		"/api/v1/search?q=x&meta=" + url.QueryEscape("image.cameraModel=ok") + "&meta=" + url.QueryEscape("broken"),
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			expectErr(t, h, http.MethodGet, target, http.StatusBadRequest, types.ErrBadRequest)
		})
	}

	// 100 filters: refused on count, and the index is never called.
	q := url.Values{"q": {"x"}}
	for i := 0; i < 100; i++ {
		q.Add("meta", "image.iso="+strconv.Itoa(i))
	}
	before := idx.query()
	e := expectErr(t, h, http.MethodGet, "/api/v1/search?"+q.Encode(), http.StatusBadRequest, types.ErrBadRequest)
	if !strings.Contains(e.Message, strconv.Itoa(maxMetaFilters)) {
		t.Errorf("message = %q, want it to state the %d filter cap", e.Message, maxMetaFilters)
	}
	if got := idx.query(); got.Q != before.Q || len(got.Meta) != len(before.Meta) {
		t.Errorf("a rejected search must not reach the index (query is now %+v)", got)
	}
}

// ---------------------------------------------------------- /search/fields

func TestSearchFields(t *testing.T) {
	idx := &fakeIndex{categories: []types.MetaCategory{
		{
			ID:         "image",
			Label:      "Images",
			Extensions: []string{"jpg", "png"},
			Fields: []types.MetaFieldDef{
				{Key: "image.cameraModel", Label: "Camera", Type: "text"},
				{Key: "image.iso", Label: "ISO", Type: "number"},
			},
		},
		{
			ID:    "video",
			Label: "Video",
			Fields: []types.MetaFieldDef{{
				Key: "video.hdr", Label: "HDR", Type: "enum",
				Values: []types.MetaEnumValue{{Value: "hdr10", Label: "HDR10"}},
			}},
		},
		// Nulls from the index must not become nulls on the wire.
		{ID: "audio", Label: "Audio"},
	}}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	var got fieldsData
	getJSON(t, h, "/api/v1/search/fields", &got)
	if len(got.Categories) != 3 {
		t.Fatalf("categories = %+v", got.Categories)
	}
	if got.Categories[0].ID != "image" || len(got.Categories[0].Fields) != 2 {
		t.Errorf("first category = %+v", got.Categories[0])
	}
	if !eq(got.Categories[0].Extensions, []string{"jpg", "png"}) {
		t.Errorf("extensions = %v", got.Categories[0].Extensions)
	}
	if v := got.Categories[1].Fields[0].Values; len(v) != 1 || v[0].Value != "hdr10" {
		t.Errorf("enum values = %+v", v)
	}
	if got.Categories[2].Extensions == nil || got.Categories[2].Fields == nil {
		t.Errorf("nil slices must serialise as [], got %+v", got.Categories[2])
	}
	// MetaFieldDef.Values is omitempty, so a non-enum field carries no
	// "values" key at all — what must never appear is a null.
	rec := do(t, h, http.MethodGet, "/api/v1/search/fields", nil)
	if strings.Contains(rec.Body.String(), "null") {
		t.Errorf("no array in the response may be null: %s", rec.Body)
	}

	// The endpoint takes no parameters; junk on the query string is ignored
	// rather than being a way to make it fail.
	getJSON(t, h, "/api/v1/search/fields?category=image&nonsense=1", &got)
	if len(got.Categories) != 3 {
		t.Errorf("parameters must not change the answer")
	}
}

func TestSearchFieldsIsNeverNull(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{}, Index: &fakeIndex{}})
	rec := do(t, h, http.MethodGet, "/api/v1/search/fields", nil)
	if !strings.Contains(rec.Body.String(), `"categories":[]`) {
		t.Errorf("body = %s, want an empty array", rec.Body)
	}
}

// ---------------------------------------------------------- /search/values

func TestSearchValues(t *testing.T) {
	idx := &fakeIndex{values: []types.MetaValue{
		{Value: "DMC-GH5", Count: 412},
		{Value: "ILCE-7M3", Count: 96},
	}}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})

	var got valuesData
	getJSON(t, h, "/api/v1/search/values?key=image.cameraModel", &got)
	if len(got.Values) != 2 || got.Values[0].Value != "DMC-GH5" || got.Values[0].Count != 412 {
		t.Fatalf("values = %+v", got.Values)
	}
	if c := idx.valuesCall(); c.key != "image.cameraModel" || c.prefix != "" || c.path != "" || c.limit != valuesLimitDefault {
		t.Errorf("call = %+v, want the default limit %d", c, valuesLimitDefault)
	}

	// prefix and path scope are forwarded verbatim.
	getJSON(t, h, "/api/v1/search/values?key=audio.artist&prefix="+url.QueryEscape("Mi les")+"&path=/mnt/user/music&limit=10", &got)
	if c := idx.valuesCall(); c.prefix != "Mi les" || c.path != "/mnt/user/music" || c.limit != 10 {
		t.Errorf("call = %+v", c)
	}

	// Over the cap is clamped, not refused (same rule as every other limit).
	getJSON(t, h, "/api/v1/search/values?key=image.iso&limit=100000", &got)
	if c := idx.valuesCall(); c.limit != valuesLimitMax {
		t.Errorf("limit = %d, want it clamped to %d", c.limit, valuesLimitMax)
	}

	// The catalog's "common" category serves bare keys, so they must be
	// queryable too — a key /search/fields offers and /search/values refuses
	// would be a dropdown that cannot be used.
	getJSON(t, h, "/api/v1/search/values?key=ext", &got)
	if c := idx.valuesCall(); c.key != "ext" {
		t.Errorf("common key = %q, want it accepted", c.key)
	}

	// values is never null.
	idx.values = nil
	rec := do(t, h, http.MethodGet, "/api/v1/search/values?key=image.iso", nil)
	if !strings.Contains(rec.Body.String(), `"values":[]`) {
		t.Errorf("body = %s, want an empty array", rec.Body)
	}
}

func TestSearchValuesValidation(t *testing.T) {
	idx := &fakeIndex{}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})
	for _, target := range []string{
		"/api/v1/search/values",
		"/api/v1/search/values?key=",
		"/api/v1/search/values?key=%20",
		"/api/v1/search/values?key=" + url.QueryEscape("Image.iso"),
		"/api/v1/search/values?key=" + url.QueryEscape("image.iso.speed"),
		"/api/v1/search/values?key=" + url.QueryEscape("image.iso9"),
		"/api/v1/search/values?key=" + url.QueryEscape("image.iso; DROP TABLE files"),
		"/api/v1/search/values?key=" + url.QueryEscape("../../etc/passwd"),
		"/api/v1/search/values?key=image.iso&limit=-1",
		"/api/v1/search/values?key=image.iso&limit=lots",
	} {
		t.Run(target, func(t *testing.T) {
			expectErr(t, h, http.MethodGet, target, http.StatusBadRequest, types.ErrBadRequest)
		})
	}
	if idx.valuesCall().key != "" {
		t.Errorf("a rejected request must not reach the index: %+v", idx.valuesCall())
	}
}

func TestSearchValuesErrorPropagates(t *testing.T) {
	idx := &fakeIndex{valuesErr: types.Errf(types.ErrIndexing, "still crawling")}
	h := NewRouter(Deps{FS: &fakeFS{}, Index: idx})
	expectErr(t, h, http.MethodGet, "/api/v1/search/values?key=image.iso",
		http.StatusServiceUnavailable, types.ErrIndexing)
}

// Both new endpoints answer INDEXING when no index is wired, like every other
// index-backed endpoint.
func TestMetaEndpointsWithoutAnIndex(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{}})
	for _, target := range []string{
		"/api/v1/search/fields",
		"/api/v1/search/values?key=image.iso",
		"/api/v1/search?minSize=1",
	} {
		t.Run(target, func(t *testing.T) {
			expectErr(t, h, http.MethodGet, target, http.StatusServiceUnavailable, types.ErrIndexing)
		})
	}
}

// /search/values is called on every keystroke of the value type-ahead, so it
// shares the expensive-request budget with /search and fs/list. /search/fields
// is a static read and does not.
func TestSearchValuesIsConcurrencyLimited(t *testing.T) {
	idx := &fakeIndex{}
	s := newServer(Deps{FS: &fakeFS{}, Index: idx})
	s.busy = newLimiter(1, 20*time.Millisecond)
	h := newRouter(s)

	release, err := s.busy.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := expectErr(t, h, http.MethodGet, "/api/v1/search/values?key=image.iso",
		http.StatusGatewayTimeout, types.ErrTimeout)
	if e.Message != "server busy" {
		t.Errorf("message = %q, want %q", e.Message, "server busy")
	}

	// The vocabulary is still readable while the budget is exhausted.
	var fields fieldsData
	getJSON(t, h, "/api/v1/search/fields", &fields)
	release()

	var got valuesData
	getJSON(t, h, "/api/v1/search/values?key=image.iso", &got)
}

// The new paths join the routing table: wrong method is 405 with Allow, and
// /search itself keeps working alongside its sub-paths.
func TestSearchSubPathRouting(t *testing.T) {
	h := NewRouter(Deps{FS: &fakeFS{}, Index: &fakeIndex{}})
	for _, target := range []string{"/api/v1/search/fields", "/api/v1/search/values?key=image.iso"} {
		rec := do(t, h, http.MethodPost, target, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", target, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
			t.Errorf("Allow = %q", allow)
		}
	}
	var got searchData
	getJSON(t, h, "/api/v1/search?q=x", &got)
}
