package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registers as "sqlite"
)

// schemaVersion is stamped into PRAGMA user_version. Bump it and add a
// migration step to migrations when the schema changes.
//
//	v1  files, names_fts, content_fts, meta
//	v2  files.meta_extracted, file_meta (embedded metadata rows)
const schemaVersion = 2

// schemaDDL is executed statement-by-statement when the database is new
// (user_version == 0).
//
// Layout:
//   - files          — one row per regular file (metadata index). `gen` is the
//     crawl generation used for mark-and-sweep deletion: every crawl stamps
//     the rows it sees with a fresh generation, then deletes rows inside the
//     crawled scope whose generation is older (those files are gone).
//   - names_fts      — external-content FTS5 over files(name, dir), kept in
//     sync by triggers. tokenchars '._-' keeps "server.log" / "my_file" as
//     single searchable tokens so typed-prefix matching works on them.
//   - content_fts    — standalone FTS5 table holding extracted text of
//     allowlisted files (path is UNINDEXED payload, body is searched).
//   - file_meta      — extracted embedded metadata (EXIF, stream layout, tags,
//     package fields): several rows per (file_id, key) are legitimate (one
//     per audio track, one per dependency). num is set for numerically
//     comparable values. Rows are deleted with their file (explicitly in the
//     sweeps, and by the FK cascade as a belt). The (key, value) index is
//     NOCASE so "=" and prefix LIKE filters, both case-insensitive, use it.
//   - meta           — key/value: crawl generation counter, last_full_scan.
var schemaDDL = []string{
	`CREATE TABLE files (
		id              INTEGER PRIMARY KEY,
		path            TEXT NOT NULL UNIQUE,
		dir             TEXT NOT NULL,
		name            TEXT NOT NULL,
		ext             TEXT NOT NULL DEFAULT '',
		size            INTEGER NOT NULL DEFAULT 0,
		mtime           INTEGER NOT NULL DEFAULT 0,
		mimeclass       TEXT NOT NULL DEFAULT '',
		content_indexed INTEGER NOT NULL DEFAULT 0,
		gen             INTEGER NOT NULL DEFAULT 0,
		meta_extracted  INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX files_dir_idx ON files(dir)`,
	`CREATE INDEX files_ext_idx ON files(ext)`,
	`CREATE INDEX files_size_idx ON files(size)`,
	`CREATE INDEX files_mtime_idx ON files(mtime)`,
	`CREATE INDEX files_gen_idx ON files(gen)`,
	`CREATE INDEX files_ci_idx ON files(content_indexed)`,
	`CREATE INDEX files_me_idx ON files(meta_extracted)`,
	`CREATE VIRTUAL TABLE names_fts USING fts5(
		name, dir,
		content='files', content_rowid='id',
		tokenize="unicode61 tokenchars '._-'"
	)`,
	`CREATE TRIGGER files_fts_ai AFTER INSERT ON files BEGIN
		INSERT INTO names_fts(rowid, name, dir) VALUES (new.id, new.name, new.dir);
	END`,
	`CREATE TRIGGER files_fts_ad AFTER DELETE ON files BEGIN
		INSERT INTO names_fts(names_fts, rowid, name, dir) VALUES ('delete', old.id, old.name, old.dir);
	END`,
	// name/dir are derived from the unique path and never change in place,
	// so this trigger exists only for schema completeness; the crawler's
	// upserts touch size/mtime/gen/content_indexed and deliberately do NOT
	// fire it (UPDATE OF name, dir).
	`CREATE TRIGGER files_fts_au AFTER UPDATE OF name, dir ON files BEGIN
		INSERT INTO names_fts(names_fts, rowid, name, dir) VALUES ('delete', old.id, old.name, old.dir);
		INSERT INTO names_fts(rowid, name, dir) VALUES (new.id, new.name, new.dir);
	END`,
	`CREATE VIRTUAL TABLE content_fts USING fts5(
		path UNINDEXED,
		body,
		tokenize='unicode61'
	)`,
	`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	fileMetaDDL[0], fileMetaDDL[1], fileMetaDDL[2], fileMetaDDL[3],
}

// fileMetaDDL creates the embedded-metadata table; shared by the fresh
// schema and the v1→v2 migration.
var fileMetaDDL = []string{
	`CREATE TABLE file_meta (
		file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
		key     TEXT NOT NULL,
		value   TEXT NOT NULL,
		num     REAL
	)`,
	`CREATE INDEX idx_meta_key_value ON file_meta(key, value COLLATE NOCASE)`,
	`CREATE INDEX idx_meta_key_num   ON file_meta(key, num)`,
	`CREATE INDEX idx_meta_file      ON file_meta(file_id)`,
}

// migrations[v] upgrades a database at user_version v to v+1. Each runs in
// one transaction and must preserve existing rows.
var migrations = map[int][]string{
	1: {
		`ALTER TABLE files ADD COLUMN meta_extracted INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX files_me_idx ON files(meta_extracted)`,
		fileMetaDDL[0], fileMetaDDL[1], fileMetaDDL[2], fileMetaDDL[3],
	},
}

// openDB opens (creating if needed) the index database with the required
// pragmas: WAL journaling, busy_timeout=5000, synchronous=NORMAL. The
// _pragma DSN parameters are applied by modernc.org/sqlite on every new
// pooled connection.
//
// The database can hold the plaintext of every content-indexed file and
// lives under a directory containers may mount, so its directory is made
// 0700 and the db file plus WAL/SHM sidecars 0600 (secureDBFiles). The
// sidecars are forced into existence with a write here so their modes can be
// tightened before anything else runs; Close and every crawl re-apply them
// in case the pool recreated a sidecar with the process umask.
func openDB(path string) (*sql.DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("index: create data dir: %w", err)
	}
	// MkdirAll leaves a pre-existing (older build, 0755) directory alone.
	if err := os.Chmod(dir, 0o700); err != nil {
		log.Printf("index: cannot restrict data dir %s: %v", dir, err)
	}
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("index: open db: %w", err)
	}
	// WAL supports many readers plus one writer; keep the pool modest so a
	// crawl doesn't starve interactive searches of file handles.
	db.SetMaxOpenConns(8)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("index: ping db: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	// A write on an already-migrated database creates the -wal/-shm sidecars
	// now rather than at the first crawl.
	if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES ('last_open', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		db.Close()
		return nil, fmt.Errorf("index: initial write: %w", err)
	}
	secureDBFiles(path)
	return db, nil
}

// secureDBFiles chmods the database file and its WAL/SHM sidecars to 0600.
// Missing sidecars are fine; other failures are logged.
func secureDBFiles(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("index: cannot restrict %s: %v", p, err)
		}
	}
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("index: read schema version: %w", err)
	}
	switch {
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return fmt.Errorf("index: db schema version %d is newer than supported %d", v, schemaVersion)
	case v == 0:
		for _, ddl := range schemaDDL {
			if _, err := db.Exec(ddl); err != nil {
				return fmt.Errorf("index: apply schema: %w (in %.60q)", err, ddl)
			}
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return fmt.Errorf("index: stamp schema version: %w", err)
		}
		return nil
	}
	for v < schemaVersion {
		steps, ok := migrations[v]
		if !ok {
			return fmt.Errorf("index: no migration path from schema version %d", v)
		}
		if err := applyMigration(db, v, steps); err != nil {
			return err
		}
		v++
	}
	return nil
}

// applyMigration runs one version step atomically, stamping the new
// user_version in the same transaction so a crash mid-way leaves the old
// version (and the old schema) intact.
func applyMigration(db *sql.DB, from int, steps []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("index: migrate v%d: %w", from, err)
	}
	defer tx.Rollback()
	for _, ddl := range steps {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("index: migrate v%d→v%d: %w (in %.60q)", from, from+1, err, ddl)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, from+1)); err != nil {
		return fmt.Errorf("index: stamp schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("index: migrate v%d: commit: %w", from, err)
	}
	log.Printf("index: migrated database schema v%d → v%d", from, from+1)
	return nil
}

// --- meta helpers ---------------------------------------------------------

func (s *Service) getMetaInt(ctx context.Context, key string) int64 {
	var v int64
	err := s.db.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM meta WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return 0
	}
	return v
}

func (s *Service) setMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// nextGeneration atomically increments and returns the crawl generation.
func (s *Service) nextGeneration(ctx context.Context) (int64, error) {
	var gen int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('generation', '1')
		 ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		 RETURNING CAST(value AS INTEGER)`).Scan(&gen)
	return gen, err
}

// --- SQL building helpers -------------------------------------------------

// escapeLike escapes LIKE metacharacters so user-supplied strings can be
// embedded in LIKE patterns with ESCAPE '\'.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// likePrefix builds the LIKE pattern matching paths strictly below the
// (cleaned) prefix p. The root "/" is special-cased: Clean leaves its trailing
// separator, so blindly appending "/%" would produce "//%" and match nothing.
func likePrefix(p string) string {
	esc := escapeLike(p)
	if strings.HasSuffix(p, "/") {
		return esc + "%"
	}
	return esc + "/%"
}

// prefixCond builds "(col = p OR col LIKE p/% ...)" over a set of path
// prefixes, parametrized and LIKE-escaped. An empty prefix list yields a
// condition that matches nothing (never invalid SQL).
func prefixCond(col string, prefixes []string) (string, []any) {
	if len(prefixes) == 0 {
		return "0", nil
	}
	parts := make([]string, 0, len(prefixes))
	args := make([]any, 0, 2*len(prefixes))
	for _, p := range prefixes {
		p = filepath.Clean(p)
		parts = append(parts, "("+col+" = ? OR "+col+" LIKE ? ESCAPE '\\')")
		args = append(args, p, likePrefix(p))
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
