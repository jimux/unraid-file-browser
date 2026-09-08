package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ffprobe limits. They are independent of the transcoder's pool: a crawl
// must never starve playback of ffmpeg slots, and vice versa.
const (
	ffprobeSlots   = 2                // concurrent ffprobe processes during a crawl
	ffprobeTimeout = 20 * time.Second // per invocation
	ffprobeMaxJSON = 4 << 20          // output larger than this is rejected
	ffprobeMaxErr  = 2048             // stderr retained for error messages

	// childFDPath is where ExtraFiles[0] appears inside the child. Handing
	// ffprobe the already-open descriptor means the path it reads is exactly
	// the file the crawler stat'ed, and a crafted file cannot redirect it.
	childFDPath = "/dev/fd/3"
	// protocolWhitelist confines ffprobe to that descriptor (file covers
	// /dev/fd/N, fd is the explicit protocol on ffmpeg >= 6.1) and its pipes.
	protocolWhitelist = "file,fd,pipe"
)

// ffprobe runs the binary under a small semaphore with fixed argv.
type ffprobe struct {
	bin  string
	slot chan struct{}
}

func newFFprobe(bin string) *ffprobe {
	return &ffprobe{bin: bin, slot: make(chan struct{}, ffprobeSlots)}
}

// probeJSON is ffprobe's output, only the fields we read. Numbers arrive as
// strings in the format section and mostly as strings in streams.
type probeJSON struct {
	Format struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		BitRate    string            `json:"bit_rate"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
	Streams []probeStream `json:"streams"`
}

type probeStream struct {
	Index            int               `json:"index"`
	CodecType        string            `json:"codec_type"`
	CodecName        string            `json:"codec_name"`
	Profile          string            `json:"profile"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	PixFmt           string            `json:"pix_fmt"`
	ColorTransfer    string            `json:"color_transfer"`
	ColorPrimaries   string            `json:"color_primaries"`
	BitsPerRawSample string            `json:"bits_per_raw_sample"`
	RFrameRate       string            `json:"r_frame_rate"`
	AvgFrameRate     string            `json:"avg_frame_rate"`
	BitRate          string            `json:"bit_rate"`
	SampleRate       string            `json:"sample_rate"`
	Channels         int               `json:"channels"`
	Duration         string            `json:"duration"`
	Tags             map[string]string `json:"tags"`
	Disposition      struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
	SideData []struct {
		Type string `json:"side_data_type"`
	} `json:"side_data_list"`
}

// run probes one file. When r is an *os.File the descriptor is inherited and
// probed as /dev/fd/3; otherwise (tests feeding synthetic readers) the path
// is passed. The call waits for a semaphore slot until ctx expires.
func (f *ffprobe) run(ctx context.Context, path string, r io.ReaderAt) (*probeJSON, error) {
	select {
	case f.slot <- struct{}{}:
		defer func() { <-f.slot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, ffprobeTimeout)
	defer cancel()

	args := []string{
		"-v", "error",
		"-protocol_whitelist", protocolWhitelist,
		"-print_format", "json",
		"-show_format", "-show_streams",
	}
	input := path
	var extra []*os.File
	if of, ok := r.(*os.File); ok {
		input = childFDPath
		extra = []*os.File{of}
	}
	args = append(args, "--", input)
	cmd := exec.CommandContext(ctx, f.bin, args...)
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdin = nil
	cmd.ExtraFiles = extra
	var errBuf boundedBuf
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	raw, rerr := io.ReadAll(io.LimitReader(stdout, ffprobeMaxJSON+1))
	if len(raw) > ffprobeMaxJSON {
		cmd.Process.Kill() //nolint:errcheck
	}
	io.Copy(io.Discard, stdout) //nolint:errcheck — drain so Wait never blocks
	werr := cmd.Wait()
	switch {
	case len(raw) > ffprobeMaxJSON:
		return nil, errors.New("ffprobe: output exceeds 4 MB")
	case ctx.Err() != nil:
		return nil, fmt.Errorf("ffprobe: %w", ctx.Err())
	case rerr != nil:
		return nil, fmt.Errorf("ffprobe: %w", rerr)
	case werr != nil:
		return nil, fmt.Errorf("ffprobe: %s", errBuf.summary(werr.Error()))
	}
	var pj probeJSON
	if err := json.Unmarshal(raw, &pj); err != nil {
		return nil, fmt.Errorf("ffprobe: unparsable output: %w", err)
	}
	return &pj, nil
}

// boundedBuf retains at most ffprobeMaxErr bytes of stderr.
type boundedBuf struct {
	mu sync.Mutex
	b  []byte
}

func (w *boundedBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := ffprobeMaxErr - len(w.b); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		w.b = append(w.b, p...)
	}
	return len(p), nil
}

func (w *boundedBuf) summary(alt string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := strings.TrimSpace(string(w.b))
	if s == "" {
		return alt
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// --- ffprobe value helpers --------------------------------------------------

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

// fps evaluates "num/den" rate strings, preferring the average rate.
func fps(avg, r string) float64 {
	for _, s := range []string{avg, r} {
		n, d, ok := strings.Cut(s, "/")
		if !ok {
			continue
		}
		nv, err1 := strconv.ParseFloat(n, 64)
		dv, err2 := strconv.ParseFloat(d, 64)
		if err1 != nil || err2 != nil || dv == 0 || nv <= 0 {
			continue
		}
		v := math.Round(nv/dv*1000) / 1000
		if v > 1000 {
			continue // nonsense
		}
		return v
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
	if err1 != nil || err2 != nil || err3 != nil || h < 0 || m < 0 || sec < 0 {
		return 0
	}
	return float64(h)*3600 + float64(m)*60 + sec
}

// tagValue looks a tag up case-insensitively (Matroska upper-cases, MP4 and
// ID3 through ffprobe lower-case).
func tagValue(tags map[string]string, names ...string) string {
	for _, n := range names {
		for k, v := range tags {
			if strings.EqualFold(k, n) && strings.TrimSpace(v) != "" {
				return v
			}
		}
	}
	return ""
}

// duration picks the container duration, falling back to per-stream values
// (Matroska often carries them only as stream tags).
func (pj *probeJSON) duration() float64 {
	if d := atof(pj.Format.Duration); d > 0 {
		return d
	}
	for _, st := range pj.Streams {
		if d := atof(st.Duration); d > 0 {
			return d
		}
		if d := parseClock(tagValue(st.Tags, "DURATION")); d > 0 {
			return d
		}
	}
	return 0
}

// container derives a single container name from ffprobe's comma list
// ("mov,mp4,m4a,3gp,3g2,mj2"): the file's own extension when it is in the
// list, else the first entry.
func (pj *probeJSON) container(ext string) string {
	names := strings.Split(pj.Format.FormatName, ",")
	for _, n := range names {
		if n == ext {
			return n
		}
	}
	if ext == "mkv" || ext == "mka" {
		for _, n := range names {
			if n == "matroska" {
				return n
			}
		}
	}
	if len(names) > 0 && names[0] != "" {
		return names[0]
	}
	return ""
}

// bitDepth reads bits_per_raw_sample, falling back to the pixel format name.
func (st *probeStream) bitDepth() int64 {
	if b := atoi(st.BitsPerRawSample); b > 0 && b <= 32 {
		return b
	}
	pf := st.PixFmt
	switch {
	case pf == "":
		return 0
	case strings.Contains(pf, "p16") || strings.HasSuffix(pf, "16le") || strings.HasSuffix(pf, "16be"):
		return 16
	case strings.Contains(pf, "p14"):
		return 14
	case strings.Contains(pf, "p12"):
		return 12
	case strings.Contains(pf, "p10"):
		return 10
	case strings.Contains(pf, "p9"):
		return 9
	}
	return 8
}

// hdr classifies the stream's dynamic range: Dolby Vision when a DOVI
// configuration record is present (it wraps PQ, so it wins), else by the
// transfer characteristic.
func (st *probeStream) hdr() string {
	for _, sd := range st.SideData {
		if strings.Contains(strings.ToLower(sd.Type), "dovi") {
			return "dolbyvision"
		}
	}
	switch st.ColorTransfer {
	case "smpte2084":
		return "hdr10"
	case "arib-std-b67":
		return "hlg"
	}
	return "none"
}
