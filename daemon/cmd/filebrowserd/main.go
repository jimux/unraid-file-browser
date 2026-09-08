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
	"syscall"
	"time"

	"unraid-filebrowser/internal/api"
	"unraid-filebrowser/internal/archive"
	"unraid-filebrowser/internal/config"
	"unraid-filebrowser/internal/fsops"
	"unraid-filebrowser/internal/index"
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
	if rootOverride || len(cfg.Index.Roots) == 0 {
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

	var idx *index.Service
	if dataDirOK {
		idx, err = index.New(filepath.Join(dataDir, "index.db"), cfg.Index, index.Options{AllowedRoots: browseRoots})
		if err != nil {
			log.Printf("warning: index unavailable: %v", err)
		} else {
			defer idx.Close()
			go idx.RunScheduler(ctx)
			deps.Index = &persistingIndex{Service: idx, cfgPath: cfgPath, dataDir: dataDir}
		}
	}

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
