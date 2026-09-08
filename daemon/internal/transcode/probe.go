package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"unraid-filebrowser/internal/types"
)

// Probe limits.
const (
	probeTimeout = 20 * time.Second
	probeMaxJSON = 4 << 20 // ffprobe output larger than this is rejected
)

// Probe is the payload of GET /media/probe and the basis of every session.
type Probe struct {
	Container   string        `json:"container"`   // ffprobe format_name, e.g. "matroska,webm"
	DurationSec float64       `json:"durationSec"` // 0 when unknown
	Bitrate     int64         `json:"bitrate"`     // bits/s, 0 when unknown
	Video       *VideoStream  `json:"video"`       // null for audio-only files
	Audio       []AudioStream `json:"audio"`
	Subtitles   []SubStream   `json:"subtitles"`
	// startSec is the container's first timestamp; segment cut points are
	// expressed to ffmpeg in this raw timeline. Not part of the contract.
	startSec float64
}

// VideoStream describes the video stream a session will use (the first real
// video stream; attached cover art is skipped).
type VideoStream struct {
	Index   int     `json:"index"`
	Codec   string  `json:"codec"`
	Profile string  `json:"profile"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	FPS     float64 `json:"fps"`
	Bitrate int64   `json:"bitrate"`
}

// AudioStream is one selectable audio track.
type AudioStream struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Channels int    `json:"channels"`
	Lang     string `json:"lang"`
	Title    string `json:"title"`
	Default  bool   `json:"default"`
}

// SubStream is one subtitle track (listed for the UI; not yet served).
type SubStream struct {
	Index int    `json:"index"`
	Codec string `json:"codec"`
	Lang  string `json:"lang"`
	Title string `json:"title"`
}

// ffprobe's JSON, only the fields we read. Numbers arrive as strings.
type probeJSON struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		StartTime  string `json:"start_time"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index        int    `json:"index"`
		CodecType    string `json:"codec_type"`
		CodecName    string `json:"codec_name"`
		Profile      string `json:"profile"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		RFrameRate   string `json:"r_frame_rate"`
		AvgFrameRate string `json:"avg_frame_rate"`
		BitRate      string `json:"bit_rate"`
		Channels     int    `json:"channels"`
		Duration     string `json:"duration"`
		Tags         struct {
			Language string `json:"language"`
			Title    string `json:"title"`
			Duration string `json:"DURATION"`
		} `json:"tags"`
		Disposition struct {
			Default     int `json:"default"`
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
}

// probe runs ffprobe over the inherited descriptor and parses the result.
func (s *Service) probe(ctx context.Context, in *os.File) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := command(ctx, s.ffprobe, in,
		"-v", "error",
		"-protocol_whitelist", protocolWhitelist,
		"-print_format", "json",
		"-show_format", "-show_streams",
		"--", childFDPath)
	var errBuf boundedBuf
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Probe{}, types.Errf(types.ErrInternal, "ffprobe: "+err.Error())
	}
	if err := cmd.Start(); err != nil {
		return Probe{}, types.Errf(types.ErrInternal, "ffprobe: "+err.Error())
	}
	// One byte past the cap tells us it was exceeded without buffering more.
	raw, rerr := io.ReadAll(io.LimitReader(stdout, probeMaxJSON+1))
	if len(raw) > probeMaxJSON {
		cmd.Process.Kill() //nolint:errcheck — already exited is fine
	}
	io.Copy(io.Discard, stdout) //nolint:errcheck — drain so Wait never blocks
	werr := cmd.Wait()
	switch {
	case len(raw) > probeMaxJSON:
		return Probe{}, types.Errf(types.ErrTooLarge, "ffprobe output exceeds 4 MB")
	case rerr != nil:
		return Probe{}, mediaErr(ctx, "ffprobe", &errBuf, rerr)
	case werr != nil:
		return Probe{}, mediaErr(ctx, "ffprobe", &errBuf, werr)
	}
	return parseProbe(raw)
}

// parseProbe converts ffprobe JSON into the contract shape.
func parseProbe(raw []byte) (Probe, error) {
	var pj probeJSON
	if err := json.Unmarshal(raw, &pj); err != nil {
		return Probe{}, types.Errf(types.ErrEncoding, "ffprobe output unparsable: "+err.Error())
	}
	p := Probe{
		Container:   pj.Format.FormatName,
		DurationSec: atof(pj.Format.Duration),
		Bitrate:     atoi(pj.Format.BitRate),
		startSec:    atof(pj.Format.StartTime),
		Audio:       []AudioStream{},
		Subtitles:   []SubStream{},
	}
	for _, st := range pj.Streams {
		switch st.CodecType {
		case "video":
			if st.Disposition.AttachedPic == 1 || p.Video != nil {
				continue
			}
			p.Video = &VideoStream{
				Index:   st.Index,
				Codec:   st.CodecName,
				Profile: st.Profile,
				Width:   st.Width,
				Height:  st.Height,
				FPS:     fps(st.AvgFrameRate, st.RFrameRate),
				Bitrate: atoi(st.BitRate),
			}
		case "audio":
			p.Audio = append(p.Audio, AudioStream{
				Index:    st.Index,
				Codec:    st.CodecName,
				Channels: st.Channels,
				Lang:     st.Tags.Language,
				Title:    st.Tags.Title,
				Default:  st.Disposition.Default == 1,
			})
		case "subtitle":
			p.Subtitles = append(p.Subtitles, SubStream{
				Index: st.Index,
				Codec: st.CodecName,
				Lang:  st.Tags.Language,
				Title: st.Tags.Title,
			})
		}
		// Matroska often carries duration only per stream (as a tag).
		if p.DurationSec == 0 {
			if d := atof(st.Duration); d > 0 {
				p.DurationSec = d
			} else if d := parseClock(st.Tags.Duration); d > 0 {
				p.DurationSec = d
			}
		}
	}
	if p.Video == nil && len(p.Audio) == 0 {
		return Probe{}, types.Errf(types.ErrEncoding, "no video or audio stream found")
	}
	return p, nil
}

func atof(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

func atoi(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// fps evaluates ffprobe's "num/den" rate strings, preferring the average.
func fps(avg, r string) float64 {
	for _, s := range []string{avg, r} {
		num, den, ok := strings.Cut(s, "/")
		if !ok {
			continue
		}
		n, err1 := strconv.ParseFloat(num, 64)
		d, err2 := strconv.ParseFloat(den, 64)
		if err1 != nil || err2 != nil || d == 0 || n <= 0 {
			continue
		}
		return math.Round(n/d*1000) / 1000
	}
	return 0
}

// parseClock reads "HH:MM:SS.fraction" (Matroska DURATION tags).
func parseClock(s string) float64 {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return 0
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0
	}
	return float64(h)*3600 + float64(m)*60 + sec
}

var errNoVideo = errors.New("no video stream")
