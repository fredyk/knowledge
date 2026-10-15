package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNoRateLimitWithoutEnvVar(t *testing.T) {
	t.Setenv("KNOWLEDGE_MAX_REQ_PER_MIN", "")
	c := New("https://example.com/v1", "", "bge-m3")
	if c.limiter != nil && c.limiter.n > 0 {
		t.Fatalf("without KNOWLEDGE_MAX_REQ_PER_MIN nothing must be throttled, got %d/min", c.limiter.n)
	}
}

func TestRateLimitEnabledByEnvVar(t *testing.T) {
	t.Setenv("KNOWLEDGE_MAX_REQ_PER_MIN", "10")
	c := New("https://example.com/v1", "", "bge-m3")
	if c.limiter == nil || c.limiter.n != 10 {
		t.Fatalf("KNOWLEDGE_MAX_REQ_PER_MIN=10 must throttle to 10/min, got %+v", c.limiter)
	}
}

func TestNoAuthorizationHeaderWithoutKey(t *testing.T) {
	var gotAuth string
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}],"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	if _, _, err := New(srv.URL, "", "m").EmbedWithUsage(context.Background(), []string{"hello"}); err != nil {
		t.Fatal(err)
	}
	if hadAuth || gotAuth != "" {
		t.Fatalf("no Authorization header may be sent without a key, got %q", gotAuth)
	}
}
