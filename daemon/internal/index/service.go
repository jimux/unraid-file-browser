package index

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"unraid-filebrowser/internal/types"
)

const (
	stateIdle       = "idle"
	stateCrawling   = "crawling"
	stateExtracting = "extracting"

	defaultParallelism  = 2
	defaultMaxFileBytes = 10 * 1024 * 1024
	defaultSchedule     = "0 3 * * *"

	minParallelism  = 1
	maxParallelism  = 16
	maxMaxFileBytes = 64 << 20
)

// safetyContentExts is the minimal extension allowlist used when neither the
// config nor the owner-tuned DefaultContentExtensions() provides one, so
// content indexing works out of the box before policy.go is filled in.
var safetyContentExts = []string{"txt", "md", "log", "json", "conf", "cfg", "ini"}

// Options carries the boot-time, trusted settings of a Service. They come
// from the daemon's flags (ultimately the root-only flash-drive settings),
// never from the API.
type Options struct {
	// AllowedRoots are the daemon's browse roots (boot-time, trusted). Every
	// index root and content include-path must be equal to or beneath one of
	// them. Required (empty → error).
	AllowedRoots []string
}

// Service owns the search index: the SQLite/FTS5 store, the crawler that
// fills it, and the cron scheduler that keeps it fresh. All methods are safe
// for concurrent use.
type Service struct {
	db     *sql.DB
	dbPath string

	// allowed is the cleaned, absolute browse-root set from Options. It is
	// immutable for the life of the Service: it bounds every index root and
	// include path (validateConfig) and is a second predicate on every search.
	allowed []string

	cfgMu sync.RWMutex
	cfg   types.IndexConfig

	// Crawl policy hooks. They default to the owner-tuned functions in
	// policy.go (SkipDir, DefaultContentExtensions); tests may inject
	// replacements without touching policy.go.
	skipDirFn     func(name, path string) bool
	contentExtsFn func() []string

	// Live status. Kept in atomics so Status() answers instantly and never
	// contends with the crawl.
	state          atomic.Value // string: idle|crawling|extracting
	current        atomic.Value // string: path being processed
	progressBits   atomic.Uint64
	filesIndexed   atomic.Int64
	contentIndexed atomic.Int64
	lastFullScan   atomic.Int64
	extracted      atomic.Int64 // lifetime count of content extractions (metrics/tests)

	// Single-flight crawl.
	crawlBusy   atomic.Bool
	crawlMu     sync.Mutex
	crawlCancel context.CancelFunc
	wg          sync.WaitGroup

	// Pause gate: non-nil while paused; closing it resumes the workers.
	pauseMu  sync.Mutex
	resumeCh chan struct{}

	// Status subscribers (SSE feed).
	subMu      sync.Mutex
	subs       map[int]chan types.IndexStatus
	nextSubID  int
	lastPush   time.Time
	timerArmed bool

	// Scheduler.
	cronMu  sync.Mutex
	cron    *cron.Cron
	schedOn bool

	closed atomic.Bool
}

// New opens (creating if necessary) the index database at dbPath and returns
// a ready Service. opts.AllowedRoots is required and trusted. cfg is the
// persisted configuration: values an older build may have written out of
// range (or roots outside the browse roots) are clamped/dropped with a log
// line rather than refusing to start — strict rejection is reserved for
// SetConfig, i.e. the API. Rows left over from a wider root set are swept
// before the service is returned. Persistence is the config layer's job.
func New(dbPath string, cfg types.IndexConfig, opts Options) (*Service, error) {
	allowed, err := normalizeAllowedRoots(opts.AllowedRoots)
	if err != nil {
		return nil, err
	}
	cfg = sanitizeConfig(cfg, allowed)
	if err := validateConfig(cfg, allowed); err != nil {
		return nil, err
	}
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	s := &Service{
		db:            db,
		dbPath:        dbPath,
		allowed:       allowed,
		cfg:           copyConfig(cfg),
		skipDirFn:     SkipDir,
		contentExtsFn: DefaultContentExtensions,
		subs:          make(map[int]chan types.IndexStatus),
	}
	s.state.Store(stateIdle)
	s.current.Store("")
	ctx := context.Background()
	if err := s.sweepOutsideRoots(ctx); err != nil {
		log.Printf("index: startup out-of-root sweep failed: %v", err)
	}
	s.lastFullScan.Store(s.getMetaInt(ctx, "last_full_scan"))
	s.recount(ctx)
	return s, nil
}

// AllowedRoots returns a copy of the boot-time browse roots that bound the
// configurable index roots.
func (s *Service) AllowedRoots() []string {
	return append([]string(nil), s.allowed...)
}

// Close cancels any running crawl, waits for it, stops the scheduler and
// closes the store. Safe to call more than once.
func (s *Service) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.crawlMu.Lock()
	if s.crawlCancel != nil {
		s.crawlCancel()
	}
	s.crawlMu.Unlock()
	s.wg.Wait()
	s.cronMu.Lock()
	if s.cron != nil {
		s.cron.Stop()
		s.cron = nil
	}
	s.schedOn = false
	s.cronMu.Unlock()
	s.subMu.Lock()
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	s.subMu.Unlock()
	secureDBFiles(s.dbPath)
	return s.db.Close()
}

// Status reports the current index state from atomic counters; it never
// blocks on crawl progress.
func (s *Service) Status() types.IndexStatus {
	st := types.IndexStatus{
		State:          s.state.Load().(string),
		FilesIndexed:   s.filesIndexed.Load(),
		ContentIndexed: s.contentIndexed.Load(),
		DBBytes:        s.dbBytes(),
		LastFullScan:   s.lastFullScan.Load(),
	}
	if st.State != stateIdle {
		st.Current, _ = s.current.Load().(string)
		st.Progress = math.Float64frombits(s.progressBits.Load())
	}
	return st
}

// Subscribe registers a status listener. The returned channel is buffered
// and receives coalesced status snapshots (at most ~1/sec, plus immediate
// pushes on state transitions); slow consumers drop updates rather than
// block the crawler. The cancel func unregisters and closes the channel.
func (s *Service) Subscribe() (<-chan types.IndexStatus, func()) {
	ch := make(chan types.IndexStatus, 16)
	s.subMu.Lock()
	id := s.nextSubID
	s.nextSubID++
	s.subs[id] = ch
	s.subMu.Unlock()
	// Seed with the current status so SSE clients render immediately.
	select {
	case ch <- s.Status():
	default:
	}
	cancel := func() {
		s.subMu.Lock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
		s.subMu.Unlock()
	}
	return ch, cancel
}

// Config returns a copy of the live configuration.
func (s *Service) Config() types.IndexConfig {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return copyConfig(s.cfg)
}

// SetConfig validates and applies a new configuration live (persistence
// happens at the api/config layer). Validation is strict — nothing is
// normalized on the caller's behalf: every root and include path must be a
// clean absolute path within AllowedRoots, the schedule must parse,
// parallelism must be 1..16 and content.maxFileBytes 1..64 MiB; violations
// return *types.APIError with ErrBadRequest and a message naming the field.
// Rows indexed under roots (or include paths) the new configuration no
// longer covers are deleted from files, names_fts and content_fts in one
// transaction. If the scheduler is running it is rebuilt with the new cron
// schedule.
func (s *Service) SetConfig(cfg types.IndexConfig) error {
	if s.closed.Load() {
		return types.Errf(types.ErrIndexing, "index service is closed")
	}
	if err := validateConfig(cfg, s.allowed); err != nil {
		return err
	}
	s.cfgMu.Lock()
	s.cfg = copyConfig(cfg)
	s.cfgMu.Unlock()
	// Search already filters by the live roots, so a failed sweep leaks
	// nothing; the crawl-end sweep retries it. Log rather than fail the
	// request and leave the live config out of step with the persisted one.
	ctx := context.Background()
	if err := s.sweepOutsideRoots(ctx); err != nil {
		log.Printf("index: out-of-root sweep after config change failed: %v", err)
	}
	s.recount(ctx)
	s.cronMu.Lock()
	if s.schedOn {
		s.restartCronLocked()
	}
	s.cronMu.Unlock()
	return nil
}

// Rescan kicks off an asynchronous crawl: path == "" means a full crawl of
// all configured roots, otherwise the given subtree only (its stale rows are
// swept scoped to that subtree). It errors only if a crawl cannot start: a
// crawl is already running (ErrIndexing), the service is closed
// (ErrIndexing), or the path is outside the configured roots (ErrBadRequest).
func (s *Service) Rescan(path string) error {
	if s.closed.Load() {
		return types.Errf(types.ErrIndexing, "index service is closed")
	}
	scope := ""
	if path != "" {
		scope = filepath.Clean(path)
		if !filepath.IsAbs(scope) || !s.pathInRoots(scope) {
			return types.Errf(types.ErrBadRequest, "rescan path is outside the configured roots")
		}
	}
	if !s.crawlBusy.CompareAndSwap(false, true) {
		return types.Errf(types.ErrIndexing, "a crawl is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.crawlMu.Lock()
	s.crawlCancel = cancel
	s.crawlMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer s.crawlBusy.Store(false)
		s.runCrawl(ctx, scope)
	}()
	return nil
}

// Pause blocks crawl workers at the next per-file gate check. It persists
// across crawls until Resume is called.
func (s *Service) Pause() {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	if s.resumeCh == nil {
		s.resumeCh = make(chan struct{})
	}
}

// Resume releases a previous Pause.
func (s *Service) Resume() {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	if s.resumeCh != nil {
		close(s.resumeCh)
		s.resumeCh = nil
	}
}

// RunScheduler blocks until ctx is done, firing a full Rescan on the
// configured cron schedule (standard 5-field). SetConfig reschedules live.
func (s *Service) RunScheduler(ctx context.Context) {
	s.cronMu.Lock()
	s.schedOn = true
	s.restartCronLocked()
	s.cronMu.Unlock()
	<-ctx.Done()
	s.cronMu.Lock()
	s.schedOn = false
	if s.cron != nil {
		s.cron.Stop()
		s.cron = nil
	}
	s.cronMu.Unlock()
}

// --- internals ------------------------------------------------------------

func (s *Service) restartCronLocked() {
	if s.cron != nil {
		s.cron.Stop()
		s.cron = nil
	}
	sched := s.Config().Schedule
	if sched == "" {
		return
	}
	c := cron.New() // default parser: standard 5-field cron
	if _, err := c.AddFunc(sched, func() {
		if err := s.Rescan(""); err != nil {
			log.Printf("index: scheduled rescan not started: %v", err)
		}
	}); err != nil {
		// validateConfig should have caught this; log defensively.
		log.Printf("index: invalid cron schedule %q: %v", sched, err)
		return
	}
	c.Start()
	s.cron = c
}

// normalizeAllowedRoots cleans the boot-time browse roots. They are trusted
// but still must be absolute; an empty set is a wiring error, not a
// "nothing allowed" configuration.
func normalizeAllowedRoots(roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("index: Options.AllowedRoots is required (no browse roots)")
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" || !filepath.IsAbs(r) {
			return nil, fmt.Errorf("index: allowed root %q must be an absolute path", r)
		}
		c := filepath.Clean(r)
		dup := false
		for _, have := range out {
			if have == c {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out, nil
}

// within reports whether p is root itself or lies beneath it, on a path
// boundary: /mnt/user2 is NOT within /mnt/user. Both must be cleaned
// absolute paths; root "/" contains every absolute path.
func within(p, root string) bool {
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == root || strings.HasPrefix(p, root+"/")
}

func withinAny(p string, roots []string) bool {
	for _, r := range roots {
		if within(p, r) {
			return true
		}
	}
	return false
}

// checkConfPath enforces the path rules of validateConfig for one entry:
// non-blank, absolute, already clean (no trailing slash, "." or ".."), and
// within the allowed roots. It does not normalize.
func checkConfPath(what, p string, allowed []string) error {
	if strings.TrimSpace(p) == "" {
		return types.Errf(types.ErrBadRequest, what+" must be a non-empty path")
	}
	if !filepath.IsAbs(p) {
		return types.Errf(types.ErrBadRequest, fmt.Sprintf("%s %q must be an absolute path", what, p))
	}
	if c := filepath.Clean(p); c != p {
		return types.Errf(types.ErrBadRequest, fmt.Sprintf("%s %q must be a clean path (did you mean %q?)", what, p, c))
	}
	if !withinAny(p, allowed) {
		return types.Errf(types.ErrBadRequest, fmt.Sprintf("%s %q is outside the browse roots %v", what, p, allowed))
	}
	return nil
}

// validateConfig is the strict, non-normalizing check applied to API input
// (and, after sanitizeConfig, to the persisted config at startup).
func validateConfig(cfg types.IndexConfig, allowed []string) error {
	if strings.TrimSpace(cfg.Schedule) == "" {
		return types.Errf(types.ErrBadRequest, "schedule is required (standard 5-field cron, e.g. \"0 3 * * *\")")
	}
	if _, err := cron.ParseStandard(cfg.Schedule); err != nil {
		return types.Errf(types.ErrBadRequest, "invalid cron schedule: "+err.Error())
	}
	if cfg.Parallelism < minParallelism || cfg.Parallelism > maxParallelism {
		return types.Errf(types.ErrBadRequest,
			fmt.Sprintf("parallelism %d is out of range %d..%d", cfg.Parallelism, minParallelism, maxParallelism))
	}
	if cfg.Content.MaxFileBytes < 1 || cfg.Content.MaxFileBytes > maxMaxFileBytes {
		return types.Errf(types.ErrBadRequest,
			fmt.Sprintf("content.maxFileBytes %d is out of range 1..%d", cfg.Content.MaxFileBytes, maxMaxFileBytes))
	}
	for _, r := range cfg.Roots {
		if err := checkConfPath("index root", r, allowed); err != nil {
			return err
		}
	}
	for _, p := range cfg.Content.IncludePaths {
		if err := checkConfPath("content include path", p, allowed); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeConfig repairs a persisted configuration so the daemon can start:
// out-of-range numbers are clamped, a missing/invalid schedule falls back to
// the default, and roots or include paths that are not clean absolute paths
// within the browse roots are dropped. Every change is logged. The result
// passes validateConfig.
func sanitizeConfig(cfg types.IndexConfig, allowed []string) types.IndexConfig {
	out := copyConfig(cfg)
	keep := func(what string, in []string) []string {
		res := make([]string, 0, len(in))
		for _, p := range in {
			c := filepath.Clean(strings.TrimSpace(p))
			if strings.TrimSpace(p) == "" || !filepath.IsAbs(c) || !withinAny(c, allowed) {
				log.Printf("index: dropping persisted %s %q: not a clean absolute path within the browse roots %v", what, p, allowed)
				continue
			}
			if c != p {
				log.Printf("index: normalizing persisted %s %q to %q", what, p, c)
			}
			res = append(res, c)
		}
		return res
	}
	out.Roots = keep("index root", cfg.Roots)
	out.Content.IncludePaths = keep("content include path", cfg.Content.IncludePaths)

	if strings.TrimSpace(out.Schedule) == "" {
		out.Schedule = defaultSchedule
	} else if _, err := cron.ParseStandard(out.Schedule); err != nil {
		log.Printf("index: persisted schedule %q is invalid (%v); using %q", out.Schedule, err, defaultSchedule)
		out.Schedule = defaultSchedule
	}
	switch {
	case out.Parallelism < minParallelism:
		if out.Parallelism != 0 {
			log.Printf("index: persisted parallelism %d below %d; using %d", out.Parallelism, minParallelism, defaultParallelism)
		}
		out.Parallelism = defaultParallelism
	case out.Parallelism > maxParallelism:
		log.Printf("index: persisted parallelism %d above %d; clamping", out.Parallelism, maxParallelism)
		out.Parallelism = maxParallelism
	}
	switch {
	case out.Content.MaxFileBytes < 1:
		if out.Content.MaxFileBytes != 0 {
			log.Printf("index: persisted content.maxFileBytes %d invalid; using %d", out.Content.MaxFileBytes, defaultMaxFileBytes)
		}
		out.Content.MaxFileBytes = defaultMaxFileBytes
	case out.Content.MaxFileBytes > maxMaxFileBytes:
		log.Printf("index: persisted content.maxFileBytes %d above %d; clamping", out.Content.MaxFileBytes, maxMaxFileBytes)
		out.Content.MaxFileBytes = maxMaxFileBytes
	}
	return out
}

func copyConfig(cfg types.IndexConfig) types.IndexConfig {
	out := cfg
	out.Roots = append([]string(nil), cfg.Roots...)
	out.Content.IncludePaths = append([]string(nil), cfg.Content.IncludePaths...)
	out.Content.Extensions = append([]string(nil), cfg.Content.Extensions...)
	return out
}

// pathInRoots reports whether the cleaned absolute path p is inside one of
// the configured index roots (which are themselves within AllowedRoots).
func (s *Service) pathInRoots(p string) bool {
	return withinAny(p, s.Config().Roots)
}

// sweepOutsideRoots deletes, in one transaction, every indexed row the live
// configuration no longer covers: files (and via triggers names_fts) outside
// the index roots, content_fts documents outside the roots or — when include
// paths are set — outside them. Files whose content document was dropped
// only because of the include paths are reset to content_indexed = 0 so a
// later widening re-extracts them. Called at startup, after every SetConfig
// and at the end of every crawl, so a crawl that was already running with
// the old roots cannot leave rows behind.
func (s *Service) sweepOutsideRoots(ctx context.Context) error {
	cfg := s.Config()
	rootCond, rootArgs := prefixCond("path", cfg.Roots)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM content_fts WHERE NOT `+rootCond, rootArgs...); err != nil {
		return err
	}
	if len(cfg.Content.IncludePaths) > 0 {
		incCond, incArgs := prefixCond("path", cfg.Content.IncludePaths)
		if _, err := tx.ExecContext(ctx, `DELETE FROM content_fts WHERE NOT `+incCond, incArgs...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE files SET content_indexed = 0 WHERE content_indexed = 1 AND NOT `+incCond, incArgs...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE NOT `+rootCond, rootArgs...); err != nil {
		return err
	}
	return tx.Commit()
}

// waitGate blocks while the service is paused (or until ctx is canceled).
// Crawl workers call it between files.
func (s *Service) waitGate(ctx context.Context) error {
	for {
		s.pauseMu.Lock()
		ch := s.resumeCh
		s.pauseMu.Unlock()
		if ch == nil {
			return ctx.Err()
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Service) setProgress(p float64) {
	s.progressBits.Store(math.Float64bits(p))
}

// dbBytes sums the on-disk size of the database and its WAL/SHM sidecars.
func (s *Service) dbBytes() int64 {
	var total int64
	for _, p := range []string{s.dbPath, s.dbPath + "-wal", s.dbPath + "-shm"} {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// recount refreshes the row-count counters from the store.
func (s *Service) recount(ctx context.Context) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&n); err == nil {
		s.filesIndexed.Store(n)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_fts`).Scan(&n); err == nil {
		s.contentIndexed.Store(n)
	}
}

// notify pushes a status snapshot to subscribers, coalescing to at most one
// push per second unless force is set (state transitions). A trailing timer
// guarantees the final coalesced update is delivered.
func (s *Service) notify(force bool) {
	s.subMu.Lock()
	if len(s.subs) == 0 {
		s.subMu.Unlock()
		return
	}
	now := time.Now()
	if !force && now.Sub(s.lastPush) < time.Second {
		if !s.timerArmed {
			s.timerArmed = true
			delay := time.Second - now.Sub(s.lastPush)
			time.AfterFunc(delay, func() {
				s.subMu.Lock()
				s.timerArmed = false
				s.subMu.Unlock()
				s.notify(true)
			})
		}
		s.subMu.Unlock()
		return
	}
	s.lastPush = now
	st := s.Status()
	for _, ch := range s.subs {
		select {
		case ch <- st:
		default: // slow consumer: drop rather than block the crawl
		}
	}
	s.subMu.Unlock()
}
