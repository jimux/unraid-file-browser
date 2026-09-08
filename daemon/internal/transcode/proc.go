package transcode

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"unraid-filebrowser/internal/types"
)

// Opener is the slice of the filesystem layer this package needs. It must
// open through a race-safe walk (fsops does: every component O_NOFOLLOW
// relative to a verified root fd); this package never calls os.Open on a
// user-derived path. For regular files the reader is an *os.File.
type Opener interface {
	Open(ctx context.Context, path string) (io.ReadCloser, types.Entry, error)
}

// childFDPath is where ExtraFiles[0] appears inside the child process.
const childFDPath = "/dev/fd/3"

// protocolWhitelist confines ffmpeg/ffprobe to the inherited descriptor
// (`file` covers /dev/fd/N; `fd` is the explicit descriptor protocol on
// ffmpeg ≥ 6.1) and its stdout pipe. A crafted playlist/concat input cannot
// make it open http://, tcp://, or another local file.
const protocolWhitelist = "file,fd,pipe"

// stderrCap bounds how much subprocess stderr is retained for messages.
const stderrCap = 4096

// openInput opens path through the Opener and hands back the descriptor a
// child will inherit. Anything that is not a plain file (an archive entry
// stream, say) is refused: ffmpeg needs to seek.
func openInput(ctx context.Context, o Opener, path string) (*os.File, types.Entry, error) {
	rc, entry, err := o.Open(ctx, path)
	if err != nil {
		return nil, types.Entry{}, err
	}
	f, ok := rc.(*os.File)
	if !ok {
		rc.Close()
		return nil, types.Entry{}, types.Errf(types.ErrBadRequest, "media inside archives cannot be streamed; download it instead")
	}
	return f, entry, nil
}

// command builds an ffmpeg/ffprobe invocation over an inherited descriptor:
// fixed argv, no shell, no stdin, bounded reaping after cancellation.
func command(ctx context.Context, bin string, in *os.File, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdin = nil
	cmd.ExtraFiles = []*os.File{in}
	return cmd
}

// boundedBuf retains at most stderrCap bytes of subprocess stderr.
type boundedBuf struct {
	mu sync.Mutex
	b  []byte
}

func (w *boundedBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := stderrCap - len(w.b); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		w.b = append(w.b, p...)
	}
	return len(p), nil
}

// summary condenses captured stderr to one short line, falling back to alt.
func (w *boundedBuf) summary(alt string) string {
	w.mu.Lock()
	s := string(w.b)
	w.mu.Unlock()
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	out := strings.Join(lines, "; ")
	if len(out) > 300 {
		out = out[:300]
	}
	if out == "" {
		return alt
	}
	return out
}

// mediaErr wraps a subprocess failure as ENCODING_ERROR — the file could not
// be decoded/encoded — unless the context ran out first, which is TIMEOUT.
func mediaErr(ctx context.Context, what string, stderr *boundedBuf, err error) error {
	if ctx.Err() != nil {
		return types.Errf(types.ErrTimeout, what+" timed out")
	}
	return types.Errf(types.ErrEncoding, what+" failed: "+stderr.summary(err.Error()))
}
