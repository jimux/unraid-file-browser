package transcode

import (
	"fmt"
	"strings"

	"unraid-filebrowser/internal/types"
)

// Modes of a session (API.md).
const (
	ModeRemux     = "remux"
	ModeTranscode = "transcode"
)

// plan is what the server decided to do with the selected streams. It is
// pure data so the decision is unit-testable without ffmpeg.
type plan struct {
	Mode   string
	Reason string

	Video      *VideoStream // nil for audio-only
	CopyVideo  bool
	OutHeight  int   // scaled output height when transcoding (0 = source)
	OutWidth   int   // matching width, even, aspect-preserving
	MaxRateBPS int64 // bitrate ceiling when transcoding video; 0 when copying

	Audio     *AudioStream // nil when the file has no audio
	CopyAudio bool
}

// tsVideoCodecs / tsAudioCodecs are the codecs an MPEG-TS segment can carry
// without re-encoding. A browser may decode VP9 or FLAC natively, but they
// have no TS mapping, so they must be transcoded regardless of `can`.
var tsVideoCodecs = map[string]bool{"h264": true, "hevc": true, "mpeg2video": true, "mpeg4": true}
var tsAudioCodecs = map[string]bool{"aac": true, "mp3": true, "mp2": true, "ac3": true, "eac3": true, "opus": true, "dts": true}

// codecAliases maps the spellings a browser capability list might use onto
// ffprobe codec names.
var codecAliases = map[string]string{
	"h265": "hevc", "h.265": "hevc", "hvc1": "hevc", "hev1": "hevc",
	"avc": "h264", "avc1": "h264", "h.264": "h264",
	"mp4a": "aac", "mpga": "mp3", "mpeg-4": "mpeg4", "dca": "dts", "e-ac-3": "eac3", "ac-3": "ac3",
	"mpeg2": "mpeg2video", "mpeg-2": "mpeg2video",
}

// codecNames are human names for reasons.
var codecNames = map[string]string{
	"h264": "H.264", "hevc": "HEVC", "vp8": "VP8", "vp9": "VP9", "av1": "AV1",
	"mpeg4": "MPEG-4 Part 2", "msmpeg4v3": "DivX 3", "mpeg2video": "MPEG-2", "mpeg1video": "MPEG-1",
	"vc1": "VC-1", "wmv3": "WMV9", "wmv2": "WMV8", "theora": "Theora", "mjpeg": "Motion JPEG",
	"aac": "AAC", "mp3": "MP3", "mp2": "MP2", "ac3": "AC-3", "eac3": "E-AC-3", "dts": "DTS",
	"truehd": "TrueHD", "flac": "FLAC", "opus": "Opus", "vorbis": "Vorbis", "alac": "ALAC",
	"wmav2": "WMA", "wmapro": "WMA Pro",
}

func codecName(c string) string {
	if n, ok := codecNames[c]; ok {
		return n
	}
	if strings.HasPrefix(c, "pcm_") {
		return "PCM"
	}
	return strings.ToUpper(c)
}

// canSet normalises the browser's capability list.
func canSet(can []string) map[string]bool {
	set := make(map[string]bool, len(can))
	for _, c := range can {
		c = strings.ToLower(strings.TrimSpace(c))
		if a, ok := codecAliases[c]; ok {
			c = a
		}
		if c != "" {
			set[c] = true
		}
	}
	return set
}

// bitrate ladder for transcoded video: ceiling by output height. The point
// of a client-requested downscale is a predictably streamable bitrate, so
// CRF alone is not enough. (API.md "Transcoding".)
func maxRateFor(height int) int64 {
	switch {
	case height <= 480:
		return 1_500_000
	case height <= 720:
		return 3_000_000
	case height <= 1080:
		return 6_000_000
	default:
		return 12_000_000
	}
}

// makePlan picks the cheapest treatment for the selected streams.
//
//   - Video and audio are copied when the browser decodes the codec AND
//     MPEG-TS can carry it; otherwise that stream (only) is re-encoded.
//   - A maxHeight below the source height forces a video transcode with
//     scaling even if the codec is playable: the user asked for less.
//   - maxHeight at or above the source never upscales.
//   - An empty `can` is treated as "nothing known": everything is re-encoded
//     to the universally playable H.264/AAC.
//
// sparseKeyframes is set by the caller when the file's keyframes are too far
// apart for the segment length (see keyframe.go); copied video would then
// yield empty segments, so video is transcoded instead.
func makePlan(p Probe, can []string, audioIndex, maxHeight int, sparseKeyframes bool) (plan, error) {
	set := canSet(can)
	var pl plan
	var reasons []string

	if p.Video != nil {
		v := *p.Video
		pl.Video = &v
		switch {
		case maxHeight > 0 && maxHeight < v.Height:
			pl.OutHeight = maxHeight &^ 1
			pl.OutWidth = evenWidth(v.Width, v.Height, pl.OutHeight)
			reasons = append(reasons, fmt.Sprintf("downscaling %dp to %dp at the client's request", v.Height, pl.OutHeight))
		case !set[v.Codec]:
			reasons = append(reasons, codecName(v.Codec)+" video is not supported by this browser")
		case !tsVideoCodecs[v.Codec]:
			reasons = append(reasons, codecName(v.Codec)+" video cannot be carried in MPEG-TS segments")
		case sparseKeyframes:
			reasons = append(reasons, "keyframes are too far apart to cut the video into segments without re-encoding")
		default:
			pl.CopyVideo = true
		}
		if !pl.CopyVideo {
			h := pl.OutHeight
			if h == 0 {
				h = v.Height
			}
			pl.MaxRateBPS = maxRateFor(h)
		}
	}

	if len(p.Audio) > 0 {
		a, err := pickAudio(p.Audio, audioIndex)
		if err != nil {
			return plan{}, err
		}
		pl.Audio = &a
		switch {
		case !set[a.Codec]:
			reasons = append(reasons, codecName(a.Codec)+" audio is not supported by this browser")
		case !tsAudioCodecs[a.Codec]:
			reasons = append(reasons, codecName(a.Codec)+" audio cannot be carried in MPEG-TS segments")
		default:
			pl.CopyAudio = true
		}
	} else if audioIndex >= 0 {
		return plan{}, types.Errf(types.ErrBadRequest, "file has no audio stream")
	}

	if (pl.Video == nil || pl.CopyVideo) && (pl.Audio == nil || pl.CopyAudio) {
		pl.Mode = ModeRemux
		pl.Reason = fmt.Sprintf("%s container is not playable directly; streams are copied without re-encoding", containerName(p.Container))
		return pl, nil
	}
	pl.Mode = ModeTranscode
	if pl.Video != nil && pl.CopyVideo {
		reasons = append(reasons, "video is copied without re-encoding")
	}
	if pl.Audio != nil && pl.CopyAudio && pl.Video != nil && !pl.CopyVideo {
		reasons = append(reasons, "audio is copied without re-encoding")
	}
	pl.Reason = strings.Join(reasons, "; ")
	return pl, nil
}

// pickAudio selects the requested stream index, else the default-flagged
// track, else the first.
func pickAudio(tracks []AudioStream, index int) (AudioStream, error) {
	if index >= 0 {
		for _, t := range tracks {
			if t.Index == index {
				return t, nil
			}
		}
		return AudioStream{}, types.Errf(types.ErrBadRequest, fmt.Sprintf("audioIndex %d is not an audio stream of this file", index))
	}
	for _, t := range tracks {
		if t.Default {
			return t, nil
		}
	}
	return tracks[0], nil
}

// evenWidth scales w:h to the new height keeping aspect, rounded to an even
// number (yuv420p needs even dimensions).
func evenWidth(w, h, newH int) int {
	if w <= 0 || h <= 0 || newH <= 0 {
		return 0
	}
	nw := int(float64(w)*float64(newH)/float64(h) + 0.5)
	return (nw + 1) &^ 1
}

var containerNames = map[string]string{
	"matroska,webm": "Matroska", "avi": "AVI", "mov,mp4,m4a,3gp,3g2,mj2": "MP4", "mpegts": "MPEG-TS",
	"asf": "WMV/ASF", "flv": "FLV", "mpeg": "MPEG-PS", "ogg": "Ogg", "wtv": "WTV", "rm": "RealMedia",
}

func containerName(f string) string {
	if n, ok := containerNames[f]; ok {
		return n
	}
	if i := strings.IndexByte(f, ','); i > 0 {
		f = f[:i]
	}
	return strings.ToUpper(f)
}
