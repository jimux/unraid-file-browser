package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"unraid-filebrowser/internal/config"
	"unraid-filebrowser/internal/types"
)

// sseMinInterval is the floor between two SSE status events (API.md: max one
// per second). Updates arriving faster are coalesced to the newest one.
const sseMinInterval = time.Second

// SSE guardrails. The daemon only emits on status change, so a stream can sit
// silent for hours: without a heartbeat neither the PHP bridge nor nginx can
// tell a live browser from a dead one, and the goroutine leaks. And without a
// per-write deadline a client that opens the stream and never reads pins that
// goroutine forever, because the server's WriteTimeout is 0 on purpose (big
// downloads).
const (
	sseHeartbeatInterval = 15 * time.Second
	sseWriteTimeout      = 10 * time.Second
	sseMaxSubscribers    = 32
)

// maxBodyBytes caps request bodies; the only bodies we take are small JSON.
const maxBodyBytes = 1 << 20

type searchData struct {
	Hits       []types.SearchHit `json:"hits"`
	Total      int               `json:"total"`
	IndexFresh bool              `json:"indexFresh"`
	TookMs     int64             `json:"tookMs"`
}

type configData struct {
	Config types.IndexConfig `json:"config"`
	// AllowedRoots are the daemon's boot-time browse roots: every entry in
	// Config.Roots must live inside one of them.
	AllowedRoots []string `json:"allowedRoots"`
}

type healthData struct {
	Version   string   `json:"version"`
	UptimeSec int64    `json:"uptimeSec"`
	IndexDB   string   `json:"indexDb"`
	Roots     []string `json:"roots"`
}

// configResponse is the shared shape of GET and PUT /index/config.
func configResponse(idx IndexService) configData {
	roots := idx.AllowedRoots()
	if roots == nil {
		roots = []string{}
	}
	cfg := idx.Config()
	// JSON clients index these lists; never hand them null.
	if cfg.Roots == nil {
		cfg.Roots = []string{}
	}
	if cfg.Content.IncludePaths == nil {
		cfg.Content.IncludePaths = []string{}
	}
	if cfg.Content.Extensions == nil {
		cfg.Content.Extensions = []string{}
	}
	return configData{Config: cfg, AllowedRoots: roots}
}

// readBody reads a small JSON request body. http.MaxBytesReader (rather than
// io.LimitReader) so an oversized body is an explicit TOO_LARGE instead of
// silent truncation surfacing as a baffling "invalid JSON".
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, types.Errf(types.ErrTooLarge, "request body is larger than 1 MiB")
		}
		return nil, types.Errf(types.ErrBadRequest, "could not read request body")
	}
	return body, nil
}

func (s *server) search(w http.ResponseWriter, r *http.Request) (any, error) {
	idx, err := s.index()
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	text := q.Get("q")
	if strings.TrimSpace(text) == "" {
		return nil, types.Errf(types.ErrBadRequest, "q is required")
	}
	mode, err := enumParam(q, "mode", "both", "name", "content", "both")
	if err != nil {
		return nil, err
	}
	minSize, err := intParam(q, "minSize", -1, -1, maxOffset)
	if err != nil {
		return nil, err
	}
	maxSize, err := intParam(q, "maxSize", -1, -1, maxOffset)
	if err != nil {
		return nil, err
	}
	after, err := intParam(q, "after", 0, 0, maxOffset)
	if err != nil {
		return nil, err
	}
	before, err := intParam(q, "before", 0, 0, maxOffset)
	if err != nil {
		return nil, err
	}
	limit, err := intParam(q, "limit", searchLimitDefault, 0, searchLimitMax)
	if err != nil {
		return nil, err
	}
	offset, err := intParam(q, "offset", 0, 0, maxOffset)
	if err != nil {
		return nil, err
	}

	query := types.SearchQuery{
		Q:       text,
		Mode:    mode,
		Path:    q.Get("path"),
		Exts:    splitExts(q.Get("ext")),
		MinSize: minSize,
		MaxSize: maxSize,
		After:   after,
		Before:  before,
		Limit:   int(limit),
		Offset:  int(offset),
	}

	start := time.Now()
	hits, total, err := idx.Search(r.Context(), query)
	took := time.Since(start)
	if err != nil {
		return nil, err
	}
	if hits == nil {
		hits = []types.SearchHit{}
	}
	st := idx.Status()
	return searchData{
		Hits:       hits,
		Total:      total,
		IndexFresh: st.State == "idle" && st.LastFullScan > 0,
		TookMs:     took.Milliseconds(),
	}, nil
}

func (s *server) indexStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	idx, err := s.index()
	if err != nil {
		return nil, err
	}
	return idx.Status(), nil
}

func (s *server) indexGetConfig(w http.ResponseWriter, r *http.Request) (any, error) {
	idx, err := s.index()
	if err != nil {
		return nil, err
	}
	return configResponse(idx), nil
}

func (s *server) indexPutConfig(w http.ResponseWriter, r *http.Request) (any, error) {
	idx, err := s.index()
	if err != nil {
		return nil, err
	}
	body, err := readBody(w, r)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, types.Errf(types.ErrBadRequest, "request body is required")
	}
	// Accept both {"config": {...}} (the response shape, symmetric for the
	// SPA) and a bare IndexConfig object.
	var wrapper struct {
		Config *types.IndexConfig `json:"config"`
	}
	var cfg types.IndexConfig
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Config != nil {
		cfg = *wrapper.Config
	} else {
		var bare types.IndexConfig
		if err := json.Unmarshal(body, &bare); err != nil {
			return nil, types.Errf(types.ErrBadRequest, "invalid JSON body")
		}
		cfg = bare
	}
	// Omitted scalar fields take their documented defaults; anything present
	// is validated strictly by the index layer (no silent clamping on PUT).
	if cfg.Schedule == "" {
		cfg.Schedule = config.DefaultSchedule
	}
	if cfg.Parallelism == 0 {
		cfg.Parallelism = config.DefaultParallelism
	}
	if cfg.Content.MaxFileBytes == 0 {
		cfg.Content.MaxFileBytes = config.DefaultMaxFileBytes
	}
	if err := idx.SetConfig(cfg); err != nil {
		return nil, err
	}
	return configResponse(idx), nil
}

func (s *server) indexRescan(w http.ResponseWriter, r *http.Request) (any, error) {
	idx, err := s.index()
	if err != nil {
		return nil, err
	}
	path := r.URL.Query().Get("path")
	body, err := readBody(w, r)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, types.Errf(types.ErrBadRequest, "invalid JSON body")
		}
		if req.Path != "" {
			path = req.Path
		}
	}
	if err := idx.Rescan(path); err != nil {
		return nil, err
	}
	return idx.Status(), nil
}

func (s *server) indexPause(w http.ResponseWriter, r *http.Request) (any, error) {
	idx, err := s.index()
	if err != nil {
		return nil, err
	}
	idx.Pause()
	return idx.Status(), nil
}

func (s *server) indexResume(w http.ResponseWriter, r *http.Request) (any, error) {
	idx, err := s.index()
	if err != nil {
		return nil, err
	}
	idx.Resume()
	return idx.Status(), nil
}

// sseAdmit reserves one of the bounded SSE slots.
func (s *server) sseAdmit() bool {
	maxSubs := int64(s.sseMax)
	if maxSubs <= 0 {
		maxSubs = sseMaxSubscribers
	}
	if s.sseSubs.Add(1) > maxSubs {
		s.sseSubs.Add(-1)
		return false
	}
	return true
}

func (s *server) sseRelease() { s.sseSubs.Add(-1) }

// indexEvents is the SSE stream. It is not wrapped: no envelope, no request
// timeout — it lives until the client disconnects. Three things keep that from
// being a resource leak: a cap on concurrent subscribers, a deadline on every
// individual write (the server has no WriteTimeout, so a client that stops
// reading would otherwise block this goroutine forever), and a heartbeat so
// both ends can notice a dead peer on an otherwise silent stream.
func (s *server) indexEvents(w http.ResponseWriter, r *http.Request) {
	idx, err := s.index()
	if err != nil {
		writeError(w, r, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, types.Errf(types.ErrInternal, "streaming is not supported"))
		return
	}
	if !s.sseAdmit() {
		writeError(w, r, types.Errf(types.ErrIndexing,
			"too many event stream subscribers; poll /index/status instead"))
		return
	}
	defer s.sseRelease()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx in front of the PHP bridge

	ctrl := http.NewResponseController(w)
	// SetWriteDeadline is unsupported on some ResponseWriters (httptest's
	// recorder, wrapped writers); the error is deliberately ignored — the
	// stream still works, it just loses this particular guarantee.
	deadline := func() { _ = ctrl.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) }

	deadline()
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, unsubscribe := idx.Subscribe()
	defer unsubscribe()

	// write emits one already-formatted SSE frame; false means the client is
	// gone (or wedged past the write deadline) and the stream must end.
	write := func(frame string) bool {
		deadline()
		if _, err := io.WriteString(w, frame); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	send := func(st types.IndexStatus) bool {
		b, err := json.Marshal(st)
		if err != nil {
			return true
		}
		return write("data: " + string(b) + "\n\n")
	}

	if !send(idx.Status()) {
		return
	}
	last := time.Now()

	beat := s.sseHeartbeat
	if beat <= 0 {
		beat = sseHeartbeatInterval
	}
	heartbeat := time.NewTicker(beat)
	defer heartbeat.Stop()

	var pending *types.IndexStatus
	coalesce := time.NewTicker(sseMinInterval)
	defer coalesce.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case st, ok := <-events:
			if !ok {
				return
			}
			if time.Since(last) >= sseMinInterval {
				if !send(st) {
					return
				}
				last = time.Now()
				pending = nil
				continue
			}
			cur := st
			pending = &cur
		case <-coalesce.C:
			if pending == nil {
				continue
			}
			if !send(*pending) {
				return
			}
			last = time.Now()
			pending = nil
		case <-heartbeat.C:
			// A comment frame: valid SSE, ignored by EventSource, but it puts
			// bytes on the wire so a closed browser surfaces as a write error
			// here and in the PHP bridge.
			if !write(": keepalive\n\n") {
				return
			}
		}
	}
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) (any, error) {
	db := "missing"
	if s.Index != nil {
		if st := s.Index.Status(); st.DBBytes > 0 {
			db = "ok"
		}
	}
	roots := s.Roots
	if roots == nil {
		roots = []string{}
	}
	return healthData{
		Version:   s.Version,
		UptimeSec: int64(time.Since(s.started).Seconds()),
		IndexDB:   db,
		Roots:     roots,
	}, nil
}
