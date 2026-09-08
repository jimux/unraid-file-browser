package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"unraid-filebrowser/internal/transcode"
	"unraid-filebrowser/internal/types"
)

// MediaService is the on-the-fly HLS transcoder (internal/transcode). Nil in
// Deps means the daemon was built or started without it; the endpoints then
// answer UNAVAILABLE, except capabilities which reports available:false.
type MediaService interface {
	Capabilities() transcode.Capabilities
	Probe(ctx context.Context, path string) (transcode.Probe, error)
	CreateSession(ctx context.Context, req transcode.SessionRequest) (transcode.SessionInfo, error)
	Playlist(id string) (string, error)
	// OpenSegment returns the segment's bytes once the producer has emitted
	// the first of them, so failures before that still get a JSON envelope.
	OpenSegment(ctx context.Context, id string, n int) (io.ReadCloser, error)
	CloseSession(id string)
}

// segmentPattern is the only segment name shape the contract allows; the
// number is validated against the session's segment count by the service.
var segmentPattern = regexp.MustCompile(`^seg-(\d{5,7})\.ts$`)

type probeData struct {
	Probe transcode.Probe `json:"probe"`
}

type sessionData struct {
	Session transcode.SessionInfo `json:"session"`
}

type closedData struct {
	OK bool `json:"ok"`
}

func (s *server) media() (MediaService, error) {
	if s.Media == nil {
		return nil, types.Errf(types.ErrUnavailable, "media transcoding is not available on this host")
	}
	return s.Media, nil
}

// mediaHeaders are on every media response: nothing sniffed, nothing in a
// shared cache. Segments override Cache-Control (see mediaSegment).
func mediaHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-store")
}

func (s *server) mediaCapabilities(w http.ResponseWriter, r *http.Request) (any, error) {
	if s.Media == nil {
		return transcode.Capabilities{
			HWAccels: []string{}, Encoders: []string{},
			Reason: "media transcoding is not configured in this daemon",
		}, nil
	}
	return s.Media.Capabilities(), nil
}

func (s *server) mediaProbe(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.media()
	if err != nil {
		return nil, err
	}
	path, err := pathParam(r.URL.Query())
	if err != nil {
		return nil, err
	}
	p, err := m.Probe(r.Context(), path)
	if err != nil {
		return nil, err
	}
	return probeData{Probe: p}, nil
}

func (s *server) mediaCreateSession(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.media()
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
	var req transcode.SessionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, types.Errf(types.ErrBadRequest, "invalid JSON body")
	}
	if strings.TrimSpace(req.Path) == "" {
		return nil, types.Errf(types.ErrBadRequest, "path is required")
	}
	if req.MaxHeight < 0 {
		return nil, types.Errf(types.ErrBadRequest, "maxHeight must be positive")
	}
	if req.AudioIndex != nil && *req.AudioIndex < 0 {
		return nil, types.Errf(types.ErrBadRequest, "audioIndex must be a stream index")
	}
	info, err := m.CreateSession(r.Context(), req)
	if err != nil {
		return nil, err
	}
	return sessionData{Session: info}, nil
}

func (s *server) mediaCloseSession(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.media()
	if err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	if !transcode.ValidSessionID(id) {
		return nil, types.Errf(types.ErrBadRequest, "malformed session id")
	}
	m.CloseSession(id)
	return closedData{OK: true}, nil
}

// mediaPlaylist serves the VOD playlist. Not JSON, so not wrapped; errors
// still render as envelopes.
func (s *server) mediaPlaylist(w http.ResponseWriter, r *http.Request) {
	m, err := s.media()
	if err != nil {
		writeError(w, r, err)
		return
	}
	pl, err := m.Playlist(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	mediaHeaders(w)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(pl)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, pl)
}

// mediaSegment streams one segment straight from ffmpeg. It is deliberately
// outside wrap(): the producer has its own per-segment deadline, and the
// request context (client disconnect) is what kills it.
func (s *server) mediaSegment(w http.ResponseWriter, r *http.Request) {
	m, err := s.media()
	if err != nil {
		writeError(w, r, err)
		return
	}
	match := segmentPattern.FindStringSubmatch(r.PathValue("seg"))
	if match == nil {
		writeError(w, r, types.Errf(types.ErrNotFound, "no such segment"))
		return
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		writeError(w, r, types.Errf(types.ErrNotFound, "no such segment"))
		return
	}
	rc, err := m.OpenSegment(r.Context(), r.PathValue("id"), n)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer rc.Close()

	mediaHeaders(w)
	h := w.Header()
	h.Set("Content-Type", "video/mp2t")
	// A segment of a given session is immutable, and the id is unguessable,
	// so the browser's private cache may keep it: a seek back does not cost a
	// second transcode. Never shared caches (the bridge is authenticated).
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("Accept-Ranges", "none")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
