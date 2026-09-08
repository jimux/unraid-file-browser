package meta

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func extractFile(t *testing.T, ex Extractor, path string) fieldMap {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	fs, err := Run(context.Background(), ex, path, f, st.Size())
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return toMap(fs)
}

func videoEx(t *testing.T) *videoExtractor {
	fixture(t, fxSDR)
	return &videoExtractor{ff: newFFprobe(ffprobeBin)}
}

func TestVideoHDR10(t *testing.T) {
	m := extractFile(t, videoEx(t), fixture(t, fxHDR10))
	checks := map[string]string{
		"video.codec": "hevc", "video.hdr": "hdr10", "video.bitDepth": "10",
		"video.width": "64", "video.height": "64", "video.container": "matroska",
		"video.audioCodec": "eac3", "video.audioLang": "eng", "video.fps": "25",
	}
	for k, v := range checks {
		if got := m.value(k); got != v {
			t.Errorf("%s = %q, want %q (all: %s)", k, got, v, keysOf(m))
		}
	}
	if d := m.num("video.durationSec"); d < 0.3 || d > 0.6 {
		t.Errorf("durationSec = %v", d)
	}
}

func TestVideoHLG(t *testing.T) {
	m := extractFile(t, videoEx(t), fixture(t, fxHLG))
	if m.value("video.hdr") != "hlg" || m.value("video.container") != "mp4" || m.value("video.codec") != "hevc" {
		t.Errorf("hlg: %s", keysOf(m))
	}
	if len(m["video.audioCodec"]) != 0 {
		t.Errorf("audio-less file reported audio: %s", keysOf(m))
	}
}

func TestVideoAC3AndSDR(t *testing.T) {
	m := extractFile(t, videoEx(t), fixture(t, fxAC3))
	if m.value("video.audioCodec") != "ac3" || m.value("video.audioChannels") != "6" || m.value("video.audioLang") != "fre" {
		t.Errorf("ac3: %s", keysOf(m))
	}
	if m.value("video.hdr") != "none" || m.value("video.bitDepth") != "8" || m.value("video.codec") != "h264" {
		t.Errorf("sdr flags: %s", keysOf(m))
	}
	m = extractFile(t, videoEx(t), fixture(t, fxSDR))
	if m.value("video.container") != "mp4" || m.value("video.audioCodec") != "aac" || m.num("video.bitrate") <= 0 {
		t.Errorf("sdr mp4: %s", keysOf(m))
	}
}

func TestVideoMultiTrack(t *testing.T) {
	m := extractFile(t, videoEx(t), fixture(t, fxMulti))
	if !m.has("video.audioCodec", "aac") || !m.has("video.audioCodec", "ac3") {
		t.Errorf("audio codecs: %v", m.values("video.audioCodec"))
	}
	if !m.has("video.audioChannels", "2") || !m.has("video.audioChannels", "6") {
		t.Errorf("audio channels: %v", m.values("video.audioChannels"))
	}
	if !m.has("video.audioLang", "eng") || !m.has("video.audioLang", "deu") {
		t.Errorf("audio langs: %v", m.values("video.audioLang"))
	}
	if !m.has("video.subtitleLang", "eng") || !m.has("video.subtitleLang", "spa") {
		t.Errorf("subtitle langs: %v", m.values("video.subtitleLang"))
	}
	if len(m["video.codec"]) != 1 {
		t.Errorf("video.codec should be single-valued: %v", m.values("video.codec"))
	}
}

// Dolby Vision cannot be synthesised with ffmpeg alone; the derivation is
// tested on hand-written ffprobe output (the shape ffprobe 6+ emits).
func TestVideoFieldsFromJSON(t *testing.T) {
	const dovi = `{"format":{"format_name":"matroska,webm","duration":"7200.5","bit_rate":"45000000"},
	"streams":[
	 {"index":0,"codec_type":"video","codec_name":"hevc","width":3840,"height":2160,"pix_fmt":"yuv420p10le",
	  "color_transfer":"smpte2084","r_frame_rate":"24000/1001","avg_frame_rate":"24000/1001",
	  "side_data_list":[{"side_data_type":"DOVI configuration record","dv_profile":8}]},
	 {"index":1,"codec_type":"audio","codec_name":"truehd","channels":8,"tags":{"language":"eng"}},
	 {"index":2,"codec_type":"audio","codec_name":"ac3","channels":6,"tags":{"language":"eng"}},
	 {"index":3,"codec_type":"audio","codec_name":"dts","channels":6,"tags":{"language":"jpn"}},
	 {"index":4,"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"}},
	 {"index":5,"codec_type":"video","codec_name":"mjpeg","disposition":{"attached_pic":1}}
	]}`
	var pj probeJSON
	if err := json.Unmarshal([]byte(dovi), &pj); err != nil {
		t.Fatal(err)
	}
	m := toMap(normalize(videoFields(&pj, "mkv")))
	want := map[string]string{
		"video.hdr": "dolbyvision", "video.codec": "hevc", "video.bitDepth": "10", "video.fps": "23.976",
		"video.width": "3840", "video.durationSec": "7200.5", "video.bitrate": "45000000", "video.container": "matroska",
	}
	for k, v := range want {
		if m.value(k) != v {
			t.Errorf("%s = %q, want %q", k, m.value(k), v)
		}
	}
	if got := strings.Join(m.values("video.audioCodec"), ","); got != "truehd,ac3,dts" {
		t.Errorf("audio codecs %q", got)
	}
	if got := m.values("video.audioLang"); len(got) != 2 { // eng deduped, jpn
		t.Errorf("audio langs %v", got)
	}
	if got := m.values("video.audioChannels"); len(got) != 2 { // 8, 6 (6 deduped)
		t.Errorf("audio channels %v", got)
	}

	// pix_fmt fallback, HLG, and a stream list with no video at all.
	const hlg = `{"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2"},"streams":[{"codec_type":"video","codec_name":"hevc","pix_fmt":"yuv420p10le","color_transfer":"arib-std-b67"}]}`
	pj = probeJSON{}
	json.Unmarshal([]byte(hlg), &pj)
	m = toMap(videoFields(&pj, "mov"))
	if m.value("video.hdr") != "hlg" || m.value("video.bitDepth") != "10" || m.value("video.container") != "mov" {
		t.Errorf("hlg json: %s", keysOf(m))
	}
	pj = probeJSON{}
	json.Unmarshal([]byte(`{"format":{},"streams":[]}`), &pj)
	if fs := videoFields(&pj, "mkv"); len(fs) != 0 {
		t.Errorf("empty probe produced %v", fs)
	}
}

// --- audio -------------------------------------------------------------------

func audioEx(t *testing.T) *audioExtractor {
	fixture(t, fxMP3)
	return &audioExtractor{ff: newFFprobe(ffprobeBin)}
}

func TestAudioMP3(t *testing.T) {
	m := extractFile(t, audioEx(t), fixture(t, fxMP3))
	want := map[string]string{
		"audio.artist": "The Crawlers", "audio.album": "Index Sessions", "audio.title": "Header Only",
		"audio.year": "2021", "audio.genre": "Electronic", "audio.track": "7", "audio.albumArtist": "Various Crawlers",
		"audio.codec": "mp3", "audio.sampleRate": "48000", "audio.channels": "1",
	}
	for k, v := range want {
		if got := m.value(k); got != v {
			t.Errorf("%s = %q, want %q (all: %s)", k, got, v, keysOf(m))
		}
	}
	if m.num("audio.bitrate") < 100_000 || m.num("audio.durationSec") < 0.4 {
		t.Errorf("stream facts: %s", keysOf(m))
	}
}

func TestAudioFormats(t *testing.T) {
	cases := []struct {
		fx     string
		artist string
		codec  string
		year   string
	}{
		{fxFLAC, "Lossless Larry", "flac", "1999"},
		{fxM4A, "Apple Alice", "aac", "2010"},
		{fxOGG, "Ogg Olga", "vorbis", "2005"},
		{fxOpus, "Opus Otto", "opus", ""},
		{fxWAV, "Wave Wanda", "pcm_s16le", ""},
	}
	for _, c := range cases {
		t.Run(c.fx, func(t *testing.T) {
			m := extractFile(t, audioEx(t), fixture(t, c.fx))
			if m.value("audio.artist") != c.artist || m.value("audio.codec") != c.codec || m.value("audio.year") != c.year {
				t.Errorf("%s: %s", c.fx, keysOf(m))
			}
			if m.num("audio.sampleRate") != 48000 || m.num("audio.durationSec") <= 0 {
				t.Errorf("%s stream facts: %s", c.fx, keysOf(m))
			}
		})
	}
}

func TestAudioWithoutFFprobe(t *testing.T) {
	ex := &audioExtractor{}
	m := extractFile(t, ex, fixture(t, fxFLAC))
	if m.value("audio.artist") != "Lossless Larry" || m.value("audio.track") != "2" {
		t.Errorf("tags without ffprobe: %s", keysOf(m))
	}
	if len(m["audio.durationSec"]) != 0 || len(m["audio.sampleRate"]) != 0 {
		t.Errorf("stream facts should be absent without ffprobe: %s", keysOf(m))
	}
	// Untagged bare MPEG frames: no tags, no error, codec implied by extension.
	m = extractFile(t, ex, fixture(t, fxNoTags))
	if m.value("audio.codec") != "mp3" || len(m) != 1 {
		t.Errorf("untagged mp3 without ffprobe: %s", keysOf(m))
	}
}

func TestAudioUntaggedWithFFprobe(t *testing.T) {
	m := extractFile(t, audioEx(t), fixture(t, fxNoTags))
	if m.value("audio.codec") != "mp3" || m.num("audio.durationSec") <= 0 || len(m["audio.artist"]) != 0 {
		t.Errorf("untagged mp3: %s", keysOf(m))
	}
}

func TestVideoExtractorOnAudioFileFails(t *testing.T) {
	// Feeding a bogus file to ffprobe must yield an error, not a panic or
	// an empty success.
	dir := t.TempDir()
	p := dir + "/junk.mkv"
	os.WriteFile(p, []byte("this is not a matroska file at all, sorry"), 0o644)
	f, _ := os.Open(p)
	defer f.Close()
	_, err := Run(context.Background(), videoEx(t), p, f, 41)
	if err == nil {
		t.Fatal("expected an error for junk input")
	}
}
