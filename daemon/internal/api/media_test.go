package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"unraid-filebrowser/internal/transcode"
	"unraid-filebrowser/internal/types"
)

const testSessionID = "0123456789abcdef0123456789abcdef"

type fakeMedia struct {
	mu       sync.Mutex
	closed   []string
	lastReq  transcode.SessionRequest
	segments map[int][]byte
	segErr   error
	opened   []int
	cancels  int
}

func (f *fakeMedia) Capabilities() transcode.Capabilities {
	return transcode.Capabilities{Available: true, FFmpeg: "7.0.2", FFprobe: "7.0.2", HWAccels: []string{}, Encoders: []string{"aac", "libx264"}}
}

func (f *fakeMedia) Probe(ctx context.Context, path string) (transcode.Probe, error) {
	if path == "/missing" {
		return transcode.Probe{}, types.Errf(types.ErrNotFound, "no such file")
	}
	return transcode.Probe{Container: "avi", DurationSec: 10, Audio: []transcode.AudioStream{}, Subtitles: []transcode.SubStream{}}, nil
}

func (f *fakeMedia) CreateSession(ctx context.Context, req transcode.SessionRequest) (transcode.SessionInfo, error) {
	f.mu.Lock()
	f.lastReq = req
	f.mu.Unlock()
	return transcode.SessionInfo{ID: testSessionID, Mode: "remux", DurationSec: 10, SegmentSec: 4, SegmentCount: 3, Playlist: transcode.PlaylistPath(testSessionID)}, nil
}

func (f *fakeMedia) Playlist(id string) (string, error) {
	if id != testSessionID {
		return "", types.Errf(types.ErrNotFound, "no such session")
	}
	return "#EXTM3U\n#EXTINF:4.000,\nseg-00000.ts\n#EXT-X-ENDLIST\n", nil
}

type ctxReader struct {
	*bytes.Reader
	ctx context.Context
	f   *fakeMedia
}

func (c *ctxReader) Close() error {
	if c.ctx.Err() != nil {
		c.f.mu.Lock()
		c.f.cancels++
		c.f.mu.Unlock()
	}
	return nil
}

func (f *fakeMedia) OpenSegment(ctx context.Context, id string, n int) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, n)
	if f.segErr != nil {
		return nil, f.segErr
	}
	if id != testSessionID {
		return nil, types.Errf(types.ErrNotFound, "no such session")
	}
	b, ok := f.segments[n]
	if !ok {
		return nil, types.Errf(types.ErrNotFound, "segment out of range")
	}
	return &ctxReader{Reader: bytes.NewReader(b), ctx: ctx, f: f}, nil
}

func (f *fakeMedia) CloseSession(id string) {
	f.mu.Lock()
	f.closed = append(f.closed, id)
	f.mu.Unlock()
}

func mediaServer(t *testing.T, m MediaService) http.Handler {
	t.Helper()
	return newRouter(newServer(Deps{FS: &fakeFS{}, Media: m}))
}

func doJSON(t *testing.T, h http.Handler, method, target string, body string) (*httptest.ResponseRecorder, envelopeJSON) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var env envelopeJSON
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("bad envelope: %v: %s", err, rec.Body.String())
		}
	}
	return rec, env
}

type envelopeJSON struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestMediaUnavailableWhenNotWired(t *testing.T) {
	h := mediaServer(t, nil)
	rec, env := doJSON(t, h, http.MethodGet, base+"/media/capabilities", "")
	if rec.Code != 200 || !env.OK {
		t.Fatalf("capabilities must never error: %d %s", rec.Code, rec.Body.String())
	}
	var caps transcode.Capabilities
	if err := json.Unmarshal(env.Data, &caps); err != nil || caps.Available || caps.Reason == "" || caps.Encoders == nil {
		t.Errorf("caps = %+v %v", caps, err)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base + "/media/probe?path=/x"},
		{http.MethodPost, base + "/media/session"},
		{http.MethodPost, base + "/media/session/" + testSessionID + "/close"},
		{http.MethodGet, base + "/media/hls/" + testSessionID + "/index.m3u8"},
		{http.MethodGet, base + "/media/hls/" + testSessionID + "/seg-00000.ts"},
	} {
		rec, env := doJSON(t, h, tc.method, tc.path, `{"path":"/x"}`)
		if rec.Code != http.StatusServiceUnavailable || env.OK || env.Error == nil || env.Error.Code != types.ErrUnavailable {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestMediaProbeAndSession(t *testing.T) {
	fm := &fakeMedia{}
	h := mediaServer(t, fm)

	rec, env := doJSON(t, h, http.MethodGet, base+"/media/probe?path=/a.avi", "")
	if rec.Code != 200 || !strings.Contains(string(env.Data), `"probe":{"container":"avi"`) {
		t.Errorf("probe: %d %s", rec.Code, rec.Body.String())
	}
	if rec, env := doJSON(t, h, http.MethodGet, base+"/media/probe", ""); rec.Code != 400 || env.Error.Code != types.ErrBadRequest {
		t.Errorf("probe without path: %d", rec.Code)
	}
	if rec, env := doJSON(t, h, http.MethodGet, base+"/media/probe?path=/missing", ""); rec.Code != 404 || env.Error.Code != types.ErrNotFound {
		t.Errorf("probe missing: %d", rec.Code)
	}

	rec, env = doJSON(t, h, http.MethodPost, base+"/media/session", `{"path":"/a.mkv","can":["h264","aac"],"audioIndex":2,"maxHeight":720}`)
	if rec.Code != 200 || !strings.Contains(string(env.Data), `"session":{"id":"`+testSessionID+`"`) {
		t.Fatalf("session: %d %s", rec.Code, rec.Body.String())
	}
	if fm.lastReq.Path != "/a.mkv" || len(fm.lastReq.Can) != 2 || fm.lastReq.AudioIndex == nil || *fm.lastReq.AudioIndex != 2 || fm.lastReq.MaxHeight != 720 {
		t.Errorf("request not passed through: %+v", fm.lastReq)
	}
	for body, want := range map[string]string{
		"":                              types.ErrBadRequest,
		"{not json":                     types.ErrBadRequest,
		`{"can":["h264"]}`:              types.ErrBadRequest,
		`{"path":"/a","maxHeight":-1}`:  types.ErrBadRequest,
		`{"path":"/a","audioIndex":-3}`: types.ErrBadRequest,
	} {
		rec, env := doJSON(t, h, http.MethodPost, base+"/media/session", body)
		if rec.Code != 400 || env.Error == nil || env.Error.Code != want {
			t.Errorf("body %q: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	rec, env = doJSON(t, h, http.MethodPost, base+"/media/session", strings.Repeat(" ", maxBodyBytes+1))
	if rec.Code != 413 || env.Error.Code != types.ErrTooLarge {
		t.Errorf("oversized body: %d", rec.Code)
	}

	// Close: idempotent, {ok:true}, malformed ids rejected before the service.
	rec, env = doJSON(t, h, http.MethodPost, base+"/media/session/"+testSessionID+"/close", "")
	if rec.Code != 200 || string(env.Data) != `{"ok":true}` {
		t.Errorf("close: %d %s", rec.Code, rec.Body.String())
	}
	doJSON(t, h, http.MethodPost, base+"/media/session/"+testSessionID+"/close", "")
	if len(fm.closed) != 2 {
		t.Errorf("close calls = %v", fm.closed)
	}
	rec, env = doJSON(t, h, http.MethodPost, base+"/media/session/..%2F..%2Fetc/close", "")
	if rec.Code != 400 && rec.Code != 404 {
		t.Errorf("traversal id: %d", rec.Code)
	}
	if len(fm.closed) != 2 {
		t.Error("malformed id must not reach the service")
	}
	// Only POST reaches the daemon through the bridge; DELETE is not a route.
	if rec, _ := doJSON(t, h, http.MethodDelete, base+"/media/session/"+testSessionID, ""); rec.Code != 404 {
		t.Errorf("DELETE: %d", rec.Code)
	}
}

func TestMediaPlaylistAndSegments(t *testing.T) {
	fm := &fakeMedia{segments: map[int][]byte{0: bytes.Repeat([]byte{0x47}, 188*3), 1: []byte("seg1")}}
	h := mediaServer(t, fm)

	req := httptest.NewRequest(http.MethodGet, base+"/media/hls/"+testSessionID+"/index.m3u8", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "private, no-store" ||
		!strings.HasPrefix(rec.Body.String(), "#EXTM3U") {
		t.Errorf("playlist: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if rec, env := doJSON(t, h, http.MethodGet, base+"/media/hls/ffffffffffffffffffffffffffffffff/index.m3u8", ""); rec.Code != 404 || env.Error.Code != types.ErrNotFound {
		t.Errorf("unknown session playlist: %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, base+"/media/hls/"+testSessionID+"/seg-00000.ts", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp2t" || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		rec.Header().Get("Cache-Control") != "private, max-age=3600" || rec.Body.Len() != 188*3 {
		t.Errorf("segment: %d %v len=%d", rec.Code, rec.Header(), rec.Body.Len())
	}
	for _, name := range []string{"seg-00002.ts", "seg-0.ts", "seg-00001.mp4", "seg-00001.ts.bak", "..%2F..%2Fetc%2Fpasswd", "index.m3u8.bak", "seg--0001.ts", "seg-99999999.ts"} {
		rec, env := doJSON(t, h, http.MethodGet, base+"/media/hls/"+testSessionID+"/"+name, "")
		if rec.Code != 404 || env.Error == nil || env.Error.Code != types.ErrNotFound {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Malformed names never reach the service; only the two in-range and
	// the one out-of-range (seg-00002) do.
	if len(fm.opened) != 2 {
		t.Errorf("service saw %v", fm.opened)
	}
	// Errors before the first byte come back as an envelope with status.
	fm.segErr = types.Errf(types.ErrTimeout, "server busy")
	rec, env := doJSON(t, h, http.MethodGet, base+"/media/hls/"+testSessionID+"/seg-00001.ts", "")
	if rec.Code != 504 || env.Error.Code != types.ErrTimeout || env.Error.Message != "server busy" {
		t.Errorf("busy: %d %s", rec.Code, rec.Body.String())
	}
}
