// Package api is the HTTP surface of filebrowserd: routing, the JSON
// envelope, parameter validation and the SSE stream. It owns everything the
// contract in API.md specifies about the wire and nothing about how bytes are
// found — the filesystem, archive and index subsystems arrive as interfaces.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"unraid-filebrowser/internal/types"
)

// requestTimeout bounds every request except the two that are streams by
// nature: raw downloads (a 40 GB file takes longer than any sane timeout) and
// the SSE event stream (which lives until the client goes away). Those two are
// routed without wrap() precisely so no deadline is attached to their context:
// a multi-gigabyte download must not be killed mid-flight.
const requestTimeout = 30 * time.Second

// Concurrency guardrails for the endpoints that can each cost a directory
// walk, an FTS query or an archive extraction. Without a cap a handful of
// clients hitting fs/list on a 50k-entry share is enough to pin the box.
const (
	busyLimit = 8
	busyWait  = 5 * time.Second
)

// maxListEntries is the ceiling on a single directory listing. Above it the
// response would be tens of megabytes of JSON that no UI can use, so we refuse
// instead of allocating it.
const maxListEntries = 250_000

// FileSystem is the real-filesystem provider (internal/fsops).
type FileSystem interface {
	List(ctx context.Context, path string) ([]types.Entry, error)
	Stat(ctx context.Context, path string) (types.Entry, error)
	Open(ctx context.Context, path string) (io.ReadCloser, types.Entry, error)
}

// ArchiveFS resolves virtual paths that reach inside archive files
// (internal/archive). IsVirtual decides the routing for every fs/* endpoint.
type ArchiveFS interface {
	IsVirtual(path string) bool
	List(ctx context.Context, vpath string) ([]types.Entry, error)
	Stat(ctx context.Context, vpath string) (types.Entry, error)
	Open(ctx context.Context, vpath string) (io.ReadCloser, types.Entry, error)
}

// IndexService is the search index (internal/index).
type IndexService interface {
	Search(ctx context.Context, q types.SearchQuery) ([]types.SearchHit, int, error)
	Status() types.IndexStatus
	Subscribe() (<-chan types.IndexStatus, func())
	Config() types.IndexConfig
	SetConfig(cfg types.IndexConfig) error
	// AllowedRoots are the boot-time browse roots (-roots); every index root
	// must lie inside one of them, and the settings UI needs the list to say
	// so before the user submits.
	AllowedRoots() []string
	Rescan(path string) error
	Pause()
	Resume()
}

// Deps are the subsystems the router serves. Archive, Index and Media may be
// nil: the daemon must still browse and stream files when the index database
// is unavailable (array not started) or ffmpeg is absent; index endpoints then
// answer INDEXING and media endpoints UNAVAILABLE.
type Deps struct {
	FS      FileSystem
	Archive ArchiveFS
	Index   IndexService
	// Media is the HLS transcoder; nil when ffmpeg is not wired in.
	Media MediaService
	// Roots are the boot-time browse roots (-roots); reported by /healthz so
	// the SPA and the plugin can show what the daemon will let them reach.
	Roots   []string
	Version string
	DevMode bool
}

type server struct {
	Deps
	started time.Time

	// busy admits a bounded number of expensive requests at a time.
	busy *limiter

	// SSE knobs; fields rather than constants so tests can shorten them.
	sseMax       int
	sseHeartbeat time.Duration
	sseSubs      atomic.Int64
}

// newServer builds the handler state. NewRouter is the exported door; tests in
// this package use newServer/newRouter directly to shorten the SSE heartbeat
// and shrink the concurrency limiter.
func newServer(d Deps) *server {
	return &server{
		Deps:         d,
		started:      time.Now(),
		busy:         newLimiter(busyLimit, busyWait),
		sseMax:       sseMaxSubscribers,
		sseHeartbeat: sseHeartbeatInterval,
	}
}

// base is the API root every endpoint hangs off (API.md).
const base = "/api/v1"

// route is one line of the contract's endpoint table.
type route struct {
	method  string
	path    string
	handler http.HandlerFunc
}

// NewRouter builds the /api/v1 handler.
func NewRouter(d Deps) http.Handler { return newRouter(newServer(d)) }

func newRouter(s *server) http.Handler {
	routes := []route{
		{http.MethodGet, base + "/fs/list", s.wrap(s.limited(s.fsList))},
		{http.MethodGet, base + "/fs/stat", s.wrap(s.fsStat)},
		{http.MethodGet, base + "/fs/view", s.wrap(s.fsView)},
		{http.MethodGet, base + "/fs/hex", s.wrap(s.fsHex)},
		{http.MethodGet, base + "/fs/raw", s.fsRaw},
		{http.MethodGet, base + "/encodings", s.wrap(s.encodings)},
		{http.MethodGet, base + "/search", s.wrap(s.limited(s.search))},
		{http.MethodGet, base + "/index/status", s.wrap(s.indexStatus)},
		{http.MethodGet, base + "/index/events", s.indexEvents},
		{http.MethodGet, base + "/index/config", s.wrap(s.indexGetConfig)},
		{http.MethodPut, base + "/index/config", s.wrap(s.indexPutConfig)},
		{http.MethodPost, base + "/index/rescan", s.wrap(s.indexRescan)},
		{http.MethodPost, base + "/index/pause", s.wrap(s.indexPause)},
		{http.MethodPost, base + "/index/resume", s.wrap(s.indexResume)},
		{http.MethodGet, base + "/healthz", s.wrap(s.healthz)},
		{http.MethodGet, base + "/media/capabilities", s.wrap(s.mediaCapabilities)},
		{http.MethodGet, base + "/media/probe", s.wrap(s.mediaProbe)},
		{http.MethodPost, base + "/media/session", s.wrap(s.mediaCreateSession)},
		{http.MethodPost, base + "/media/session/{id}/close", s.wrap(s.mediaCloseSession)},
		{http.MethodGet, base + "/media/hls/{id}/index.m3u8", s.mediaPlaylist},
		{http.MethodGet, base + "/media/hls/{id}/{seg}", s.mediaSegment},
	}

	mux := http.NewServeMux()
	allowed := make(map[string][]string, len(routes))
	for _, rt := range routes {
		mux.HandleFunc(rt.method+" "+rt.path, rt.handler)
		allowed[rt.path] = append(allowed[rt.path], rt.method)
	}
	// Catch-all so that *every* JSON client sees the envelope, including for a
	// mistyped path — net/http's own 404 is plain text. Registering "/" means
	// the mux can no longer produce 405 by itself, so the method check that
	// would have produced it lives here instead.
	mux.HandleFunc("/", s.unrouted(allowed))

	var h http.Handler = mux
	if s.DevMode {
		h = withCORS(h)
	}
	return h
}

// unrouted answers anything the contract does not define: 405 (with Allow) when
// the path exists under another method, 404 otherwise — both as envelopes.
func (s *server) unrouted(allowed map[string][]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		methods, ok := allowed[strings.TrimSuffix(r.URL.Path, "/")]
		if !ok {
			methods, ok = allowed[r.URL.Path]
		}
		if ok {
			w.Header().Set("Allow", strings.Join(append(methods, http.MethodOptions), ", "))
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(envelope{OK: false, Error: &errorBody{
				Code:    types.ErrBadRequest,
				Message: r.Method + " is not allowed on " + r.URL.Path,
			}})
			return
		}
		writeError(w, r, types.Errf(types.ErrNotFound, "no such endpoint: "+r.URL.Path))
	}
}

// handler is an endpoint that returns the payload to wrap in the envelope, or
// an error to render as one.
type handler func(w http.ResponseWriter, r *http.Request) (any, error)

// wrap applies the request timeout and renders the JSON envelope.
func (s *server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()

		data, err := h(w, r.WithContext(ctx))
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeOK(w, data)
	}
}

// withCORS opens the API up for the Vite dev server. Production serves through
// the authenticated PHP bridge on a unix socket and never sets these.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "*")
		h.Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// archive reports whether path addresses something inside an archive.
func (s *server) isVirtual(path string) bool {
	return s.Archive != nil && s.Archive.IsVirtual(path)
}

// list routes a listing to the archive resolver or the real filesystem, and
// refuses listings too big to serialise (see maxListEntries).
func (s *server) list(ctx context.Context, path string) ([]types.Entry, error) {
	var (
		entries []types.Entry
		err     error
	)
	switch {
	case s.isVirtual(path):
		entries, err = s.Archive.List(ctx, path)
	case s.FS == nil:
		return nil, types.Errf(types.ErrInternal, "filesystem unavailable")
	default:
		entries, err = s.FS.List(ctx, path)
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > maxListEntries {
		return nil, types.Errf(types.ErrTooLarge,
			"directory too large to list ("+strconv.Itoa(len(entries))+" entries, limit "+
				strconv.Itoa(maxListEntries)+"); narrow with search")
	}
	return entries, nil
}

func (s *server) stat(ctx context.Context, path string) (types.Entry, error) {
	if s.isVirtual(path) {
		return s.Archive.Stat(ctx, path)
	}
	if s.FS == nil {
		return types.Entry{}, types.Errf(types.ErrInternal, "filesystem unavailable")
	}
	return s.FS.Stat(ctx, path)
}

func (s *server) open(ctx context.Context, path string) (io.ReadCloser, types.Entry, error) {
	if s.isVirtual(path) {
		return s.Archive.Open(ctx, path)
	}
	if s.FS == nil {
		return nil, types.Entry{}, types.Errf(types.ErrInternal, "filesystem unavailable")
	}
	return s.FS.Open(ctx, path)
}

// index returns the index service or an INDEXING error when none is wired.
func (s *server) index() (IndexService, error) {
	if s.Index == nil {
		return nil, types.Errf(types.ErrIndexing, "index is unavailable")
	}
	return s.Index, nil
}
