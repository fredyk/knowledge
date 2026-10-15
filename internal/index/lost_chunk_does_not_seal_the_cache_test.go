package index

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fredyk/knowledge/internal/config"
	"github.com/fredyk/knowledge/internal/embed"
	"github.com/fredyk/knowledge/internal/store"
)

// The bug this test exists to prevent (2026-09-14): a "knowledge regenerate"
// pass closed with 128 chunks lost to a 429 from the embeddings proxy. The
// document had been sealed in the document store with its REAL mtime and size
// before its chunks reached the index, so the next pass took it for done: cache
// hit, skipped, and a manual half-indexed IN SILENCE forever. It was only
// recovered by touching the file by hand to invalidate the store.
//
// The rule: the seal (mtime, size) that makes a document be skipped is only
// written when ALL its chunks are in.

// embeddingServer serves fake embeddings, and fails with 429 on the calls it is
// told to: failFrom=0 fails all of them, failFrom=1 lets the first through and
// drops the rest.
func embeddingServer(t *testing.T, failFrom int) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if failFrom >= 0 && int(n) > failFrom {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit"}`))
			return
		}
		type datum struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		resp := struct {
			Data  []datum `json:"data"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}{}
		for i := range req.Input {
			resp.Data = append(resp.Data, datum{Embedding: []float32{0.1, 0.2, 0.3, 0.4}, Index: i})
		}
		resp.Usage.TotalTokens = len(req.Input)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testDocument(t *testing.T, dir, name string, chunks int) string {
	t.Helper()
	// Each paragraph is larger than the chunk size to force splitting.
	text := strings.Repeat(strings.Repeat("the cache box is not a delivery certificate. ", 40)+"\n\n", chunks)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func openTestStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func noWaitBetweenRetries(t *testing.T) {
	t.Helper()
	previous := embedRetryBackoff
	embedRetryBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { embedRetryBackoff = previous })
}

func isSealed(t *testing.T, s *store.Store, path string) bool {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, mtime, size, ok, err := s.GetDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return false
	}
	return mtime == info.ModTime().Unix() && size == info.Size()
}

func TestAChunkThatCouldNotBeEmbeddedDoesNotSealTheDocumentInTheCache(t *testing.T) {
	noWaitBetweenRetries(t)
	tmp := t.TempDir()
	doc := testDocument(t, tmp, "manual.md", 3)
	s := openTestStore(t, tmp)
	cfg := &config.Config{ChunkSize: 1200, ChunkOverlap: 200, Workers: 2}

	srv, _ := embeddingServer(t, 0) // all 429
	rep, err := processFiles(context.Background(), s, embed.New(srv.URL, "k", "m"), []string{doc}, false, time.Now(), cfg)
	if err != nil {
		t.Fatalf("processFiles: %v", err)
	}
	if rep.Failed == 0 {
		t.Fatalf("a 429 from the proxy has to be counted: Failed = 0")
	}
	if rep.Incomplete != 1 {
		t.Errorf("the document was left half-done and the summary does not say so: Incomplete = %d, expected 1", rep.Incomplete)
	}
	if rep.Chunks != 0 {
		t.Errorf("not a single chunk got in and the summary says %d new chunks", rep.Chunks)
	}
	if isSealed(t, s, doc) {
		t.Fatalf("the document was sealed in the cache with its chunks lost: the next pass will take it for good")
	}

	// Second pass, now with a healthy proxy: it has to process it again.
	srvOK, _ := embeddingServer(t, -1)
	rep2, err := processFiles(context.Background(), s, embed.New(srvOK.URL, "k", "m"), []string{doc}, false, time.Now(), cfg)
	if err != nil {
		t.Fatalf("processFiles 2: %v", err)
	}
	if rep2.Skipped != 0 {
		t.Errorf("the second pass took a half-done document for good: Skipped = %d", rep2.Skipped)
	}
	if rep2.Processed != 1 {
		t.Errorf("the second pass did not reindex it: Processed = %d", rep2.Processed)
	}
	if rep2.Failed != 0 || rep2.Incomplete != 0 {
		t.Errorf("the second pass was clean and does not look like it: Failed = %d, Incomplete = %d", rep2.Failed, rep2.Incomplete)
	}
	if !isSealed(t, s, doc) {
		t.Errorf("a whole document has to end up sealed")
	}
	chunks, err := s.AllChunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != rep2.Chunks || len(chunks) == 0 {
		t.Errorf("the document's chunks are not all there: %d in the index, %d in the summary", len(chunks), rep2.Chunks)
	}
}

func TestAWholeDocumentIsSealedAndTheNextPassSkipsIt(t *testing.T) {
	noWaitBetweenRetries(t)
	tmp := t.TempDir()
	doc := testDocument(t, tmp, "manual.md", 2)
	s := openTestStore(t, tmp)
	cfg := &config.Config{ChunkSize: 1200, ChunkOverlap: 200, Workers: 2}
	srv, calls := embeddingServer(t, -1)

	rep, err := processFiles(context.Background(), s, embed.New(srv.URL, "k", "m"), []string{doc}, false, time.Now(), cfg)
	if err != nil {
		t.Fatalf("processFiles: %v", err)
	}
	if rep.Processed != 1 || rep.Chunks == 0 {
		t.Fatalf("it was not indexed: Processed = %d, Chunks = %d", rep.Processed, rep.Chunks)
	}
	before := atomic.LoadInt64(calls)

	rep2, err := processFiles(context.Background(), s, embed.New(srv.URL, "k", "m"), []string{doc}, false, time.Now(), cfg)
	if err != nil {
		t.Fatalf("processFiles 2: %v", err)
	}
	if rep2.Skipped != 1 || rep2.Processed != 0 {
		t.Errorf("the cache stopped working: Skipped = %d, Processed = %d", rep2.Skipped, rep2.Processed)
	}
	if n := atomic.LoadInt64(calls); n != before {
		t.Errorf("a document that was already whole was re-embedded: %d extra calls", n-before)
	}
}

func TestAHalfDoneDocumentIsNotSealedEither(t *testing.T) {
	noWaitBetweenRetries(t)
	t.Setenv("KNOWLEDGE_API_BATCH_SIZE", "1")
	tmp := t.TempDir()
	doc := testDocument(t, tmp, "manual.md", 4)
	s := openTestStore(t, tmp)
	// A single worker so the first call is the one that gets in, not a race.
	cfg := &config.Config{ChunkSize: 1200, ChunkOverlap: 200, Workers: 1}
	srv, _ := embeddingServer(t, 1) // the first passes, the rest 429

	rep, err := processFiles(context.Background(), s, embed.New(srv.URL, "k", "m"), []string{doc}, false, time.Now(), cfg)
	if err != nil {
		t.Fatalf("processFiles: %v", err)
	}
	if rep.Failed == 0 {
		t.Fatalf("no chunk failed: the test server is not dropping anything")
	}
	if isSealed(t, s, doc) {
		t.Errorf("a document with part of its chunks in and part lost must NOT end up sealed")
	}
	if rep.Incomplete != 1 {
		t.Errorf("Incomplete = %d, expected 1", rep.Incomplete)
	}
}

// 761 of the 7,732 documents of the real index were like this on 2026-09-15:
// sealed with their mtime and size, counted in "docs", and without a single
// chunk inside. The previous version, which sealed before embedding, left them
// there. A seal without chunks is not an indexed document: it is a hole that
// skips itself forever, and searching it returns empty without saying something
// is missing.
func TestADocumentSealedWithoutASingleChunkIsNotSkipped(t *testing.T) {
	noWaitBetweenRetries(t)
	tmp := t.TempDir()
	doc := testDocument(t, tmp, "lsc-modificaciones.md", 2)
	s := openTestStore(t, tmp)
	cfg := &config.Config{ChunkSize: 1200, ChunkOverlap: 200, Workers: 2}

	// The damage from the previous version, exactly: sealed document, zero chunks.
	info, err := os.Stat(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDocument(doc, info.ModTime(), info.Size()); err != nil {
		t.Fatal(err)
	}

	srv, _ := embeddingServer(t, -1)
	rep, err := processFiles(context.Background(), s, embed.New(srv.URL, "k", "m"), []string{doc}, false, time.Now(), cfg)
	if err != nil {
		t.Fatalf("processFiles: %v", err)
	}
	if rep.Skipped != 0 {
		t.Errorf("a seal without chunks was taken for good: Skipped = %d", rep.Skipped)
	}
	if rep.Processed != 1 || rep.Chunks == 0 {
		t.Fatalf("it was not reindexed: Processed = %d, Chunks = %d", rep.Processed, rep.Chunks)
	}
	chunks, err := s.AllChunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != rep.Chunks {
		t.Errorf("the chunks do not add up: %d in the index, %d in the summary", len(chunks), rep.Chunks)
	}
}

// The proxy answers "429 ... retry_after_seconds: 20" and the one that retried
// did so blindly at 1 s, 2 s and 4 s: at 7 s it gave up and lost the chunks of
// that document. Now the wait the server asks for has the final say.
func TestTheProxyRequestedWaitIsHonoredAndNoChunkIsLost(t *testing.T) {
	noWaitBetweenRetries(t) // the blind wait is left at 1 ms: the one measured is the server's
	const requestedWait = 300 * time.Millisecond

	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if atomic.AddInt64(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded (63 req/min)","retry_after_seconds":0.3}`))
			return
		}
		type datum struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		resp := struct {
			Data  []datum `json:"data"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}{}
		for i := range req.Input {
			resp.Data = append(resp.Data, datum{Embedding: []float32{0.1, 0.2, 0.3, 0.4}, Index: i})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	doc := testDocument(t, tmp, "manual.md", 1)
	s := openTestStore(t, tmp)
	cfg := &config.Config{ChunkSize: 1200, ChunkOverlap: 200, Workers: 1}

	start := time.Now()
	rep, err := processFiles(context.Background(), s, embed.New(srv.URL, "k", "m"), []string{doc}, false, time.Now(), cfg)
	if err != nil {
		t.Fatalf("processFiles: %v", err)
	}
	elapsed := time.Since(start)

	if rep.Failed != 0 || rep.Incomplete != 0 {
		t.Errorf("a 429 with a timely retry cannot lose anything: Failed = %d, Incomplete = %d", rep.Failed, rep.Incomplete)
	}
	if elapsed < requestedWait {
		t.Errorf("it retried before what the server asked for: %v of %v", elapsed, requestedWait)
	}
	if !isSealed(t, s, doc) {
		t.Errorf("the document came in whole and was not sealed")
	}
}

// The seal of a document that already came in whole cannot wait for the whole
// pass to finish: a pass of 6,000 documents that is cut off for whatever reason
// would throw away hours of already-paid and already-written embeddings. It is
// sealed as soon as its chunks are in, with the pass still running.
func TestADocumentIsSealedAsSoonAsItIsComplete(t *testing.T) {
	noWaitBetweenRetries(t)
	tmp := t.TempDir()
	first := testDocument(t, tmp, "1-first.md", 1)
	second := testDocument(t, tmp, "2-second.md", 1)
	s := openTestStore(t, tmp)
	cfg := &config.Config{ChunkSize: 1200, ChunkOverlap: 200, Workers: 1}

	firstSealed := make(chan bool, 1)
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// On the second call the first document is already whole: it has to be
		// sealed BEFORE the pass ends.
		if atomic.AddInt64(&calls, 1) == 2 {
			seen := false
			for i := 0; i < 100 && !seen; i++ {
				seen = isSealed(t, s, first)
				if !seen {
					time.Sleep(20 * time.Millisecond)
				}
			}
			firstSealed <- seen
		}
		type datum struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		resp := struct {
			Data  []datum `json:"data"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}{}
		for i := range req.Input {
			resp.Data = append(resp.Data, datum{Embedding: []float32{0.1, 0.2, 0.3, 0.4}, Index: i})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	if _, err := processFiles(context.Background(), s, embed.New(srv.URL, "k", "m"), []string{first, second}, false, time.Now(), cfg); err != nil {
		t.Fatalf("processFiles: %v", err)
	}
	select {
	case seen := <-firstSealed:
		if !seen {
			t.Error("the first document was whole inside and was still unsealed with the pass running")
		}
	default:
		t.Fatal("the second call to the embedder never happened")
	}
	if !isSealed(t, s, second) {
		t.Error("the second document was not sealed")
	}
}
