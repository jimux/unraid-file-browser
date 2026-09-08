package transcode

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// bundledDirs are searched, in order, before PATH. The companion plugin
// (filebrowser-ffmpeg) installs into its own private directory rather than a
// shared one, so it can neither shadow nor be shadowed by an ffmpeg the admin
// installed another way; /usr/local/sbin is kept for installs that predate the
// split. A tested bundled build is preferred over whatever is on PATH.
var bundledDirs = []string{"/usr/local/filebrowser/bin", "/usr/local/sbin"}

// Discover locates ffmpeg and ffprobe: the bundled locations first, then
// PATH. Missing tools yield empty strings; the Service then reports
// available:false and every media endpoint answers UNAVAILABLE.
func Discover() (ffmpeg, ffprobe string) {
	return find("ffmpeg"), find("ffprobe")
}

func find(name string) string {
	for _, dir := range bundledDirs {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// Capabilities is the payload of GET /media/capabilities.
type Capabilities struct {
	Available bool   `json:"available"`
	FFmpeg    string `json:"ffmpeg"`  // version, "" when missing
	FFprobe   string `json:"ffprobe"` // version, "" when missing
	// Paths are reported so an operator can see which binary is in use.
	FFmpegPath  string `json:"ffmpegPath,omitempty"`
	FFprobePath string `json:"ffprobePath,omitempty"`
	// HWAccels is ffmpeg's `-hwaccels` list (may be non-empty on builds that
	// cannot actually use any of them; Encoders is what matters).
	HWAccels []string `json:"hwaccels"`
	// Encoders are the H.264/HEVC/AAC encoders this build actually offers,
	// e.g. ["aac","libx264"] on the bundled static build.
	Encoders []string `json:"encoders"`
	// HWEncoder is the hardware H.264 encoder the daemon will try first, or
	// "" when it will use software only (see chooseHW).
	HWEncoder string `json:"hwEncoder,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// inspect runs the three cheap introspection commands once at construction.
func inspect(ctx context.Context, ffmpeg, ffprobe string) Capabilities {
	caps := Capabilities{HWAccels: []string{}, Encoders: []string{}}
	switch {
	case ffmpeg == "" && ffprobe == "":
		caps.Reason = "ffmpeg and ffprobe are not installed"
		return caps
	case ffmpeg == "":
		caps.Reason = "ffmpeg is not installed"
		return caps
	case ffprobe == "":
		caps.Reason = "ffprobe is not installed"
		return caps
	}
	caps.FFmpegPath, caps.FFprobePath = ffmpeg, ffprobe
	caps.FFmpeg = versionOf(ctx, ffmpeg)
	caps.FFprobe = versionOf(ctx, ffprobe)
	if caps.FFmpeg == "" || caps.FFprobe == "" {
		caps.Reason = "ffmpeg or ffprobe failed to run"
		return caps
	}
	caps.HWAccels = parseHWAccels(run(ctx, ffmpeg, "-hide_banner", "-hwaccels"))
	caps.Encoders = parseEncoders(run(ctx, ffmpeg, "-hide_banner", "-encoders"))
	if !contains(caps.Encoders, "libx264") {
		caps.Reason = "ffmpeg lacks the libx264 encoder"
		return caps
	}
	if !contains(caps.Encoders, "aac") {
		caps.Reason = "ffmpeg lacks the aac encoder"
		return caps
	}
	caps.Available = true
	return caps
}

// run executes an introspection command with a short deadline and returns
// its stdout (empty on any failure — introspection is best effort).
func run(ctx context.Context, bin string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return out
}

// versionOf extracts "N.N.N" from `ffmpeg -version`'s first line
// ("ffmpeg version 7.0.2-static https://..." → "7.0.2-static").
func versionOf(ctx context.Context, bin string) string {
	out := run(ctx, bin, "-hide_banner", "-version")
	line, _, _ := bytes.Cut(out, []byte("\n"))
	fields := strings.Fields(string(line))
	// "<tool> version <ver> Copyright ..."
	if len(fields) >= 3 && fields[1] == "version" {
		return fields[2]
	}
	return ""
}

// parseHWAccels reads `ffmpeg -hwaccels`: a header line then one name per
// line.
func parseHWAccels(out []byte) []string {
	var list []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" || strings.HasSuffix(ln, ":") {
			continue
		}
		list = append(list, ln)
	}
	sort.Strings(list)
	if list == nil {
		list = []string{}
	}
	return list
}

// parseEncoders reads `ffmpeg -encoders` and keeps the encoders relevant to
// HLS output: anything producing h264/hevc plus aac. Lines look like
// " V....D libx264              libx264 H.264 / AVC ... (codec h264)".
func parseEncoders(out []byte) []string {
	var list []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || len(fields[0]) != 6 {
			continue // legend / header lines
		}
		name := fields[1]
		switch {
		case name == "aac", name == "libfdk_aac":
		case strings.HasPrefix(name, "h264_"), name == "libx264", name == "libopenh264":
		case strings.HasPrefix(name, "hevc_"), name == "libx265":
		default:
			continue
		}
		list = append(list, name)
	}
	sort.Strings(list)
	if list == nil {
		list = []string{}
	}
	return list
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// renderNode is the DRM render device VAAPI needs.
const renderNode = "/dev/dri/renderD128"

// chooseHW decides whether hardware H.264 encoding is worth trying. Both
// conditions must hold: the render node exists AND this ffmpeg build lists
// the VAAPI encoder. The bundled fully-static build has no VAAPI at all, so
// the answer there is always "" and the software path is the one that runs;
// a hwaccel-capable ffmpeg dropped into a bundled dir lights this up with
// no code change. The device check is injected so it is unit-testable.
func chooseHW(encoders []string, deviceExists func(string) bool) string {
	if !contains(encoders, "h264_vaapi") {
		return ""
	}
	if !deviceExists(renderNode) {
		return ""
	}
	return "h264_vaapi"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
