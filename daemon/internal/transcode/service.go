package transcode

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"unraid-filebrowser/internal/types"
)

// Options tunes a Service. Zero values select the documented defaults.
type Options struct {
	FFmpegPath  string // "" → Discover()
	FFprobePath string // "" → Discover()

	// MaxProcs caps simultaneous ffmpeg processes (segment producers and
	// keyframe lookups). Default 3. hls.js fetches segments sequentially, so
	// one player normally holds at most one slot at a time; the cap is the
	// number of players that can transcode concurrently before a request
	// waits, then fails with TIMEOUT "server busy" after ProcWait.
	MaxProcs int
	ProcWait time.Duration // default 10s

	// SegmentSec is the nominal segment length for transcoded video (default
	// 4: short enough that a slow CPU still starts playback quickly).
	// RemuxSegmentSec is used whenever video is copied (default 10: copy cuts
	// must fall on keyframes, and x264's default keyframe interval is ~10 s,
	// so longer segments keep the cuts clean without costing CPU).
	SegmentSec      float64
	RemuxSegmentSec float64

	SegmentTimeout time.Duration // wall time per segment, default 60s
	MaxSessions    int           // default 8; idle ones are LRU-evicted
	SessionTTL     time.Duration // idle expiry, default 10m
	SweepInterval  time.Duration // default 1m

	// DeviceExists is the probe for the VAAPI render node; nil → os.Stat.
	DeviceExists func(string) bool
	// DisableHW forces software encoding even when hardware is detected.
	DisableHW bool
}

func (o *Options) defaults() {
	if o.MaxProcs <= 0 {
		o.MaxProcs = 3
	}
	if o.ProcWait <= 0 {
		o.ProcWait = 10 * time.Second
	}
	if o.SegmentSec <= 0 {
		o.SegmentSec = 4
	}
	if o.RemuxSegmentSec <= 0 {
		o.RemuxSegmentSec = 10
	}
	if o.SegmentTimeout <= 0 {
		o.SegmentTimeout = 60 * time.Second
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = 8
	}
	if o.SessionTTL <= 0 {
		o.SessionTTL = 10 * time.Minute
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = time.Minute
	}
	if o.DeviceExists == nil {
		o.DeviceExists = fileExists
	}
}

// maxSegments bounds a playlist (≈ 4.6 days at 4 s); beyond it the file is
// refused rather than the playlist becoming megabytes.
const maxSegments = 100_000

// Service produces HLS sessions and segments. It satisfies the api package's
// MediaService interface.
type Service struct {
	opener  Opener
	opts    Options
	ffmpeg  string
	ffprobe string
	caps    Capabilities
	hwEnc   string // "" = software only

	sem      chan struct{}
	sessions *store

	running atomic.Int64 // live child processes (tests assert this drains)

	stopOnce sync.Once
	stop     chan struct{}
}

// New builds a Service. It never fails: without ffmpeg/ffprobe the service
// reports available:false and every operation returns UNAVAILABLE, so the
// daemon still browses and downloads.
func New(opener Opener, opts Options) *Service {
	opts.defaults()
	ffmpeg, ffprobe := opts.FFmpegPath, opts.FFprobePath
	if ffmpeg == "" || ffprobe == "" {
		dm, dp := Discover()
		if ffmpeg == "" {
			ffmpeg = dm
		}
		if ffprobe == "" {
			ffprobe = dp
		}
	}
	s := &Service{
		opener:   opener,
		opts:     opts,
		ffmpeg:   ffmpeg,
		ffprobe:  ffprobe,
		sem:      make(chan struct{}, opts.MaxProcs),
		sessions: newStore(opts.MaxSessions, opts.SessionTTL),
		stop:     make(chan struct{}),
	}
	s.caps = inspect(context.Background(), ffmpeg, ffprobe)
	if s.caps.Available && !opts.DisableHW {
		s.hwEnc = chooseHW(s.caps.Encoders, opts.DeviceExists)
		s.caps.HWEncoder = s.hwEnc
	}
	if s.caps.Available {
		go s.sweeper()
	}
	return s
}

// Close stops the session sweeper. Running segment producers finish on their
// own (their HTTP requests own them).
func (s *Service) Close() { s.stopOnce.Do(func() { close(s.stop) }) }

func (s *Service) sweeper() {
	t := time.NewTicker(s.opts.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			if n := s.sessions.sweep(now); n > 0 {
				slog.Debug("media: swept idle sessions", "n", n)
			}
		}
	}
}

// Capabilities never errors (GET /media/capabilities).
func (s *Service) Capabilities() Capabilities { return s.caps }

func (s *Service) available() error {
	if !s.caps.Available {
		reason := s.caps.Reason
		if reason == "" {
			reason = "transcoding is unavailable"
		}
		return types.Errf(types.ErrUnavailable, reason)
	}
	return nil
}

// acquire takes a process slot, waiting at most ProcWait.
func (s *Service) acquire(ctx context.Context) (func(), error) {
	release := func() { <-s.sem }
	select {
	case s.sem <- struct{}{}:
		return release, nil
	default:
	}
	t := time.NewTimer(s.opts.ProcWait)
	defer t.Stop()
	select {
	case s.sem <- struct{}{}:
		return release, nil
	case <-t.C:
		return nil, types.Errf(types.ErrTimeout, "server busy")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Probe runs ffprobe on a file (GET /media/probe).
func (s *Service) Probe(ctx context.Context, path string) (Probe, error) {
	if err := s.available(); err != nil {
		return Probe{}, err
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return Probe{}, err
	}
	defer release()
	in, _, err := openInput(ctx, s.opener, path)
	if err != nil {
		return Probe{}, err
	}
	defer in.Close()
	s.running.Add(1)
	defer s.running.Add(-1)
	return s.probe(ctx, in)
}

// CreateSession probes the file, picks the cheapest treatment and registers
// a session (POST /media/session).
func (s *Service) CreateSession(ctx context.Context, req SessionRequest) (SessionInfo, error) {
	if err := s.available(); err != nil {
		return SessionInfo{}, err
	}
	if req.MaxHeight < 0 {
		return SessionInfo{}, types.Errf(types.ErrBadRequest, "maxHeight must be positive")
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return SessionInfo{}, err
	}
	defer release()

	in, _, err := openInput(ctx, s.opener, req.Path)
	if err != nil {
		return SessionInfo{}, err
	}
	s.running.Add(1)
	p, err := s.probe(ctx, in)
	s.running.Add(-1)
	in.Close()
	if err != nil {
		return SessionInfo{}, err
	}
	if p.DurationSec <= 0 {
		return SessionInfo{}, types.Errf(types.ErrEncoding, "the file's duration is unknown; a seekable stream cannot be built")
	}

	audioIndex := -1
	if req.AudioIndex != nil {
		audioIndex = *req.AudioIndex
	}
	pl, err := makePlan(p, req.Can, audioIndex, req.MaxHeight, false)
	if err != nil {
		return SessionInfo{}, err
	}
	segSec := s.opts.SegmentSec
	if pl.Video != nil && pl.CopyVideo {
		segSec = s.opts.RemuxSegmentSec
		count := segmentCount(p.DurationSec, segSec)
		sparse, err := s.sparseKeyframes(ctx, req.Path, pl.Video.Index, segSec, count)
		if err != nil {
			return SessionInfo{}, err
		}
		if sparse {
			pl, err = makePlan(p, req.Can, audioIndex, req.MaxHeight, true)
			if err != nil {
				return SessionInfo{}, err
			}
			segSec = s.opts.SegmentSec
		}
	}
	count := segmentCount(p.DurationSec, segSec)
	if count > maxSegments {
		return SessionInfo{}, types.Errf(types.ErrTooLarge, "file is too long to stream")
	}

	id, err := newSessionID()
	if err != nil {
		return SessionInfo{}, err
	}
	now := time.Now()
	ss := &session{id: id, path: req.Path, probe: p, plan: pl, segSec: segSec, count: count, created: now, lastUsed: now}
	if err := s.sessions.put(ss); err != nil {
		return SessionInfo{}, err
	}
	return s.info(ss), nil
}

func (s *Service) info(ss *session) SessionInfo {
	pl := ss.plan
	si := SessionInfo{
		ID:           ss.id,
		Mode:         pl.Mode,
		Reason:       pl.Reason,
		DurationSec:  ss.probe.DurationSec,
		SegmentSec:   ss.segSec,
		SegmentCount: ss.count,
		Playlist:     PlaylistPath(ss.id),
	}
	if pl.Video != nil {
		v := &SessionVideo{Codec: pl.Video.Codec, Width: pl.Video.Width, Height: pl.Video.Height, Copied: pl.CopyVideo}
		if !pl.CopyVideo {
			v.Codec = "h264"
			v.Bitrate = pl.MaxRateBPS
			if pl.OutHeight > 0 {
				v.Width, v.Height = pl.OutWidth, pl.OutHeight
			}
		}
		si.Video = v
	}
	if pl.Audio != nil {
		a := &SessionAudio{Codec: pl.Audio.Codec, Channels: pl.Audio.Channels, Lang: pl.Audio.Lang, Index: pl.Audio.Index, Copied: pl.CopyAudio}
		if !pl.CopyAudio {
			a.Codec = "aac"
			a.Channels = 2
		}
		si.Audio = a
	}
	return si
}

// PlaylistPath is the API path of a session's playlist.
func PlaylistPath(id string) string { return "/api/v1/media/hls/" + id + "/index.m3u8" }

// Playlist renders the VOD playlist (GET /media/hls/<id>/index.m3u8).
func (s *Service) Playlist(id string) (string, error) {
	if err := s.available(); err != nil {
		return "", err
	}
	ss, err := s.sessions.get(id)
	if err != nil {
		return "", err
	}
	return playlist(ss.probe.DurationSec, ss.segSec, ss.count), nil
}

// OpenSegment starts producing segment n and returns a stream of its bytes
// once ffmpeg has produced the first of them (GET /media/hls/<id>/seg-N.ts).
// The caller must Close the stream; that kills the producer if the client
// went away and releases the process slot.
func (s *Service) OpenSegment(ctx context.Context, id string, n int) (io.ReadCloser, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	ss, err := s.sessions.get(id)
	if err != nil {
		return nil, err
	}
	if n < 0 || n >= ss.count {
		return nil, types.Errf(types.ErrNotFound, "segment out of range")
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	ss.begin()
	ctx, cancel := s.deadline(ctx)
	done := func() { cancel(); ss.end(); release() }
	fail := func(err error) (io.ReadCloser, error) { done(); return nil, err }

	sp, err := s.planSegment(ctx, ss, n)
	if err != nil {
		return fail(err)
	}
	ss.mu.Lock()
	useHW := s.hwEnc != "" && !ss.hwFailed
	ss.mu.Unlock()
	r, err := s.startSegment(ctx, ss, sp, useHW)
	if err != nil && useHW && ctx.Err() == nil {
		// Hardware path failed before producing anything: remember and
		// retry once in software.
		slog.Warn("media: hardware encode failed, falling back to software", "session", id, "err", err)
		ss.mu.Lock()
		ss.hwFailed = true
		ss.mu.Unlock()
		r, err = s.startSegment(ctx, ss, sp, false)
	}
	if err != nil {
		return fail(err)
	}
	return &segmentStream{segmentReader: r, done: done}, nil
}

// CloseSession forgets a session (POST /media/session/<id>/close). Idempotent;
// a running producer finishes its current segment.
func (s *Service) CloseSession(id string) {
	if ValidSessionID(id) {
		s.sessions.remove(id)
	}
}
