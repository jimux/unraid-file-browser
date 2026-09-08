package transcode

import (
	"strings"
	"testing"
	"time"

	"unraid-filebrowser/internal/types"
)

func sampleProbe() Probe {
	return Probe{
		Container: "matroska,webm", DurationSec: 100,
		Video: &VideoStream{Index: 0, Codec: "h264", Width: 1920, Height: 1080, FPS: 24},
		Audio: []AudioStream{
			{Index: 1, Codec: "ac3", Channels: 6, Lang: "eng"},
			{Index: 2, Codec: "aac", Channels: 2, Lang: "fre", Default: true},
		},
	}
}

func TestMakePlan(t *testing.T) {
	web := []string{"h264", "vp9", "aac", "opus", "mp3"}
	cases := []struct {
		name      string
		probe     func() Probe
		can       []string
		audio     int
		maxH      int
		sparse    bool
		mode      string
		copyV     bool
		copyA     bool
		outH      int
		reasonHas string
		wantErr   string
	}{
		{name: "everything playable → remux (default audio track)", probe: sampleProbe, can: web, audio: -1,
			mode: ModeRemux, copyV: true, copyA: true, reasonHas: "Matroska"},
		{name: "ac3 track selected → audio-only transcode", probe: sampleProbe, can: web, audio: 1,
			mode: ModeTranscode, copyV: true, copyA: false, reasonHas: "AC-3 audio is not supported"},
		{name: "unknown audio index", probe: sampleProbe, can: web, audio: 7, wantErr: types.ErrBadRequest},
		{name: "empty can → full transcode", probe: sampleProbe, can: nil, audio: -1,
			mode: ModeTranscode, copyV: false, copyA: false, reasonHas: "H.264 video is not supported"},
		{name: "hevc video not in can", probe: func() Probe {
			p := sampleProbe()
			p.Video.Codec = "hevc"
			return p
		}, can: web, audio: -1, mode: ModeTranscode, copyV: false, copyA: true, reasonHas: "HEVC video is not supported"},
		{name: "vp9 playable but not TS-muxable", probe: func() Probe {
			p := sampleProbe()
			p.Video.Codec = "vp9"
			return p
		}, can: web, audio: -1, mode: ModeTranscode, copyV: false, copyA: true, reasonHas: "cannot be carried in MPEG-TS"},
		{name: "flac audio playable but not TS-muxable", probe: func() Probe {
			p := sampleProbe()
			p.Audio = []AudioStream{{Index: 1, Codec: "flac", Channels: 2}}
			return p
		}, can: append(web, "flac"), audio: -1, mode: ModeTranscode, copyV: true, copyA: false, reasonHas: "FLAC audio cannot be carried"},
		{name: "maxHeight below source forces transcode even when playable", probe: sampleProbe, can: web, audio: -1, maxH: 720,
			mode: ModeTranscode, copyV: false, copyA: true, outH: 720, reasonHas: "downscaling 1080p to 720p"},
		{name: "maxHeight at source height does not upscale or force", probe: sampleProbe, can: web, audio: -1, maxH: 1080,
			mode: ModeRemux, copyV: true, copyA: true},
		{name: "maxHeight above source ignored", probe: sampleProbe, can: web, audio: -1, maxH: 2160,
			mode: ModeRemux, copyV: true, copyA: true},
		{name: "sparse keyframes force video transcode", probe: sampleProbe, can: web, audio: -1, sparse: true,
			mode: ModeTranscode, copyV: false, copyA: true, reasonHas: "keyframes are too far apart"},
		{name: "aliases in can (h.264, mp4a)", probe: sampleProbe, can: []string{"H.264", "MP4A"}, audio: -1,
			mode: ModeRemux, copyV: true, copyA: true},
		{name: "audio only file", probe: func() Probe {
			return Probe{Container: "flac", DurationSec: 30, Audio: []AudioStream{{Index: 0, Codec: "flac", Channels: 2}}}
		}, can: web, audio: -1, mode: ModeTranscode, copyA: false, reasonHas: "FLAC audio is not supported"},
		{name: "video only file remux", probe: func() Probe {
			p := sampleProbe()
			p.Audio = nil
			return p
		}, can: web, audio: -1, mode: ModeRemux, copyV: true},
		{name: "audioIndex on file without audio", probe: func() Probe {
			p := sampleProbe()
			p.Audio = nil
			return p
		}, can: web, audio: 1, wantErr: types.ErrBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pl, err := makePlan(tc.probe(), tc.can, tc.audio, tc.maxH, tc.sparse)
			if tc.wantErr != "" {
				ae, ok := err.(*types.APIError)
				if !ok || ae.Code != tc.wantErr {
					t.Fatalf("want %s, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if pl.Mode != tc.mode {
				t.Errorf("mode = %s, want %s (reason %q)", pl.Mode, tc.mode, pl.Reason)
			}
			if pl.Video != nil && pl.CopyVideo != tc.copyV {
				t.Errorf("copyVideo = %v, want %v", pl.CopyVideo, tc.copyV)
			}
			if pl.Audio != nil && pl.CopyAudio != tc.copyA {
				t.Errorf("copyAudio = %v, want %v", pl.CopyAudio, tc.copyA)
			}
			if pl.OutHeight != tc.outH {
				t.Errorf("outHeight = %d, want %d", pl.OutHeight, tc.outH)
			}
			if tc.outH > 0 && pl.OutWidth != 1280 {
				t.Errorf("outWidth = %d, want 1280", pl.OutWidth)
			}
			if tc.reasonHas != "" && !strings.Contains(pl.Reason, tc.reasonHas) {
				t.Errorf("reason %q lacks %q", pl.Reason, tc.reasonHas)
			}
			if pl.Video != nil && !pl.CopyVideo && pl.MaxRateBPS == 0 {
				t.Error("transcoded video must carry a bitrate ceiling")
			}
			if pl.Video != nil && pl.CopyVideo && pl.MaxRateBPS != 0 {
				t.Error("copied video must not carry a bitrate ceiling")
			}
		})
	}
}

func TestBitrateLadder(t *testing.T) {
	for h, want := range map[int]int64{360: 1_500_000, 480: 1_500_000, 720: 3_000_000, 1080: 6_000_000, 1440: 12_000_000, 2160: 12_000_000} {
		if got := maxRateFor(h); got != want {
			t.Errorf("maxRateFor(%d) = %d, want %d", h, got, want)
		}
	}
	if w := evenWidth(1920, 1080, 720); w != 1280 {
		t.Errorf("evenWidth 1080p→720p = %d", w)
	}
	if w := evenWidth(1998, 1080, 480); w%2 != 0 {
		t.Errorf("width must be even, got %d", w)
	}
}

func TestPlaylist(t *testing.T) {
	const dur, seg = 10.0, 4.0
	n := segmentCount(dur, seg)
	if n != 3 {
		t.Fatalf("segmentCount = %d, want 3", n)
	}
	pl := playlist(dur, seg, n)
	lines := strings.Split(strings.TrimSpace(pl), "\n")
	if lines[0] != "#EXTM3U" || lines[len(lines)-1] != "#EXT-X-ENDLIST" {
		t.Fatalf("bad framing:\n%s", pl)
	}
	for _, want := range []string{"#EXT-X-PLAYLIST-TYPE:VOD", "#EXT-X-TARGETDURATION:4", "#EXT-X-MEDIA-SEQUENCE:0"} {
		if !strings.Contains(pl, want) {
			t.Errorf("missing %s", want)
		}
	}
	var sum float64
	var names []string
	for i, ln := range lines {
		if strings.HasPrefix(ln, "#EXTINF:") {
			var d float64
			if _, err := parseEXTINF(ln, &d); err != nil {
				t.Fatal(err)
			}
			sum += d
			names = append(names, lines[i+1])
		}
	}
	if !near(sum, dur, 0.001) {
		t.Errorf("EXTINF sum = %v, want %v", sum, dur)
	}
	if len(names) != 3 || names[0] != "seg-00000.ts" || names[2] != "seg-00002.ts" {
		t.Errorf("segment names = %v", names)
	}
	for _, nm := range names {
		if strings.ContainsAny(nm, "/?") {
			t.Errorf("segment URI must be a bare relative name: %q", nm)
		}
	}
	if segmentCount(8, 4) != 2 || segmentCount(8.4, 4) != 2 || segmentCount(8.6, 4) != 3 || segmentCount(0, 4) != 0 || segmentCount(0.2, 4) != 1 {
		t.Error("segmentCount edge cases")
	}
	// A folded tail lengthens the last segment and the target duration.
	pl2 := playlist(8.4, 4, segmentCount(8.4, 4))
	if !strings.Contains(pl2, "#EXT-X-TARGETDURATION:5") || !strings.Contains(pl2, "#EXTINF:4.400,") {
		t.Errorf("tail folding:\n%s", pl2)
	}
}

func parseEXTINF(ln string, d *float64) (int, error) {
	var err error
	s := strings.TrimSuffix(strings.TrimPrefix(ln, "#EXTINF:"), ",")
	*d, err = parseFloat(s)
	return 0, err
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmtSscan(s, &f)
	return f, err
}

func TestChooseHW(t *testing.T) {
	yes := func(string) bool { return true }
	no := func(string) bool { return false }
	if got := chooseHW([]string{"aac", "libx264"}, yes); got != "" {
		t.Errorf("no vaapi encoder but device: %q", got)
	}
	if got := chooseHW([]string{"aac", "h264_vaapi", "libx264"}, no); got != "" {
		t.Errorf("encoder but no device: %q", got)
	}
	if got := chooseHW([]string{"aac", "h264_vaapi", "libx264"}, yes); got != "h264_vaapi" {
		t.Errorf("both present: %q", got)
	}
	if got := chooseHW(nil, yes); got != "" {
		t.Errorf("nothing: %q", got)
	}
}

func TestParseIntrospection(t *testing.T) {
	enc := parseEncoders([]byte(`Encoders:
 V..... = Video
 A..... = Audio
 ------
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 V....D h264_vaapi           H.264/AVC (VAAPI) (codec h264)
 V....D libx265              libx265 H.265 / HEVC (codec hevc)
 A....D aac                  AAC (Advanced Audio Coding)
 A....D ac3                  ATSC A/52A (AC-3)
 V..... mpeg4                MPEG-4 part 2
`))
	if strings.Join(enc, ",") != "aac,h264_vaapi,libx264,libx265" {
		t.Errorf("encoders = %v", enc)
	}
	hw := parseHWAccels([]byte("Hardware acceleration methods:\nvdpau\ncuda\n"))
	if strings.Join(hw, ",") != "cuda,vdpau" {
		t.Errorf("hwaccels = %v", hw)
	}
	if parseHWAccels(nil) == nil || parseEncoders(nil) == nil {
		t.Error("empty lists must be non-nil for JSON")
	}
}

func TestParseFramecrc(t *testing.T) {
	k, err := parseFramecrc([]byte("#extradata 0: 46, 0x371f0ef3\n#software: Lavf\n#tb 0: 1/1000\n#media_type 0: video\n0,       4920,       5000,       40,      739, 0x40036de8\n"))
	if err != nil || !k.ok || !near(k.pts, 5.0, 1e-9) || !near(k.dts, 4.92, 1e-9) {
		t.Errorf("got %+v, %v", k, err)
	}
	k, err = parseFramecrc([]byte("#tb 0: 1/12800\n0, 101376, 102400, 512, 739, 0x8b9f6eb6\n"))
	if err != nil || !near(k.pts, 8.0, 1e-9) || !near(k.dts, 7.92, 1e-9) {
		t.Errorf("mp4 tb: %+v, %v", k, err)
	}
	k, err = parseFramecrc([]byte("#tb 0: 1/25\n"))
	if err != nil || k.ok {
		t.Errorf("no packet should be !ok: %+v %v", k, err)
	}
}

func TestParseProbe(t *testing.T) {
	raw := []byte(`{"streams":[
	 {"index":0,"codec_type":"video","codec_name":"mjpeg","width":600,"height":600,"disposition":{"attached_pic":1}},
	 {"index":1,"codec_type":"video","codec_name":"hevc","profile":"Main 10","width":3840,"height":2160,"avg_frame_rate":"24000/1001","r_frame_rate":"24000/1001","bit_rate":"","tags":{"DURATION":"01:30:00.500000000"}},
	 {"index":2,"codec_type":"audio","codec_name":"eac3","channels":6,"tags":{"language":"eng","title":"Surround"},"disposition":{"default":1}},
	 {"index":3,"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"}}],
	 "format":{"format_name":"matroska,webm","start_time":"0.000000","bit_rate":"25000000"}}`)
	p, err := parseProbe(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Video == nil || p.Video.Index != 1 || p.Video.Codec != "hevc" || p.Video.Width != 3840 {
		t.Errorf("video = %+v (cover art must be skipped)", p.Video)
	}
	if !near(p.Video.FPS, 23.976, 0.001) {
		t.Errorf("fps = %v", p.Video.FPS)
	}
	if !near(p.DurationSec, 5400.5, 1e-6) {
		t.Errorf("duration from DURATION tag = %v", p.DurationSec)
	}
	if len(p.Audio) != 1 || !p.Audio[0].Default || p.Audio[0].Title != "Surround" || p.Audio[0].Channels != 6 {
		t.Errorf("audio = %+v", p.Audio)
	}
	if len(p.Subtitles) != 1 || p.Subtitles[0].Codec != "subrip" {
		t.Errorf("subs = %+v", p.Subtitles)
	}
	if p.Bitrate != 25_000_000 {
		t.Errorf("bitrate = %d", p.Bitrate)
	}
	if _, err := parseProbe([]byte(`{"streams":[],"format":{}}`)); err == nil {
		t.Error("no streams must be an error")
	}
}

func TestStoreEvictionAndSweep(t *testing.T) {
	st := newStore(2, time.Minute)
	now := time.Now()
	a := &session{id: strings.Repeat("a", 32), lastUsed: now.Add(-3 * time.Minute)}
	b := &session{id: strings.Repeat("b", 32), lastUsed: now.Add(-2 * time.Minute)}
	c := &session{id: strings.Repeat("c", 32), lastUsed: now}
	for _, ss := range []*session{a, b} {
		if err := st.put(ss); err != nil {
			t.Fatal(err)
		}
	}
	b.begin() // busy: must not be evicted
	if err := st.put(c); err != nil {
		t.Fatal(err)
	}
	if _, err := st.get(a.id); err == nil {
		t.Error("oldest idle session should have been evicted")
	}
	if _, err := st.get(b.id); err != nil {
		t.Error("busy session must survive eviction")
	}
	// Both remaining busy → refuse.
	c.begin()
	d := &session{id: strings.Repeat("d", 32), lastUsed: now}
	if err := st.put(d); err == nil {
		t.Error("all-busy store must refuse a new session")
	} else if ae := err.(*types.APIError); ae.Code != types.ErrTimeout {
		t.Errorf("code = %s", ae.Code)
	}
	b.end()
	c.end()
	b.mu.Lock()
	b.lastUsed = now.Add(-2 * time.Minute)
	b.mu.Unlock()
	if n := st.sweep(now); n != 1 || st.len() != 1 {
		t.Errorf("sweep removed %d, len %d", n, st.len())
	}
	if _, err := st.get("not-hex"); err == nil {
		t.Error("malformed id must be rejected")
	}
	if !ValidSessionID(strings.Repeat("0", 32)) || ValidSessionID(strings.Repeat("0", 31)) || ValidSessionID("../../etc") {
		t.Error("ValidSessionID")
	}
}

func TestSegmentArgsShape(t *testing.T) {
	ss := &session{
		probe: Probe{startSec: 0},
		plan: plan{Video: &VideoStream{Index: 0, Codec: "h264", Width: 1920, Height: 1080}, CopyVideo: true,
			Audio: &AudioStream{Index: 1, Codec: "ac3"}, CopyAudio: false},
		segSec: 10, count: 5,
	}
	s := &Service{ffmpeg: "ffmpeg"}
	args := s.segmentArgs(ss, segmentPlan{startSec: 10, endRaw: 19.96, copyVideo: true}, false)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-nostdin", "-protocol_whitelist file,fd,pipe", "-noaccurate_seek -ss 10.130435 -i /dev/fd/3",
		"-to 19.960000", "-copyts -output_ts_offset 1.000000", "-map 0:0 -c:v copy", "-map 0:1 -c:a aac -b:a 192k -ac 2", "-f mpegts pipe:1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q:\n%s", want, joined)
		}
	}
	// Outputs: exactly one, stdout. Nothing may name a file path.
	if args[len(args)-1] != "pipe:1" || strings.Count(joined, "pipe:1") != 1 {
		t.Errorf("segment output must be stdout only: %s", joined)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "/") && a != childFDPath {
			t.Errorf("argv carries a filesystem path %q", a)
		}
	}

	// Transcoded, downscaled video with the hardware recipe.
	ss.plan.CopyVideo = false
	ss.plan.OutWidth, ss.plan.OutHeight, ss.plan.MaxRateBPS = 1280, 720, 3_000_000
	sw := strings.Join(s.segmentArgs(ss, segmentPlan{startSec: 0, endRaw: 4}, false), " ")
	for _, want := range []string{"-c:v libx264 -preset veryfast -crf 23 -maxrate 3000000 -bufsize 6000000", "-vf scale=1280:720", "-pix_fmt yuv420p"} {
		if !strings.Contains(sw, want) {
			t.Errorf("software argv lacks %q:\n%s", want, sw)
		}
	}
	if strings.Contains(sw, "noaccurate_seek") {
		t.Error("transcoded video must seek accurately")
	}
	hw := strings.Join(s.segmentArgs(ss, segmentPlan{startSec: 0, endRaw: 4}, true), " ")
	for _, want := range []string{"-hwaccel vaapi -hwaccel_output_format vaapi -vaapi_device /dev/dri/renderD128", "-c:v h264_vaapi", "scale_vaapi=w=1280:h=720"} {
		if !strings.Contains(hw, want) {
			t.Errorf("hardware argv lacks %q:\n%s", want, hw)
		}
	}
	// Last segment reads to the end.
	last := strings.Join(s.segmentArgs(ss, segmentPlan{startSec: 40, endRaw: -1}, false), " ")
	if strings.Contains(last, "-to ") {
		t.Error("last segment must not carry -to")
	}
}
