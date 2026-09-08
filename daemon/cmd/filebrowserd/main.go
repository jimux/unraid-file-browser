// filebrowserd — the Unraid file browser daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"unraid-filebrowser/internal/api"
	"unraid-filebrowser/internal/archive"
	"unraid-filebrowser/internal/config"
	"unraid-filebrowser/internal/fsops"
	"unraid-filebrowser/internal/index"
	"unraid-filebrowser/internal/meta"
	"unraid-filebrowser/internal/transcode"
	"unraid-filebrowser/internal/types"
)

var version = "dev" // set via -ldflags at build time

// persistingIndex saves the config file whenever the SPA applies a new index
// configuration, so settings survive a daemon restart.
type persistingIndex struct {
	*index.Service
	cfgPath string
	dataDir string
}

func (p *persistingIndex) SetConfig(cfg types.IndexConfig) error {
	if err := p.Service.SetConfig(cfg); err != nil {
		return err
	}
	full := config.Config{DataDir: p.dataDir, Index: cfg}
	if err := full.Save(p.cfgPath); err != nil {
		log.Printf("warning: config applied but not persisted: %v", err)
	}
	return nil
}

// lazyIndex serves index endpoints while the database is still opening.
//
// Opening the index does real work (schema migration, an out-of-root sweep,
// permission fixups) against a database that lives on the array, so on a large
// index or a spun-down disk it can take a minute. Doing that before binding
// the socket made rc.filebrowserd's 10s start timeout fire and report a
// perfectly healthy daemon as failed — and it contradicted the design rule
// that browsing and streaming never depend on the index. So the listener comes
// up first and this stands in until the real service is ready.
type lazyIndex struct {
	ready atomic.Pointer[persistingIndex]
	// cfg is what was loaded from disk, so the settings page can render before
	// the database is open.
	cfg   types.IndexConfig
	roots []string
}

func (l *lazyIndex) set(p *persistingIndex) { l.ready.Store(p) }

var errStarting = types.Errf(types.ErrIndexing, "the index is still starting")

func (l *lazyIndex) Search(ctx context.Context, q types.SearchQuery) ([]types.SearchHit, int, error) {
	if p := l.ready.Load(); p != nil {
		return p.Search(ctx, q)
	}
	return nil, 0, errStarting
}

func (l *lazyIndex) Status() types.IndexStatus {
	if p := l.ready.Load(); p != nil {
		return p.Status()
	}
	return types.IndexStatus{State: "starting"}
}

// Subscribe hands back a channel that starts forwarding as soon as the real
// service exists, so an SSE client that connected during startup does not have
// to reconnect to see progress.
func (l *lazyIndex) Subscribe() (<-chan types.IndexStatus, func()) {
	if p := l.ready.Load(); p != nil {
		return p.Subscribe()
	}
	out := make(chan types.IndexStatus, 1)
	done := make(chan struct{})
	var once sync.Once
	cancel := func() { once.Do(func() { close(done) }) }
	go func() {
		defer close(out)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		var real *persistingIndex
		for real == nil {
			select {
			case <-done:
				return
			case <-tick.C:
				real = l.ready.Load()
			}
		}
		src, unsub := real.Subscribe()
		defer unsub()
		for {
			select {
			case <-done:
				return
			case st, ok := <-src:
				if !ok {
					return
				}
				select {
				case out <- st:
				case <-done:
					return
				}
			}
		}
	}()
	return out, cancel
}

func (l *lazyIndex) Config() types.IndexConfig {
	if p := l.ready.Load(); p != nil {
		return p.Config()
	}
	return l.cfg
}

func (l *lazyIndex) SetConfig(cfg types.IndexConfig) error {
	if p := l.ready.Load(); p != nil {
		return p.SetConfig(cfg)
	}
	return errStarting
}

func (l *lazyIndex) AllowedRoots() []string { return l.roots }

// MetaFields is answerable before the database opens: the catalog is static,
// compiled into the daemon, so the search UI can render its dropdowns while
// the index is still starting. Values, which are read out of the database,
// cannot be.
func (l *lazyIndex) MetaFields() []types.MetaCategory {
	if p := l.ready.Load(); p != nil {
		return p.MetaFields()
	}
	return meta.Catalog()
}

func (l *lazyIndex) MetaValues(ctx context.Context, key, prefix, pathScope string, limit int) ([]types.MetaValue, error) {
	if p := l.ready.Load(); p != nil {
		return p.MetaValues(ctx, key, prefix, pathScope, limit)
	}
	return nil, errStarting
}

func (l *lazyIndex) Rescan(path string) error {
	if p := l.ready.Load(); p != nil {
		return p.Rescan(path)
	}
	return errStarting
}

func (l *lazyIndex) Pause() {
	if p := l.ready.Load(); p != nil {
		p.Pause()
	}
}

func (l *lazyIndex) Resume() {
	if p := l.ready.Load(); p != nil {
		p.Resume()
	}
}

// anyWithin reports whether at least one candidate lies at or beneath one of
// the roots, using whole-path-element matching so /mnt/userX is not "within"
// /mnt/user.
func anyWithin(candidates, roots []string) bool {
	for _, c := range candidates {
		c = filepath.Clean(c)
		for _, r := range roots {
			r = filepath.Clean(r)
			if c == r || r == "/" || strings.HasPrefix(c, strings.TrimSuffix(r, "/")+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// parseRoots validates the -roots flag: absolute, cleaned, non-empty paths.
func parseRoots(list string) ([]string, error) {
	var out []string
	for _, r := range strings.Split(list, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("-roots: %q is not an absolute path", r)
		}
		out = append(out, filepath.Clean(r))
	}
	if len(out) == 0 {
		return nil, errors.New("-roots: at least one browse root is required")
	}
	return out, nil
}

func main() {
	var (
		dev     = flag.Bool("dev", false, "development mode: TCP listener, no auth, CORS *")
		listen  = flag.String("listen", "127.0.0.1:8384", "TCP address for -dev mode")
		socket  = flag.String("socket", "/var/run/filebrowserd.sock", "unix socket path (production)")
		dataDir = flag.String("data", "/mnt/user/appdata/filebrowser", "data directory (config, index db, temp)")
		roots   = flag.String("roots", "/mnt/user", "comma-separated browse roots; the API can never read outside these")
		root    = flag.String("root", "", "dev-mode shorthand for -roots (single path)")
		showVer = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	log.SetPrefix("filebrowserd: ")
	// Everything this daemon creates (index db + sqlite sidecars, temp layers,
	// config) is private; the index layer chmods too, but never race it.
	syscall.Umask(0o077)
	browseRoots := *roots
	if *root != "" {
		browseRoots = *root
	}
	if err := run(*dev, *listen, *socket, *dataDir, browseRoots, *root != ""); err != nil {
		log.Fatal(err)
	}
}

func run(dev bool, listen, socket, dataDir, rootList string, rootOverride bool) error {
	if rootOverride && !dev {
		return errors.New("-root is only valid with -dev; use -roots in production")
	}
	browseRoots, err := parseRoots(rootList)
	if err != nil {
		return err
	}

	cfgPath := filepath.Join(dataDir, "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.DataDir = dataDir
	// Index roots are API-settable and therefore untrusted; the index package
	// rejects anything outside the browse roots. A fresh install (or a dev
	// -root override) starts with index roots == browse roots.
	// Index roots must lie inside the browse roots or the index layer drops
	// them. If that would leave nothing to crawl -- a fresh install whose
	// default root is /mnt/user while -roots names something narrower -- fall
	// back to the browse roots instead of silently indexing nothing.
	if rootOverride || len(cfg.Index.Roots) == 0 || !anyWithin(cfg.Index.Roots, browseRoots) {
		cfg.Index.Roots = append([]string(nil), browseRoots...)
	}

	tempDir := filepath.Join(dataDir, "tmp")
	dataDirOK := true
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		// The array may not be mounted yet; browsing must still work.
		log.Printf("warning: data dir unavailable (%v); running without index", err)
		dataDirOK = false
	}

	fs := fsops.New(browseRoots)
	if n, err := archive.SweepTemp(tempDir); err == nil && n > 0 {
		log.Printf("removed %d stale temp file(s)", n)
	}
	resolver := archive.New(fs, archive.Options{TempDir: tempDir})

	// Transcoding is optional: New() discovers ffmpeg/ffprobe and never fails,
	// reporting unavailability through the media endpoints instead.
	media := transcode.New(fs, transcode.Options{})
	defer media.Close()
	if caps := media.Capabilities(); caps.Available {
		log.Printf("media: ffmpeg %s, hwaccels %v", caps.FFmpeg, caps.HWAccels)
	} else {
		log.Printf("media: transcoding unavailable (%s)", caps.Reason)
	}

	deps := api.Deps{
		FS:      fs,
		Archive: resolver,
		Media:   media,
		Roots:   browseRoots,
		Version: version,
		DevMode: dev,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lazy := &lazyIndex{cfg: cfg.Index, roots: browseRoots}
	deps.Index = lazy
	if dataDirOK {
		// Opening the index can take a minute on a cold array; do it behind the
		// listener so browsing, viewing and playback are available immediately.
		go func() {
			// FFprobePath is what enables video (and richer audio) metadata
			// extraction; without it those categories stay silently empty.
			idx, err := index.New(filepath.Join(dataDir, "index.db"), cfg.Index, index.Options{
				AllowedRoots: browseRoots,
				FFprobePath:  media.Capabilities().FFprobePath,
			})
			if err != nil {
				log.Printf("warning: index unavailable: %v", err)
				return
			}
			lazy.set(&persistingIndex{Service: idx, cfgPath: cfgPath, dataDir: dataDir})
			log.Printf("index ready")
			idx.RunScheduler(ctx)
		}()
	}
	// Closed through the lazy holder: an open still in flight at shutdown is
	// abandoned with the process, which is harmless (SQLite recovers from its
	// WAL) and avoids blocking exit on a cold disk.
	defer func() {
		if p := lazy.ready.Load(); p != nil {
			p.Close()
		}
	}()

	var ln net.Listener
	if dev {
		ln, err = net.Listen("tcp", listen)
		log.Printf("dev mode: listening on http://%s (auth disabled)", listen)
	} else {
		// A stale socket from an unclean shutdown blocks the bind.
		if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
		ln, err = net.Listen("unix", socket)
		if err == nil {
			// The PHP bridge runs as the webGUI user; root+group rw.
			if cerr := os.Chmod(socket, 0o660); cerr != nil {
				log.Printf("warning: chmod socket: %v", cerr)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	srv := &http.Server{
		Handler:           api.NewRouter(deps),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: /index/events streams indefinitely and fs/raw
		// serves multi-GB files; per-request deadlines live in the handlers
		// (SSE arms a per-write deadline itself).
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	log.Printf("version %s, browse roots %v, index roots %v, data %s", version, browseRoots, cfg.Index.Roots, dataDir)

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}
