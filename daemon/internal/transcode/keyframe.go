package transcode

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"unraid-filebrowser/internal/types"
)

// seekSlackSec counters a heuristic inside ffmpeg: when the video has
// B-frames and the demuxer does not seek by pts (Matroska, AVI — unlike MP4),
// `-ss T` actually seeks to T − 3/23 s, so a boundary that sits exactly on a
// keyframe lands on the previous one. Adding the same amount back puts copy
// cuts on the keyframe at or just before the boundary. It is applied to
// every copy-mode seek and to the lookups that predict them, so the two
// always agree; transcoded segments seek accurately and do not need it.
const seekSlackSec = 3.0 / 23.0

// kfPoint is where ffmpeg's `-ss` lands for a given time: the last keyframe
// at or before it, in the source's raw timeline.
type kfPoint struct {
	pts float64 // seconds
	dts float64 // seconds; the decode-order cut point
	ok  bool    // false when the lookup found no video packet
}

const keyframeLookupTimeout = 15 * time.Second

// lookupKeyframe asks ffmpeg itself where `-ss atSec` lands, so the answer is
// exactly the keyframe a copy segment starting there will begin with (the
// same seek code, same demuxer). The framecrc muxer prints one line per
// packet with dts and pts in the stream timebase; `-frames:v 1` stops after
// the first. This is a ~50 ms process: a seek plus one packet read.
//
// Each lookup opens the input afresh: the child inherits a descriptor whose
// offset it moves, and on some platforms /dev/fd/N shares that offset.
func (s *Service) lookupKeyframe(ctx context.Context, path string, videoIndex int, atSec float64) (kfPoint, error) {
	in, _, err := openInput(ctx, s.opener, path)
	if err != nil {
		return kfPoint{}, err
	}
	defer in.Close()
	ctx, cancel := context.WithTimeout(ctx, keyframeLookupTimeout)
	defer cancel()
	cmd := command(ctx, s.ffmpeg, in,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", protocolWhitelist,
		"-ss", ftoa(atSec+seekSlackSec),
		"-i", childFDPath,
		"-copyts",
		"-map", "0:"+strconv.Itoa(videoIndex),
		"-c", "copy",
		"-frames:v", "1",
		"-f", "framecrc", "pipe:1")
	var errBuf boundedBuf
	cmd.Stderr = &errBuf
	s.running.Add(1)
	defer s.running.Add(-1)
	out, err := cmd.Output()
	if err != nil {
		return kfPoint{}, mediaErr(ctx, "keyframe lookup", &errBuf, err)
	}
	return parseFramecrc(out)
}

// parseFramecrc reads framecrc output: "#tb 0: 1/1000" gives the timebase,
// "0, <dts>, <pts>, <dur>, <size>, <crc>" the packet.
func parseFramecrc(out []byte) (kfPoint, error) {
	var tbNum, tbDen float64 = 0, 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(ln, "#tb 0:"):
			frac := strings.TrimSpace(strings.TrimPrefix(ln, "#tb 0:"))
			n, d, ok := strings.Cut(frac, "/")
			if !ok {
				return kfPoint{}, types.Errf(types.ErrEncoding, "keyframe lookup: bad timebase "+frac)
			}
			tbNum, _ = strconv.ParseFloat(n, 64)
			tbDen, _ = strconv.ParseFloat(d, 64)
		case strings.HasPrefix(ln, "#"), ln == "":
		default:
			fields := strings.Split(ln, ",")
			if len(fields) < 3 || tbDen == 0 {
				return kfPoint{}, types.Errf(types.ErrEncoding, "keyframe lookup: unparsable packet line")
			}
			dts, err1 := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
			pts, err2 := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
			if err1 != nil || err2 != nil {
				return kfPoint{}, types.Errf(types.ErrEncoding, "keyframe lookup: unparsable timestamps")
			}
			tb := tbNum / tbDen
			return kfPoint{pts: pts * tb, dts: dts * tb, ok: true}, nil
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return kfPoint{}, types.Errf(types.ErrEncoding, "keyframe lookup: "+err.Error())
	}
	return kfPoint{}, nil // no packet: seek landed past the last keyframe
}

// keyframeAt returns the (cached) keyframe for boundary m of a session,
// opening the input afresh for the lookup process.
func (s *Service) keyframeAt(ctx context.Context, ss *session, m int) (kfPoint, error) {
	ss.mu.Lock()
	if k, ok := ss.kf[m]; ok {
		ss.mu.Unlock()
		return k, nil
	}
	ss.mu.Unlock()

	k, err := s.lookupKeyframe(ctx, ss.path, ss.plan.Video.Index, float64(m)*ss.segSec)
	if err != nil {
		return kfPoint{}, err
	}
	ss.mu.Lock()
	if ss.kf == nil {
		ss.kf = make(map[int]kfPoint)
	}
	ss.kf[m] = k
	ss.mu.Unlock()
	return k, nil
}

// sparseKeyframes samples the first few segment boundaries of a copy-capable
// video and reports whether two consecutive boundaries land on the same
// keyframe — i.e. a GOP longer than the segment. Copying such a file would
// alternate copied and re-encoded segments (quality pumping every few
// seconds), so the caller transcodes it consistently instead. Sampling is
// bounded to keep session creation quick; a long GOP later in the file is
// handled per segment (see segmentPlan).
func (s *Service) sparseKeyframes(ctx context.Context, path string, videoIndex int, segSec float64, count int) (bool, error) {
	const samples = 5
	var prev kfPoint
	for m := 0; m <= samples && m < count; m++ {
		k, err := s.lookupKeyframe(ctx, path, videoIndex, float64(m)*segSec)
		if err != nil {
			return false, err
		}
		if !k.ok {
			return true, nil // cannot even find a keyframe: do not attempt copy cuts
		}
		if m > 0 && k.pts <= prev.pts {
			return true, nil
		}
		prev = k
	}
	return false, nil
}

// ftoa formats seconds for ffmpeg arguments (no exponent, fixed precision).
func ftoa(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("%.6f", sec)
}
