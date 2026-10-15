package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fredyk/knowledge/internal/config"
	"github.com/fredyk/knowledge/internal/embed"
)

// Options drives a single fetch invocation.
type Options struct {
	Since         time.Time // first date to consider (inclusive)
	Until         time.Time // last date to consider (inclusive); defaults to today
	Threshold     float32   // cosine threshold for the fine filter; default 0.55
	DryRun        bool      // do not download or save state; print matches
	BOEOnly       bool
	DOGOnly       bool
	KnowledgeRoot string // where to write the new files; default ~/knowledge
}

// Report is what the CLI prints after a run.
type Report struct {
	BOENew     int
	BOESkipped int
	DOGNew     int
	DOGSkipped int
	Elapsed    time.Duration
}

// queryVectorsCache memoises the embeddings of the relevance queries between
// invocations within the same process.
type queryVectorsCache struct {
	vecs [][]float32
}

// Run executes the full fetch pipeline: walks every date in [Since, Until]
// (inclusive), fetches the BOE/DOG sumario for each, applies coarse filters,
// then ranks the survivors against the relevance queries via cosine, then
// downloads PDFs + extracts TXT for the items above threshold. State is
// persisted between runs.
func Run(ctx context.Context, cfg *config.Config, opts Options) (Report, error) {
	start := time.Now()
	rep := Report{}

	if opts.Threshold == 0 {
		opts.Threshold = 0.55
	}
	if opts.Until.IsZero() {
		opts.Until = time.Now()
	}
	if opts.KnowledgeRoot == "" {
		home, _ := os.UserHomeDir()
		opts.KnowledgeRoot = filepath.Join(home, "knowledge")
	}

	// Embedding queries once. We re-create the client because cfg is the same
	// as used by index/search.
	embClient := embed.New(cfg.APIBase, cfg.APIKey, cfg.Model)
	queryVecs, err := embedQueries(ctx, embClient)
	if err != nil {
		return rep, fmt.Errorf("embed queries: %w", err)
	}

	httpc := &http.Client{Timeout: 60 * time.Second}

	// Persisted state — used to advance the "since" floor.
	state, err := LoadState()
	if err != nil {
		return rep, err
	}
	effectiveSince := opts.Since
	if effectiveSince.IsZero() {
		// fall back to state, then to today-7d
		if state.LastBOEDate != "" {
			if d, err := time.Parse("2006-01-02", state.LastBOEDate); err == nil {
				effectiveSince = d.AddDate(0, 0, 1)
			}
		}
		if effectiveSince.IsZero() {
			effectiveSince = time.Now().AddDate(0, 0, -7)
		}
	}

	slog.Info("fetch start",
		"since", effectiveSince.Format("2006-01-02"),
		"until", opts.Until.Format("2006-01-02"),
		"threshold", opts.Threshold,
		"dry_run", opts.DryRun,
	)

	lastBOEISO := state.LastBOEDate
	lastDOGISO := state.LastDOGDate

	for d := effectiveSince; !d.After(opts.Until); d = d.AddDate(0, 0, 1) {
		isoDate := d.Format("2006-01-02")
		yyyymmdd := d.Format("20060102")

		if !opts.DOGOnly {
			items, err := FetchBOESumario(ctx, httpc, yyyymmdd)
			if err != nil {
				slog.Warn("boe sumario error", "date", isoDate, "err", err)
			} else {
				kept, skipped, err := processItems(ctx, embClient, queryVecs, items, opts, "BOE")
				if err != nil {
					return rep, err
				}
				rep.BOENew += kept
				rep.BOESkipped += skipped
				lastBOEISO = isoDate
			}
		}

		if !opts.BOEOnly {
			year := d.Format("2006")
			items, err := FetchDOGSumario(ctx, httpc, year, yyyymmdd)
			if err != nil {
				slog.Warn("dog sumario error", "date", isoDate, "err", err)
			} else {
				kept, skipped, err := processItems(ctx, embClient, queryVecs, items, opts, "DOG")
				if err != nil {
					return rep, err
				}
				rep.DOGNew += kept
				rep.DOGSkipped += skipped
				lastDOGISO = isoDate
			}
		}
	}

	rep.Elapsed = time.Since(start)

	if !opts.DryRun {
		state.LastBOEDate = lastBOEISO
		state.LastDOGDate = lastDOGISO
		state.Stats.TotalFetched += rep.BOENew + rep.DOGNew
		state.Stats.TotalSkippedIrrelevant += rep.BOESkipped + rep.DOGSkipped
		state.Stats.LastRun = time.Now().UTC()
		if err := SaveState(state); err != nil {
			return rep, fmt.Errorf("save state: %w", err)
		}
	}

	return rep, nil
}

// processItems applies coarse + fine (cosine) filtering. Returns (kept, skipped).
func processItems(ctx context.Context, ec *embed.Client, queryVecs [][]float32, items []Item, opts Options, source string) (int, int, error) {
	if len(items) == 0 {
		return 0, 0, nil
	}
	// Coarse pass.
	var candidates []Item
	skipped := 0
	for _, it := range items {
		axis := CoarseFilter(it)
		if axis == "" {
			skipped++
			continue
		}
		it.MatchedAxis = axis
		candidates = append(candidates, it)
	}
	if len(candidates) == 0 {
		return 0, skipped, nil
	}

	// Embed candidate titles in batches of 32.
	titles := make([]string, len(candidates))
	for i, it := range candidates {
		titles[i] = it.Title
	}
	vecs, err := embedBatched(ctx, ec, titles, 32)
	if err != nil {
		return 0, skipped, fmt.Errorf("embed titles: %w", err)
	}

	// Cosine vs queries.
	kept := 0
	for i := range candidates {
		score, _ := MaxCosine(vecs[i], queryVecs)
		candidates[i].Score = score
		if score < opts.Threshold {
			skipped++
			continue
		}
		if opts.DryRun {
			fmt.Printf("[%s] %s %.3f %s · %s | %s\n",
				source, candidates[i].MatchedAxis, score, candidates[i].Date,
				truncate(candidates[i].Department, 50), truncate(candidates[i].Title, 120))
			kept++
			continue
		}
		downloaded, err := downloadItem(ctx, candidates[i], opts.KnowledgeRoot)
		if err != nil {
			slog.Warn("download failed", "id", candidates[i].ID, "err", err)
			continue
		}
		if downloaded {
			kept++
		}
	}
	return kept, skipped, nil
}

func embedQueries(ctx context.Context, ec *embed.Client) ([][]float32, error) {
	queries, err := LoadRelevanceQueries(RelevanceQueriesPath())
	if err != nil {
		return nil, err
	}
	return embedBatched(ctx, ec, queries, 32)
}

func embedBatched(ctx context.Context, ec *embed.Client, inputs []string, batch int) ([][]float32, error) {
	out := make([][]float32, 0, len(inputs))
	for i := 0; i < len(inputs); i += batch {
		end := i + batch
		if end > len(inputs) {
			end = len(inputs)
		}
		// Trim very long inputs to keep upstream happy. The embedding model
		// truncates anyway but the gateway has been observed to return 520
		// on large bursts. ~2000 chars is plenty for a BOE/DOG title.
		batchInputs := make([]string, end-i)
		for k, s := range inputs[i:end] {
			if len(s) > 2000 {
				s = s[:2000]
			}
			batchInputs[k] = s
		}

		var (
			vecs    [][]float32
			lastErr error
		)
		for attempt := 0; attempt < 4; attempt++ {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			v, _, err := ec.EmbedWithUsage(ctx, batchInputs)
			if err == nil {
				vecs = v
				lastErr = nil
				break
			}
			lastErr = err
			delay := time.Second * time.Duration(1<<attempt) // 1s, 2s, 4s, 8s
			slog.Warn("fetch embed retry", "attempt", attempt+1, "delay", delay, "err", err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		if lastErr != nil {
			return nil, lastErr
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// downloadItem fetches the PDF and writes meta+txt into the corpus tree.
// Returns (downloaded=true, nil) if a new file was written, (false, nil) if
// the file was already on disk (idempotent re-run), or (false, err) on error.
func downloadItem(ctx context.Context, it Item, root string) (bool, error) {
	year, month, day := splitISODate(it.Date)
	var dir string
	switch it.Source {
	case "BOE":
		dir = filepath.Join(root, "00-boe-diario", year, month, day)
	case "DOG":
		dir = filepath.Join(root, "00-dog-diario", year, month, day)
	default:
		return false, fmt.Errorf("unknown source: %s", it.Source)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}

	pdfPath := filepath.Join(dir, sanitizeID(it.ID)+".pdf")
	txtPath := filepath.Join(dir, sanitizeID(it.ID)+".txt")

	if _, err := os.Stat(pdfPath); err == nil {
		// Already on disk: skip (idempotent re-runs). Do NOT re-append meta
		// to avoid duplicate lines across re-runs.
		return false, nil
	}

	if it.URLPDF == "" {
		return false, errors.New("item has no pdf url")
	}
	if err := downloadFile(ctx, it.URLPDF, pdfPath); err != nil {
		return false, err
	}
	if err := runPdftotext(pdfPath, txtPath); err != nil {
		slog.Warn("pdftotext failed", "pdf", pdfPath, "err", err)
		// not fatal — regenerate falls back to .pdf direct ingest
	}
	if err := appendMeta(dir, it); err != nil {
		return true, err
	}
	return true, nil
}

func appendMeta(dir string, it Item) error {
	metaPath := filepath.Join(dir, "meta.jsonl")
	f, err := os.OpenFile(metaPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(struct {
		ID         string    `json:"id"`
		Date       string    `json:"date"`
		Source     string    `json:"source"`
		Section    string    `json:"section"`
		Department string    `json:"department"`
		Title      string    `json:"title"`
		Score      float32   `json:"score"`
		Axis       string    `json:"axis"`
		URLPDF     string    `json:"url_pdf"`
		URLHTML    string    `json:"url_html"`
		FetchedAt  time.Time `json:"fetched_at"`
	}{
		ID: it.ID, Date: it.Date, Source: it.Source,
		Section: it.SectionName, Department: it.Department,
		Title: it.Title, Score: it.Score, Axis: it.MatchedAxis,
		URLPDF: it.URLPDF, URLHTML: it.URLHTML, FetchedAt: time.Now().UTC(),
	})
}

func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "knowledge-cli/0.1")
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("download http %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func runPdftotext(pdf, txt string) error {
	cmd := exec.Command("pdftotext", "-layout", pdf, txt)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func splitISODate(iso string) (string, string, string) {
	// YYYY-MM-DD
	if len(iso) != 10 {
		return "", "", ""
	}
	return iso[0:4], iso[5:7], iso[8:10]
}

func sanitizeID(id string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, id)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
