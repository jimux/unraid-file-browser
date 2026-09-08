package index

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"unraid-filebrowser/internal/meta"
	"unraid-filebrowser/internal/types"
)

const (
	metaRowBatchSize = 200 // files per file_meta write transaction
	maxMetaValues    = 500 // MetaValues limit cap
	maxMetaFilters   = 16  // per search
)

// metaResult is one file's extraction outcome bound for the writer.
type metaResult struct {
	id     int64
	fields []meta.Field // nil: nothing to store (unsupported/error) — still marked
}

// metaPass extracts embedded metadata from files that (a) carry an extension
// an extractor covers, (b) sit inside the crawled scope, and (c) still have
// meta_extracted = 0 (new or changed since the last pass, or never seen by
// this schema). Extraction reads headers only, on cfg.Parallelism workers;
// ffprobe-backed extractors additionally share a two-slot semaphore inside
// the meta package so a crawl never runs more than two ffprobe processes
// regardless of the worker count. Results flow to one batching writer.
// Candidates are visited in path order so reads stay directory-local on
// spinning disks. Every candidate is marked processed afterwards, whether it
// produced rows or not, so a broken file is not re-read on every crawl until
// it changes.
func (s *Service) metaPass(ctx context.Context, cfg types.IndexConfig, roots []string) {
	scopeCond, scopeArgs := prefixCond("path", roots)

	// Changed files: drop their stale rows up front so a file that changed
	// and now yields nothing does not keep its old metadata.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM file_meta WHERE file_id IN (SELECT id FROM files WHERE meta_extracted = 0 AND `+scopeCond+`)`,
		scopeArgs...); err != nil {
		log.Printf("index: stale metadata cleanup failed: %v", err)
	}

	q := `SELECT id, path, ext FROM files WHERE meta_extracted = 0 AND ext IN (` +
		placeholders(len(s.metaExts)) + `) AND ` + scopeCond + ` ORDER BY path`
	args := make([]any, 0, len(s.metaExts)+len(scopeArgs))
	for _, e := range s.metaExts {
		args = append(args, e)
	}
	args = append(args, scopeArgs...)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		log.Printf("index: metadata candidate query failed: %v", err)
		return
	}
	type cand struct {
		id   int64
		path string
		ext  string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.path, &c.ext); err == nil {
			cands = append(cands, c)
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
	jobs := make(chan cand)
	results := make(chan metaResult, 64)
	var workers sync.WaitGroup
	for i := 0; i < par; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for c := range jobs {
				if err := s.waitGate(ctx); err != nil {
					continue // canceled: drain quickly
				}
				if ctx.Err() != nil {
					continue
				}
				s.current.Store(c.path)
				fields := s.extractMeta(ctx, c.path, c.ext)
				s.metaExtracted.Add(1)
				select {
				case results <- metaResult{id: c.id, fields: fields}:
				case <-ctx.Done():
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, c := range cands {
			select {
			case jobs <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	done := 0
	batch := make([]metaResult, 0, metaRowBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if ctx.Err() == nil {
			if err := s.flushMeta(ctx, batch); err != nil {
				log.Printf("index: metadata batch failed: %v", err)
			}
		}
		done += len(batch)
		s.setProgress(min(0.99, float64(done)/float64(total)))
		s.notify(false)
		batch = batch[:0]
	}
	for r := range results {
		batch = append(batch, r)
		if len(batch) >= metaRowBatchSize {
			flush()
		}
	}
	flush()
}

// extractMeta opens one candidate and runs its extractor. The file is opened
// O_NOFOLLOW (the walk never follows symlinks, and neither must this) and
// re-checked to be a regular file: the path was stat'ed some time ago and
// could have been swapped since. Errors are logged at most once per file
// per crawl and yield nil (the file is still marked processed).
func (s *Service) extractMeta(ctx context.Context, path, ext string) []meta.Field {
	ex, ok := s.extractors[ext]
	if !ok {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	fields, err := meta.Run(ctx, ex, path, f, st.Size())
	if err != nil {
		if !errors.Is(err, meta.ErrUnsupported) && ctx.Err() == nil {
			log.Printf("index: metadata %s: %v", path, err)
		}
		return nil
	}
	return fields
}

// flushMeta writes one batch: per file, replace its rows and mark it
// processed. Single writer, one transaction per batch (WAL-friendly).
func (s *Service) flushMeta(ctx context.Context, batch []metaResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	del, err := tx.PrepareContext(ctx, `DELETE FROM file_meta WHERE file_id = ?`)
	if err != nil {
		return err
	}
	defer del.Close()
	ins, err := tx.PrepareContext(ctx, `INSERT INTO file_meta (file_id, key, value, num) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	upd, err := tx.PrepareContext(ctx, `UPDATE files SET meta_extracted = 1 WHERE id = ?`)
	if err != nil {
		return err
	}
	defer upd.Close()
	var withRows int64
	for _, r := range batch {
		if _, err := del.ExecContext(ctx, r.id); err != nil {
			return err
		}
		for _, f := range r.fields {
			var num any
			if f.Num != nil {
				num = *f.Num
			}
			if _, err := ins.ExecContext(ctx, r.id, f.Key, f.Value, num); err != nil {
				return err
			}
		}
		if len(r.fields) > 0 {
			withRows++
		}
		// The file may have been swept between candidate selection and now;
		// the UPDATE then affects nothing, which is fine.
		if _, err := upd.ExecContext(ctx, r.id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.metaIndexed.Add(withRows)
	return nil
}

// --- query side ---------------------------------------------------------------

// MetaFields returns the searchable-field catalog for the UI (categories,
// extensions, typed fields with labels). Static; safe before any crawl.
func (s *Service) MetaFields() []types.MetaCategory {
	return meta.Catalog()
}

// MetaValues returns the distinct values of one metadata key with their file
// counts, for the UI's value dropdowns: optionally narrowed to values with a
// case-insensitive prefix and to files under pathScope, ordered by count
// descending then value ascending, confined to the live roots. The key must
// be a catalog key (embedded keys hit file_meta; the common key "ext" is
// answered from files.ext; other common keys are not enumerable and yield
// ErrBadRequest). limit is capped at 500; <= 0 means the cap.
func (s *Service) MetaValues(ctx context.Context, key, prefix, pathScope string, limit int) ([]types.MetaValue, error) {
	if s.closed.Load() {
		return nil, types.Errf(types.ErrIndexing, "index service is closed")
	}
	def, ok := metaFieldDefs[key]
	if !ok {
		return nil, types.Errf(types.ErrBadRequest, "unknown metadata key: "+key)
	}
	if limit <= 0 || limit > maxMetaValues {
		limit = maxMetaValues
	}
	scopeSQL, scopeArgs := s.scopeFilter()
	var conds []string
	var args []any
	if pathScope != "" {
		p := filepath.Clean(pathScope)
		conds = append(conds, `(f.dir = ? OR f.dir LIKE ? ESCAPE '\')`)
		args = append(args, p, likePrefix(p))
	}

	var q string
	switch {
	case strings.Contains(key, "."):
		conds = append([]string{`m.key = ?`}, conds...)
		args = append([]any{key}, args...)
		if prefix != "" {
			conds = append(conds, `m.value LIKE ? ESCAPE '\'`)
			args = append(args, escapeLike(prefix)+"%")
		}
		q = `SELECT m.value, COUNT(*) FROM file_meta m JOIN files f ON f.id = m.file_id WHERE ` +
			strings.Join(conds, " AND ") + scopeSQL +
			` GROUP BY m.value ORDER BY COUNT(*) DESC, m.value ASC LIMIT ?`
	case key == "ext":
		conds = append(conds, `f.ext <> ''`)
		if prefix != "" {
			conds = append(conds, `f.ext LIKE ? ESCAPE '\'`)
			args = append(args, escapeLike(strings.ToLower(strings.TrimPrefix(prefix, ".")))+"%")
		}
		q = `SELECT f.ext, COUNT(*) FROM files f WHERE ` + strings.Join(conds, " AND ") + scopeSQL +
			` GROUP BY f.ext ORDER BY COUNT(*) DESC, f.ext ASC LIMIT ?`
	case key == "mime":
		conds = append(conds, `f.mimeclass <> ''`)
		if prefix != "" {
			conds = append(conds, `f.mimeclass LIKE ? ESCAPE '\'`)
			args = append(args, escapeLike(prefix)+"%")
		}
		q = `SELECT f.mimeclass, COUNT(*) FROM files f WHERE ` + strings.Join(conds, " AND ") + scopeSQL +
			` GROUP BY f.mimeclass ORDER BY COUNT(*) DESC, f.mimeclass ASC LIMIT ?`
	default:
		return nil, types.Errf(types.ErrBadRequest, fmt.Sprintf("values of %q (%s) cannot be enumerated", key, def.Type))
	}
	args = append(args, scopeArgs...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, wrapDBErr(err)
	}
	defer rows.Close()
	out := make([]types.MetaValue, 0, 32)
	for rows.Next() {
		var v types.MetaValue
		if err := rows.Scan(&v.Value, &v.Count); err != nil {
			return nil, wrapDBErr(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBErr(err)
	}
	// SQLite's GROUP BY is binary; the NOCASE index does not merge case
	// variants, so "Panasonic" and "PANASONIC" are distinct values here.
	// That is what the UI wants: the stored spellings.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return out, nil
}

// metaFieldDefs is the flattened catalog used to validate filter keys.
var metaFieldDefs = meta.FieldDefs()
