package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"unraid-filebrowser/internal/fsops"
)

// Fixtures are generated once per test binary with the local ffmpeg. Every
// test that needs ffmpeg skips cleanly when it is absent.
var (
	fixDir     string
	ffmpegBin  string
	ffprobeBin string
)

const (
	fxMP4    = "bars_h264_aac.mp4"   // GOP 1 s: directly playable, remux only
	fxMKV    = "bars_h264_ac3.mkv"   // GOP 1 s: audio-only transcode
	fxSparse = "bars_sparse_aac.mkv" // GOP 5 s: keyframes too far apart to copy
	fxAVI    = "bars_mpeg4_mp3.avi"  // full transcode
	fxFLAC   = "sine.flac"           // audio only
	fxBig    = "bars_720p.mp4"       // large output: for cancellation tests
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ffmpegBin, ffprobeBin = Discover()
	if ffmpegBin != "" && ffprobeBin != "" {
		dir, err := os.MkdirTemp("", "transcode-fixtures-")
		if err != nil {
			panic(err)
		}
		// fsops resolves symlinks in roots (macOS /var → /private/var); use
		// the resolved path so request paths match exactly.
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real
		}
		fixDir = dir
		if err := makeFixtures(dir); err != nil {
			fmt.Fprintln(os.Stderr, "fixtures:", err)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	code := m.Run()
	if fixDir != "" {
		os.RemoveAll(fixDir)
	}
	os.Exit(code)
}

func makeFixtures(dir string) error {
	const bars = "smptebars=size=320x240:rate=25"
	const tone = "sine=frequency=440:sample_rate=48000"
	gen := func(name string, args ...string) error {
		full := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", bars, "-f", "lavfi", "-i", tone, "-t", "10"}, args...)
		full = append(full, filepath.Join(dir, name))
		out, err := exec.Command(ffmpegBin, full...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v: %s", name, err, out)
		}
		return nil
	}
	x264 := func(gop int) []string {
		return []string{"-c:v", "libx264", "-preset", "veryfast", "-g", strconv.Itoa(gop), "-keyint_min", strconv.Itoa(gop),
			"-sc_threshold", "0", "-pix_fmt", "yuv420p"}
	}
	steps := []func() error{
		func() error { return gen(fxMP4, append(x264(25), "-c:a", "aac", "-b:a", "96k")...) },
		func() error { return gen(fxMKV, append(x264(25), "-c:a", "ac3", "-b:a", "192k")...) },
		func() error { return gen(fxSparse, append(x264(125), "-c:a", "aac", "-b:a", "96k")...) },
		func() error { return gen(fxAVI, "-c:v", "mpeg4", "-q:v", "5", "-c:a", "libmp3lame", "-b:a", "128k") },
		func() error {
			out, err := exec.Command(ffmpegBin, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", tone, "-t", "10", "-c:a", "flac", filepath.Join(dir, fxFLAC)).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %v: %s", fxFLAC, err, out)
			}
			return nil
		},
		func() error {
			out, err := exec.Command(ffmpegBin, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30", "-f", "lavfi", "-i", tone, "-t", "10",
				"-c:v", "libx264", "-preset", "ultrafast", "-crf", "18", "-g", "30", "-pix_fmt", "yuv420p",
				"-c:a", "aac", filepath.Join(dir, fxBig)).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %v: %s", fxBig, err, out)
			}
			return nil
		},
	}
	for _, st := range steps {
		if err := st(); err != nil {
			return err
		}
	}
	return nil
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if fixDir == "" {
		t.Skip("ffmpeg/ffprobe not installed; skipping real-media test")
	}
}

func fx(name string) string { return filepath.Join(fixDir, name) }

// newTestService builds a Service over the fixture dir with short segments so
// the whole suite stays quick: 2 s transcoded segments, 4 s copied ones.
func newTestService(t *testing.T, opts Options) *Service {
	t.Helper()
	requireFFmpeg(t)
	if opts.SegmentSec == 0 {
		opts.SegmentSec = 2
	}
	if opts.RemuxSegmentSec == 0 {
		opts.RemuxSegmentSec = 4
	}
	opts.FFmpegPath, opts.FFprobePath = ffmpegBin, ffprobeBin
	opts.DisableHW = true // software is the path under test
	s := New(fsops.New([]string{fixDir}), opts)
	t.Cleanup(s.Close)
	if !s.Capabilities().Available {
		t.Fatalf("service unavailable: %s", s.Capabilities().Reason)
	}
	return s
}

func intp(i int) *int { return &i }

// tsInfo is what ffprobe says about a produced segment.
type tsInfo struct {
	format      string
	videoCodec  string
	audioCodec  string
	firstVPTS   float64 // seconds; -1 when no video
	lastVPTS    float64
	firstVDTS   float64
	lastVDTS    float64
	videoFrames int
	keyframes   []float64
	firstAPTS   float64 // -1 when no audio
	lastAPTS    float64
}

// probeTS writes seg to a temp file and inspects it with ffprobe.
func probeTS(t *testing.T, seg []byte) tsInfo {
	t.Helper()
	if len(seg) == 0 {
		t.Fatal("empty segment")
	}
	f := filepath.Join(t.TempDir(), "seg.ts")
	if err := os.WriteFile(f, seg, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffprobeBin, "-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", "-show_packets",
		"-show_entries", "packet=stream_index,codec_type,pts_time,dts_time,flags", f).Output()
	if err != nil {
		t.Fatalf("ffprobe on produced segment: %v", err)
	}
	var d struct {
		Format struct {
			FormatName string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
		Packets []struct {
			StreamIndex int    `json:"stream_index"`
			CodecType   string `json:"codec_type"`
			PTS         string `json:"pts_time"`
			DTS         string `json:"dts_time"`
			Flags       string `json:"flags"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("ffprobe json: %v", err)
	}
	info := tsInfo{format: d.Format.FormatName, firstVPTS: -1, firstAPTS: -1}
	vidx, aidx := -1, -1
	for _, st := range d.Streams {
		switch st.CodecType {
		case "video":
			info.videoCodec = st.CodecName
			vidx = st.Index
		case "audio":
			info.audioCodec = st.CodecName
			aidx = st.Index
		}
	}
	for _, p := range d.Packets {
		pts, _ := strconv.ParseFloat(p.PTS, 64)
		dts, _ := strconv.ParseFloat(p.DTS, 64)
		switch p.StreamIndex {
		case vidx:
			if info.firstVPTS < 0 || pts < info.firstVPTS {
				info.firstVPTS = pts
			}
			if pts > info.lastVPTS {
				info.lastVPTS = pts
			}
			if info.videoFrames == 0 || dts < info.firstVDTS {
				info.firstVDTS = dts
			}
			if dts > info.lastVDTS {
				info.lastVDTS = dts
			}
			info.videoFrames++
			if strings.Contains(p.Flags, "K") {
				info.keyframes = append(info.keyframes, pts)
			}
		case aidx:
			if info.firstAPTS < 0 || pts < info.firstAPTS {
				info.firstAPTS = pts
			}
			if pts > info.lastAPTS {
				info.lastAPTS = pts
			}
		}
	}
	return info
}

// fetchSegment pulls one whole segment through the service.
func fetchSegment(t *testing.T, s *Service, id string, n int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rc, err := s.OpenSegment(ctx, id, n)
	if err != nil {
		t.Fatalf("OpenSegment(%d): %v", n, err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		t.Fatalf("read segment %d: %v", n, err)
	}
	return buf.Bytes()
}

func near(a, b, tol float64) bool { return a-b <= tol && b-a <= tol }

// childProcs counts this process's live children (ffmpeg/ffprobe).
func childProcs(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		// pgrep exits 1 when nothing matches.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return 0
		}
		t.Skipf("pgrep unavailable: %v", err)
	}
	return len(strings.Fields(string(out)))
}

func openFDs(t *testing.T) int {
	t.Helper()
	// Readdirnames rather than ReadDir: the latter lstats every entry and
	// fails when a descriptor closes mid-scan.
	d, err := os.Open("/dev/fd")
	if err != nil {
		t.Skipf("/dev/fd unreadable: %v", err)
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		t.Skipf("/dev/fd unreadable: %v", err)
	}
	return len(names)
}

// waitFor polls cond up to d.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
