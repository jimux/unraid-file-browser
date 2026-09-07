package archive

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"unraid-filebrowser/internal/types"
)

// dummyPassword is always passed via -p so 7zz never prompts interactively
// on encrypted archives; wrong-password failures surface as ARCHIVE_ERROR.
const dummyPassword = "filebrowserd-no-interactive-prompt"

// stderrCap bounds how much subprocess stderr is retained for messages.
const stderrCap = 4096

func (r *Resolver) sevenZipBin() (string, error) {
	if r.seven == "" {
		return "", types.Errf(types.ErrArchive, "7-Zip (7zz) binary not available; cannot read this archive format")
	}
	return r.seven, nil
}

// sevenZipListBlob lists an archive via `7zz l -slt -ba`, materializing
// nested layers to a temp file first.
func (r *Resolver) sevenZipListBlob(ctx context.Context, b blob) ([]entryRecord, error) {
	bin, err := r.sevenZipBin()
	if err != nil {
		return nil, err
	}
	af, cleanup, err := b.file(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// -spd disables wildcard interpretation of names; -- stops switch
	// parsing so hostile file names cannot inject flags.
	cmd := sevenZipCommand(ctx, bin, af, "l", "-slt", "-ba", "-spd", "-p"+dummyPassword, "--", af.path)
	var errBuf boundedBuf
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, types.Errf(types.ErrArchive, "7zz: "+err.Error())
	}
	if err := cmd.Start(); err != nil {
		return nil, types.Errf(types.ErrArchive, "7zz: "+err.Error())
	}
	recs, perr := parseSLT(stdout, r.opts.MaxEntries)
	if perr != nil {
		// Parsing stopped early (too many entries or garbage): do not sit
		// draining an arbitrarily long listing, kill the producer.
		cmd.Process.Kill() //nolint:errcheck — already exited is fine
	}
	io.Copy(io.Discard, stdout) //nolint:errcheck — drain so Wait never blocks
	werr := cmd.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if perr != nil {
		var ae *types.APIError
		if errors.As(perr, &ae) {
			return nil, perr
		}
		return nil, types.Errf(types.ErrArchive, "7zz listing unparsable: "+perr.Error())
	}
	if werr != nil {
		return nil, types.Errf(types.ErrArchive, "7zz list failed: "+errBuf.summary(werr.Error()))
	}
	return recs, nil
}

// sevenZipCommand builds a 7zz invocation. When the archive is an open
// descriptor it is inherited as fd 3 and named /dev/fd/3 in args, so the
// child reads exactly the file RealFS.Open resolved instead of
// re-resolving a path of its own.
func sevenZipCommand(ctx context.Context, bin string, af archiveFile, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 3 * time.Second
	if af.fd != nil {
		cmd.ExtraFiles = []*os.File{af.fd}
	}
	return cmd
}

// parseSLT parses `7zz l -slt -ba` output: blank-line-separated blocks of
// "Key = Value" lines. Exported fields used: Path, Size, Modified,
// Attributes (leading token containing 'D' → directory), Folder. Parsing
// stops with ErrArchive once more than maxEntries (> 0) records are seen.
func parseSLT(r io.Reader, maxEntries int) ([]entryRecord, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var (
		recs               []entryRecord
		p, size, mod       string
		attrs, folder, sym string
		tooMany            bool
	)
	flush := func() {
		if p == "" {
			return
		}
		if maxEntries > 0 && len(recs) >= maxEntries {
			tooMany = true
			return
		}
		rec := entryRecord{raw: p, size: -1}
		if n, err := strconv.ParseInt(size, 10, 64); err == nil {
			rec.size = n
		}
		rec.mtime = parseSevenZipTime(mod)
		if attrHasD(attrs) || folder == "+" {
			rec.isDir = true
			if rec.size < 0 {
				rec.size = 0
			}
		}
		rec.symlink = sym
		recs = append(recs, rec)
		p, size, mod, attrs, folder, sym = "", "", "", "", "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			if tooMany {
				return nil, errTooManyEntries()
			}
			continue
		}
		key, val, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		switch key {
		case "Path":
			p = val
		case "Size":
			size = val
		case "Modified":
			mod = val
		case "Attributes":
			attrs = val
		case "Folder":
			folder = val
		case "Symbolic Link":
			sym = val
		}
	}
	flush()
	if tooMany {
		return nil, errTooManyEntries()
	}
	return recs, sc.Err()
}

// attrHasD reports whether the first token of a 7zz Attributes value (e.g.
// "D_ drwxr-xr-x" or "A_ -rw-r--r--") marks a directory.
func attrHasD(attrs string) bool {
	tok := attrs
	if i := strings.IndexByte(tok, ' '); i >= 0 {
		tok = tok[:i]
	}
	return strings.ContainsRune(tok, 'D')
}

// parseSevenZipTime parses "2006-01-02 15:04:05" with an optional fractional
// second suffix, in local time (what 7zz prints). Returns 0 when unknown.
func parseSevenZipTime(s string) int64 {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return 0
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// sevenZipOpenBlob streams one entry to stdout via `7zz e -so`.
func (r *Resolver) sevenZipOpenBlob(ctx context.Context, b blob, rec entryRecord) (io.ReadCloser, error) {
	bin, err := r.sevenZipBin()
	if err != nil {
		return nil, err
	}
	af, cleanup, err := b.file(ctx)
	if err != nil {
		return nil, err
	}
	cmd := sevenZipCommand(ctx, bin, af, "e", "-so", "-spd", "-p"+dummyPassword, "--", af.path, rec.storedName())
	errBuf := &boundedBuf{}
	cmd.Stderr = errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, types.Errf(types.ErrArchive, "7zz: "+err.Error())
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, types.Errf(types.ErrArchive, "7zz: "+err.Error())
	}
	return &procReader{
		ctx:     ctx,
		r:       stdout,
		cmd:     cmd,
		stderr:  errBuf,
		cleanup: cleanup,
		expect:  rec.size,
	}, nil
}

// procReader adapts a 7zz extraction subprocess to io.ReadCloser. A nonzero
// exit surfaces as ARCHIVE_ERROR (with a stderr snippet) instead of a silent
// short read; so does a clean exit that produced no bytes for an entry the
// listing says is non-empty (`7zz e -so` exits 0 on a name it cannot
// match). Close kills the process and removes any temp layer file.
type procReader struct {
	ctx     context.Context
	r       io.Reader // stdout pipe
	cmd     *exec.Cmd
	stderr  *boundedBuf
	cleanup func() error
	expect  int64 // listed entry size; -1 unknown
	total   int64 // bytes delivered so far

	mu      sync.Mutex
	waited  bool
	waitErr error
	closed  bool
}

func (p *procReader) wait() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.waited {
		p.waitErr = p.cmd.Wait()
		p.waited = true
	}
	return p.waitErr
}

func (p *procReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.total += int64(n)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		if werr := p.wait(); werr != nil {
			if p.ctx.Err() != nil {
				return n, types.Errf(types.ErrTimeout, "archive extraction timed out")
			}
			return n, types.Errf(types.ErrArchive, "7zz extraction failed: "+p.stderr.summary(werr.Error()))
		}
		if p.expect > 0 && p.total == 0 {
			return n, types.Errf(types.ErrArchive, "7zz produced no output for a non-empty entry: "+p.stderr.summary("entry not matched"))
		}
		return n, io.EOF
	}
	if p.ctx.Err() != nil {
		return n, types.Errf(types.ErrTimeout, "archive extraction timed out")
	}
	return n, err
}

func (p *procReader) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	if p.cmd.Process != nil {
		p.cmd.Process.Kill() //nolint:errcheck — already exited is fine
	}
	p.wait() //nolint:errcheck — reap; exit status irrelevant on close
	if p.cleanup != nil {
		return p.cleanup()
	}
	return nil
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
			w.b = append(w.b, p[:room]...)
		} else {
			w.b = append(w.b, p...)
		}
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
