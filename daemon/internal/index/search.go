package index

import (
	"context"
	"fmt"
	"html"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"unraid-filebrowser/internal/types"
)

const (
	likeFallbackScore = 0.1
	maxSearchLimit    = 1000
	defaultLimit      = 100

	// maxSearchWindow caps offset+limit: deep pages would otherwise force
	// the "both" merge to fetch that many rows per branch.
	maxSearchWindow = 10000

	// minLikeRunes is the shortest term the LIKE substring fallback will
	// run for: it is an unindexed scan of every name, so it only runs for
	// terms selective enough to be useful.
	minLikeRunes = 3

	// maxSnippetRunes bounds a rendered snippet. FTS5's snippet() bounds by
	// tokens, not characters, so a body made of one enormous token would
	// otherwise come back whole.
	maxSnippetRunes = 600
)

// Search runs a query against the index and returns the requested page of
// hits plus the total. Modes: "name" (filename/dir FTS with prefix matching
// and a LIKE substring fallback), "content" (full-text over extracted
// bodies, with <mark> snippets), "both" (merged, deduped by path with name
// hits winning ties). Results are always confined to the live index roots
// and, as a second belt, to AllowedRoots — rows a wider earlier root set
// left behind are never returned.
//
// Q is optional: a query with no searchable terms but at least one filter
// (path, ext, size, mtime, meta) is a filter-only search over the files
// table. No terms and no filters is ErrBadRequest. Metadata filters are
// ANDed EXISTS subqueries (see metaFilterSQL). Sort is relevance (default
// when Q is present; falls back to name without it), name, size or mtime,
// with Dir asc/desc.
//
// Paging is pushed into SQL (LIMIT/OFFSET, and COUNT queries for the total)
// so the work is bounded by the page, not by the number of matches; in
// "both" mode each branch contributes at most offset+limit+1 rows before
// the merge, and the total is an exact UNION count. offset+limit may not
// exceed maxSearchWindow (ErrBadRequest).
//
// User query text never reaches SQL/FTS syntax raw: the MATCH expression is
// rebuilt from tokenized terms and quoted phrases with FTS operators
// stripped, and every filter value is a bound parameter.
func (s *Service) Search(ctx context.Context, q types.SearchQuery) ([]types.SearchHit, int, error) {
	if s.closed.Load() {
		return nil, 0, types.Errf(types.ErrIndexing, "index service is closed")
	}
	mode := q.Mode
	if mode == "" {
		mode = "both"
	}
	switch mode {
	case "name", "content", "both":
	default:
		return nil, 0, types.Errf(types.ErrBadRequest, "invalid search mode: "+mode)
	}
	terms, phrases := parseQuery(q.Q)
	hasQ := len(terms)+len(phrases) > 0
	filterSQL, filterArgs, nFilters, err := buildFilters(q)
	if err != nil {
		return nil, 0, err
	}
	if !hasQ && nFilters == 0 {
		return nil, 0, types.Errf(types.ErrBadRequest, "query contains no searchable terms and no filters")
	}
	sortKey, desc, err := resolveSort(q.Sort, q.Dir, hasQ)
	if err != nil {
		return nil, 0, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	if offset+limit > maxSearchWindow {
		return nil, 0, types.Errf(types.ErrBadRequest,
			fmt.Sprintf("offset+limit must not exceed %d; narrow the query instead", maxSearchWindow))
	}

	scopeSQL, scopeArgs := s.scopeFilter()
	filterSQL += scopeSQL
	filterArgs = append(filterArgs, scopeArgs...)

	if !hasQ {
		br := filterBranch(filterSQL, filterArgs)
		br.order = orderBy(sortKey, desc, br.order)
		n, err := s.countBranch(ctx, br)
		if err != nil {
			return nil, 0, wrapDBErr(err)
		}
		if n == 0 || offset >= n {
			return []types.SearchHit{}, n, nil
		}
		hits, err := s.queryBranch(ctx, br, limit, offset)
		if err != nil {
			return nil, 0, wrapDBErr(err)
		}
		return hits, n, nil
	}

	var nameBr, contentBr *branch
	var nameTotal, contentTotal int
	if mode != "content" {
		br := ftsNameBranch(terms, phrases, filterSQL, filterArgs)
		n, err := s.countBranch(ctx, br)
		if err != nil {
			return nil, 0, wrapDBErr(err)
		}
		if n == 0 {
			// Substring fallback, only when FTS found nothing and the terms
			// are long enough to make a full scan worthwhile.
			if lb := likeNameBranch(terms, phrases, filterSQL, filterArgs); lb != nil {
				if n, err = s.countBranch(ctx, lb); err != nil {
					return nil, 0, wrapDBErr(err)
				}
				br = lb
			}
		}
		if n > 0 {
			br.order = orderBy(sortKey, desc, br.order)
			nameBr, nameTotal = br, n
		}
	}
	if mode != "name" {
		br := contentBranch(terms, phrases, filterSQL, filterArgs)
		n, err := s.countBranch(ctx, br)
		if err != nil {
			return nil, 0, wrapDBErr(err)
		}
		if n > 0 {
			br.order = orderBy(sortKey, desc, br.order)
			contentBr, contentTotal = br, n
		}
	}

	switch {
	case nameBr == nil && contentBr == nil:
		return []types.SearchHit{}, 0, nil
	case contentBr == nil:
		hits, err := s.queryBranch(ctx, nameBr, limit, offset)
		if err != nil {
			return nil, 0, wrapDBErr(err)
		}
		return hits, nameTotal, nil
	case nameBr == nil:
		hits, err := s.queryBranch(ctx, contentBr, limit, offset)
		if err != nil {
			return nil, 0, wrapDBErr(err)
		}
		return hits, contentTotal, nil
	}

	// Both branches have hits: exact distinct-path total, then the top
	// offset+limit+1 of each branch merged in Go. Any hit in the true merged
	// window is within the first offset+limit of its own branch (both
	// branches and the merge share one total order), so the page is exact;
	// only the score boost a name hit inherits from a content twin ranked
	// below the window is missed.
	total, err := s.countUnion(ctx, nameBr, contentBr)
	if err != nil {
		return nil, 0, wrapDBErr(err)
	}
	if offset >= total {
		return []types.SearchHit{}, total, nil
	}
	k := offset + limit + 1
	nameHits, err := s.queryBranch(ctx, nameBr, k, 0)
	if err != nil {
		return nil, 0, wrapDBErr(err)
	}
	contentHits, err := s.queryBranch(ctx, contentBr, k, 0)
	if err != nil {
		return nil, 0, wrapDBErr(err)
	}
	merged := mergeHitsBy(nameHits, contentHits, hitLess(sortKey, desc))
	if offset >= len(merged) {
		return []types.SearchHit{}, total, nil
	}
	end := min(offset+limit, len(merged))
	return merged[offset:end], total, nil
}

// resolveSort validates Sort/Dir. Relevance without query text has nothing
// to rank by and becomes name ascending.
func resolveSort(sortKey, dir string, hasQ bool) (string, bool, error) {
	switch sortKey {
	case "":
		if hasQ {
			sortKey = "relevance"
		} else {
			sortKey = "name"
		}
	case "relevance":
		if !hasQ {
			sortKey = "name"
		}
	case "name", "size", "mtime":
	default:
		return "", false, types.Errf(types.ErrBadRequest, "invalid sort: "+sortKey)
	}
	desc := false
	switch dir {
	case "", "asc":
	case "desc":
		desc = true
	default:
		return "", false, types.Errf(types.ErrBadRequest, "invalid sort direction: "+dir)
	}
	return sortKey, desc, nil
}

// orderBy renders the ORDER BY clause for a sort key; relevance keeps the
// branch's own ranking clause (bm25, or name for the LIKE fallback).
func orderBy(sortKey string, desc bool, relevance string) string {
	d := "ASC"
	if desc {
		d = "DESC"
	}
	switch sortKey {
	case "name":
		return "ORDER BY f.name COLLATE NOCASE " + d + ", f.path " + d
	case "size":
		return "ORDER BY f.size " + d + ", f.path " + d
	case "mtime":
		return "ORDER BY f.mtime " + d + ", f.path " + d
	}
	return relevance
}

// hitLess is the Go-side comparator matching orderBy, used when merging
// the two branches of a "both" search. nil means relevance ordering.
func hitLess(sortKey string, desc bool) func(a, b types.SearchHit) bool {
	var cmp func(a, b types.SearchHit) int
	switch sortKey {
	case "name":
		cmp = func(a, b types.SearchHit) int {
			return strings.Compare(strings.ToLower(a.Entry.Name), strings.ToLower(b.Entry.Name))
		}
	case "size":
		cmp = func(a, b types.SearchHit) int { return cmpInt(a.Entry.Size, b.Entry.Size) }
	case "mtime":
		cmp = func(a, b types.SearchHit) int { return cmpInt(a.Entry.Mtime, b.Entry.Mtime) }
	default:
		return nil
	}
	return func(a, b types.SearchHit) bool {
		c := cmp(a, b)
		if c == 0 {
			c = strings.Compare(a.Entry.Path, b.Entry.Path)
		}
		if desc {
			return c > 0
		}
		return c < 0
	}
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// scopeFilter renders the root confinement applied to every search branch:
// the live index roots AND the boot-time AllowedRoots (redundant while the
// config is valid, cheap, and independent of it). An empty root set matches
// nothing.
func (s *Service) scopeFilter() (string, []any) {
	cfgCond, cfgArgs := prefixCond("f.path", s.Config().Roots)
	allowCond, allowArgs := prefixCond("f.path", s.allowed)
	return " AND " + cfgCond + " AND " + allowCond, append(cfgArgs, allowArgs...)
}

func wrapDBErr(err error) error {
	if ae, ok := err.(*types.APIError); ok {
		return ae
	}
	if strings.Contains(err.Error(), "fts5: syntax error") {
		return types.Errf(types.ErrBadRequest, "unsupported query syntax")
	}
	return types.Errf(types.ErrIndexing, "index query failed: "+err.Error())
}

// parseQuery tokenizes the raw user query into bare terms and double-quoted
// phrases. Bare FTS operator tokens (AND/OR/NOT/NEAR) are dropped, and any
// token without at least one letter or digit is discarded; everything kept
// is later re-quoted, so FTS syntax characters (^ : ( ) * etc.) are inert.
func parseQuery(raw string) (terms, phrases []string) {
	var cur strings.Builder
	inQuote := false
	flushTerm := func() {
		t := cur.String()
		cur.Reset()
		if t == "" || isOperatorToken(t) || !hasSearchable(t) {
			return
		}
		terms = append(terms, t)
	}
	flushPhrase := func() {
		p := cur.String()
		cur.Reset()
		if hasSearchable(p) {
			phrases = append(phrases, p)
		}
	}
	for _, r := range raw {
		switch {
		case r == '"':
			if inQuote {
				flushPhrase()
				inQuote = false
			} else {
				flushTerm()
				inQuote = true
			}
		case !inQuote && unicode.IsSpace(r):
			flushTerm()
		default:
			cur.WriteRune(r)
		}
	}
	if inQuote {
		flushPhrase() // unterminated quote: treat the rest as a phrase
	} else {
		flushTerm()
	}
	return terms, phrases
}

func isOperatorToken(t string) bool {
	switch t {
	case "AND", "OR", "NOT", "NEAR":
		return true
	}
	return false
}

func hasSearchable(t string) bool {
	for _, r := range t {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// quoteFTS wraps s as an FTS5 string literal ("" escapes an embedded quote),
// neutralizing all FTS syntax inside it.
func quoteFTS(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// buildMatch assembles the FTS5 MATCH expression: quoted bare terms get a
// trailing * (prefix match), quoted phrases match exactly; parts are
// space-joined (implicit AND).
func buildMatch(terms, phrases []string) string {
	parts := make([]string, 0, len(terms)+len(phrases))
	for _, t := range terms {
		parts = append(parts, quoteFTS(t)+"*")
	}
	for _, p := range phrases {
		parts = append(parts, quoteFTS(p))
	}
	return strings.Join(parts, " ")
}

// buildFilters renders the SearchQuery filters as an SQL fragment over the
// files alias f (" AND ..." or empty), fully parametrized, plus the number
// of filters applied. MinSize/MaxSize use -1 as "unset"; After/Before use 0.
// Metadata filters are validated against the catalog; a bad key, operator
// or value is ErrBadRequest.
func buildFilters(q types.SearchQuery) (string, []any, int, error) {
	var conds []string
	var args []any
	if q.Path != "" {
		p := filepath.Clean(q.Path)
		conds = append(conds, `(f.dir = ? OR f.dir LIKE ? ESCAPE '\')`)
		args = append(args, p, likePrefix(p))
	}
	if len(q.Exts) > 0 {
		conds = append(conds, `f.ext IN (`+placeholders(len(q.Exts))+`)`)
		for _, e := range q.Exts {
			args = append(args, strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), ".")))
		}
	}
	if q.MinSize >= 0 {
		conds = append(conds, `f.size >= ?`)
		args = append(args, q.MinSize)
	}
	if q.MaxSize >= 0 {
		conds = append(conds, `f.size <= ?`)
		args = append(args, q.MaxSize)
	}
	if q.After > 0 {
		conds = append(conds, `f.mtime >= ?`)
		args = append(args, q.After)
	}
	if q.Before > 0 {
		conds = append(conds, `f.mtime <= ?`)
		args = append(args, q.Before)
	}
	if len(q.Meta) > maxMetaFilters {
		return "", nil, 0, types.Errf(types.ErrBadRequest,
			fmt.Sprintf("at most %d metadata filters per search", maxMetaFilters))
	}
	for _, mf := range q.Meta {
		sql, a, err := metaFilterSQL(mf)
		if err != nil {
			return "", nil, 0, err
		}
		conds = append(conds, sql)
		args = append(args, a...)
	}
	if len(conds) == 0 {
		return "", nil, 0, nil
	}
	return " AND " + strings.Join(conds, " AND "), args, len(conds), nil
}

// metaFilterSQL renders one MetaFilter as a parametrized condition over the
// files alias f.
//
// Catalog keys with a category prefix ("video.hdr") become EXISTS subqueries
// on file_meta: "=" and "~" match value case-insensitively (numeric fields
// compare num when the value parses as a number, so 1920 matches "1920"
// however it was formatted); "!=" is NOT EXISTS of the "=" predicate, i.e.
// "no value of this key equals V" — a file with an AC-3 and an AAC track
// does not match audioCodec != ac3, and a file without the key does; the
// ordering operators use num and are only valid on number/bytes/date fields
// (dates accept RFC3339, YYYY-MM-DD or unix seconds).
//
// The common keys map onto files columns: name, ext and mime take the text
// operators; size and mtime the numeric ones.
func metaFilterSQL(f types.MetaFilter) (string, []any, error) {
	def, ok := metaFieldDefs[f.Key]
	if !ok {
		return "", nil, types.Errf(types.ErrBadRequest, "unknown metadata key: "+f.Key)
	}
	op := f.Op
	switch op {
	case "=", "!=", "~", "<", "<=", ">", ">=":
	default:
		return "", nil, types.Errf(types.ErrBadRequest, fmt.Sprintf("invalid operator %q for %s", op, f.Key))
	}
	numeric := def.Type == "number" || def.Type == "bytes" || def.Type == "date"
	ordering := op == "<" || op == "<=" || op == ">" || op == ">="
	if ordering && !numeric {
		return "", nil, types.Errf(types.ErrBadRequest,
			fmt.Sprintf("operator %s needs a numeric field; %s is %s", op, f.Key, def.Type))
	}
	value := strings.TrimSpace(f.Value)
	var numVal float64
	haveNum := false
	if numeric {
		if v, ok := parseNumericValue(value, def.Type); ok {
			numVal, haveNum = v, true
		} else if ordering || op == "=" || op == "!=" {
			return "", nil, types.Errf(types.ErrBadRequest,
				fmt.Sprintf("value %q is not a valid %s for %s", f.Value, def.Type, f.Key))
		}
	}

	if !strings.Contains(f.Key, ".") {
		// Common column.
		var col string
		switch f.Key {
		case "name":
			col = "f.name"
		case "ext":
			col = "f.ext"
			value = strings.ToLower(strings.TrimPrefix(value, "."))
		case "mime":
			col = "f.mimeclass"
		case "size":
			col = "f.size"
		case "mtime":
			col = "f.mtime"
		default:
			return "", nil, types.Errf(types.ErrBadRequest, "unsupported common key: "+f.Key)
		}
		if numeric {
			if op == "~" {
				return "", nil, types.Errf(types.ErrBadRequest, "operator ~ is not valid for "+f.Key)
			}
			if op == "!=" {
				op = "<>"
			}
			return col + " " + op + " ?", []any{numVal}, nil
		}
		switch op {
		case "=":
			return col + " = ? COLLATE NOCASE", []any{value}, nil
		case "!=":
			return col + " <> ? COLLATE NOCASE", []any{value}, nil
		default: // ~
			return col + ` LIKE ? ESCAPE '\'`, []any{"%" + escapeLike(value) + "%"}, nil
		}
	}

	const exists = `EXISTS (SELECT 1 FROM file_meta m WHERE m.file_id = f.id AND m.key = ? AND `
	switch op {
	case "~":
		return exists + `m.value LIKE ? ESCAPE '\')`, []any{f.Key, "%" + escapeLike(value) + "%"}, nil
	case "=", "!=":
		var pred string
		var arg any
		if haveNum {
			pred, arg = `m.num = ?`, numVal
		} else {
			pred, arg = `m.value = ? COLLATE NOCASE`, value
		}
		sql := exists + pred + `)`
		if op == "!=" {
			sql = "NOT " + sql
		}
		return sql, []any{f.Key, arg}, nil
	default:
		return exists + `m.num ` + op + ` ?)`, []any{f.Key, numVal}, nil
	}
}

// parseNumericValue reads a filter value for a numeric field: a plain
// number, or for dates also RFC3339 / YYYY-MM-DD (unix seconds out).
func parseNumericValue(v, typ string) (float64, bool) {
	if v == "" {
		return 0, false
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
		return n, true
	}
	if typ == "date" {
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				return float64(t.Unix()), true
			}
		}
	}
	return 0, false
}

// branch is one composable search query: the same FROM/WHERE body serves the
// page query (SELECT cols ... ORDER BY ... LIMIT ? OFFSET ?), the COUNT(*)
// total, and the UNION count of "both" mode. cols always yields
// path, name, ext, size, mtime, score and — for content — the raw snippet.
type branch struct {
	cols    string
	body    string // "FROM ... WHERE ..." including filters and root scope
	order   string // "ORDER BY ..." with a path tiebreak for stable paging
	args    []any
	kind    string // "name" | "content" (SearchHit.MatchedIn)
	snippet bool
}

// ftsNameBranch queries names_fts (name weighted 10x over dir in bm25).
func ftsNameBranch(terms, phrases []string, filterSQL string, filterArgs []any) *branch {
	return &branch{
		cols:  `f.path, f.name, f.ext, f.size, f.mtime, -bm25(names_fts, 10.0, 1.0)`,
		body:  `FROM names_fts JOIN files f ON f.id = names_fts.rowid WHERE names_fts MATCH ?` + filterSQL,
		order: `ORDER BY bm25(names_fts, 10.0, 1.0), f.path`,
		args:  append([]any{buildMatch(terms, phrases)}, filterArgs...),
		kind:  "name",
	}
}

// filterBranch is the filter-only query (no search text): every file in
// scope that satisfies the filters, scored 0, ordered by the sort key
// (orderBy replaces the default).
func filterBranch(filterSQL string, filterArgs []any) *branch {
	return &branch{
		cols:  `f.path, f.name, f.ext, f.size, f.mtime, 0.0`,
		body:  `FROM files f WHERE 1 = 1` + filterSQL,
		order: `ORDER BY f.name COLLATE NOCASE, f.path`,
		args:  append([]any(nil), filterArgs...),
		kind:  "name",
	}
}

// likeNameBranch is the LIKE %term% substring fallback over files.name —
// the hits users expect when a term sits mid-token (e.g. "log" inside
// "server.log", which tokenizes as one token). It is an unindexed scan, so
// it returns nil unless every term has at least minLikeRunes characters.
func likeNameBranch(terms, phrases []string, filterSQL string, filterArgs []any) *branch {
	var conds []string
	var args []any
	for _, t := range append(append([]string(nil), terms...), phrases...) {
		if utf8.RuneCountInString(t) < minLikeRunes {
			return nil
		}
		conds = append(conds, `f.name LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(t)+"%")
	}
	return &branch{
		cols:  `f.path, f.name, f.ext, f.size, f.mtime, ` + strconv.FormatFloat(likeFallbackScore, 'g', -1, 64),
		body:  `FROM files f WHERE ` + strings.Join(conds, " AND ") + filterSQL,
		order: `ORDER BY f.name, f.path`,
		args:  append(args, filterArgs...),
		kind:  "name",
	}
}

// contentBranch queries content_fts and carries the raw snippet. Snippet
// mark delimiters travel through SQL as 0x02/0x03 control characters
// (stripped from bodies at extraction time), so the whole snippet can be
// HTML-escaped before the delimiters become <mark> tags.
func contentBranch(terms, phrases []string, filterSQL string, filterArgs []any) *branch {
	return &branch{
		cols: `f.path, f.name, f.ext, f.size, f.mtime, -bm25(content_fts),
		        snippet(content_fts, 1, char(2), char(3), '…', 12)`,
		body:    `FROM content_fts JOIN files f ON f.path = content_fts.path WHERE content_fts MATCH ?` + filterSQL,
		order:   `ORDER BY bm25(content_fts), f.path`,
		args:    append([]any{buildMatch(terms, phrases)}, filterArgs...),
		kind:    "content",
		snippet: true,
	}
}

func (s *Service) countBranch(ctx context.Context, b *branch) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) `+b.body, b.args...).Scan(&n)
	return n, err
}

// countUnion counts the distinct paths matched by either branch.
func (s *Service) countUnion(ctx context.Context, a, b *branch) (int, error) {
	var n int
	args := make([]any, 0, len(a.args)+len(b.args))
	args = append(args, a.args...)
	args = append(args, b.args...)
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (SELECT f.path `+a.body+` UNION SELECT f.path `+b.body+`)`,
		args...).Scan(&n)
	return n, err
}

// queryBranch fetches one page of a branch, ranked. The result is never nil.
func (s *Service) queryBranch(ctx context.Context, b *branch, limit, offset int) ([]types.SearchHit, error) {
	args := make([]any, 0, len(b.args)+2)
	args = append(args, b.args...)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+b.cols+` `+b.body+` `+b.order+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hits := make([]types.SearchHit, 0, min(limit, 64))
	for rows.Next() {
		var path, name, ext, snip string
		var size, mtime int64
		var score float64
		if b.snippet {
			err = rows.Scan(&path, &name, &ext, &size, &mtime, &score, &snip)
		} else {
			err = rows.Scan(&path, &name, &ext, &size, &mtime, &score)
		}
		if err != nil {
			return nil, err
		}
		h := types.SearchHit{
			Entry:     makeEntry(path, name, ext, size, mtime),
			Score:     score,
			MatchedIn: b.kind,
		}
		if b.snippet {
			h.Snippet = renderSnippet(snip)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// renderSnippet bounds the raw snippet (truncateSnippet), HTML-escapes it,
// then turns the 0x02/0x03 delimiters into <mark> tags.
func renderSnippet(raw string) string {
	esc := html.EscapeString(truncateSnippet(raw, maxSnippetRunes))
	esc = strings.ReplaceAll(esc, "\x02", "<mark>")
	esc = strings.ReplaceAll(esc, "\x03", "</mark>")
	return esc
}

// truncateSnippet cuts raw down to about max runes centred on the first
// mark delimiter (or the start when there is none), without splitting a
// UTF-8 sequence and without materializing a rune slice of the input. A
// mark opened before the window or left open at its end is re-balanced so
// every <mark> still closes; cut edges get an ellipsis.
func truncateSnippet(raw string, max int) string {
	if len(raw) <= max || (len(raw) <= 4*max && utf8.RuneCountInString(raw) <= max) {
		return raw
	}
	center := strings.IndexByte(raw, '\x02')
	if center < 0 {
		center = 0
	}
	start, back := center, 0
	for back < max/2 && start > 0 {
		_, sz := utf8.DecodeLastRuneInString(raw[:start])
		start -= sz
		back++
	}
	end := center
	for n := 0; n < max-back && end < len(raw); n++ {
		_, sz := utf8.DecodeRuneInString(raw[end:])
		end += sz
	}
	out := raw[start:end]

	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	depth := 0
	for i := 0; i < len(out); i++ {
		switch out[i] {
		case '\x02':
			depth++
		case '\x03':
			if depth == 0 {
				prefix += "\x02" // window opened inside a mark
			} else {
				depth--
			}
		}
	}
	if depth > 0 {
		suffix = "\x03"
	}
	if end < len(raw) {
		suffix += "…"
	}
	return prefix + out + suffix
}

func makeEntry(path, name, ext string, size, mtime int64) types.Entry {
	typ := types.TypeFile
	if types.IsArchiveName(name) {
		typ = types.TypeArchive
	}
	return types.Entry{
		Name:  name,
		Path:  path,
		Type:  typ,
		Size:  size,
		Mtime: mtime,
		Mime:  mimeFor(ext),
	}
}

// mergeHits combines name and content hits: deduped by path (the name hit is
// kept, inheriting the content hit's snippet and the better score), then
// sorted by score descending with name hits winning ties.
func mergeHits(nameHits, contentHits []types.SearchHit) []types.SearchHit {
	return mergeHitsBy(nameHits, contentHits, nil)
}

// mergeHitsBy is mergeHits with an explicit ordering; nil less means
// relevance (score desc, name hits first, path).
func mergeHitsBy(nameHits, contentHits []types.SearchHit, less func(a, b types.SearchHit) bool) []types.SearchHit {
	out := make([]types.SearchHit, 0, len(nameHits)+len(contentHits))
	byPath := make(map[string]int, len(nameHits))
	for _, h := range nameHits {
		byPath[h.Entry.Path] = len(out)
		out = append(out, h)
	}
	for _, h := range contentHits {
		if i, ok := byPath[h.Entry.Path]; ok {
			if out[i].Snippet == "" {
				out[i].Snippet = h.Snippet
			}
			if h.Score > out[i].Score {
				out[i].Score = h.Score
			}
			continue
		}
		out = append(out, h)
	}
	if less != nil {
		sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].MatchedIn != out[j].MatchedIn {
			return out[i].MatchedIn == "name"
		}
		return out[i].Entry.Path < out[j].Entry.Path
	})
	return out
}
