package api

import (
	"net/url"
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
)

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
