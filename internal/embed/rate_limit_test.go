package embed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The embedding endpoint states exactly how long to wait:
//
//	429 {"error":"rate limit exceeded for https://example.com (63 req/min)","retry_after_seconds":20}
//
// The client used to throw that away and the retry ran blindly: 1 s, 2 s and
// 4 s. At 7 s it gave up and the chunks of that document were lost, when it was
// enough to wait the 20 s the server asked for.
func TestRateLimit429ReportsRequestedWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limit exceeded for https://example.com (63 req/min)","retry_after_seconds":20}`))
	}))
	defer srv.Close()

	_, _, err := New(srv.URL, "k", "m").EmbedWithUsage(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("a 429 must return an error")
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("a 429 must arrive as RateLimitError, got %T: %v", err, err)
	}
	if rl.RetryAfter != 20*time.Second {
		t.Errorf("the wait requested by the server was not collected: RetryAfter = %v", rl.RetryAfter)
	}
}

// Some proxies send it in the standard header.
func TestRetryAfterHeaderIsHonoured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer srv.Close()

	_, _, err := New(srv.URL, "k", "m").EmbedWithUsage(context.Background(), []string{"hello"})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("got %T: %v", err, err)
	}
	if rl.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, expected 7s", rl.RetryAfter)
	}
}

// A 500 is not a rate limit: it must not pass as one.
func Test500IsNotARateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	_, _, err := New(srv.URL, "k", "m").EmbedWithUsage(context.Background(), []string{"hello"})
	var rl *RateLimitError
	if errors.As(err, &rl) {
		t.Fatalf("a 500 was counted as a rate limit: %v", err)
	}
	if err == nil {
		t.Fatal("a 500 must return an error")
	}
}

// The upstream limit does not send retry_after_seconds: it states in the
// message when it lifts, in UTC.
func TestResetMessageIsParsed(t *testing.T) {
	when := time.Now().UTC().Add(37 * time.Second).Format("2006-01-02 15:04:05")
	body := `{"error":{"message":"Rate limit exceeded for api_key: ***. Limit type: requests. Current limit: 60, Remaining: 0. Limit resets at: ` + when + ` UTC","type":"None","param":"None","code":"429"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	_, _, err := New(srv.URL, "k", "m").EmbedWithUsage(context.Background(), []string{"hello"})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("got %T: %v", err, err)
	}
	if rl.RetryAfter < 30*time.Second || rl.RetryAfter > 40*time.Second {
		t.Errorf("the time at which the limit lifts was not collected: RetryAfter = %v", rl.RetryAfter)
	}
}

// A reset hour already past, or an absurd one, must not stop the pass for half
// an hour: the server clock is not ours.
func TestCrazyResetHourIsCapped(t *testing.T) {
	for _, when := range []string{
		time.Now().UTC().Add(-time.Hour).Format("2006-01-02 15:04:05"),
		time.Now().UTC().Add(6 * time.Hour).Format("2006-01-02 15:04:05"),
	} {
		body := `{"error":{"message":"Rate limit exceeded. Limit resets at: ` + when + ` UTC"}}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(body))
		}))
		_, _, err := New(srv.URL, "k", "m").EmbedWithUsage(context.Background(), []string{"hello"})
		srv.Close()
		var rl *RateLimitError
		if !errors.As(err, &rl) {
			t.Fatalf("got %T: %v", err, err)
		}
		if rl.RetryAfter > maxWait {
			t.Errorf("with reset at %q it would wait %v", when, rl.RetryAfter)
		}
	}
}
