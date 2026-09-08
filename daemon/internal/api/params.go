package api

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"unraid-filebrowser/internal/types"
)

// Window and page limits from API.md.
const (
	listLimitDefault = 1000
	listLimitMax     = 10000

	viewLengthDefault = 262144
	viewLengthMax     = 1048576

	hexLengthDefault = 4096
	hexLengthMax     = 65536

	searchLimitDefault = 100
	searchLimitMax     = 1000

	valuesLimitDefault = 200
	valuesLimitMax     = 500
)

// maxMetaFilters caps how many `meta=` clauses one search may carry. Each one
// costs the index another join, and a UI never builds more than a handful; the
// cap is what stops a hand-written URL from turning into an expensive query.
const maxMetaFilters = 16

// metaKeyPattern is the metadata key naming convention: `<category>.<field>`,
// e.g. `image.cameraModel` — lowercase category, lowerCamelCase field.
// The optional second segment admits the catalog's "common" category, whose
// keys are bare (`name`, `ext`, `size`, `mtime`, `mime`) because they are
// columns every indexed file has rather than extracted metadata. Every key
// /search/fields serves must be one /search and /search/values accept, or the
// UI's dropdowns would offer fields it cannot then query. Anything else is
// rejected here, before it reaches the index.
var metaKeyPattern = regexp.MustCompile(`^[a-z]+(\.[A-Za-z]+)?$`)

// metaKeyHint is the tail of every "that is not a key" message.
const metaKeyHint = "expected <category>.<field> (e.g. image.cameraModel) or a common key (e.g. size)"

// metaOps are the comparison operators a `meta=` clause may use, longest
// first: the scanner must see `>=` before it sees `>`.
var metaOps = []string{">=", "<=", "!=", "=", "~", ">", "<"}

// quoteItem renders a piece of user input for an error message: bounded in
// length so a pathological parameter cannot turn into a pathological response,
// and quoted so an empty or space-only value is visible.
func quoteItem(s string) string {
	const maxEcho = 64
	if len(s) > maxEcho {
		s = s[:maxEcho] + "…"
	}
	return strconv.Quote(s)
}

// metaKeyParam validates a metadata key against the naming convention.
func metaKeyParam(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", types.Errf(types.ErrBadRequest, "key is required")
	}
	if !metaKeyPattern.MatchString(key) {
		return "", types.Errf(types.ErrBadRequest,
			"key "+quoteItem(key)+" is not a metadata key; "+metaKeyHint)
	}
	return key, nil
}

// parseMetaFilters parses the repeated `meta` parameter. Each value is
// `<key><op><value>` — the first operator *after* the key wins, so a value may
// itself contain `=` (`meta=audio.title=a=b` filters for the literal `a=b`).
// Values arrive already percent-decoded by net/http; nothing here decodes,
// unescapes or interprets them further — they are handed to the index verbatim
// and parametrised there.
func parseMetaFilters(q url.Values) ([]types.MetaFilter, error) {
	raw := q["meta"]
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxMetaFilters {
		return nil, types.Errf(types.ErrBadRequest,
			"at most "+strconv.Itoa(maxMetaFilters)+" meta filters are allowed, got "+strconv.Itoa(len(raw)))
	}
	out := make([]types.MetaFilter, 0, len(raw))
	for _, item := range raw {
		f, err := parseMetaFilter(item)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// parseMetaFilter parses one `<key><op><value>` clause.
func parseMetaFilter(item string) (types.MetaFilter, error) {
	s := strings.TrimSpace(item)
	bad := func(why string) (types.MetaFilter, error) {
		return types.MetaFilter{}, types.Errf(types.ErrBadRequest, "meta filter "+quoteItem(item)+" "+why)
	}
	if s == "" {
		return bad("is empty; expected <key><op><value>, e.g. image.iso>=800")
	}

	// Scan for the first operator. Two-character operators are tested before
	// the single-character ones at the same position so `>=` never parses as
	// `>` followed by a value of "=800".
	idx, op := -1, ""
	for i := 0; i < len(s) && idx < 0; i++ {
		for _, cand := range metaOps {
			if strings.HasPrefix(s[i:], cand) {
				idx, op = i, cand
				break
			}
		}
	}
	if idx < 0 {
		return bad("has no operator; expected one of " + strings.Join(metaOps, " ") + " after the key")
	}

	key := strings.TrimSpace(s[:idx])
	if key == "" {
		return bad("has no key before the operator")
	}
	if !metaKeyPattern.MatchString(key) {
		return bad("has an invalid key " + quoteItem(key) + "; " + metaKeyHint)
	}
	value := strings.TrimSpace(s[idx+len(op):])
	if value == "" {
		return bad("has an empty value")
	}
	return types.MetaFilter{Key: key, Op: op, Value: value}, nil
}

// pathParam reads the required `path` parameter.
func pathParam(q url.Values) (string, error) {
	p := q.Get("path")
	if strings.TrimSpace(p) == "" {
		return "", types.Errf(types.ErrBadRequest, "path is required")
	}
	return p, nil
}

// intParam reads an integer parameter. Absent or empty yields def; values
// below minimum are rejected, values above maximum are clamped (a client
// asking for a bigger window gets the biggest allowed one, with truncated=true
// telling it there is more).
func intParam(q url.Values, name string, def, minimum, maximum int64) (int64, error) {
	raw := strings.TrimSpace(q.Get(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, types.Errf(types.ErrBadRequest, name+" must be an integer")
	}
	if v < minimum {
		return 0, types.Errf(types.ErrBadRequest, name+" must be at least "+strconv.FormatInt(minimum, 10))
	}
	if v > maximum {
		v = maximum
	}
	return v, nil
}

// boolParam accepts the usual spellings; anything else is a bad request.
func boolParam(q url.Values, name string, def bool) (bool, error) {
	raw := strings.ToLower(strings.TrimSpace(q.Get(name)))
	if raw == "" {
		return def, nil
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, types.Errf(types.ErrBadRequest, name+" must be a boolean")
	}
}

// enumParam validates a parameter against an allowed set.
func enumParam(q url.Values, name, def string, allowed ...string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(q.Get(name)))
	if raw == "" {
		return def, nil
	}
	for _, a := range allowed {
		if raw == a {
			return raw, nil
		}
	}
	return "", types.Errf(types.ErrBadRequest, name+" must be one of "+strings.Join(allowed, "|"))
}

// splitExts parses the comma-separated `ext` filter into bare lowercase
// extensions ("go", "md"), tolerating dots and spaces.
func splitExts(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		e := strings.ToLower(strings.TrimSpace(part))
		e = strings.TrimPrefix(e, ".")
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

// typeRank groups entry types for `sort=type`: directories, then archives
// (browsable like directories), then plain files, then symlinks.
func typeRank(t types.EntryType) int {
	switch t {
	case types.TypeDir:
		return 0
	case types.TypeArchive:
		return 1
	case types.TypeFile:
		return 2
	default:
		return 3
	}
}

// sortEntries orders a listing in place. dirsFirst is applied on top of the
// sort key and is not affected by the direction, which is what a file manager
// user expects.
func sortEntries(entries []types.Entry, key, dir string, dirsFirst bool) {
	desc := dir == "desc"
	byName := func(a, b types.Entry) int {
		la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
		switch {
		case la < lb:
			return -1
		case la > lb:
			return 1
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	}
	cmp := byName
	switch key {
	case "size":
		cmp = func(a, b types.Entry) int {
			switch {
			case a.Size < b.Size:
				return -1
			case a.Size > b.Size:
				return 1
			}
			return byName(a, b)
		}
	case "mtime":
		cmp = func(a, b types.Entry) int {
			switch {
			case a.Mtime < b.Mtime:
				return -1
			case a.Mtime > b.Mtime:
				return 1
			}
			return byName(a, b)
		}
	case "type":
		cmp = func(a, b types.Entry) int {
			ra, rb := typeRank(a.Type), typeRank(b.Type)
			switch {
			case ra < rb:
				return -1
			case ra > rb:
				return 1
			}
			return byName(a, b)
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if dirsFirst {
			ad, bd := a.Type == types.TypeDir, b.Type == types.TypeDir
			if ad != bd {
				return ad
			}
		}
		c := cmp(a, b)
		if desc {
			return c > 0
		}
		return c < 0
	})
}

// page slices a sorted listing; an offset past the end yields an empty page.
func page(entries []types.Entry, offset, limit int64) []types.Entry {
	if offset >= int64(len(entries)) {
		return []types.Entry{}
	}
	end := offset + limit
	if end > int64(len(entries)) {
		end = int64(len(entries))
	}
	return entries[offset:end]
}
