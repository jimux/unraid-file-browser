package transcode

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"sync"

	"unraid-filebrowser/internal/types"
)

// tsOffsetSec is added to every segment's timestamps. Segment 0 of most files
// starts with a negative dts (B-frame reorder, AAC encoder priming); ffmpeg
// would silently shift that one segment to non-negative and no other, leaving
// a small discontinuity at the first boundary. A constant offset on all
// segments keeps every dts positive and the timeline uniform; hls.js aligns
// the media timeline to the playlist from the first segment it parses, so
// the absolute value is invisible to the player.
const tsOffsetSec = 1.0

// segmentPlan is the per-segment decision: where to start, where to stop,
// and whether video is copied for this particular segment.
type segmentPlan struct {
	startSec  float64 // -ss, relative to the file start (playlist time)
	endRaw    float64 // -to in the source's raw timeline; < 0 = read to end
	copyVideo bool
}

// planSegment works out segment n of a session.
//
// Transcoded video (or no video) is trivial: exact nominal boundaries, the
// decoder starts at startSec and frames with pts ≥ the next boundary are
// dropped. Copied video can only start on a keyframe, so segment n runs from
// the keyframe ffmpeg's seek lands on for boundary n to the keyframe boundary
// n+1 lands on, cut in decode order (dts) so the two are exactly contiguous.
// When two consecutive boundaries land on the same keyframe (a GOP longer
// than the segment — the sampling at session creation makes this rare) the
// later segment cannot start on a fresh keyframe and is re-encoded from its
// nominal start instead, so no segment is ever empty and coverage stays
// gapless.
func (s *Service) planSegment(ctx context.Context, ss *session, n int) (segmentPlan, error) {
	sp := segmentPlan{startSec: float64(n) * ss.segSec, endRaw: -1}
	last := n == ss.count-1
	nominalEnd := func(m int) float64 { return float64(m)*ss.segSec + ss.probe.startSec }

	if ss.plan.Video == nil || !ss.plan.CopyVideo {
		if !last {
			sp.endRaw = nominalEnd(n + 1)
		}
		return sp, nil
	}

	// kfCut reports whether boundary m starts on a fresh keyframe.
	kfCut := func(m int) (bool, kfPoint, error) {
		if m == 0 {
			return true, kfPoint{}, nil
		}
		k, err := s.keyframeAt(ctx, ss, m)
		if err != nil {
			return false, kfPoint{}, err
		}
		prev, err := s.keyframeAt(ctx, ss, m-1)
		if err != nil {
			return false, kfPoint{}, err
		}
		return k.ok && prev.ok && k.pts > prev.pts, k, nil
	}

	startOnKF, _, err := kfCut(n)
	if err != nil {
		return segmentPlan{}, err
	}
	sp.copyVideo = startOnKF
	if last {
		return sp, nil
	}
	nextOnKF, next, err := kfCut(n + 1)
	if err != nil {
		return segmentPlan{}, err
	}
	switch {
	case nextOnKF && sp.copyVideo:
		sp.endRaw = next.dts // decode-order cut: next segment begins with exactly this keyframe
	case nextOnKF:
		sp.endRaw = next.pts // transcoded frames are cut on pts; next segment copies from the keyframe
	case sp.copyVideo:
		// Next segment is re-encoded from its nominal start; stop this copy
		// where frames displayed from that instant begin, in decode order.
		sp.endRaw = nominalEnd(n+1) - (next.pts - next.dts)
	default:
		sp.endRaw = nominalEnd(n + 1)
	}
	return sp, nil
}

// segmentArgs builds the fixed argv for one segment. useHW selects the
// hardware encoder for transcoded video.
func (s *Service) segmentArgs(ss *session, sp segmentPlan, useHW bool) []string {
	pl := ss.plan
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", protocolWhitelist,
	}
	transcodeVideo := pl.Video != nil && !sp.copyVideo
	if transcodeVideo && useHW {
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi", "-vaapi_device", renderNode)
	}
	if pl.Video != nil && sp.copyVideo {
		// The seek lands on a keyframe before startSec; accurate seeking would
		// trim re-encoded audio to startSec and open a hole in front of it.
		args = append(args, "-noaccurate_seek")
	}
	seek := sp.startSec
	if pl.Video != nil && sp.copyVideo {
		seek += seekSlackSec // see keyframe.go
	}
	args = append(args, "-ss", ftoa(seek), "-i", childFDPath)
	if sp.endRaw >= 0 {
		args = append(args, "-to", ftoa(sp.endRaw))
	}
	args = append(args, "-copyts", "-output_ts_offset", ftoa(tsOffsetSec), "-muxdelay", "0", "-muxpreload", "0")
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1", "-sn", "-dn")

	if pl.Video != nil {
		args = append(args, "-map", "0:"+strconv.Itoa(pl.Video.Index))
		switch {
		case sp.copyVideo:
			args = append(args, "-c:v", "copy")
		case useHW:
			args = append(args, hwVideoArgs(pl)...)
		default:
			args = append(args, swVideoArgs(pl)...)
		}
	}
	if pl.Audio != nil {
		args = append(args, "-map", "0:"+strconv.Itoa(pl.Audio.Index))
		if pl.CopyAudio {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
		}
	}
	return append(args, "-f", "mpegts", "pipe:1")
}

// swVideoArgs is the libx264 recipe: fast preset, CRF quality with a bitrate
// ceiling from the ladder, browser-safe profile/level and pixel format. The
// first frame of every encode is an IDR, which is the only keyframe a
// segment needs; -sc_threshold 0 keeps x264 from adding more on scene cuts.
func swVideoArgs(pl plan) []string {
	args := []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-maxrate", strconv.FormatInt(pl.MaxRateBPS, 10), "-bufsize", strconv.FormatInt(2*pl.MaxRateBPS, 10),
		"-profile:v", "high", "-level", "4.1", "-pix_fmt", "yuv420p", "-sc_threshold", "0"}
	if pl.OutHeight > 0 {
		args = append(args, "-vf", "scale="+strconv.Itoa(pl.OutWidth)+":"+strconv.Itoa(pl.OutHeight))
	}
	return args
}

// hwVideoArgs is the VAAPI recipe (only reachable when chooseHW said yes).
func hwVideoArgs(pl plan) []string {
	args := []string{"-c:v", "h264_vaapi", "-profile:v", "high",
		"-rc_mode", "VBR", "-b:v", strconv.FormatInt(pl.MaxRateBPS*3/4, 10),
		"-maxrate", strconv.FormatInt(pl.MaxRateBPS, 10), "-bufsize", strconv.FormatInt(2*pl.MaxRateBPS, 10)}
	if pl.OutHeight > 0 {
		args = append(args, "-vf", "scale_vaapi=w="+strconv.Itoa(pl.OutWidth)+":h="+strconv.Itoa(pl.OutHeight))
	}
	return args
}

// peekSize is how much of a segment is read before the caller is told the
// process is producing output. The MPEG-TS header tables alone are a few
// hundred bytes, so a healthy ffmpeg clears this immediately.
const peekSize = 64 << 10

// startSegment launches ffmpeg for one segment and waits until it has either
// produced its first bytes or exited. A process that dies before producing
// anything is reported as an error (so the HTTP layer can still send an
// envelope); after that, failures can only abort the stream.
func (s *Service) startSegment(ctx context.Context, ss *session, sp segmentPlan, useHW bool) (*segmentReader, error) {
	in, _, err := openInput(ctx, s.opener, ss.path)
	if err != nil {
		return nil, err
	}
	cmd := command(ctx, s.ffmpeg, in, s.segmentArgs(ss, sp, useHW)...)
	errBuf := &boundedBuf{}
	cmd.Stderr = errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, types.Errf(types.ErrInternal, "ffmpeg: "+err.Error())
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		return nil, types.Errf(types.ErrInternal, "ffmpeg: "+err.Error())
	}
	in.Close() // the child holds its own descriptor now
	s.running.Add(1)
	r := &segmentReader{ctx: ctx, cmd: cmd, stdout: stdout, stderr: errBuf, svc: s}

	buf := make([]byte, peekSize)
	n, rerr := io.ReadAtLeast(stdout, buf, 1)
	if n == 0 {
		werr := r.wait()
		if werr == nil && rerr != nil && !errors.Is(rerr, io.EOF) {
			werr = rerr
		}
		if werr == nil {
			werr = errors.New("produced no output")
		}
		return nil, mediaErr(ctx, "ffmpeg", errBuf, werr)
	}
	r.peek = buf[:n]
	return r, nil
}

// segmentReader streams one segment from a running ffmpeg. Close always kills
// and reaps the process, so a client that hangs up mid-segment leaves no
// zombie behind.
type segmentReader struct {
	ctx    context.Context
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *boundedBuf
	svc    *Service
	peek   []byte

	mu      sync.Mutex
	waited  bool
	waitErr error
	closed  bool
}

func (r *segmentReader) wait() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.waited {
		r.waitErr = r.cmd.Wait()
		r.waited = true
		r.svc.running.Add(-1)
	}
	return r.waitErr
}

func (r *segmentReader) Read(p []byte) (int, error) {
	if len(r.peek) > 0 {
		n := copy(p, r.peek)
		r.peek = r.peek[n:]
		return n, nil
	}
	n, err := r.stdout.Read(p)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		if werr := r.wait(); werr != nil {
			return n, mediaErr(r.ctx, "ffmpeg", r.stderr, werr)
		}
		return n, io.EOF
	}
	if r.ctx.Err() != nil {
		return n, types.Errf(types.ErrTimeout, "segment production timed out")
	}
	return n, err
}

// Close kills the process if it is still running and reaps it.
func (r *segmentReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	if r.cmd.Process != nil {
		r.cmd.Process.Kill() //nolint:errcheck — already exited is fine
	}
	r.wait() //nolint:errcheck — exit status irrelevant on close
	return nil
}

// segmentStream is what OpenSegment hands out: the reader plus the
// bookkeeping (semaphore slot, session activity, timeout) released on Close.
type segmentStream struct {
	*segmentReader
	done func()
	once sync.Once
}

func (st *segmentStream) Close() error {
	err := st.segmentReader.Close()
	st.once.Do(st.done)
	return err
}

// deadline binds a segment's total wall time.
func (s *Service) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.opts.SegmentTimeout)
}
