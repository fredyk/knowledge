package embed

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"
)

// limiter lets at most n requests through per window. It is a sliding window,
// not a bucket: that is how the upstream limit is counted.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	recent []time.Time
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window}
}

// limiterFromEnv reads KNOWLEDGE_MAX_REQ_PER_MIN. The limiter is off by default:
// it is enabled only when the variable is greater than zero.
func limiterFromEnv() *limiter {
	n := 0
	if v := os.Getenv("KNOWLEDGE_MAX_REQ_PER_MIN"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			n = parsed
		}
	}
	return newLimiter(n, time.Minute)
}

func (l *limiter) waitTurn(ctx context.Context) {
	if l == nil || l.n <= 0 {
		return
	}
	for {
		l.mu.Lock()
		now := time.Now()
		live := l.recent[:0]
		for _, t := range l.recent {
			if now.Sub(t) < l.window {
				live = append(live, t)
			}
		}
		l.recent = live
		if len(l.recent) < l.n {
			l.recent = append(l.recent, now)
			l.mu.Unlock()
			return
		}
		wait := l.window - now.Sub(l.recent[0])
		l.mu.Unlock()
		if wait <= 0 {
			wait = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
