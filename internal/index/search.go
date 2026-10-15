package index

import (
	"container/heap"
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fredyk/knowledge/internal/config"
	"github.com/fredyk/knowledge/internal/embed"
	"github.com/fredyk/knowledge/internal/store"
)

// SearchHit represents a single search result.
type SearchHit struct {
	Score   float32 `json:"score"`
	Path    string  `json:"path"`
	Ord     int     `json:"ord"`
	Snippet string  `json:"snippet"`
	Text    string  `json:"text"`
}

// cosine computes the cosine similarity between two vectors a and b.
// It normalizes both vectors before computing the dot product.
func cosine(a, b []float32) float32 {
	var dot, normA, normB float64
	n := len(a)
	if n != len(b) {
		return 0
	}
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}

// pathKey returns a dedup key for a path: the directory + basename without extension.
// e.g. "/path/lsc-consolidado.pdf" -> "/path|lsc-consolidado"
//
//	"/path/lsc.txt" -> "/path|lsc"
func pathKey(p string) string {
	base := filepath.Base(p)
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	return filepath.Dir(p) + "|" + base
}

// areDedupTwin checks whether two hits should be considered duplicates.
// They are if they share the same directory, same chunk ordinal, and their
// basename-without-extension are "similar" — meaning one is a prefix of the
// other separated by a hyphen, or they are identical.
// This catches: lsc.txt vs lsc-consolidado.pdf, lcsp.txt vs lcsp-consolidado.pdf, etc.
func areDedupTwin(pathA, pathB string, ordA, ordB int) bool {
	if ordA != ordB {
		return false
	}
	dirA := filepath.Dir(pathA)
	dirB := filepath.Dir(pathB)
	if dirA != dirB {
		return false
	}
	baseA := pathKey(pathA)
	baseB := pathKey(pathB)
	// Extract just the basename part after "|"
	sepA := strings.LastIndex(baseA, "|")
	sepB := strings.LastIndex(baseB, "|")
	nameA := baseA[sepA+1:]
	nameB := baseB[sepB+1:]
	if nameA == nameB {
		return true
	}
	// Check if one name is a prefix of the other separated by a hyphen
	// e.g. "lsc" vs "lsc-consolidado"
	if strings.HasPrefix(nameA, nameB+"-") || strings.HasPrefix(nameB, nameA+"-") {
		return true
	}
	// Check if removing "-consolidado", "-cons", "-merged" suffixes makes them equal
	suffixes := []string{"-consolidado", "-cons", "-merged", "-final", "-v2", "-v1"}
	for _, sfx := range suffixes {
		cleanA := strings.TrimSuffix(nameA, sfx)
		cleanB := strings.TrimSuffix(nameB, sfx)
		if cleanA == cleanB && cleanA != nameA {
			return true
		}
	}
	return false
}

// snippet returns a snippet of text around the first sentence or line break, trimmed.
func snippet(text string) string {
	text = strings.TrimSpace(text)
	if len(text) == 0 {
		return ""
	}

	// Find a good truncation point: prefer line break or period followed by space
	runes := []rune(text)
	limit := 200
	if len(runes) <= limit {
		s := string(runes)
		return s
	}

	truncated := string(runes[:limit])

	// Try to break at a line boundary first
	if idx := strings.Index(truncated, "\n"); idx > 100 {
		truncated = truncated[:idx]
	} else if idx := strings.Index(truncated[100:], ". "); idx > 0 {
		truncated = truncated[:100+idx+2]
	}

	// Trim trailing whitespace
	truncated = strings.TrimSpace(truncated)

	// Add ellipsis if truncated
	if len(runes) > 200 {
		truncated += "..."
	}
	return truncated
}

type scoredHit struct {
	hit SearchHit
	s   float64
}

type candidate struct {
	id    int64
	score float32
}

// minHeap is a min-heap by score: the top is the worst of those kept, the
// first to go when a better one arrives.
type minHeap []candidate

func (m minHeap) Len() int           { return len(m) }
func (m minHeap) Less(i, j int) bool { return m[i].score < m[j].score }
func (m minHeap) Swap(i, j int)      { m[i], m[j] = m[j], m[i] }
func (m *minHeap) Push(x any)        { *m = append(*m, x.(candidate)) }
func (m *minHeap) Pop() any {
	old := *m
	x := old[len(old)-1]
	*m = old[:len(old)-1]
	return x
}

// bestChunks returns the n chunks closest to the query, from best to worst, and
// how many embedded chunks were scored in total.
func bestChunks(s *store.Store, queryVec []float32, n int) ([]candidate, int, error) {
	m := make(minHeap, 0, n)
	total := 0
	err := s.EachEmbedding(func(id int64, emb []float32) {
		if len(emb) == 0 {
			return
		}
		total++
		sc := cosine(queryVec, emb)
		if len(m) < n {
			heap.Push(&m, candidate{id, sc})
		} else if sc > m[0].score {
			m[0] = candidate{id, sc}
			heap.Fix(&m, 0)
		}
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(m, func(i, j int) bool { return m[i].score > m[j].score })
	return m, total, nil
}

// dedupe reads the text of the candidates and keeps the first k that are not
// twins of one already chosen (foo.txt vs foo-consolidado.pdf).
func dedupe(s *store.Store, best []candidate, k int) ([]scoredHit, error) {
	ids := make([]int64, len(best))
	for i, c := range best {
		ids[i] = c.id
	}
	rows, err := s.ChunksByID(ids)
	if err != nil {
		return nil, fmt.Errorf("search load chunks: %w", err)
	}
	deduped := make([]scoredHit, 0, k)
	for _, c := range best {
		row, ok := rows[c.id]
		if !ok {
			continue
		}
		keep := true
		for _, d := range deduped {
			if areDedupTwin(row.Path, d.hit.Path, row.Ord, d.hit.Ord) {
				keep = false
				break
			}
		}
		if !keep {
			continue
		}
		deduped = append(deduped, scoredHit{
			hit: SearchHit{Score: c.score, Path: row.Path, Ord: row.Ord, Snippet: snippet(row.Text), Text: row.Text},
			s:   float64(c.score),
		})
		if len(deduped) >= k {
			break
		}
	}
	return deduped, nil
}

// Search embeds the query, scans the index without holding it in memory,
// and returns the top-K most similar chunks.
func Search(ctx context.Context, cfg *config.Config, query string, k int) ([]SearchHit, error) {
	s, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("search open store: %w", err)
	}
	defer s.Close()

	e := embed.New(cfg.APIBase, cfg.APIKey, cfg.Model)

	// Embed the query with 3 retries and exponential backoff (1s, 2s, 4s).
	// Mirrors the retry pattern in Regenerate's worker pool (see index.go).
	var queryVec []float32
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		vectors, err := e.Embed(ctx, []string{query})
		if err != nil {
			lastErr = err
			delay := time.Second * time.Duration(1<<attempt)
			slog.Warn("search embed retry", "attempt", attempt+1, "delay", delay, "err", err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
				continue
			}
		}
		if len(vectors) == 0 || len(vectors[0]) == 0 {
			return nil, fmt.Errorf("search: empty query embedding")
		}
		queryVec = vectors[0]
		lastErr = nil
		break
	}
	if lastErr != nil {
		return nil, fmt.Errorf("search: embed query failed after 3 retries: %w", lastErr)
	}

	if k <= 0 {
		k = 8
	}

	// The index is scanned without holding it in memory: for each chunk only its
	// id and score are kept if it makes it among the best. The text is read at
	// the end, and only for those. A margin over k is requested because dedup
	// discards twins; if it still falls short, it repeats with a larger margin.
	var hits []scoredHit
	for n := 4 * k; ; n *= 4 {
		best, total, err := bestChunks(s, queryVec, n)
		if err != nil {
			return nil, fmt.Errorf("search load chunks: %w", err)
		}
		hits, err = dedupe(s, best, k)
		if err != nil {
			return nil, err
		}
		if len(hits) >= k || total <= n {
			break
		}
	}

	// Extract just SearchHit
	results := make([]SearchHit, len(hits))
	for i, h := range hits {
		results[i] = h.hit
	}
	return results, nil
}
