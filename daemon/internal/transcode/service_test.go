package transcode

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"unraid-filebrowser/internal/fsops"
	"unraid-filebrowser/internal/types"
)

func TestCapabilitiesReal(t *testing.T) {
	s := newTestService(t, Options{})
	c := s.Capabilities()
	if !c.Available || c.FFmpeg == "" || c.FFprobe == "" {
		t.Fatalf("caps = %+v", c)
	}
	if !contains(c.Encoders, "libx264") || !contains(c.Encoders, "aac") {
		t.Errorf("encoders = %v", c.Encoders)
	}
	if c.HWEncoder != "" {
		t.Errorf("tests run software only, got hw %q", c.HWEncoder)
	}
}

func TestUnavailableWithoutFFmpeg(t *testing.T) {
	s := New(fsops.New([]string{os.TempDir()}), Options{FFmpegPath: "/nonexistent/ffmpeg", FFprobePath: "/nonexistent/ffprobe"})
	defer s.Close()
	c := s.Capabilities()
	if c.Available || c.Reason == "" {
		t.Fatalf("caps = %+v", c)
	}
	if c.HWAccels == nil || c.Encoders == nil {
		t.Error("lists must be non-nil for JSON")
	}
	_, err := s.Probe(context.Background(), "/x")
	if ae, ok := err.(*types.APIError); !ok || ae.Code != types.ErrUnavailable {
		t.Errorf("Probe err = %v", err)
	}
	_, err = s.CreateSession(context.Background(), SessionRequest{Path: "/x"})
	if ae, ok := err.(*types.APIError); !ok || ae.Code != types.ErrUnavailable {
		t.Errorf("CreateSession err = %v", err)
	}
	_, err = s.Playlist(strings.Repeat("0", 32))
	if ae, ok := err.(*types.APIError); !ok || ae.Code != types.ErrUnavailable {
		t.Errorf("Playlist err = %v", err)
	}
}

func TestProbeFixtures(t *testing.T) {
	s := newTestService(t, Options{})
	ctx := context.Background()
	cases := []struct {
		file, container, vcodec, acodec string
		channels                        int
	}{
		{fxMP4, "mov,mp4,m4a,3gp,3g2,mj2", "h264", "aac", 1},
		{fxMKV, "matroska,webm", "h264", "ac3", 1},
		{fxAVI, "avi", "mpeg4", "mp3", 1},
		{fxFLAC, "flac", "", "flac", 1},
	}
	for _, tc := range cases {
		p, err := s.Probe(ctx, fx(tc.file))
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if p.Container != tc.container {
			t.Errorf("%s container = %q", tc.file, p.Container)
		}
		if !near(p.DurationSec, 10, 0.2) {
			t.Errorf("%s duration = %v", tc.file, p.DurationSec)
		}
		if tc.vcodec == "" {
			if p.Video != nil {
				t.Errorf("%s: expected no video", tc.file)
			}
		} else if p.Video == nil || p.Video.Codec != tc.vcodec || p.Video.Width != 320 || p.Video.Height != 240 || !near(p.Video.FPS, 25, 0.01) {
			t.Errorf("%s video = %+v", tc.file, p.Video)
		}
		if len(p.Audio) != 1 || p.Audio[0].Codec != tc.acodec || p.Audio[0].Channels != tc.channels {
			t.Errorf("%s audio = %+v", tc.file, p.Audio)
		}
		if p.Subtitles == nil {
			t.Errorf("%s: subtitles must be [] not null", tc.file)
		}
	}
	if _, err := s.Probe(ctx, fx("missing.mkv")); err == nil {
		t.Error("missing file must error")
	}
	if err := os.WriteFile(fx("garbage.bin"), []byte("this is not media at all, not even close\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Probe(ctx, fx("garbage.bin"))
	if ae, ok := err.(*types.APIError); !ok || ae.Code != types.ErrEncoding {
		t.Errorf("garbage probe err = %v", err)
	}
}

// checkSegments fetches every segment of a session, verifies each with
// ffprobe and checks the whole set tiles the timeline contiguously.
func checkSegments(t *testing.T, s *Service, info SessionInfo, vcodec, acodec string, exactStarts bool) []tsInfo {
	t.Helper()
	var infos []tsInfo
	for n := 0; n < info.SegmentCount; n++ {
		seg := fetchSegment(t, s, info.ID, n)
		ti := probeTS(t, seg)
		if ti.format != "mpegts" {
			t.Fatalf("seg %d format = %q", n, ti.format)
		}
		if ti.videoCodec != vcodec || ti.audioCodec != acodec {
			t.Errorf("seg %d codecs = %s/%s, want %s/%s", n, ti.videoCodec, ti.audioCodec, vcodec, acodec)
		}
		nominal := float64(n)*info.SegmentSec + tsOffsetSec
		if vcodec != "" {
			if len(ti.keyframes) == 0 || !near(ti.keyframes[0], ti.firstVPTS, 0.2) {
				t.Errorf("seg %d must open on a keyframe: kf=%v first=%v", n, ti.keyframes, ti.firstVPTS)
			}
			if exactStarts && !near(ti.firstVPTS, nominal, 0.021) {
				t.Errorf("seg %d video starts at %.3f, want ≈ %.3f", n, ti.firstVPTS, nominal)
			}
			if !exactStarts && (ti.firstVPTS > nominal+seekSlackSec+0.021 || ti.firstVPTS < nominal-info.SegmentSec) {
				t.Errorf("seg %d video starts at %.3f, want keyframe ≤ %.3f within one segment", n, ti.firstVPTS, nominal)
			}
		}
		if acodec != "" && !near(ti.firstAPTS, nominal, 0.15) && exactStarts {
			t.Errorf("seg %d audio starts at %.3f, want ≈ %.3f", n, ti.firstAPTS, nominal)
		}
		infos = append(infos, ti)
	}
	// Contiguity: the next segment's first video dts is one frame after the
	// previous segment's last dts (25 fps → 0.04 s), audio joins within a
	// frame or so.
	for n := 1; n < len(infos); n++ {
		prev, cur := infos[n-1], infos[n]
		if vcodec != "" && !near(cur.firstVDTS-prev.lastVDTS, 0.04, 0.045) {
			t.Errorf("video gap between seg %d and %d: %.3f → %.3f", n-1, n, prev.lastVDTS, cur.firstVDTS)
		}
		if acodec != "" && (cur.firstAPTS-prev.lastAPTS > 0.1 || cur.firstAPTS-prev.lastAPTS < -0.1) {
			t.Errorf("audio join between seg %d and %d: %.3f → %.3f", n-1, n, prev.lastAPTS, cur.firstAPTS)
		}
	}
	return infos
}

func TestRemuxMP4(t *testing.T) {
	s := newTestService(t, Options{})
	info, err := s.CreateSession(context.Background(), SessionRequest{Path: fx(fxMP4), Can: []string{"h264", "aac"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != ModeRemux || info.SegmentSec != 4 || info.SegmentCount != 3 || !info.Video.Copied || !info.Audio.Copied {
		t.Fatalf("session = %+v video=%+v audio=%+v", info, info.Video, info.Audio)
	}
	if info.Playlist != "/api/v1/media/hls/"+info.ID+"/index.m3u8" || !ValidSessionID(info.ID) {
		t.Errorf("playlist path/id: %s", info.Playlist)
	}
	pl, err := s.Playlist(info.ID)
	if err != nil || !strings.Contains(pl, "seg-00002.ts") || !strings.HasSuffix(pl, "#EXT-X-ENDLIST\n") {
		t.Fatalf("playlist: %v\n%s", err, pl)
	}
	// Keyframes every second, so boundaries at 0/4/8 s are hit exactly.
	infos := checkSegments(t, s, info, "h264", "aac", true)
	if !near(infos[2].lastVPTS, 10+tsOffsetSec-0.04, 0.05) {
		t.Errorf("last segment should reach the end: last pts %.3f", infos[2].lastVPTS)
	}
	if s.running.Load() != 0 {
		t.Errorf("processes still running: %d", s.running.Load())
	}
}

func TestAudioOnlyTranscodeMKV(t *testing.T) {
	s := newTestService(t, Options{})
	info, err := s.CreateSession(context.Background(), SessionRequest{Path: fx(fxMKV), Can: []string{"h264", "aac", "mp3"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != ModeTranscode || !info.Video.Copied || info.Audio.Copied || info.Audio.Codec != "aac" || info.Audio.Channels != 2 {
		t.Fatalf("session = %+v video=%+v audio=%+v", info, info.Video, info.Audio)
	}
	if !strings.Contains(info.Reason, "AC-3 audio is not supported") {
		t.Errorf("reason = %q", info.Reason)
	}
	if info.SegmentSec != 4 {
		t.Errorf("copied video should use the remux segment length, got %v", info.SegmentSec)
	}
	checkSegments(t, s, info, "h264", "aac", true)
}

func TestFullTranscodeAVIAndMidFileSeek(t *testing.T) {
	s := newTestService(t, Options{})
	info, err := s.CreateSession(context.Background(), SessionRequest{Path: fx(fxAVI), Can: []string{"h264", "aac"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != ModeTranscode || info.Video.Copied || info.Audio.Copied || info.SegmentSec != 2 || info.SegmentCount != segmentCount(info.DurationSec, 2) {
		t.Fatalf("session = %+v", info)
	}
	if info.Video.Codec != "h264" || info.Video.Bitrate != 1_500_000 {
		t.Errorf("video = %+v", info.Video)
	}
	// Seek first: a mid-file segment must be producible without the earlier
	// ones ever having been requested.
	ti := probeTS(t, fetchSegment(t, s, info.ID, 3))
	if ti.videoCodec != "h264" || ti.audioCodec != "aac" || !near(ti.firstVPTS, 6+tsOffsetSec, 0.021) {
		t.Fatalf("mid-file seg = %+v", ti)
	}
	checkSegments(t, s, info, "h264", "aac", true)
}

func TestDownscaleForcesTranscode(t *testing.T) {
	s := newTestService(t, Options{})
	info, err := s.CreateSession(context.Background(), SessionRequest{Path: fx(fxMP4), Can: []string{"h264", "aac"}, MaxHeight: 120})
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != ModeTranscode || info.Video.Copied || info.Video.Height != 120 || info.Video.Width != 160 || !info.Audio.Copied {
		t.Fatalf("session = %+v video=%+v", info, info.Video)
	}
	if !strings.Contains(info.Reason, "downscaling 240p to 120p") {
		t.Errorf("reason = %q", info.Reason)
	}
	ti := probeTS(t, fetchSegment(t, s, info.ID, 1))
	if ti.videoCodec != "h264" || ti.audioCodec != "aac" || !near(ti.firstVPTS, 2+tsOffsetSec, 0.021) {
		t.Errorf("seg = %+v", ti)
	}
	// At or above the source height: no forcing.
	info, err = s.CreateSession(context.Background(), SessionRequest{Path: fx(fxMP4), Can: []string{"h264", "aac"}, MaxHeight: 240})
	if err != nil || info.Mode != ModeRemux {
		t.Errorf("maxHeight == source: %+v %v", info, err)
	}
}

func TestAudioOnlyFile(t *testing.T) {
	s := newTestService(t, Options{})
	info, err := s.CreateSession(context.Background(), SessionRequest{Path: fx(fxFLAC), Can: []string{"h264", "aac"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Video != nil || info.Mode != ModeTranscode || info.Audio.Codec != "aac" {
		t.Fatalf("session = %+v", info)
	}
	checkSegments(t, s, info, "", "aac", true)
}

func TestSparseKeyframesFallBackToTranscode(t *testing.T) {
	s := newTestService(t, Options{})
	info, err := s.CreateSession(context.Background(), SessionRequest{Path: fx(fxSparse), Can: []string{"h264", "aac"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != ModeTranscode || info.Video.Copied || info.SegmentSec != 2 {
		t.Fatalf("5 s GOP with 4 s segments must transcode video: %+v video=%+v", info, info.Video)
	}
	if !strings.Contains(info.Reason, "keyframes are too far apart") {
		t.Errorf("reason = %q", info.Reason)
	}
	// With segments longer than the GOP it is copied.
	s2 := newTestService(t, Options{RemuxSegmentSec: 5})
	info, err = s2.CreateSession(context.Background(), SessionRequest{Path: fx(fxSparse), Can: []string{"h264", "aac"}})
	if err != nil || info.Mode != ModeRemux {
		t.Fatalf("5 s GOP with 5 s segments should remux: %+v %v", info, err)
	}
	checkSegments(t, s2, info, "h264", "aac", true)
}

// TestHybridCopyPlan drives the per-segment fallback directly: a copied
// video whose 5 s GOP is longer than the 4 s segment. Boundaries 0 and 4 s
// land on the same keyframe (0), 8 s lands on 5 s.
func TestHybridCopyPlan(t *testing.T) {
	s := newTestService(t, Options{})
	p, err := s.Probe(context.Background(), fx(fxSparse))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := makePlan(p, []string{"h264", "aac"}, -1, 0, false)
	if err != nil || !pl.CopyVideo {
		t.Fatal(err)
	}
	ss := &session{id: strings.Repeat("e", 32), path: fx(fxSparse), probe: p, plan: pl, segSec: 4, count: 3, lastUsed: time.Now()}
	if err := s.sessions.put(ss); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sp0, err := s.planSegment(ctx, ss, 0)
	if err != nil {
		t.Fatal(err)
	}
	sp1, err := s.planSegment(ctx, ss, 1)
	if err != nil {
		t.Fatal(err)
	}
	sp2, err := s.planSegment(ctx, ss, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !sp0.copyVideo || sp1.copyVideo || !sp2.copyVideo {
		t.Fatalf("modes: %+v %+v %+v", sp0, sp1, sp2)
	}
	// seg0 copies from 0 and stops just before frames displayed from 4 s;
	// seg1 re-encodes exactly [4, 5); seg2 copies from the 5 s keyframe to the end.
	if !near(sp0.endRaw, 4-0.08, 0.05) || !near(sp1.startSec, 4, 1e-9) || !near(sp1.endRaw, 5, 1e-6) || sp2.endRaw >= 0 || !near(sp2.startSec, 8, 1e-9) {
		t.Fatalf("plans: %+v %+v %+v", sp0, sp1, sp2)
	}
	var infos []tsInfo
	for n := 0; n < 3; n++ {
		ti := probeTS(t, fetchSegment(t, s, ss.id, n))
		if ti.videoCodec != "h264" || ti.audioCodec != "aac" || len(ti.keyframes) == 0 {
			t.Fatalf("seg %d = %+v", n, ti)
		}
		infos = append(infos, ti)
	}
	if !near(infos[0].firstVPTS, 0+tsOffsetSec, 0.05) || !near(infos[1].firstVPTS, 4+tsOffsetSec, 0.021) || !near(infos[2].firstVPTS, 5+tsOffsetSec, 0.021) {
		t.Errorf("starts: %.3f %.3f %.3f", infos[0].firstVPTS, infos[1].firstVPTS, infos[2].firstVPTS)
	}
	// Coverage without holes: each segment's last displayed frame is within a
	// frame or two of the next one's first.
	for n := 1; n < 3; n++ {
		gap := infos[n].firstVPTS - infos[n-1].lastVPTS
		if gap > 0.1 || gap < -0.1 {
			t.Errorf("boundary %d: last %.3f → first %.3f", n, infos[n-1].lastVPTS, infos[n].firstVPTS)
		}
	}
}

func TestErrorsAndClose(t *testing.T) {
	s := newTestService(t, Options{})
	ctx := context.Background()
	code := func(err error) string {
		var ae *types.APIError
		if errors.As(err, &ae) {
			return ae.Code
		}
		return "<" + err.Error() + ">"
	}
	if _, err := s.OpenSegment(ctx, "nope", 0); code(err) != types.ErrBadRequest {
		t.Errorf("malformed id: %v", err)
	}
	if _, err := s.OpenSegment(ctx, strings.Repeat("0", 32), 0); code(err) != types.ErrNotFound {
		t.Errorf("unknown id: %v", err)
	}
	if _, err := s.Playlist(strings.Repeat("0", 32)); code(err) != types.ErrNotFound {
		t.Errorf("unknown playlist: %v", err)
	}
	info, err := s.CreateSession(ctx, SessionRequest{Path: fx(fxMP4), Can: []string{"h264", "aac"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenSegment(ctx, info.ID, info.SegmentCount); code(err) != types.ErrNotFound {
		t.Errorf("out of range: %v", err)
	}
	if _, err := s.OpenSegment(ctx, info.ID, -1); code(err) != types.ErrNotFound {
		t.Errorf("negative: %v", err)
	}
	if _, err := s.CreateSession(ctx, SessionRequest{Path: fx(fxMP4), Can: []string{"h264"}, AudioIndex: intp(9)}); code(err) != types.ErrBadRequest {
		t.Errorf("bad audioIndex: %v", err)
	}
	if _, err := s.CreateSession(ctx, SessionRequest{Path: fx("missing.mp4")}); code(err) != types.ErrNotFound {
		t.Errorf("missing file: %v", err)
	}
	if _, err := s.CreateSession(ctx, SessionRequest{Path: "/etc/passwd"}); code(err) != types.ErrForbidden {
		t.Errorf("outside root must be FORBIDDEN: %v", err)
	}
	s.CloseSession(info.ID)
	s.CloseSession(info.ID) // idempotent
	s.CloseSession("garbage")
	if _, err := s.Playlist(info.ID); code(err) != types.ErrNotFound {
		t.Errorf("closed session: %v", err)
	}
}

func TestConcurrencyCap(t *testing.T) {
	s := newTestService(t, Options{MaxProcs: 1, ProcWait: 200 * time.Millisecond})
	ctx := context.Background()
	info, err := s.CreateSession(ctx, SessionRequest{Path: fx(fxMP4), Can: []string{"h264", "aac"}})
	if err != nil {
		t.Fatal(err)
	}
	// A stream holds its slot until closed, even after ffmpeg has exited.
	rc, err := s.OpenSegment(ctx, info.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.OpenSegment(ctx, info.ID, 1)
	if ae, ok := err.(*types.APIError); !ok || ae.Code != types.ErrTimeout || ae.Message != "server busy" {
		t.Fatalf("over the cap: %v", err)
	}
	// A probe competes for the same slots.
	if _, err := s.Probe(ctx, fx(fxAVI)); err == nil {
		t.Error("probe should also be refused while the slot is held")
	}
	io.Copy(io.Discard, rc) //nolint:errcheck
	rc.Close()
	rc2, err := s.OpenSegment(ctx, info.ID, 1)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	io.Copy(io.Discard, rc2) //nolint:errcheck
	rc2.Close()
}

// TestSequentialPullDoesNotStarveOthers models the SPA's aggressive prefetch:
// one player pulling segments back to back must neither be throttled nor
// push a second concurrent player into "server busy".
func TestSequentialPullDoesNotStarveOthers(t *testing.T) {
	s := newTestService(t, Options{MaxProcs: 2})
	ctx := context.Background()
	a, err := s.CreateSession(ctx, SessionRequest{Path: fx(fxAVI), Can: []string{"h264", "aac"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateSession(ctx, SessionRequest{Path: fx(fxMKV), Can: []string{"h264", "aac"}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	pull := func(info SessionInfo, rounds int) {
		defer wg.Done()
		for r := 0; r < rounds; r++ {
			for n := 0; n < info.SegmentCount; n++ {
				rc, err := s.OpenSegment(ctx, info.ID, n)
				if err != nil {
					errs <- err
					return
				}
				if _, err := io.Copy(io.Discard, rc); err != nil {
					errs <- err
				}
				rc.Close()
			}
		}
	}
	wg.Add(2)
	go pull(a, 2) // 10 transcoded segments, no pause
	go pull(b, 2) // 6 copied+aac segments concurrently
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("player error: %v", err)
	}
	if s.running.Load() != 0 {
		t.Errorf("processes still running: %d", s.running.Load())
	}
}

func TestCancelKillsProcess(t *testing.T) {
	s := newTestService(t, Options{SegmentSec: 10})
	base := childProcs(t)
	info, err := s.CreateSession(context.Background(), SessionRequest{Path: fx(fxBig), Can: nil}) // full transcode, 720p, whole file
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := s.OpenSegment(ctx, info.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Do not read: the producer blocks on its pipe and stays alive.
	if s.running.Load() != 1 || childProcs(t) != base+1 {
		t.Fatalf("expected one live ffmpeg: running=%d children=%d (base %d)", s.running.Load(), childProcs(t), base)
	}
	cancel()
	buf := make([]byte, 1<<20)
	for {
		if _, err := rc.Read(buf); err != nil {
			break
		}
	}
	rc.Close()
	if !waitFor(5*time.Second, func() bool { return s.running.Load() == 0 && childProcs(t) == base }) {
		t.Fatalf("ffmpeg lingered after cancel: running=%d children=%d (base %d)", s.running.Load(), childProcs(t), base)
	}

	// Per-segment timeout has the same effect.
	s2 := newTestService(t, Options{SegmentSec: 10, SegmentTimeout: 300 * time.Millisecond})
	info2, err := s2.CreateSession(context.Background(), SessionRequest{Path: fx(fxBig), Can: nil})
	if err != nil {
		t.Fatal(err)
	}
	rc2, err := s2.OpenSegment(context.Background(), info2.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	for {
		if _, err := rc2.Read(buf); err != nil {
			gotErr = err
			break
		}
	}
	rc2.Close()
	if ae, ok := gotErr.(*types.APIError); !ok || ae.Code != types.ErrTimeout {
		t.Errorf("timeout should surface as TIMEOUT, got %v", gotErr)
	}
	if !waitFor(5*time.Second, func() bool { return s2.running.Load() == 0 && childProcs(t) == base }) {
		t.Fatalf("ffmpeg lingered after timeout: children=%d (base %d)", childProcs(t), base)
	}
}

// TestNoLeaksOverManySegments pulls a few dozen segments back to back — the
// aggressive-prefetch pattern — and checks processes, descriptors and
// goroutines return to baseline, and that nothing was written to disk.
func TestNoLeaksOverManySegments(t *testing.T) {
	s := newTestService(t, Options{})
	ctx := context.Background()
	before, err := os.ReadDir(fixDir)
	if err != nil {
		t.Fatal(err)
	}
	var sessions []SessionInfo
	for _, f := range []string{fxAVI, fxMKV, fxMP4} {
		info, err := s.CreateSession(ctx, SessionRequest{Path: fx(f), Can: []string{"h264", "aac"}})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, info)
	}
	runtime.GC()
	baseProcs, baseFDs, baseGo := childProcs(t), openFDs(t), runtime.NumGoroutine()

	pulled := 0
	for round := 0; round < 3; round++ {
		for _, info := range sessions {
			for n := 0; n < info.SegmentCount; n++ {
				rc, err := s.OpenSegment(ctx, info.ID, n)
				if err != nil {
					t.Fatalf("round %d seg %d: %v", round, n, err)
				}
				if _, err := io.Copy(io.Discard, rc); err != nil {
					t.Fatalf("read: %v", err)
				}
				rc.Close()
				pulled++
			}
		}
	}
	if pulled < 30 {
		t.Fatalf("only %d segments pulled", pulled)
	}
	if !waitFor(3*time.Second, func() bool {
		runtime.GC()
		return s.running.Load() == 0 && childProcs(t) == baseProcs && openFDs(t) <= baseFDs+1 && runtime.NumGoroutine() <= baseGo+1
	}) {
		t.Errorf("leak after %d segments: procs %d→%d, fds %d→%d, goroutines %d→%d, running=%d",
			pulled, baseProcs, childProcs(t), baseFDs, openFDs(t), baseGo, runtime.NumGoroutine(), s.running.Load())
	}
	after, err := os.ReadDir(fixDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("segment production changed the media directory: %d → %d entries", len(before), len(after))
	}
	for i := range before {
		bi, _ := before[i].Info()
		ai, _ := after[i].Info()
		if before[i].Name() != after[i].Name() || bi.Size() != ai.Size() || !bi.ModTime().Equal(ai.ModTime()) {
			t.Errorf("file %s changed", before[i].Name())
		}
	}
}
