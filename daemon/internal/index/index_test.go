package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"unraid-filebrowser/internal/types"
)

// --- helpers ---------------------------------------------------------------

func testConfig(roots ...string) types.IndexConfig {
	return types.IndexConfig{
		Roots:       roots,
		Schedule:    defaultSchedule,
		Parallelism: 2,
		Content:     types.ContentRules{MaxFileBytes: defaultMaxFileBytes},
	}
}

// newService opens a service whose browse roots are the config's roots (or
// a throwaway directory when the config has none).
func newService(t *testing.T, cfg types.IndexConfig) *Service {
	t.Helper()
	allowed := cfg.Roots
	if len(allowed) == 0 {
		allowed = []string{t.TempDir()}
	}
	return newServiceAllowed(t, cfg, allowed)
}

func newServiceAllowed(t *testing.T, cfg types.IndexConfig, allowed []string) *Service {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "index.db"), cfg, Options{AllowedRoots: allowed})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func waitIdle(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !s.crawlBusy.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("crawl did not finish in time")
}

func crawl(t *testing.T, s *Service) {
	t.Helper()
	if err := s.Rescan(""); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	waitIdle(t, s)
}

// nq builds a SearchQuery with the size filters unset (-1 per contract).
func nq(q, mode string) types.SearchQuery {
	return types.SearchQuery{Q: q, Mode: mode, MinSize: -1, MaxSize: -1}
}

func mustSearch(t *testing.T, s *Service, q types.SearchQuery) ([]types.SearchHit, int) {
	t.Helper()
	hits, total, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search(%q): %v", q.Q, err)
	}
	return hits, total
}

func hitPaths(hits []types.SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Entry.Path
	}
	return out
}

func apiCode(t *testing.T, err error) string {
	t.Helper()
	var ae *types.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *types.APIError, got %T: %v", err, err)
	}
	return ae.Code
}

// --- crawl + name search ---------------------------------------------------

func TestCrawlAndNameSearch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "report_2024.txt"), "annual numbers")
	writeFile(t, filepath.Join(root, "notes.md"), "misc")
	writeFile(t, filepath.Join(root, "sub", "the great escape.log"), "movie log")
	writeFile(t, filepath.Join(root, "sub", "server.log"), "lines")

	s := newService(t, testConfig(root))
	crawl(t, s)

	if got := s.Status().FilesIndexed; got != 4 {
		t.Fatalf("FilesIndexed = %d, want 4", got)
	}

	t.Run("prefix", func(t *testing.T) {
		hits, total := mustSearch(t, s, nq("repo", "name"))
		if total != 1 || len(hits) != 1 {
			t.Fatalf("total=%d hits=%v, want exactly report_2024.txt", total, hitPaths(hits))
		}
		h := hits[0]
		if h.Entry.Name != "report_2024.txt" || h.MatchedIn != "name" {
			t.Fatalf("unexpected hit %+v", h)
		}
		if h.Entry.Size != int64(len("annual numbers")) {
			t.Fatalf("Size = %d", h.Entry.Size)
		}
		if h.Entry.Type != types.TypeFile {
			t.Fatalf("Type = %s", h.Entry.Type)
		}
	})

	t.Run("multi-term is AND", func(t *testing.T) {
		_, total := mustSearch(t, s, nq("great escape", "name"))
		if total != 1 {
			t.Fatalf("total = %d, want 1", total)
		}
		_, total = mustSearch(t, s, nq("great server", "name"))
		if total != 0 {
			t.Fatalf("total = %d, want 0 (terms in different names)", total)
		}
	})

	t.Run("phrase", func(t *testing.T) {
		hits, total := mustSearch(t, s, nq(`"the great"`, "name"))
		if total != 1 || hits[0].Entry.Name != "the great escape.log" {
			t.Fatalf("phrase search: total=%d hits=%v", total, hitPaths(hits))
		}
		// Out-of-order phrase must not match (and the LIKE fallback must not
		// resurrect it as a substring).
		_, total = mustSearch(t, s, nq(`"great the"`, "name"))
		if total != 0 {
			t.Fatalf("reversed phrase matched, total=%d", total)
		}
	})

	t.Run("substring fallback", func(t *testing.T) {
		// "server.log" is one FTS token (tokenchars '.'), so a mid-token
		// substring never prefix-matches; the LIKE fallback must find it.
		hits, total := mustSearch(t, s, nq("erver", "name"))
		if total != 1 || hits[0].Entry.Name != "server.log" {
			t.Fatalf("fallback: total=%d hits=%v", total, hitPaths(hits))
		}
		if hits[0].Score != likeFallbackScore {
			t.Fatalf("fallback score = %v, want %v", hits[0].Score, likeFallbackScore)
		}
	})

	t.Run("dotted name via FTS prefix", func(t *testing.T) {
		hits, _ := mustSearch(t, s, nq("server.l", "name"))
		if len(hits) != 1 || hits[0].Entry.Name != "server.log" {
			t.Fatalf("dotted prefix: %v", hitPaths(hits))
		}
	})
}

// --- filters ---------------------------------------------------------------

func TestSearchFilters(t *testing.T) {
	root := t.TempDir()
	// All names share the term "data" so the query matches everything and the
	// filters do the narrowing.
	small := filepath.Join(root, "a", "data_small.txt")
	large := filepath.Join(root, "a", "data_large.log")
	other := filepath.Join(root, "b", "data_other.txt")
	writeFile(t, small, "x")                            // 1 byte
	writeFile(t, large, strings.Repeat("y", 100))       // 100 bytes
	writeFile(t, other, strings.Repeat("z", 10))        // 10 bytes
	old := time.Unix(1_000_000, 0)                      // mtimes far apart
	newer := time.Unix(2_000_000, 0)                    //
	if err := os.Chtimes(small, old, old); err != nil { // small = old
		t.Fatal(err)
	}
	if err := os.Chtimes(large, newer, newer); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, newer, newer); err != nil {
		t.Fatal(err)
	}

	s := newService(t, testConfig(root))
	crawl(t, s)

	t.Run("ext", func(t *testing.T) {
		q := nq("data", "name")
		q.Exts = []string{".TXT"} // dot + case must be normalized
		hits, total := mustSearch(t, s, q)
		if total != 2 {
			t.Fatalf("ext filter total=%d hits=%v", total, hitPaths(hits))
		}
		for _, h := range hits {
			if !strings.HasSuffix(h.Entry.Name, ".txt") {
				t.Fatalf("non-txt hit %s", h.Entry.Name)
			}
		}
	})

	t.Run("size", func(t *testing.T) {
		q := nq("data", "name")
		q.MinSize = 5
		q.MaxSize = 50
		hits, total := mustSearch(t, s, q)
		if total != 1 || hits[0].Entry.Name != "data_other.txt" {
			t.Fatalf("size filter: %v", hitPaths(hits))
		}
	})

	t.Run("mtime", func(t *testing.T) {
		q := nq("data", "name")
		q.After = 1_500_000
		_, total := mustSearch(t, s, q)
		if total != 2 {
			t.Fatalf("after filter total=%d", total)
		}
		q = nq("data", "name")
		q.Before = 1_500_000
		hits, total := mustSearch(t, s, q)
		if total != 1 || hits[0].Entry.Name != "data_small.txt" {
			t.Fatalf("before filter: %v", hitPaths(hits))
		}
	})

	t.Run("path scope", func(t *testing.T) {
		q := nq("data", "name")
		q.Path = filepath.Join(root, "b")
		hits, total := mustSearch(t, s, q)
		if total != 1 || hits[0].Entry.Name != "data_other.txt" {
			t.Fatalf("path scope: %v", hitPaths(hits))
		}
	})

	t.Run("pagination", func(t *testing.T) {
		q := nq("data", "name")
		q.Limit = 2
		hits, total := mustSearch(t, s, q)
		if total != 3 || len(hits) != 2 {
			t.Fatalf("page1: total=%d len=%d", total, len(hits))
		}
		q.Offset = 2
		hits, total = mustSearch(t, s, q)
		if total != 3 || len(hits) != 1 {
			t.Fatalf("page2: total=%d len=%d", total, len(hits))
		}
		q.Offset = 99
		hits, total = mustSearch(t, s, q)
		if total != 3 || len(hits) != 0 {
			t.Fatalf("past-end page: total=%d len=%d", total, len(hits))
		}
	})
}

// --- content indexing ------------------------------------------------------

func contentConfig(root string) types.IndexConfig {
	cfg := testConfig(root)
	cfg.Content = types.ContentRules{
		Enabled:      true,
		Extensions:   []string{"txt", "log"},
		MaxFileBytes: 64,
	}
	return cfg
}

func TestContentIndexing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "good.txt"), "the quick brown fox jumps")
	writeFile(t, filepath.Join(root, "binary.txt"), "head\x00tail quickish")      // NUL in first 8K
	writeFile(t, filepath.Join(root, "toobig.txt"), strings.Repeat("quick ", 20)) // 120 B > 64 cap
	writeFile(t, filepath.Join(root, "wrongext.md"), "quick as well")

	s := newService(t, contentConfig(root))
	crawl(t, s)

	hits, total := mustSearch(t, s, nq("quick", "content"))
	if total != 1 || len(hits) != 1 {
		t.Fatalf("content hits = %v (total %d), want only good.txt", hitPaths(hits), total)
	}
	h := hits[0]
	if h.Entry.Name != "good.txt" || h.MatchedIn != "content" {
		t.Fatalf("unexpected hit %+v", h)
	}
	if !strings.Contains(h.Snippet, "<mark>quick</mark>") {
		t.Fatalf("snippet %q lacks <mark>quick</mark>", h.Snippet)
	}
	if got := s.Status().ContentIndexed; got != 1 {
		t.Fatalf("ContentIndexed = %d, want 1", got)
	}

	// Phrase semantics against the body.
	_, total = mustSearch(t, s, nq(`"brown fox"`, "content"))
	if total != 1 {
		t.Fatalf(`"brown fox" total = %d`, total)
	}
	_, total = mustSearch(t, s, nq(`"fox brown"`, "content"))
	if total != 0 {
		t.Fatalf(`"fox brown" total = %d`, total)
	}
}

func TestContentIncludePaths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "inc", "in.txt"), "wanted magicword")
	writeFile(t, filepath.Join(root, "out", "out.txt"), "unwanted magicword")

	cfg := contentConfig(root)
	cfg.Content.IncludePaths = []string{filepath.Join(root, "inc")}
	s := newService(t, cfg)
	crawl(t, s)

	hits, total := mustSearch(t, s, nq("magicword", "content"))
	if total != 1 || hits[0].Entry.Name != "in.txt" {
		t.Fatalf("include-paths leak: %v", hitPaths(hits))
	}
}

func TestContentExtensionFallback(t *testing.T) {
	// With no configured extensions and the (nil-returning) owner policy, the
	// built-in safety list must apply; injecting a policy fn must override it.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.md"), "fallback needleone")
	writeFile(t, filepath.Join(root, "b.xyz"), "custom needletwo")

	cfg := testConfig(root)
	cfg.Content.Enabled = true // no Extensions
	s := newService(t, cfg)
	if s.skipDirFn == nil || s.contentExtsFn == nil {
		t.Fatal("policy hooks not wired to defaults")
	}
	crawl(t, s)
	_, total := mustSearch(t, s, nq("needleone", "content"))
	if total != 1 {
		t.Fatalf("safety-list ext md not indexed, total=%d", total)
	}
	_, total = mustSearch(t, s, nq("needletwo", "content"))
	if total != 0 {
		t.Fatalf("xyz indexed under safety list, total=%d", total)
	}

	// Injected policy list takes over from the safety list.
	s2 := newService(t, cfg)
	s2.contentExtsFn = func() []string { return []string{"xyz"} }
	crawl(t, s2)
	_, total, err := s2.Search(context.Background(), nq("needletwo", "content"))
	if err != nil || total != 1 {
		t.Fatalf("injected ext list: total=%d err=%v", total, err)
	}
}

// --- incremental rescan ----------------------------------------------------

func TestIncrementalRescan(t *testing.T) {
	root := t.TempDir()
	f1 := filepath.Join(root, "one.txt")
	f2 := filepath.Join(root, "two.txt")
	f3 := filepath.Join(root, "three.txt")
	writeFile(t, f1, "alpha content")
	writeFile(t, f2, "beta content")
	writeFile(t, f3, "gamma content")

	s := newService(t, contentConfig(root))
	crawl(t, s)
	if got := s.extracted.Load(); got != 3 {
		t.Fatalf("first crawl extracted %d, want 3", got)
	}

	// Unchanged rescan extracts nothing.
	crawl(t, s)
	if got := s.extracted.Load(); got != 3 {
		t.Fatalf("no-op rescan re-extracted (count %d)", got)
	}

	// Touch one file: only it is re-extracted.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(f1, later, later); err != nil {
		t.Fatal(err)
	}
	crawl(t, s)
	if got := s.extracted.Load(); got != 4 {
		t.Fatalf("touched rescan extracted %d total, want 4", got)
	}

	// Delete a file: row and content document are swept.
	if err := os.Remove(f2); err != nil {
		t.Fatal(err)
	}
	crawl(t, s)
	if got := s.Status().FilesIndexed; got != 2 {
		t.Fatalf("FilesIndexed after delete = %d, want 2", got)
	}
	_, total := mustSearch(t, s, nq("two", "name"))
	if total != 0 {
		t.Fatalf("deleted file still name-searchable")
	}
	_, total = mustSearch(t, s, nq("beta", "content"))
	if total != 0 {
		t.Fatalf("deleted file still content-searchable")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM files WHERE path = ?`, f2).Scan(&n); err != nil || n != 0 {
		t.Fatalf("files row survived delete (n=%d err=%v)", n, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM content_fts WHERE path = ?`, f2).Scan(&n); err != nil || n != 0 {
		t.Fatalf("content_fts row survived delete (n=%d err=%v)", n, err)
	}
}

func TestScopedRescan(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "ax.txt"), "1")
	writeFile(t, filepath.Join(root, "b", "bx.txt"), "1")

	s := newService(t, testConfig(root))
	crawl(t, s)
	lastFull := s.Status().LastFullScan
	if lastFull == 0 {
		t.Fatal("LastFullScan not stamped by full crawl")
	}

	// Delete under a/ but rescan only b/: a's stale row must survive (scoped
	// sweep) and LastFullScan must not move.
	if err := os.Remove(filepath.Join(root, "a", "ax.txt")); err != nil {
		t.Fatal(err)
	}
	if err := s.Rescan(filepath.Join(root, "b")); err != nil {
		t.Fatalf("scoped rescan: %v", err)
	}
	waitIdle(t, s)
	if _, total := mustSearch(t, s, nq("ax", "name")); total != 1 {
		t.Fatalf("scoped rescan swept outside its scope")
	}
	if got := s.Status().LastFullScan; got != lastFull {
		t.Fatalf("scoped rescan moved LastFullScan %d -> %d", lastFull, got)
	}

	// Scoped rescan of a/ now sweeps the deleted row.
	if err := s.Rescan(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	if _, total := mustSearch(t, s, nq("ax", "name")); total != 0 {
		t.Fatalf("scoped rescan did not sweep its own scope")
	}
}

// --- policy injection ------------------------------------------------------

func TestSkipDirInjection(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep", "kept.txt"), "1")
	writeFile(t, filepath.Join(root, "node_modules", "buried.txt"), "1")
	writeFile(t, filepath.Join(root, "keep", "node_modules", "alsoburied.txt"), "1")

	s := newService(t, testConfig(root))
	s.skipDirFn = func(name, _ string) bool { return name == "node_modules" }
	crawl(t, s)

	if got := s.Status().FilesIndexed; got != 1 {
		t.Fatalf("FilesIndexed = %d, want 1 (pruned dirs indexed?)", got)
	}
	if _, total := mustSearch(t, s, nq("buried", "name")); total != 0 {
		t.Fatalf("pruned subtree is searchable")
	}
	if _, total := mustSearch(t, s, nq("kept", "name")); total != 1 {
		t.Fatalf("kept file missing")
	}
}

// --- pause / resume --------------------------------------------------------

func TestPauseResume(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		writeFile(t, filepath.Join(root, "f"+strings.Repeat("x", i)+".txt"), "1")
	}
	s := newService(t, testConfig(root))

	s.Pause()
	if err := s.Rescan(""); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if !s.crawlBusy.Load() {
		t.Fatal("paused crawl finished")
	}
	if got := s.Status().FilesIndexed; got != 0 {
		t.Fatalf("paused crawl indexed %d files", got)
	}
	s.Resume()
	waitIdle(t, s)
	if got := s.Status().FilesIndexed; got != 20 {
		t.Fatalf("FilesIndexed after resume = %d, want 20", got)
	}
}

// --- subscribe -------------------------------------------------------------

func TestSubscribe(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 30; i++ {
		writeFile(t, filepath.Join(root, "s"+strings.Repeat("a", i)+".txt"), "1")
	}
	s := newService(t, testConfig(root))

	ch, cancel := s.Subscribe()
	defer cancel()

	// Seed snapshot arrives without any crawl.
	select {
	case st := <-ch:
		if st.State != stateIdle {
			t.Fatalf("seed state = %s", st.State)
		}
	case <-time.After(time.Second):
		t.Fatal("no seed status")
	}

	if err := s.Rescan(""); err != nil {
		t.Fatal(err)
	}
	sawCrawling, sawIdle := false, false
	deadline := time.After(10 * time.Second)
	for !sawIdle {
		select {
		case st, ok := <-ch:
			if !ok {
				t.Fatal("channel closed early")
			}
			switch st.State {
			case stateCrawling:
				sawCrawling = true
			case stateIdle:
				if st.FilesIndexed == 30 {
					sawIdle = true
				}
			}
		case <-deadline:
			t.Fatalf("no final idle status (sawCrawling=%v)", sawCrawling)
		}
	}
	if !sawCrawling {
		t.Fatal("never observed crawling state")
	}

	cancel()
	if _, ok := <-ch; ok {
		// Drain: after cancel the channel must eventually report closed.
		for range ch {
		}
	}

	// A subscriber that never reads must not block the crawler.
	ch2, cancel2 := s.Subscribe()
	defer cancel2()
	_ = ch2
	crawl(t, s)
}

// --- FTS-operator injection ------------------------------------------------

func TestQueryInjectionNeverErrors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "foo bar baz.txt"), "foo bar body")
	cfg := contentConfig(root)
	s := newService(t, cfg)
	crawl(t, s)

	queries := []string{
		"foo OR bar",
		"a NEAR b",
		`he said "unterminated`,
		`stray"quote`,
		`col:value`,
		`(paren`,
		`foo*`,
		`foo NOT bar`,
		`NEAR(a b)`,
		`^caret`,
		`term-with-dash`,
		`term.with.dots`,
		"\"phrase with \"\"doubled\"\" quotes\"",
	}
	for _, raw := range queries {
		for _, mode := range []string{"name", "content", "both"} {
			if _, _, err := s.Search(context.Background(), nq(raw, mode)); err != nil {
				t.Errorf("Search(%q, %s) errored: %v", raw, mode, err)
			}
		}
	}

	// Dropped operators degrade to plain terms: "foo OR bar" = foo AND bar.
	hits, total := mustSearch(t, s, nq("foo OR bar", "name"))
	if total != 1 || hits[0].Entry.Name != "foo bar baz.txt" {
		t.Fatalf("operator-stripped search: %v", hitPaths(hits))
	}

	// Queries with nothing searchable are a clean BAD_REQUEST.
	for _, raw := range []string{"", "   ", `*`, `""`, `"..."`, "OR", "NEAR"} {
		_, _, err := s.Search(context.Background(), nq(raw, "both"))
		if err == nil {
			t.Errorf("Search(%q) succeeded, want BAD_REQUEST", raw)
			continue
		}
		if code := apiCode(t, err); code != types.ErrBadRequest {
			t.Errorf("Search(%q) code = %s, want BAD_REQUEST", raw, code)
		}
	}
}

// --- both mode / merging ---------------------------------------------------

func TestBothModeDedupe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha.txt"), "alpha beta gamma")
	writeFile(t, filepath.Join(root, "other.txt"), "only alpha in the body")

	s := newService(t, contentConfig(root))
	crawl(t, s)

	hits, total := mustSearch(t, s, nq("alpha", "both"))
	if total != 2 || len(hits) != 2 {
		t.Fatalf("both-mode: total=%d hits=%v", total, hitPaths(hits))
	}
	byName := map[string]types.SearchHit{}
	for _, h := range hits {
		byName[h.Entry.Name] = h
	}
	a, ok := byName["alpha.txt"]
	if !ok {
		t.Fatalf("alpha.txt missing from %v", hitPaths(hits))
	}
	if a.MatchedIn != "name" {
		t.Fatalf("deduped hit MatchedIn = %s, want name", a.MatchedIn)
	}
	if !strings.Contains(a.Snippet, "<mark>alpha</mark>") {
		t.Fatalf("deduped hit lost its content snippet: %q", a.Snippet)
	}
	o := byName["other.txt"]
	if o.MatchedIn != "content" || o.Snippet == "" {
		t.Fatalf("content-only hit wrong: %+v", o)
	}
}

func TestMergeHitsOrdering(t *testing.T) {
	mk := func(path, in string, score float64) types.SearchHit {
		return types.SearchHit{Entry: types.Entry{Path: path}, Score: score, MatchedIn: in}
	}
	merged := mergeHits(
		[]types.SearchHit{mk("/n1", "name", 2.0), mk("/tie", "name", 1.0)},
		[]types.SearchHit{mk("/c1", "content", 3.0), mk("/tie2", "content", 1.0)},
	)
	if len(merged) != 4 {
		t.Fatalf("len = %d", len(merged))
	}
	if merged[0].Entry.Path != "/c1" || merged[1].Entry.Path != "/n1" {
		t.Fatalf("score ordering wrong: %v", hitPaths(merged))
	}
	// Equal scores: the name hit precedes the content hit.
	if merged[2].Entry.Path != "/tie" || merged[3].Entry.Path != "/tie2" {
		t.Fatalf("tie ordering wrong: %v", hitPaths(merged))
	}
}

// --- concurrency -----------------------------------------------------------

func TestConcurrentSearchDuringCrawl(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 400; i++ {
		name := fmt.Sprintf("file%03d.txt", i)
		writeFile(t, filepath.Join(root, "d"+string(rune('a'+i%7)), name), strings.Repeat("word ", 10))
	}
	s := newService(t, contentConfig(root))

	if err := s.Rescan(""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := s.Search(context.Background(), nq("file", "both")); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	waitIdle(t, s)
	close(stop)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent search failed during crawl: %v", err)
	}
	if got := s.Status().FilesIndexed; got != 400 {
		t.Fatalf("FilesIndexed = %d, want 400", got)
	}
}

// --- config / errors / lifecycle -------------------------------------------

func TestConfigValidation(t *testing.T) {
	base := t.TempDir()
	s := newServiceAllowed(t, testConfig(base), []string{base})
	bad := testConfig(base)
	bad.Schedule = "61 * * * *"
	if err := s.SetConfig(bad); err == nil {
		t.Fatal("SetConfig accepted invalid cron")
	} else if code := apiCode(t, err); code != types.ErrBadRequest {
		t.Fatalf("SetConfig invalid cron code = %s", code)
	}
	bad = testConfig("  ")
	if err := s.SetConfig(bad); err == nil {
		t.Fatal("SetConfig accepted blank root")
	}

	newRoot := filepath.Join(base, "sub")
	cfg := testConfig(newRoot)
	cfg.Parallelism = 4
	if err := s.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig valid: %v", err)
	}
	got := s.Config()
	if len(got.Roots) != 1 || got.Roots[0] != newRoot || got.Schedule != "0 3 * * *" {
		t.Fatalf("Config() = %+v", got)
	}
	// Returned config is a copy: mutating it must not leak back.
	got.Roots[0] = "/mutated"
	if s.Config().Roots[0] != newRoot {
		t.Fatal("Config() aliases internal state")
	}
}

func TestRescanErrors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "f.txt"), "1")
	s := newService(t, testConfig(root))

	if err := s.Rescan("/definitely/not/under/roots"); err == nil {
		t.Fatal("Rescan outside roots succeeded")
	} else if code := apiCode(t, err); code != types.ErrBadRequest {
		t.Fatalf("outside-roots code = %s", code)
	}

	s.Pause()
	if err := s.Rescan(""); err != nil {
		t.Fatal(err)
	}
	if err := s.Rescan(""); err == nil {
		t.Fatal("second concurrent Rescan succeeded")
	} else if code := apiCode(t, err); code != types.ErrIndexing {
		t.Fatalf("busy code = %s", code)
	}
	s.Resume()
	waitIdle(t, s)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Rescan(""); err == nil {
		t.Fatal("Rescan on closed service succeeded")
	} else if code := apiCode(t, err); code != types.ErrIndexing {
		t.Fatalf("closed code = %s", code)
	}
	if _, _, err := s.Search(context.Background(), nq("f", "name")); err == nil {
		t.Fatal("Search on closed service succeeded")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSearchBadMode(t *testing.T) {
	s := newService(t, testConfig(t.TempDir()))
	_, _, err := s.Search(context.Background(), nq("x", "banana"))
	if err == nil || apiCode(t, err) != types.ErrBadRequest {
		t.Fatalf("bad mode err = %v", err)
	}
}

func TestEmptyRootsCrawl(t *testing.T) {
	// A config with no roots is valid; a crawl must be a clean no-op (this
	// exercises the empty prefixCond path in sweep/contentPass).
	cfg := types.IndexConfig{}
	cfg.Content.Enabled = true
	s := newService(t, cfg)
	crawl(t, s)
	if got := s.Status().FilesIndexed; got != 0 {
		t.Fatalf("FilesIndexed = %d", got)
	}
}

func TestStatusAndDBBytes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "1")
	s := newService(t, testConfig(root))
	st := s.Status()
	if st.State != stateIdle || st.LastFullScan != 0 {
		t.Fatalf("fresh status = %+v", st)
	}
	if st.DBBytes <= 0 {
		t.Fatalf("DBBytes = %d, want > 0 (db file exists)", st.DBBytes)
	}
	crawl(t, s)
	st = s.Status()
	if st.FilesIndexed != 1 || st.LastFullScan == 0 {
		t.Fatalf("post-crawl status = %+v", st)
	}

	// LastFullScan survives reopen (persisted in meta).
	dbPath := s.dbPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := New(dbPath, testConfig(root), Options{AllowedRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Status().LastFullScan; got != st.LastFullScan {
		t.Fatalf("LastFullScan not persisted: %d != %d", got, st.LastFullScan)
	}
	if got := s2.Status().FilesIndexed; got != 1 {
		t.Fatalf("reopened FilesIndexed = %d", got)
	}
}

// --- scheduler -------------------------------------------------------------

func TestScheduler(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sched.txt"), "1")
	cfg := testConfig(root)
	cfg.Schedule = "@every 1s" // ParseStandard accepts descriptors
	s := newService(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunScheduler(ctx)
		close(done)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && s.Status().LastFullScan == 0 {
		time.Sleep(25 * time.Millisecond)
	}
	if s.Status().LastFullScan == 0 {
		cancel()
		t.Fatal("scheduler never fired a full scan")
	}

	// Live reschedule while the scheduler runs must not error or panic.
	cfg.Schedule = "0 3 * * *"
	if err := s.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig during scheduler: %v", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunScheduler did not return after cancel")
	}
	waitIdle(t, s)
}

// --- root confinement (finding 1) ------------------------------------------

func TestRootConfinement(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "mnt", "user")
	sibling := allowed + "2" // /mnt/user2: shares the string prefix, not the tree
	for _, d := range []string{filepath.Join(allowed, "share"), sibling, filepath.Join(base, "boot")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	s := newServiceAllowed(t, testConfig(allowed), []string{allowed})
	got := s.AllowedRoots()
	if len(got) != 1 || got[0] != allowed {
		t.Fatalf("AllowedRoots() = %v", got)
	}
	got[0] = "/mutated"
	if s.AllowedRoots()[0] != allowed {
		t.Fatal("AllowedRoots() aliases internal state")
	}

	bad := []struct {
		name string
		mut  func(c *types.IndexConfig)
	}{
		{"root /", func(c *types.IndexConfig) { c.Roots = []string{"/"} }},
		{"root outside", func(c *types.IndexConfig) { c.Roots = []string{filepath.Join(base, "boot")} }},
		{"root sibling prefix", func(c *types.IndexConfig) { c.Roots = []string{sibling} }},
		{"root parent", func(c *types.IndexConfig) { c.Roots = []string{filepath.Dir(allowed)} }},
		{"root relative", func(c *types.IndexConfig) { c.Roots = []string{"mnt/user"} }},
		{"root trailing slash", func(c *types.IndexConfig) { c.Roots = []string{allowed + "/"} }},
		{"root dotdot", func(c *types.IndexConfig) { c.Roots = []string{allowed + "/../user2"} }},
		{"root blank", func(c *types.IndexConfig) { c.Roots = []string{allowed, " "} }},
		{"include outside", func(c *types.IndexConfig) { c.Content.IncludePaths = []string{base} }},
		{"include sibling", func(c *types.IndexConfig) { c.Content.IncludePaths = []string{sibling} }},
		{"include relative", func(c *types.IndexConfig) { c.Content.IncludePaths = []string{"share"} }},
		{"schedule empty", func(c *types.IndexConfig) { c.Schedule = "" }},
		{"schedule invalid", func(c *types.IndexConfig) { c.Schedule = "61 * * * *" }},
		{"parallelism 0", func(c *types.IndexConfig) { c.Parallelism = 0 }},
		{"parallelism 17", func(c *types.IndexConfig) { c.Parallelism = 17 }},
		{"maxFileBytes 0", func(c *types.IndexConfig) { c.Content.MaxFileBytes = 0 }},
		{"maxFileBytes over 64MiB", func(c *types.IndexConfig) { c.Content.MaxFileBytes = 64<<20 + 1 }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(allowed)
			tc.mut(&cfg)
			err := s.SetConfig(cfg)
			if err == nil {
				t.Fatalf("SetConfig accepted %+v", cfg)
			}
			if code := apiCode(t, err); code != types.ErrBadRequest {
				t.Fatalf("code = %s (%v), want BAD_REQUEST", code, err)
			}
			if live := s.Config(); len(live.Roots) != 1 || live.Roots[0] != allowed {
				t.Fatalf("rejected config leaked into live config: %+v", live)
			}
		})
	}

	good := testConfig(filepath.Join(allowed, "share"))
	good.Content.IncludePaths = []string{filepath.Join(allowed, "share", "deep")}
	good.Parallelism = 16
	good.Content.MaxFileBytes = 64 << 20
	if err := s.SetConfig(good); err != nil {
		t.Fatalf("SetConfig rejected valid config: %v", err)
	}
	// Rescan is confined the same way (and cannot be fooled by "..").
	if err := s.Rescan(filepath.Join(allowed, "share", "..", "..", "user2")); err == nil {
		t.Fatal("Rescan escaped the roots via ..")
	}
}

func TestNewSanitizesPersistedConfig(t *testing.T) {
	allowed := t.TempDir()
	inside := filepath.Join(allowed, "share")
	cfg := types.IndexConfig{
		Roots:       []string{"/", "/boot", inside + "/", "relative", allowed + "2"},
		Schedule:    "not a cron",
		Parallelism: 99,
		Content: types.ContentRules{
			IncludePaths: []string{"/etc", inside},
			MaxFileBytes: -5,
		},
	}
	s, err := New(filepath.Join(t.TempDir(), "index.db"), cfg, Options{AllowedRoots: []string{allowed + "/"}})
	if err != nil {
		t.Fatalf("New refused to start on a bad persisted config: %v", err)
	}
	defer s.Close()
	if got := s.AllowedRoots(); len(got) != 1 || got[0] != allowed {
		t.Fatalf("AllowedRoots not cleaned: %v", got)
	}
	live := s.Config()
	if len(live.Roots) != 1 || live.Roots[0] != inside {
		t.Fatalf("Roots = %v, want [%s]", live.Roots, inside)
	}
	if len(live.Content.IncludePaths) != 1 || live.Content.IncludePaths[0] != inside {
		t.Fatalf("IncludePaths = %v", live.Content.IncludePaths)
	}
	if live.Schedule != defaultSchedule || live.Parallelism != maxParallelism || live.Content.MaxFileBytes != defaultMaxFileBytes {
		t.Fatalf("sanitized config = %+v", live)
	}
	// The sanitized config is what SetConfig would accept.
	if err := s.SetConfig(live); err != nil {
		t.Fatalf("sanitized config fails validation: %v", err)
	}

	cfg2 := types.IndexConfig{Roots: []string{allowed}, Content: types.ContentRules{MaxFileBytes: 100 << 20}}
	s2, err := New(filepath.Join(t.TempDir(), "index.db"), cfg2, Options{AllowedRoots: []string{allowed}})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if l := s2.Config(); l.Parallelism != defaultParallelism || l.Content.MaxFileBytes != maxMaxFileBytes || l.Schedule != defaultSchedule {
		t.Fatalf("defaults/clamps wrong: %+v", l)
	}

	// The browse roots themselves are mandatory and must be absolute.
	if _, err := New(filepath.Join(t.TempDir(), "index.db"), testConfig(allowed), Options{}); err == nil {
		t.Fatal("New accepted empty AllowedRoots")
	}
	if _, err := New(filepath.Join(t.TempDir(), "index.db"), testConfig(allowed), Options{AllowedRoots: []string{"rel"}}); err == nil {
		t.Fatal("New accepted relative AllowedRoots")
	}
}

// --- root narrowing: sweep + search predicate (finding 2) -------------------

func TestNarrowRootsSweepsAndFilters(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "keep")
	drop := filepath.Join(root, "drop")
	writeFile(t, filepath.Join(keep, "shared_k.txt"), "sharedword keepbody")
	writeFile(t, filepath.Join(drop, "shared_d.txt"), "sharedword dropbody")

	cfg := contentConfig(root)
	s := newServiceAllowed(t, cfg, []string{root})
	crawl(t, s)
	for _, mode := range []string{"name", "content", "both"} {
		if _, total := mustSearch(t, s, nq(mode2q(mode), mode)); total != 2 {
			t.Fatalf("before narrowing, %s total = %d, want 2", mode, total)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	dropped := filepath.Join(drop, "shared_d.txt")

	// Narrowing only the content include paths drops content, keeps names.
	cfg.Content.IncludePaths = []string{keep}
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if hits, total := mustSearch(t, s, nq("sharedword", "content")); total != 1 || hits[0].Entry.Path == dropped {
		t.Fatalf("content outside include paths survived: %v", hitPaths(hits))
	}
	if _, total := mustSearch(t, s, nq("shared", "name")); total != 2 {
		t.Fatalf("include-path narrowing removed name rows, total=%d", total)
	}
	if n := count(`SELECT COUNT(*) FROM content_fts WHERE path = ?`, dropped); n != 0 {
		t.Fatal("content_fts row outside include paths not deleted")
	}
	if n := count(`SELECT content_indexed FROM files WHERE path = ?`, dropped); n != 0 {
		t.Fatal("content_indexed not reset for a dropped document")
	}
	if st := s.Status(); st.FilesIndexed != 2 || st.ContentIndexed != 1 {
		t.Fatalf("status after include narrowing = %+v", st)
	}

	// Narrowing the roots removes everything outside, immediately.
	cfg.Roots = []string{keep}
	cfg.Content.IncludePaths = nil
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"name", "content", "both"} {
		hits, total := mustSearch(t, s, nq(mode2q(mode), mode))
		if total != 1 || len(hits) != 1 || !strings.HasPrefix(hits[0].Entry.Path, keep+"/") {
			t.Fatalf("after narrowing, %s: total=%d hits=%v", mode, total, hitPaths(hits))
		}
	}
	if n := count(`SELECT COUNT(*) FROM files WHERE path = ?`, dropped); n != 0 {
		t.Fatal("files row outside roots not deleted")
	}
	if n := count(`SELECT COUNT(*) FROM content_fts WHERE path = ?`, dropped); n != 0 {
		t.Fatal("content_fts row outside roots not deleted")
	}
	if n := count(`SELECT COUNT(*) FROM names_fts WHERE names_fts MATCH '"shared_d"*'`); n != 0 {
		t.Fatal("names_fts row outside roots not deleted")
	}
	if st := s.Status(); st.FilesIndexed != 1 || st.ContentIndexed != 1 {
		t.Fatalf("status after root narrowing = %+v", st)
	}

	// Second belt: a stale row that somehow exists outside the roots (e.g.
	// a crawl that raced the config change) is never returned by any mode.
	planted := filepath.Join(drop, "shared_planted.txt")
	if _, err := s.db.Exec(`INSERT INTO files (path, dir, name, ext, size, mtime, gen) VALUES (?, ?, ?, 'txt', 1, 1, 1)`,
		planted, drop, "shared_planted.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO content_fts (path, body) VALUES (?, 'sharedword planted')`, planted); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"name", "content", "both"} {
		hits, total := mustSearch(t, s, nq(mode2q(mode), mode))
		if total != 1 || len(hits) != 1 || hits[0].Entry.Path == planted {
			t.Fatalf("planted out-of-root row leaked in %s: total=%d %v", mode, total, hitPaths(hits))
		}
	}
	// The LIKE fallback path is filtered too ("lanted" is mid-token).
	if hits, total := mustSearch(t, s, nq("lanted", "name")); total != 0 {
		t.Fatalf("LIKE fallback leaked out-of-root row: %v", hitPaths(hits))
	}
	// And the next crawl physically removes it.
	crawl(t, s)
	if n := count(`SELECT COUNT(*) FROM files WHERE path = ?`, planted); n != 0 {
		t.Fatal("crawl-end sweep left the out-of-root row")
	}

	// Reopening with the narrowed config sweeps rows a wider persisted root
	// set left behind (simulated by planting again, then reopening).
	if _, err := s.db.Exec(`INSERT INTO files (path, dir, name, ext, size, mtime, gen) VALUES (?, ?, ?, 'txt', 1, 1, 1)`,
		planted, drop, "shared_planted.txt"); err != nil {
		t.Fatal(err)
	}
	dbPath := s.dbPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := New(dbPath, cfg, Options{AllowedRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM files WHERE path = ?`, planted).Scan(&n); err != nil || n != 0 {
		t.Fatalf("startup sweep left the out-of-root row (n=%d err=%v)", n, err)
	}
	if got := s2.Status().FilesIndexed; got != 1 {
		t.Fatalf("reopened FilesIndexed = %d, want 1", got)
	}
}

// mode2q picks a query that matches both fixture files of
// TestNarrowRootsSweepsAndFilters in the given mode.
func mode2q(mode string) string {
	if mode == "content" {
		return "sharedword"
	}
	return "shared" // name prefix; in both mode the content branch adds nothing new
}

// --- bounded paging (finding 3) ---------------------------------------------

func TestSearchPagingBounded(t *testing.T) {
	root := "/srv/paging-fixture" // never touched on disk: rows are inserted directly
	s := newServiceAllowed(t, testConfig(root), []string{root})

	// Generate the fixture inside SQLite: 50k rows through the driver one
	// by one is prohibitively slow under -race.
	const nFiles = 50_000
	if _, err := s.db.Exec(`
		WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?2)
		INSERT INTO files (path, dir, name, ext, size, mtime, gen)
		SELECT ?1 || '/d' || printf('%02d', i % 100) || '/file' || printf('%05d', i) || '.txt',
		       ?1 || '/d' || printf('%02d', i % 100),
		       'file' || printf('%05d', i) || '.txt', 'txt', i, i, 1
		FROM seq`, root, nFiles); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO content_fts (path, body)
		SELECT path, 'needle file number ' || size FROM files WHERE size % 10 = 0`); err != nil {
		t.Fatal(err)
	}
	s.recount(context.Background())
	if got := s.Status(); got.FilesIndexed != nFiles || got.ContentIndexed != nFiles/10 {
		t.Fatalf("fixture: %+v", got)
	}

	one := func(q, mode string) types.SearchQuery {
		sq := nq(q, mode)
		sq.Limit = 1
		return sq
	}
	cases := []struct {
		name  string
		q     types.SearchQuery
		total int
	}{
		{"name fts", one("file", "name"), nFiles},
		{"content fts", one("needle", "content"), nFiles / 10},
		{"both", one("file", "both"), nFiles},
		{"like fallback", one("ile0", "name"), 10_000}, // file0xxxx
	}
	// Warm once so one-off driver/statement setup is not attributed to a case.
	mustSearch(t, s, one("file", "both"))
	const maxAlloc = 5 << 20
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			hits, total := mustSearch(t, s, tc.q)
			took := time.Since(start)
			runtime.ReadMemStats(&after)
			delta := after.TotalAlloc - before.TotalAlloc
			t.Logf("%s: total=%d hits=%d alloc=%d bytes took=%s", tc.name, total, len(hits), delta, took)
			if total != tc.total || len(hits) != 1 {
				t.Fatalf("total=%d hits=%d, want total=%d hits=1", total, len(hits), tc.total)
			}
			if delta > maxAlloc {
				t.Fatalf("limit=1 search allocated %d bytes (> %d): rows are being materialized", delta, maxAlloc)
			}
		})
	}

	// The remaining subtests page over the "file0" prefix: exactly 10,000
	// rows (file00000..file09999), enough to exercise the window cap while
	// keeping each query cheap under -race.
	const nPrefix = 10_000
	t.Run("pages are stable and disjoint", func(t *testing.T) {
		seen := map[string]bool{}
		for page := 0; page < 3; page++ {
			q := nq("file0", "name")
			q.Limit = 7
			q.Offset = page * 7
			hits, total := mustSearch(t, s, q)
			if total != nPrefix || len(hits) != 7 {
				t.Fatalf("page %d: total=%d len=%d", page, total, len(hits))
			}
			for _, p := range hitPaths(hits) {
				if seen[p] {
					t.Fatalf("path %s repeated across pages", p)
				}
				seen[p] = true
			}
			// Re-running the same page yields the same rows (deterministic order).
			again, _ := mustSearch(t, s, q)
			if strings.Join(hitPaths(again), ",") != strings.Join(hitPaths(hits), ",") {
				t.Fatalf("page %d not stable", page)
			}
		}
		// Both-mode paging past the end is clean.
		q := nq("needle", "both")
		q.Offset = nFiles/10 + 5
		if hits, total := mustSearch(t, s, q); total != nFiles/10 || len(hits) != 0 {
			t.Fatalf("past-end both page: total=%d len=%d", total, len(hits))
		}
	})

	t.Run("window cap", func(t *testing.T) {
		q := nq("file0", "name")
		q.Offset = maxSearchWindow - 1
		q.Limit = 2
		if _, _, err := s.Search(context.Background(), q); err == nil || apiCode(t, err) != types.ErrBadRequest {
			t.Fatalf("offset+limit > %d accepted: %v", maxSearchWindow, err)
		}
		q.Offset = maxSearchWindow - 2
		if hits, _ := mustSearch(t, s, q); len(hits) != 2 {
			t.Fatalf("offset+limit == cap rejected or short: %d hits", len(hits))
		}
	})

	t.Run("like fallback needs 3 runes", func(t *testing.T) {
		// "le" is a mid-token substring of every name but too short for a scan.
		if _, total := mustSearch(t, s, nq("le", "name")); total != 0 {
			t.Fatalf("2-rune LIKE fallback ran, total=%d", total)
		}
	})
}

// --- db permissions (finding 4) --------------------------------------------

func TestDBFilePermissions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "1")
	dir := filepath.Join(t.TempDir(), "data")
	// Pre-existing world-readable dir from an older build must be tightened.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "index.db")
	s, err := New(dbPath, testConfig(root), Options{AllowedRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	perm := func(p string) os.FileMode {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		return fi.Mode().Perm()
	}
	if got := perm(dir); got != 0o700 {
		t.Fatalf("data dir mode = %o, want 700", got)
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if got := perm(p); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", p, got)
		}
	}
	crawl(t, s)
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			if got := perm(p); got != 0o600 {
				t.Fatalf("after crawl %s mode = %o, want 600", p, got)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := perm(dbPath); got != 0o600 {
		t.Fatalf("after close db mode = %o, want 600", got)
	}
}

// --- boundary-aware root checks, including "/" (finding 5) -----------------

func TestWithinBoundary(t *testing.T) {
	cases := []struct {
		p, root string
		want    bool
	}{
		{"/mnt/user", "/mnt/user", true},
		{"/mnt/user/x/y", "/mnt/user", true},
		{"/mnt/user2", "/mnt/user", false},
		{"/mnt/user2/x", "/mnt/user", false},
		{"/mnt", "/mnt/user", false},
		{"/", "/mnt/user", false},
		{"/", "/", true},
		{"/etc/shadow", "/", true},
		{"/mnt/user", "/", true},
	}
	for _, tc := range cases {
		if got := within(tc.p, tc.root); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.p, tc.root, got, tc.want)
		}
	}

	// A service rooted at "/" (legitimate when the admin sets it on flash)
	// must recognise every absolute path as in-root.
	s := newServiceAllowed(t, testConfig("/"), []string{"/"})
	if !s.pathInRoots("/etc/hosts") || !s.pathInRoots("/") {
		t.Fatal("root / does not contain absolute paths")
	}
	if s.pathInRoots("relative/path") {
		t.Fatal("root / contains a relative path")
	}
	// Search scope for root "/" is a valid, all-matching predicate.
	if _, _, err := s.Search(context.Background(), nq("anything", "both")); err != nil {
		t.Fatalf("search with root /: %v", err)
	}
}

// --- snippet length bound (finding 6) --------------------------------------

func TestSnippetBounded(t *testing.T) {
	balanced := func(t *testing.T, out string) {
		t.Helper()
		if o, c := strings.Count(out, "<mark>"), strings.Count(out, "</mark>"); o != c {
			t.Fatalf("unbalanced marks (%d open, %d close) in %q", o, c, out)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("snippet is not valid UTF-8")
		}
		if n := utf8.RuneCountInString(out); n > maxSnippetRunes*6 {
			// "<" escapes to 4 runes and marks add a few; anything near the
			// raw size means truncation did not happen.
			t.Fatalf("snippet has %d runes", n)
		}
	}

	t.Run("unit: huge token after the mark", func(t *testing.T) {
		raw := "aaa \x02needle\x03 " + strings.Repeat("<", 200_000) + " aaa"
		out := renderSnippet(raw)
		balanced(t, out)
		if !strings.Contains(out, "<mark>needle</mark>") || !strings.HasPrefix(out, "aaa ") || !strings.HasSuffix(out, "…") {
			t.Fatalf("centred window lost the mark or edges: %.80q", out)
		}
	})
	t.Run("unit: mark cut at the window end", func(t *testing.T) {
		out := renderSnippet("\x02" + strings.Repeat("x", 100_000) + "\x03")
		balanced(t, out)
		if !strings.HasPrefix(out, "<mark>") {
			t.Fatalf("%.40q", out)
		}
	})
	t.Run("unit: window opens inside a mark", func(t *testing.T) {
		raw := strings.Repeat("p", 1000) + "\x03" + strings.Repeat("q", 100) + "\x02m\x03" + strings.Repeat("r", 1000)
		out := renderSnippet(raw)
		balanced(t, out)
		if !strings.HasPrefix(out, "…<mark>") {
			t.Fatalf("%.40q", out)
		}
	})
	t.Run("unit: multibyte runes are not split", func(t *testing.T) {
		raw := strings.Repeat("é", 700) + "\x02ñ\x03" + strings.Repeat("ü", 700)
		balanced(t, renderSnippet(raw))
	})
	t.Run("unit: short snippets pass through", func(t *testing.T) {
		raw := "the \x02quick\x03 brown fox"
		if got := renderSnippet(raw); got != "the <mark>quick</mark> brown fox" {
			t.Fatalf("%q", got)
		}
	})

	t.Run("end to end", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "bomb.txt"), "aaa "+strings.Repeat("<", 300_000)+" aaa")
		cfg := contentConfig(root)
		cfg.Content.MaxFileBytes = 1 << 20
		s := newServiceAllowed(t, cfg, []string{root})
		crawl(t, s)
		hits, total := mustSearch(t, s, nq("aaa", "content"))
		if total != 1 {
			t.Fatalf("total = %d", total)
		}
		balanced(t, hits[0].Snippet)
		if len(hits[0].Snippet) > 8*1024 {
			t.Fatalf("snippet is %d bytes", len(hits[0].Snippet))
		}
		if !strings.Contains(hits[0].Snippet, "<mark>aaa</mark>") {
			t.Fatalf("snippet lost its mark: %.80q", hits[0].Snippet)
		}
	})
}
