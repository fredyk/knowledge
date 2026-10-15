package index

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/fredyk/knowledge/internal/config"
)

// The bug this test exists to prevent (2026-09-15): "knowledge search" loaded
// ALL the chunks of the index -text and embedding- into memory to compare the
// query. With 697,299 chunks that is about 11 GB: a search took minutes and ate
// the machine's RAM.
//
// The rule: a search walks the index without holding it in memory. Only the
// text and path of the few chunks that end up in the result are read.

// The test server always answers the query with [0.1, 0.2, 0.3, 0.4].
func proximityVector(i int) []float32 {
	// The larger i is, the farther it moves away from the query.
	d := float32(i) / 100
	return []float32{0.1, 0.2 + d, 0.3, 0.4 - d/2}
}

func TestSearchDoesNotLoadAllIndexText(t *testing.T) {
	tmp := t.TempDir()
	s := openTestStore(t, tmp)
	const chunks = 3000
	text := strings.Repeat("x", 40_000) // 3000 x 40 KB = 120 MB of text
	for d := 0; d < chunks/10; d++ {
		id, err := s.UpsertDocumentPending(filepath.Join(tmp, fmt.Sprintf("doc-%04d.md", d)))
		if err != nil {
			t.Fatal(err)
		}
		for o := 0; o < 10; o++ {
			if err := s.InsertChunk(id, o, text, proximityVector(d*10+o)); err != nil {
				t.Fatal(err)
			}
		}
	}

	srv, _ := embeddingServer(t, -1)
	cfg := &config.Config{APIBase: srv.URL, APIKey: "k", Model: "m", DBPath: filepath.Join(tmp, "index.db")}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	hits, err := Search(context.Background(), cfg, "query", 8)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 8 {
		t.Fatalf("expected 8 results and got %d", len(hits))
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("allocated during the search: %d MB", allocated>>20)
	if allocated > 40<<20 {
		t.Fatalf("a search allocated %d MB with 120 MB of text in the index: it is loading the whole index", allocated>>20)
	}
}

func TestSearchReturnsClosestInOrderWithoutTwins(t *testing.T) {
	tmp := t.TempDir()
	s := openTestStore(t, tmp)

	// Two twins (lsc.txt and lsc-consolidado.pdf) with the closest chunks:
	// only one per ord can come out.
	lsc, _ := s.UpsertDocumentPending(filepath.Join(tmp, "lsc.txt"))
	lscCons, _ := s.UpsertDocumentPending(filepath.Join(tmp, "lsc-consolidado.pdf"))
	other, _ := s.UpsertDocumentPending(filepath.Join(tmp, "other.md"))
	for o := 0; o < 5; o++ {
		mustInsert(t, s.InsertChunk(lsc, o, fmt.Sprintf("lsc %d", o), proximityVector(o)))
		mustInsert(t, s.InsertChunk(lscCons, o, fmt.Sprintf("lsc-cons %d", o), proximityVector(o)))
	}
	for o := 0; o < 50; o++ {
		mustInsert(t, s.InsertChunk(other, o, fmt.Sprintf("other %d", o), proximityVector(5+o)))
	}

	srv, _ := embeddingServer(t, -1)
	cfg := &config.Config{APIBase: srv.URL, APIKey: "k", Model: "m", DBPath: filepath.Join(tmp, "index.db")}
	hits, err := Search(context.Background(), cfg, "query", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 8 {
		t.Fatalf("expected 8 results and got %d", len(hits))
	}
	if !sort.SliceIsSorted(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score }) {
		t.Errorf("results do not come out sorted by score")
	}
	seen := map[int]bool{}
	for i, h := range hits {
		if strings.HasPrefix(filepath.Base(h.Path), "lsc") {
			if seen[h.Ord] {
				t.Errorf("ord %d of the LSC comes out twice: twins were not deduplicated", h.Ord)
			}
			seen[h.Ord] = true
		}
		if h.Text == "" || h.Snippet == "" {
			t.Errorf("result %d has no text", i)
		}
	}
	if len(seen) != 5 {
		t.Errorf("the 5 ords of the LSC, which are the closest, should come out; %d come out", len(seen))
	}
	if hits[5].Text != "other 0" || hits[7].Text != "other 2" {
		t.Errorf("after the LSC should come other 0..2; %q and %q come out", hits[5].Text, hits[7].Text)
	}
}

func mustInsert(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
