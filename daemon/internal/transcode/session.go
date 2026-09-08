package transcode

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"sync"
	"time"

	"unraid-filebrowser/internal/types"
)

// SessionRequest is the body of POST /media/session.
type SessionRequest struct {
	Path string `json:"path"`
	// Can is the browser's codec capability list, e.g.
	// ["h264","vp9","aac","opus","mp3"]. Empty means "unknown" and forces a
	// full transcode to H.264/AAC.
	Can []string `json:"can"`
	// AudioIndex selects an audio stream by ffprobe index; nil = default.
	AudioIndex *int `json:"audioIndex"`
	// MaxHeight caps the output height. Below the source height it forces a
	// video transcode (the user asked for less); at or above it is ignored.
	MaxHeight int `json:"maxHeight"`
}

// SessionInfo is the payload of POST /media/session.
type SessionInfo struct {
	ID           string        `json:"id"`
	Mode         string        `json:"mode"` // "remux" | "transcode"
	Reason       string        `json:"reason"`
	DurationSec  float64       `json:"durationSec"`
	SegmentSec   float64       `json:"segmentSec"`
	SegmentCount int           `json:"segmentCount"`
	Playlist     string        `json:"playlist"` // /api/v1/media/hls/<id>/index.m3u8
	Video        *SessionVideo `json:"video"`    // null for audio-only
	Audio        *SessionAudio `json:"audio"`    // null when the file has no audio
}

// SessionVideo describes what the segments will carry.
type SessionVideo struct {
	Codec  string `json:"codec"` // output codec: "h264" when transcoding, else the source codec
	Width  int    `json:"width"`
	Height int    `json:"height"`
	// Bitrate is the ceiling (bits/s) applied when transcoding; 0 when the
	// stream is copied.
	Bitrate int64 `json:"bitrate"`
	Copied  bool  `json:"copied"`
}

// SessionAudio describes the audio track the segments will carry.
type SessionAudio struct {
	Codec    string `json:"codec"`
	Channels int    `json:"channels"`
	Lang     string `json:"lang"`
	Index    int    `json:"index"` // source stream index that was selected
	Copied   bool   `json:"copied"`
}

// session is the in-memory record behind a playlist. Everything a segment
// needs is here; nothing is on disk.
type session struct {
	id      string
	path    string
	probe   Probe
	plan    plan
	segSec  float64
	count   int
	created time.Time

	mu       sync.Mutex
	lastUsed time.Time
	active   int  // in-flight segment producers
	hwFailed bool // hardware encode failed once; stay in software
	// kf caches keyframe lookups by boundary index (keyframe.go).
	kf map[int]kfPoint
}

func (ss *session) touch() {
	ss.mu.Lock()
	ss.lastUsed = time.Now()
	ss.mu.Unlock()
}

func (ss *session) begin() {
	ss.mu.Lock()
	ss.active++
	ss.lastUsed = time.Now()
	ss.mu.Unlock()
}

func (ss *session) end() {
	ss.mu.Lock()
	ss.active--
	ss.lastUsed = time.Now()
	ss.mu.Unlock()
}

// idle reports whether the session has no producer running and was last
// touched before cutoff.
func (ss *session) idle(cutoff time.Time) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.active == 0 && ss.lastUsed.Before(cutoff)
}

// sessionIDPattern is the only shape a session id may take on the wire.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidSessionID reports whether id is well-formed (16 random bytes, hex).
func ValidSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", types.Errf(types.ErrInternal, "random source unavailable")
	}
	return hex.EncodeToString(b[:]), nil
}

// store holds live sessions with an LRU bound and an idle TTL.
type store struct {
	mu       sync.Mutex
	sessions map[string]*session
	max      int
	ttl      time.Duration
}

func newStore(max int, ttl time.Duration) *store {
	return &store{sessions: make(map[string]*session), max: max, ttl: ttl}
}

func (st *store) get(id string) (*session, error) {
	if !ValidSessionID(id) {
		return nil, types.Errf(types.ErrBadRequest, "malformed session id")
	}
	st.mu.Lock()
	ss := st.sessions[id]
	st.mu.Unlock()
	if ss == nil {
		return nil, types.Errf(types.ErrNotFound, "no such media session (expired or closed)")
	}
	ss.touch()
	return ss, nil
}

// put adds a session, evicting the least recently used idle one when full.
// When every session is busy the new one is refused: the caller is told the
// server is busy rather than an active player being killed.
func (st *store) put(ss *session) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.sessions) >= st.max {
		var victim *session
		for _, cand := range st.sessions {
			cand.mu.Lock()
			busy := cand.active > 0
			lu := cand.lastUsed
			cand.mu.Unlock()
			if busy {
				continue
			}
			if victim == nil || lu.Before(victim.lastUsed) {
				victim = cand
			}
		}
		if victim == nil {
			return types.Errf(types.ErrTimeout, "server busy: too many active media sessions")
		}
		delete(st.sessions, victim.id)
	}
	st.sessions[ss.id] = ss
	return nil
}

func (st *store) remove(id string) {
	st.mu.Lock()
	delete(st.sessions, id)
	st.mu.Unlock()
}

// sweep drops sessions idle for longer than the TTL.
func (st *store) sweep(now time.Time) int {
	cutoff := now.Add(-st.ttl)
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for id, ss := range st.sessions {
		if ss.idle(cutoff) {
			delete(st.sessions, id)
			n++
		}
	}
	return n
}

func (st *store) len() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions)
}
