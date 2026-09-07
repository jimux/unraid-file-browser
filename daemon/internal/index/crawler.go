package index

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"unraid-filebrowser/internal/types"
)

const (
	metaBatchSize    = 500  // file upserts per transaction (metadata pass)
	contentBatchSize = 100  // extracted documents per transaction (content pass)
	sniffLen         = 8192 // leading bytes checked for NUL (binary sniff)
)

type fileRec struct {
	path, dir, name, ext, mimeclass string
	size, mtime                     int64
}

type extractResult struct {
	path string
	body string
	ok   bool // false: skipped (unreadable/binary) — mark processed, no content row
}

// runCrawl performs one crawl: a single-threaded metadata walk (stat only),
// a scoped mark-and-sweep of vanished rows, then — when content indexing is
// enabled — a parallel content-extraction pass. scope == "" means all
// configured roots (a "full" scan, which also stamps LastFullScan).
func (s *Service) runCrawl(ctx context.Context, scope string) {
	cfg := s.Config()
	full := scope == ""
	roots := make([]string, 0, len(cfg.Roots))
	if full {
		for _, r := range cfg.Roots {
			roots = append(roots, filepath.Clean(r))
		}
	} else {
		roots = append(roots, filepath.Clean(scope))
	}

	s.state.Store(stateCrawling)
	s.setProgress(0)
	s.notify(true)
	defer func() {
		bg := context.Background()
		// The config may have narrowed while this crawl (which used a
		// snapshot of the old roots) was running: drop anything it wrote
		// outside the live roots before reporting idle.
		if err := s.sweepOutsideRoots(bg); err != nil {
			log.Printf("index: out-of-root sweep failed: %v", err)
		}
		secureDBFiles(s.dbPath)
		s.recount(bg)
		s.state.Store(stateIdle)
		s.current.Store("")
		s.setProgress(0)
		s.notify(true)
	}()

	gen, err := s.nextGeneration(ctx)
	if err != nil {
		log.Printf("index: cannot start crawl generation: %v", err)
		return
	}

	// Progress denominator: the row count previously known inside the scope.
	prior := s.countInScope(ctx, roots)
	var seen int64
	for _, root := range roots {
		if ctx.Err() != nil {
			break
		}
		s.walkRoot(ctx, root, gen, prior, &seen)
	}
	if ctx.Err() != nil {
		return // canceled: leave rows alone, do not sweep on a partial walk
	}
	s.sweep(ctx, roots, gen)

	if cfg.Content.Enabled && ctx.Err() == nil {
		s.state.Store(stateExtracting)
		s.setProgress(0)
		s.notify(true)
		s.contentPass(ctx, cfg, roots)
	}

	if full && ctx.Err() == nil {
		now := time.Now().Unix()
		if err := s.setMeta(ctx, "last_full_scan", strconv.FormatInt(now, 10)); err == nil {
			s.lastFullScan.Store(now)
		}
	}
}

func (s *Service) countInScope(ctx context.Context, roots []string) int64 {
	cond, args := prefixCond("path", roots)
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE `+cond, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// upsertSQL stamps the new generation on every seen row. When size+mtime are
// unchanged the row's content_indexed state is preserved (no re-extraction);
// when they changed it is reset to 0 so the content pass picks the file up
// again. It never touches name/dir, so the names_fts update trigger
// (UPDATE OF name, dir) does not fire and the FTS index is not churned.
const upsertSQL = `
INSERT INTO files (path, dir, name, ext, size, mtime, mimeclass, content_indexed, gen)
VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)
ON CONFLICT(path) DO UPDATE SET
	gen             = excluded.gen,
	content_indexed = CASE WHEN files.size = excluded.size AND files.mtime = excluded.mtime
	                       THEN files.content_indexed ELSE 0 END,
	size            = excluded.size,
	mtime           = excluded.mtime,
	mimeclass       = excluded.mimeclass`

// walkRoot is the single-threaded metadata pass over one root: stat-only,
// symlinks never followed (and not indexed — the array has loops), SkipDir
// policy pruning, batched upserts (~metaBatchSize rows per transaction).
func (s *Service) walkRoot(ctx context.Context, root string, gen, prior int64, seen *int64) {
	batch := make([]fileRec, 0, metaBatchSize)
	lastCount := time.Now()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.flushBatch(ctx, batch, gen); err != nil && ctx.Err() == nil {
			log.Printf("index: metadata batch failed: %v", err)
		}
		batch = batch[:0]
		// Refresh the visible row count at most ~1/sec (COUNT(*) is not free
		// on a large table).
		if time.Since(lastCount) >= time.Second {
			var n int64
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&n); err == nil {
				s.filesIndexed.Store(n)
			}
			lastCount = time.Now()
		}
		s.notify(false)
	}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if walkErr != nil {
			return nil // unreadable entry: skip, keep walking
		}
		if d.IsDir() {
			if p != root && s.skipDirFn(d.Name(), p) {
				return filepath.SkipDir
			}
			if err := s.waitGate(ctx); err != nil {
				return filepath.SkipAll
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks (never followed), sockets, devices, ...
		}
		if err := s.waitGate(ctx); err != nil {
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		s.current.Store(p)
		*seen++
		if prior > 0 {
			s.setProgress(min(0.99, float64(*seen)/float64(prior)))
		}
		name := d.Name()
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
		batch = append(batch, fileRec{
			path:      p,
			dir:       filepath.Dir(p),
			name:      name,
			ext:       ext,
			mimeclass: mimeClass(name, ext),
			size:      info.Size(),
			mtime:     info.ModTime().Unix(),
		})
		if len(batch) >= metaBatchSize {
			flush()
		}
		return nil
	})
	if err != nil {
		log.Printf("index: walk %s: %v", root, err)
	}
	flush()
}

func (s *Service) flushBatch(ctx context.Context, batch []fileRec, gen int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, upsertSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range batch {
		if _, err := stmt.ExecContext(ctx, r.path, r.dir, r.name, r.ext, r.size, r.mtime, r.mimeclass, gen); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// sweep deletes rows inside the crawled scope that the walk did not stamp
// with the current generation (mark-and-sweep): those files no longer exist.
// Their content_fts documents are removed first.
func (s *Service) sweep(ctx context.Context, roots []string, gen int64) {
	cond, args := prefixCond("path", roots)
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM content_fts WHERE path IN (SELECT path FROM files WHERE gen < ? AND `+cond+`)`,
		append([]any{gen}, args...)...); err != nil {
		log.Printf("index: content sweep failed: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM files WHERE gen < ? AND `+cond, append([]any{gen}, args...)...); err != nil {
		log.Printf("index: sweep failed: %v", err)
	}
}

// contentPass extracts text from files that (a) sit under an IncludePaths
// prefix (an empty IncludePaths list means everything in scope), (b) carry
// an allowlisted extension (config > DefaultContentExtensions() > built-in
// safety list), (c) are within MaxFileBytes, and (d) still have
// content_indexed = 0 (new or changed since last extraction). Extraction
// runs on cfg.Parallelism workers feeding one batching writer goroutine
// (WAL-friendly single writer, plain sequential reads — no readahead
// tricks).
func (s *Service) contentPass(ctx context.Context, cfg types.IndexConfig, roots []string) {
	rules := cfg.Content
	exts := rules.Extensions
	if len(exts) == 0 {
		exts = s.contentExtsFn()
	}
	if len(exts) == 0 {
		exts = safetyContentExts
	}
	maxB := rules.MaxFileBytes
	if maxB <= 0 {
		maxB = defaultMaxFileBytes
	}

	scopeCond, scopeArgs := prefixCond("path", roots)

	// Drop stale content documents of changed files up front, so a file that
	// changed and no longer qualifies does not keep its old text around.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM content_fts WHERE path IN (SELECT path FROM files WHERE content_indexed = 0 AND `+scopeCond+`)`,
		scopeArgs...); err != nil {
		log.Printf("index: stale content cleanup failed: %v", err)
	}

	// Collect candidates first so no read cursor stays open during writes.
	q := `SELECT path FROM files WHERE content_indexed = 0 AND size <= ? AND ext IN (` +
		placeholders(len(exts)) + `) AND ` + scopeCond
	args := make([]any, 0, 2+len(exts)+len(scopeArgs))
	args = append(args, maxB)
	for _, e := range exts {
		args = append(args, strings.ToLower(strings.TrimPrefix(e, ".")))
	}
	args = append(args, scopeArgs...)
	if len(rules.IncludePaths) > 0 {
		incCond, incArgs := prefixCond("path", rules.IncludePaths)
		q += ` AND ` + incCond
		args = append(args, incArgs...)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		log.Printf("index: content candidate query failed: %v", err)
		return
	}
	var cands []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			cands = append(cands, p)
		}
	}
	rows.Close()
	total := len(cands)
	if total == 0 {
		return
	}

	par := cfg.Parallelism
	if par <= 0 {
		par = defaultParallelism
	}

	jobs := make(chan string)
	results := make(chan extractResult, 64)
	var workers sync.WaitGroup
	for i := 0; i < par; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for p := range jobs {
				if err := s.waitGate(ctx); err != nil {
					continue // canceled: drain remaining jobs quickly
				}
				if ctx.Err() != nil {
					continue
				}
				s.current.Store(p)
				body, ok := extractText(p, maxB)
				s.extracted.Add(1)
				select {
				case results <- extractResult{path: p, body: body, ok: ok}:
				case <-ctx.Done():
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, p := range cands {
			select {
			case jobs <- p:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	// Single batching writer.
	done := 0
	batch := make([]extractResult, 0, contentBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if ctx.Err() == nil {
			if err := s.flushContent(ctx, batch); err != nil {
				log.Printf("index: content batch failed: %v", err)
			}
		}
		done += len(batch)
		s.setProgress(min(0.99, float64(done)/float64(total)))
		s.notify(false)
		batch = batch[:0]
	}
	for r := range results {
		batch = append(batch, r)
		if len(batch) >= contentBatchSize {
			flush()
		}
	}
	flush()
}

func (s *Service) flushContent(ctx context.Context, batch []extractResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	del, err := tx.PrepareContext(ctx, `DELETE FROM content_fts WHERE path = ?`)
	if err != nil {
		return err
	}
	defer del.Close()
	ins, err := tx.PrepareContext(ctx, `INSERT INTO content_fts (path, body) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	upd, err := tx.PrepareContext(ctx, `UPDATE files SET content_indexed = 1 WHERE path = ?`)
	if err != nil {
		return err
	}
	defer upd.Close()
	var inserted int64
	for _, r := range batch {
		if _, err := del.ExecContext(ctx, r.path); err != nil {
			return err
		}
		if r.ok {
			if _, err := ins.ExecContext(ctx, r.path, r.body); err != nil {
				return err
			}
			inserted++
		}
		// Mark processed either way so binary/unreadable files are not
		// re-sniffed on every crawl until they change.
		if _, err := upd.ExecContext(ctx, r.path); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.contentIndexed.Add(inserted)
	return nil
}

// extractText reads a file for content indexing: a NUL byte in the first
// 8 KiB marks it binary (skipped); otherwise up to maxBytes are read (a file
// that grew past the cap since stat is truncated, not skipped) and decoded
// as UTF-8 with replacement characters for invalid sequences.
func extractText(path string, maxBytes int64) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", false
	}
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		return "", false // binary
	}
	data := head
	if remain := maxBytes - int64(n); remain > 0 {
		rest, err := io.ReadAll(io.LimitReader(f, remain))
		if err != nil {
			return "", false
		}
		data = append(data, rest...)
	}
	if int64(len(data)) > maxBytes {
		data = data[:maxBytes]
	}
	return sanitizeBody(strings.ToValidUTF8(string(data), "�")), true
}

// sanitizeBody strips characters that would confuse storage or snippet
// rendering: NUL and the 0x02/0x03 markers used as snippet delimiters.
func sanitizeBody(s string) string {
	if !strings.ContainsAny(s, "\x00\x02\x03") {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case 0x00, 0x02, 0x03:
			return -1
		}
		return r
	}, s)
}

// mimeFor returns a best-effort MIME type for an extension (no parameters).
func mimeFor(ext string) string {
	if ext == "" {
		return ""
	}
	m := mime.TypeByExtension("." + ext)
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// mimeClass computes the coarse class stored in files.mimeclass:
// "archive" for browsable archives, else the MIME major type ("text",
// "image", ...), else "".
func mimeClass(name, ext string) string {
	if types.IsArchiveName(name) {
		return "archive"
	}
	m := mimeFor(ext)
	if i := strings.IndexByte(m, '/'); i >= 0 {
		return m[:i]
	}
	return m
}
