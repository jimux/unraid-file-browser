package meta

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/dhowden/tag"
)

// audioTagBudget bounds the bytes a tag parser may pull from one file. ID3v2
// tags with embedded cover art run to a few MB; anything beyond this is not
// a tag, it is a file trying to be read whole.
const audioTagBudget = 16 << 20

// audioExtractor reads music tags with dhowden/tag (ID3v1/v2, FLAC and OGG
// Vorbis comments, MP4 atoms, DSF) and, when ffprobe is available, the
// stream facts tags cannot carry (codec, bitrate, sample rate, channels,
// duration). Formats dhowden/tag does not parse (wav, aiff, wma, ape, aac,
// mka) still get tags through ffprobe's format tags.
type audioExtractor struct{ ff *ffprobe }

func (*audioExtractor) Kind() Kind           { return KindAudio }
func (*audioExtractor) Extensions() []string { return audioExts }

func (a *audioExtractor) Extract(ctx context.Context, path string, r io.ReaderAt, size int64) ([]Field, error) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	var out []Field
	tagged := false

	br := newBudgetReader(r, size, audioTagBudget)
	m, err := tag.ReadFrom(br)
	if err == nil && m != nil {
		tagged = true
		out = append(out,
			text("audio.artist", m.Artist()),
			text("audio.albumArtist", m.AlbumArtist()),
			text("audio.album", m.Album()),
			text("audio.title", m.Title()),
			text("audio.genre", m.Genre()),
		)
		if y := m.Year(); y > 0 && y < 3000 {
			out = append(out, numInt("audio.year", int64(y)))
		}
		if t, _ := m.Track(); t > 0 && t < 10000 {
			out = append(out, numInt("audio.track", int64(t)))
		}
	}

	if a.ff != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pj, perr := a.ff.run(ctx, path, r)
		if perr == nil {
			out = append(out, audioStreamFields(pj, tagged)...)
		} else if !tagged {
			return nil, perr
		}
	} else if !tagged {
		// No tag parser matched and no ffprobe: the only remaining facts
		// are container-implied.
		if c := codecFromExt(ext); c != "" {
			return []Field{text("audio.codec", c)}, nil
		}
		if errors.Is(err, tag.ErrNoTagsFound) {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 && !tagged {
		if c := codecFromExt(ext); c != "" {
			out = append(out, text("audio.codec", c))
		}
	}
	return dedupe(out), nil
}

// audioStreamFields converts a probe of an audio file: the first audio
// stream's facts plus, when no tag library matched, the format-level tags.
func audioStreamFields(pj *probeJSON, tagged bool) []Field {
	var out []Field
	for i := range pj.Streams {
		st := &pj.Streams[i]
		if st.CodecType != "audio" {
			continue
		}
		if st.CodecName != "" {
			out = append(out, text("audio.codec", st.CodecName))
		}
		if sr := atoi(st.SampleRate); sr > 0 && sr <= 1_000_000 {
			out = append(out, numInt("audio.sampleRate", sr))
		}
		if st.Channels > 0 && st.Channels <= 64 {
			out = append(out, numInt("audio.channels", int64(st.Channels)))
		}
		break
	}
	if b := atoi(pj.Format.BitRate); b > 0 {
		out = append(out, numInt("audio.bitrate", b))
	}
	if d := pj.duration(); d > 0 {
		out = append(out, num("audio.durationSec", d))
	}
	if !tagged {
		t := pj.Format.Tags
		out = append(out,
			text("audio.artist", tagValue(t, "artist")),
			text("audio.albumArtist", tagValue(t, "album_artist", "albumartist")),
			text("audio.album", tagValue(t, "album")),
			text("audio.title", tagValue(t, "title")),
			text("audio.genre", tagValue(t, "genre")),
		)
		if y := yearOf(tagValue(t, "date", "year")); y > 0 {
			out = append(out, numInt("audio.year", y))
		}
		if n := leadingInt(tagValue(t, "track")); n > 0 && n < 10000 {
			out = append(out, numInt("audio.track", n))
		}
	}
	return out
}

// yearOf extracts a four-digit year from "2019", "2019-05-01" or "2019/05".
func yearOf(s string) int64 {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return 0
	}
	y := leadingInt(s[:4])
	if y < 1000 || y > 2999 || len(s) > 4 && s[4] >= '0' && s[4] <= '9' {
		return 0
	}
	return y
}

// leadingInt parses the leading decimal digits of s ("3/12" → 3).
func leadingInt(s string) int64 {
	var n int64
	seen := false
	for i := 0; i < len(s) && i < 9; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
		seen = true
	}
	if !seen {
		return 0
	}
	return n
}

// codecFromExt names the codec a container implies when nothing better is
// known. Ambiguous containers (m4a: AAC or ALAC; ogg: Vorbis or Opus; mka)
// return "".
func codecFromExt(ext string) string {
	switch ext {
	case "mp3":
		return "mp3"
	case "flac":
		return "flac"
	case "opus":
		return "opus"
	case "aac":
		return "aac"
	case "ape":
		return "ape"
	case "dsf":
		return "dsd"
	}
	return ""
}
