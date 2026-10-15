package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The proxy admits 63 requests per minute and the 4 workers fired blindly:
// the 429 was not an accident, it was the normal state of a large pass, and each
// one cost three 20 s waits before the chunks of that document were lost. The
// client now waits below the limit only when one is explicitly configured.
func TestLimiterDoesNotExceedTheWindow(t *testing.T) {
	lim := newLimiter(2, 100*time.Millisecond)
	start := time.Now()
	for i := 0; i < 5; i++ {
		lim.waitTurn(context.Background())
	}
	// 5 requests at 2 per window: the 3rd waits one window and the 5th another.
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("went past the window: 5 requests at 2 per window in %v", elapsed)
	}
}

func TestNoLimitConfiguredDoesNotSlowDown(t *testing.T) {
	lim := newLimiter(0, time.Minute)
	start := time.Now()
	for i := 0; i < 50; i++ {
		lim.waitTurn(context.Background())
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("throttled without being asked to: %v", elapsed)
	}
}

func TestLimiterHandlesConcurrentWorkers(t *testing.T) {
	var inFlight, peak int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&inFlight, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}],"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "m")
	c.limiter = newLimiter(4, 50*time.Millisecond)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := c.EmbedWithUsage(context.Background(), []string{"hello"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("12 requests at 4 per window ran in %v", elapsed)
	}
}
