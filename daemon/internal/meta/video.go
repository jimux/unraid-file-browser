package meta

import (
	"context"
	"io"
	"path/filepath"
	"strings"
)

// videoExtractor describes a video file through ffprobe: the first real
// video stream (cover art skipped), every audio track (multi-valued rows)
// and every subtitle language.
type videoExtractor struct{ ff *ffprobe }

func (*videoExtractor) Kind() Kind           { return KindVideo }
func (*videoExtractor) Extensions() []string { return videoExts }

func (v *videoExtractor) Extract(ctx context.Context, path string, r io.ReaderAt, size int64) ([]Field, error) {
	pj, err := v.ff.run(ctx, path, r)
	if err != nil {
		return nil, err
	}
	return videoFields(pj, strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))), nil
}

// videoFields converts a probe into rows. Exported through tests only; kept
// separate from the subprocess so the derivations (HDR, bit depth, container)
// can be unit-tested against hand-written ffprobe JSON.
func videoFields(pj *probeJSON, ext string) []Field {
	var out []Field
	haveVideo := false
	for i := range pj.Streams {
		st := &pj.Streams[i]
		switch st.CodecType {
		case "video":
			if haveVideo || st.Disposition.AttachedPic == 1 || st.CodecName == "" {
				continue
			}
			// Still-image codecs inside a video container are cover art
			// even when not flagged.
			switch st.CodecName {
			case "mjpeg", "png", "bmp", "gif", "tiff", "webp":
				if len(pj.Streams) > 1 {
					continue
				}
			}
			haveVideo = true
			out = append(out, text("video.codec", st.CodecName))
			if st.Width > 0 && st.Height > 0 && st.Width <= 65536 && st.Height <= 65536 {
				out = append(out, numInt("video.width", int64(st.Width)), numInt("video.height", int64(st.Height)))
			}
			out = append(out, text("video.hdr", st.hdr()))
			if bd := st.bitDepth(); bd > 0 {
				out = append(out, numInt("video.bitDepth", bd))
			}
			if f := fps(st.AvgFrameRate, st.RFrameRate); f > 0 {
				out = append(out, num("video.fps", f))
			}
		case "audio":
			if st.CodecName != "" {
				out = append(out, text("video.audioCodec", st.CodecName))
			}
			if st.Channels > 0 && st.Channels <= 64 {
				out = append(out, numInt("video.audioChannels", int64(st.Channels)))
			}
			if l := lang(st.Tags); l != "" {
				out = append(out, text("video.audioLang", l))
			}
		case "subtitle":
			if l := lang(st.Tags); l != "" {
				out = append(out, text("video.subtitleLang", l))
			}
		}
	}
	if !haveVideo && len(out) == 0 {
		return nil
	}
	if d := pj.duration(); d > 0 {
		out = append(out, num("video.durationSec", d))
	}
	if b := atoi(pj.Format.BitRate); b > 0 {
		out = append(out, numInt("video.bitrate", b))
	}
	if c := pj.container(ext); c != "" {
		out = append(out, text("video.container", c))
	}
	return dedupe(out)
}

// lang returns the stream's language tag, lowercased, "und" dropped.
func lang(tags map[string]string) string {
	l := strings.ToLower(strings.TrimSpace(tagValue(tags, "language")))
	if l == "" || l == "und" {
		return ""
	}
	return l
}

// dedupe drops exact (key,value) repeats: three English audio tracks are
// one "video.audioLang=eng" row, not three.
func dedupe(in []Field) []Field {
	type kv struct{ k, v string }
	seen := make(map[kv]bool, len(in))
	out := in[:0]
	for _, f := range in {
		id := kv{f.Key, f.Value}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, f)
	}
	return out
}
