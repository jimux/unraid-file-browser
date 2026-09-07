package api

import (
	"context"
	"net/http"
	"time"

	"unraid-filebrowser/internal/types"
)

// limiter bounds how many expensive requests run at once. It is a plain
// semaphore with a bounded wait: a request that cannot get a slot within wait
// is told the server is busy rather than being queued behind an unbounded
// backlog (which is how a box with 50k-entry shares falls over).
type limiter struct {
	sem  chan struct{}
	wait time.Duration
}

func newLimiter(n int, wait time.Duration) *limiter {
	if n < 1 {
		n = 1
	}
	return &limiter{sem: make(chan struct{}, n), wait: wait}
}

// acquire takes a slot, returning the release func. The error is already an
// API error: TIMEOUT/504 with an honest message — the request never ran, so
// INDEXING (which means "the index is busy") would be a lie.
func (l *limiter) acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	release := func() { <-l.sem }
	select {
	case l.sem <- struct{}{}:
		return release, nil
	default:
	}

	t := time.NewTimer(l.wait)
	defer t.Stop()
	select {
	case l.sem <- struct{}{}:
		return release, nil
	case <-t.C:
		return nil, types.Errf(types.ErrTimeout, "server busy")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// limited runs h while holding a slot on the expensive-request semaphore.
func (s *server) limited(h handler) handler {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		release, err := s.busy.acquire(r.Context())
		if err != nil {
			return nil, err
		}
		defer release()
		return h(w, r)
	}
}
