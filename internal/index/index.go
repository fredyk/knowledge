package index

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/fredyk/knowledge/internal/chunk"
	"github.com/fredyk/knowledge/internal/config"
	"github.com/fredyk/knowledge/internal/embed"
	"github.com/fredyk/knowledge/internal/store"
	"github.com/xuri/excelize/v2"
)

// defaultAPIBatchSize is the maximum number of chunks sent to /embeddings in a
// single HTTP call. The upstream endpoint rejects batches larger than 32 with
// HTTP 422. Override via KNOWLEDGE_API_BATCH_SIZE.
const defaultAPIBatchSize = 32

func apiBatchSize() int {
	if v := os.Getenv("KNOWLEDGE_API_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultAPIBatchSize
}

// maxFileSize is the default maximum file size to index (10 MB).
// Override via KNOWLEDGE_MAX_FILE_SIZE (in bytes).
const defaultMaxFileSize = 10 * 1024 * 1024

func maxFileSize() int64 {
	if v := os.Getenv("KNOWLEDGE_MAX_FILE_SIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxFileSize
}

// excludedDirs are directory basenames that are always skipped during walk.
var excludedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"__pycache__":  true,
	"venv":         true,
	".venv":        true,
	"env":          true,
	".cache":       true,
	"target":       true,
	"dist":         true,
	"build":        true,
	".next":        true,
	".nuxt":        true,
}

// excludedBinaryExts are file extensions that are always skipped (binary).
var excludedBinaryExts = map[string]bool{
	".so": true, ".dll": true, ".exe": true, ".bin": true, ".dat": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".bmp": true, ".ico": true, ".webp": true, ".svg": true, ".tiff": true,
	".mp3": true, ".mp4": true, ".avi": true, ".mov": true, ".wav": true, ".flac": true, ".ogg": true, ".wma": true, ".mkv": true, ".webm": true,
	".zip": true, ".tar": true, ".gz": true, ".rar": true, ".7z": true, ".bz2": true, ".xz": true,
	".pyc": true, ".pyo": true, ".class": true,
}

type Report struct {
	Docs      int // total docs in the walk
	Skipped   int // docs not modified (cache hit)
	Processed int // docs actually processed (new + modified)
	Chunks    int // new chunks created in this run
	Tokens    int
	Failed    int
	// Unreadable are documents that could not even be READ: a .pdf that is
	// actually HTML, a corrupt file, a denied permission. They used to be
	// skipped with a slog.Warn and counted nowhere, so the summary said
	// "0 failed" while the document disappeared from the index.
	Unreadable int
	// Incomplete are documents left HALF-DONE in this pass: some of their
	// chunks did not reach the index. They are NOT sealed in the document
	// store, so the next pass tries them again in full.
	Incomplete int
	Orphans    int // docs removed because the file no longer exists on disk
	Elapsed    time.Duration
}

// supportedExts maps file extensions that we attempt to read as text.
var supportedExts = map[string]bool{
	".txt":  true,
	".md":   true,
	".pdf":  true,
	".html": true,
	".htm":  true,
	".docx": true,
	".xlsx": true,
	".csv":  true,
	".tsv":  true,
	".json": true,
	".yaml": true,
	".yml":  true,
	".eml":  true,
}

var pdftotextAvailable atomic.Bool
var pandocAvailable atomic.Bool
var docxAvailable atomic.Bool

// embedJob is sent to workers for embedding.
type embedJob struct {
	docID  int64
	ords   []int
	chunks []string
}

// embedResult holds the vectors returned from the API.
type embedResult struct {
	vectors [][]float32
	docID   int64
	ords    []int
	texts   []string
	tokens  int
}

// walkAndCollect walks a single root and collects eligible file paths,
// applying exclusion filters and size limits.
func walkAndCollect(root string) ([]string, error) {
	var files []string
	maxSize := maxFileSize()

	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip excluded directories
			if excludedDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		// Check file size
		if info.Size() > maxSize {
			slog.Debug("skipping file too large", "path", path, "size", info.Size())
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))

		// Check if extension is excluded (binary)
		if excludedBinaryExts[ext] {
			return nil
		}

		// For files with no recognized extension, try UTF-8 validation later
		if ext != "" && !supportedExts[ext] {
			return nil
		}

		// PDF requires pdftotext
		if ext == ".pdf" && !pdftotextAvailable.Load() {
			return nil
		}

		files = append(files, path)
		return nil
	}); err != nil {
		return nil, err
	}

	// Filter out PDFs that have a TXT twin with the same basename.
	if len(files) > 0 {
		dirTxtBasenames := make(map[string]map[string]bool)
		for _, fpath := range files {
			ext := strings.ToLower(filepath.Ext(fpath))
			if ext != ".txt" {
				continue
			}
			dir := filepath.Dir(fpath)
			base := filepath.Base(fpath)
			if dirTxtBasenames[dir] == nil {
				dirTxtBasenames[dir] = make(map[string]bool)
			}
			dirTxtBasenames[dir][base] = true
		}

		filtered := make([]string, 0, len(files))
		for _, fpath := range files {
			ext := strings.ToLower(filepath.Ext(fpath))
			if ext == ".pdf" {
				dir := filepath.Dir(fpath)
				base := filepath.Base(fpath)
				nameNoExt := strings.TrimSuffix(base, ".pdf")
				txtName := nameNoExt + ".txt"
				if dirTxtBasenames[dir] != nil && dirTxtBasenames[dir][txtName] {
					slog.Info("skipping pdf duplicate (txt twin exists)", "path", fpath)
					continue
				}
			}
			filtered = append(filtered, fpath)
		}
		files = filtered
	}

	return files, nil
}

// embedRetryBackoff is the wait between embedder retries. It is a variable so
// tests can shorten it without changing production behavior.
var embedRetryBackoff = func(attempt int) time.Duration {
	return time.Second * time.Duration(1<<attempt)
}

// docInProgress keeps track of a document while it is indexed: how many chunks
// were sent to be embedded, how many actually reached the index and how many
// were lost. Only one that adds up is sealed in the document store.
type docInProgress struct {
	path     string
	mtime    time.Time
	size     int64
	expected int
	received int
	lost     int
	sealed   bool
}

// matches says whether the document came in whole: all its chunks received and
// none lost.
func (d *docInProgress) matches() bool {
	return d.lost == 0 && d.received == d.expected
}

// tracking guards the map of in-progress documents, which the main loop, the
// embedder workers and the writer all touch at once.
type tracking struct {
	mu    sync.Mutex
	docs  map[int64]*docInProgress
	order []int64
}

func newTracking() *tracking {
	return &tracking{docs: make(map[int64]*docInProgress)}
}

func (t *tracking) open(docID int64, path string, mtime time.Time, size int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.docs[docID] = &docInProgress{path: path, mtime: mtime, size: size}
	t.order = append(t.order, docID)
}

func (t *tracking) expect(docID int64, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.docs[docID]; d != nil {
		d.expected += n
	}
}

func (t *tracking) received(docID int64, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.docs[docID]; d != nil {
		d.received += n
	}
}

// readyToSeal marks and returns the document if it has just come in whole. It
// is called by the writer after each batch: the seal does not wait for the end
// of the pass, which on a large corpus is hours of already-written embeddings
// betting that nobody cuts the pass.
func (t *tracking) readyToSeal(docID int64) *docInProgress {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.docs[docID]
	if d == nil || d.sealed || d.expected == 0 || !d.matches() {
		return nil
	}
	d.sealed = true
	return d
}

func (t *tracking) unmarkSealed(docID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.docs[docID]; d != nil {
		d.sealed = false
	}
}

func (t *tracking) lost(docID int64, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.docs[docID]; d != nil {
		d.lost += n
	}
}

// processFiles runs the embedding pipeline on the given file list.
// It handles cache checking, content reading, chunking, embedding, and writing.
func processFiles(ctx context.Context, s *store.Store, e *embed.Client, files []string, force bool, start time.Time, cfg *config.Config) (Report, error) {
	jobCh := make(chan embedJob, cfg.Workers*4)
	resultCh := make(chan embedResult, cfg.Workers*4)

	var totalChunks int64
	var docCount int64
	var skippedDocs int64
	var processedDocs int64
	var failedChunks int64
	var unreadableDocs int64
	progress := newTracking()
	var totalTokens int64

	var wg sync.WaitGroup
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				select {
				case <-ctx.Done():
					return
				default:
				}

				var lastErr error
				success := false
				for attempt := 0; attempt < 3; attempt++ {
					select {
					case <-ctx.Done():
						return
					default:
					}

					vectors, tokens, err := e.EmbedWithUsage(ctx, job.chunks)
					if err != nil {
						lastErr = err
						delay := embedRetryBackoff(attempt)
						// When the proxy says how long to wait, that wins:
						// retrying earlier just hits the same limit again.
						var rateErr *embed.RateLimitError
						if errors.As(err, &rateErr) && rateErr.RetryAfter > delay {
							delay = rateErr.RetryAfter
						}
						slog.Warn("embed retry", "attempt", attempt+1, "delay", delay, "err", err)
						select {
						case <-ctx.Done():
							return
						case <-time.After(delay):
							continue
						}
					}
					resultCh <- embedResult{vectors: vectors, docID: job.docID, ords: job.ords, texts: job.chunks, tokens: tokens}
					success = true
					break
				}
				if !success {
					atomic.AddInt64(&failedChunks, int64(len(job.chunks)))
					progress.lost(job.docID, len(job.chunks))
					slog.Error("embed failed after 3 retries; chunks dropped", "doc", job.docID, "n_chunks", len(job.chunks), "err", lastErr)
				}
			}
		}()
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for result := range resultCh {
			var toInsert []store.ChunkToInsert
			for i, v := range result.vectors {
				toInsert = append(toInsert, store.ChunkToInsert{
					DocID:     result.docID,
					Ord:       result.ords[i],
					Text:      result.texts[i],
					Embedding: v,
				})
			}
			if err := s.BatchInsertChunks(toInsert, 256); err != nil {
				// A batch that fails to insert means lost chunks just like a 429:
				// they count in the summary and leave the document unsealed.
				atomic.AddInt64(&failedChunks, int64(len(toInsert)))
				progress.lost(result.docID, len(toInsert))
				slog.Error("writer batch insert", "doc", result.docID, "n_chunks", len(toInsert), "err", err)
			} else {
				atomic.AddInt64(&totalChunks, int64(len(toInsert)))
				progress.received(result.docID, len(toInsert))
				if d := progress.readyToSeal(result.docID); d != nil {
					if err := s.SealDocument(result.docID, d.mtime, d.size); err != nil {
						// Without a seal there is no lie: the next pass repeats it.
						progress.unmarkSealed(result.docID)
						slog.Error("could not seal document", "doc", d.path, "err", err)
					}
				}
			}
			atomic.AddInt64(&totalTokens, int64(result.tokens))
		}
	}()

	for _, fpath := range files {
		select {
		case <-ctx.Done():
			return Report{}, ctx.Err()
		default:
		}

		atomic.AddInt64(&docCount, 1)

		info, err := os.Stat(fpath)
		if err != nil {
			slog.Warn("stat file", "path", fpath, "err", err)
			continue
		}

		if !force {
			oldID, oldMtime, oldSize, ok, err := s.GetDocument(fpath)
			if err != nil {
				return Report{}, fmt.Errorf("regenerate get doc %s: %w", fpath, err)
			}
			if ok {
				// A seal is only worth anything if there really are chunks
				// behind it. The version that sealed before embedding left 761
				// sealed and empty documents: counted in "docs", skipped on
				// every pass and mute in every search.
				conChunks, err := s.DocumentHasChunks(oldID)
				if err != nil {
					return Report{}, fmt.Errorf("regenerate chunks doc %s: %w", fpath, err)
				}
				if conChunks && oldMtime == info.ModTime().Unix() && oldSize == info.Size() {
					slog.Debug("cache hit, skipping", "path", fpath)
					atomic.AddInt64(&skippedDocs, 1)
					continue
				}
				if !conChunks {
					slog.Warn("document sealed without a single chunk: reindexing", "path", fpath)
				}
				slog.Info("doc changed, reindexing", "path", fpath)
				if err := s.DeleteDocument(fpath); err != nil {
					return Report{}, fmt.Errorf("regenerate delete stale doc %s: %w", fpath, err)
				}
			}
		}

		content, err := readFileContent(fpath)
		if err != nil {
			// Counted as well as warned: the warning drowns in a 35 MB cron.log
			// and the summary said "0 failed" while the document fell out of the index.
			atomic.AddInt64(&unreadableDocs, 1)
			slog.Warn("read file", "path", fpath, "err", err)
			continue
		}
		if strings.TrimSpace(content) == "" {
			continue
		}

		// It is opened as PENDING: the seal with the real mtime and size -the
		// only thing that makes a later pass skip it- is set at the end, and
		// only if all its chunks made it in. It used to be sealed here, before
		// requesting a single embedding: a 429 from the proxy left the document
		// half-done and the next pass silently took it as good.
		docID, err := s.UpsertDocumentPending(fpath)
		if err != nil {
			return Report{}, fmt.Errorf("regenerate upsert doc %s: %w", fpath, err)
		}
		progress.open(docID, fpath, info.ModTime(), info.Size())

		chks := chunk.Chunk(content, cfg.ChunkSize, cfg.ChunkOverlap)
		if len(chks) == 0 {
			continue
		}

		atomic.AddInt64(&processedDocs, 1)
		progress.expect(docID, len(chks))

		batchSize := apiBatchSize()
		for batchStart := 0; batchStart < len(chks); batchStart += batchSize {
			batchEnd := batchStart + batchSize
			if batchEnd > len(chks) {
				batchEnd = len(chks)
			}
			batchChunks := chks[batchStart:batchEnd]
			batchOrds := make([]int, len(batchChunks))
			for i := range batchChunks {
				batchOrds[i] = batchStart + i
			}
			jobCh <- embedJob{docID: docID, ords: batchOrds, chunks: batchChunks}
		}

		if p := atomic.LoadInt64(&processedDocs); p%10 == 0 {
			fmt.Fprintf(os.Stderr, "processed %d/%d docs · %d chunks · elapsed %.0fs\n", p, len(files), atomic.LoadInt64(&totalChunks), time.Since(start).Seconds())
		}
	}

	close(jobCh)
	wg.Wait()
	close(resultCh)
	<-writerDone

	// Whatever is left unsealed: documents with no chunks to seal (a file that
	// does not yield a single piece) and whatever the writer could not close.
	var incomplete int
	for _, docID := range progress.order {
		d := progress.docs[docID]
		if d.sealed {
			continue
		}
		if d.matches() {
			if err := s.SealDocument(docID, d.mtime, d.size); err != nil {
				return Report{}, fmt.Errorf("regenerate seal doc %s: %w", d.path, err)
			}
			continue
		}
		incomplete++
		slog.Error("document half-done: not sealed in the cache, the next pass repeats it in full",
			"doc", d.path, "chunks_received", d.received, "chunks_expected", d.expected, "chunks_lost", d.lost)
	}

	return Report{
		Docs:       int(atomic.LoadInt64(&docCount)),
		Skipped:    int(atomic.LoadInt64(&skippedDocs)),
		Processed:  int(atomic.LoadInt64(&processedDocs)),
		Chunks:     int(atomic.LoadInt64(&totalChunks)),
		Tokens:     int(atomic.LoadInt64(&totalTokens)),
		Failed:     int(atomic.LoadInt64(&failedChunks)),
		Unreadable: int(atomic.LoadInt64(&unreadableDocs)),
		Incomplete: incomplete,
	}, nil
}

// purgeOrphans deletes documents whose paths are no longer on disk.
// A path is only eligible for deletion if it falls under one of the
// given roots. Paths under other known_roots are preserved.
func purgeOrphans(s *store.Store, roots []string) (int64, error) {
	allPaths, err := s.AllDocumentPaths()
	if err != nil {
		return 0, fmt.Errorf("regenerate list paths: %w", err)
	}

	// Build the set of root prefixes we protect.
	rootPrefixes := make([]string, 0, len(roots))
	for _, r := range roots {
		rAbs, err := filepath.Abs(r)
		if err != nil {
			rAbs = r
		}
		rootPrefixes = append(rootPrefixes, rAbs)
		if !strings.HasSuffix(rAbs, string(os.PathSeparator)) {
			rootPrefixes = append(rootPrefixes, rAbs+string(os.PathSeparator))
		}
	}

	// Also include previously registered known_roots so we never delete
	// paths from roots that were indexed in prior runs.
	knownRoots, err := s.KnownRoots()
	if err != nil {
		return 0, err
	}
	for _, r := range knownRoots {
		rAbs, err := filepath.Abs(r)
		if err != nil {
			rAbs = r
		}
		found := false
		for _, p := range rootPrefixes {
			if p == rAbs || strings.HasPrefix(rAbs, p) {
				found = true
				break
			}
		}
		if !found {
			rootPrefixes = append(rootPrefixes, rAbs)
			if !strings.HasSuffix(rAbs, string(os.PathSeparator)) {
				rootPrefixes = append(rootPrefixes, rAbs+string(os.PathSeparator))
			}
		}
	}

	var orphans int64
	for _, p := range allPaths {
		pAbs, err := filepath.Abs(p)
		if err != nil {
			pAbs = p
		}
		// Check if this path falls under any of our protected roots.
		underProtected := false
		for _, rp := range rootPrefixes {
			if pAbs == rp || strings.HasPrefix(pAbs, rp) {
				underProtected = true
				break
			}
		}
		if !underProtected {
			continue
		}
		if _, err := os.Stat(p); err != nil && os.IsNotExist(err) {
			if err := s.DeleteDocument(p); err != nil {
				slog.Warn("orphan delete", "path", p, "err", err)
				continue
			}
			orphans++
		}
	}
	if orphans > 0 {
		slog.Info("orphans purged", "count", orphans)
	}
	return orphans, nil
}

// Regenerate processes a single root.
// It registers the root in known_roots so that orphan purge won't
// delete paths from other indexed roots.
func Regenerate(ctx context.Context, cfg *config.Config, root string, force bool) (Report, error) {
	roots := []string{root}
	return RegenerateRoots(ctx, cfg, roots, force)
}

// RegenerateRoots processes multiple roots in sequence.
// Each root is registered in known_roots and all roots are considered
// during orphan purge.
func RegenerateRoots(ctx context.Context, cfg *config.Config, roots []string, force bool) (Report, error) {
	if len(roots) == 0 {
		return Report{}, nil
	}

	start := time.Now()

	s, err := store.Open(cfg.DBPath)
	if err != nil {
		return Report{}, fmt.Errorf("regenerate open store: %w", err)
	}
	defer s.Close()

	if force {
		if err := s.Reset(); err != nil {
			return Report{}, fmt.Errorf("regenerate reset: %w", err)
		}
	}

	e := embed.New(cfg.APIBase, cfg.APIKey, cfg.Model)

	// Check tool availability
	if _, err := exec.LookPath("pdftotext"); err == nil {
		pdftotextAvailable.Store(true)
	} else {
		slog.Warn("pdftotext not found in PATH, .pdf files will be skipped")
	}
	if _, err := exec.LookPath("pandoc"); err == nil {
		pandocAvailable.Store(true)
	} else {
		docxAvailable.Store(false)
	}

	var combined Report
	combined.Docs = 0
	combined.Skipped = 0
	combined.Processed = 0
	combined.Chunks = 0
	combined.Tokens = 0
	combined.Failed = 0
	combined.Unreadable = 0
	combined.Incomplete = 0

	for _, root := range roots {
		// Register this root so orphan purge protects it.
		if err := s.RegisterRoot(root); err != nil {
			return Report{}, fmt.Errorf("regenerate register root %s: %w", root, err)
		}

		files, err := walkAndCollect(root)
		if err != nil {
			return Report{}, fmt.Errorf("regenerate walk %s: %w", root, err)
		}

		report, err := processFiles(ctx, s, e, files, force, start, cfg)
		if err != nil {
			return Report{}, err
		}

		combined.Docs += report.Docs
		combined.Skipped += report.Skipped
		combined.Processed += report.Processed
		combined.Chunks += report.Chunks
		combined.Tokens += report.Tokens
		combined.Failed += report.Failed
		combined.Unreadable += report.Unreadable
		combined.Incomplete += report.Incomplete

		if p := combined.Processed; p%10 == 0 {
			fmt.Fprintf(os.Stderr, "processed %d docs total · %d chunks · elapsed %.0fs\n", p, combined.Chunks, time.Since(start).Seconds())
		}
	}

	orphans, err := purgeOrphans(s, roots)
	if err != nil {
		return Report{}, err
	}
	combined.Orphans = int(orphans)

	elapsed := time.Since(start)
	combined.Elapsed = elapsed
	return combined, nil
}

func readFileContent(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".pdf":
		return pdfToText(path)
	case ".docx":
		return docxToText(path)
	case ".xlsx":
		return xlsxToText(path)
	case ".eml":
		return emlToText(path)
	case ".html", ".htm":
		return htmlToText(path)
	case ".csv", ".tsv", ".txt", ".md", ".json", ".yaml", ".yml":
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if ext == ".csv" || ext == ".tsv" {
			return csvToText(data, ext)
		}
		if !utf8.Valid(data) {
			return "", nil
		}
		return string(data), nil
	default:
		// No recognized extension: try reading as UTF-8 text.
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if !utf8.Valid(data) {
			return "", nil
		}
		return string(data), nil
	}
}

func pdfToText(path string) (string, error) {
	cmd := exec.Command("pdftotext", "-layout", path, "-")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext %s: %w (stderr: %s)", path, err, stderr.String())
	}
	return stdout.String(), nil
}

func docxToText(path string) (string, error) {
	// Try Go library first
	if docxAvailable.Load() {
		text, err := docxGoRead(path)
		if err == nil {
			return text, nil
		}
		slog.Debug("docx go read failed, trying pandoc", "path", path, "err", err)
	}

	// Fallback to pandoc
	if pandocAvailable.Load() {
		cmd := exec.Command("pandoc", "-t", "plain", path)
		var stdout strings.Builder
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("pandoc docx %s: %w", path, err)
		}
		return stdout.String(), nil
	}

	slog.Warn("docx support: neither go library nor pandoc available, skipping", "path", path)
	return "", nil
}

func xlsxToText(path string) (string, error) {
	r, err := excelize.OpenFile(path)
	if err != nil {
		return "", fmt.Errorf("exelize open %s: %w", path, err)
	}
	defer r.Close()

	var parts []string
	for _, name := range r.GetSheetList() {
		rows, err := r.GetRows(name)
		if err != nil {
			continue
		}
		for _, row := range rows {
			parts = append(parts, strings.Join(row, " "))
		}
	}
	return strings.Join(parts, "\n"), nil
}

func emlToText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func htmlToText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	// Try pandoc for better HTML-to-text conversion
	if pandocAvailable.Load() {
		cmd := exec.Command("pandoc", "-t", "plain")
		cmd.Stdin = strings.NewReader(string(data))
		var stdout strings.Builder
		cmd.Stdout = &stdout
		if err := cmd.Run(); err == nil {
			return stdout.String(), nil
		}
	}

	// Fallback: strip HTML tags with goquery-like approach using html package
	return stripHTML(string(data)), nil
}

func stripHTML(htmlStr string) string {
	var sb strings.Builder
	inTag := false
	for _, r := range htmlStr {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
			sb.WriteByte(' ')
		default:
			if !inTag {
				sb.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

func csvToText(data []byte, ext string) (string, error) {
	sep := ','
	if ext == ".tsv" {
		sep = '\t'
	}
	reader := csv.NewReader(strings.NewReader(string(data)))
	reader.Comma = sep
	reader.LazyQuotes = true

	var parts []string
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			slog.Debug("csv read error, falling back to raw text", "err", err)
			return string(data), nil
		}
		parts = append(parts, strings.Join(record, " "))
	}
	return strings.Join(parts, "\n"), nil
}

// --- docx Go library fallback ---

// Minimal docx extraction using the XML structure of .docx files (ZIP of XML).
func docxGoRead(path string) (string, error) {
	cmd := exec.Command("unzip", "-p", path, "word/document.xml")
	var xmlData strings.Builder
	cmd.Stdout = &xmlData
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("unzip docx word/document.xml: %w", err)
	}

	return extractTextFromDocxXML(xmlData.String()), nil
}

func extractTextFromDocxXML(xmlStr string) string {
	var sb strings.Builder
	inTag := false
	inText := false
	var currentTag strings.Builder
	var textBuf strings.Builder

	for i := 0; i < len(xmlStr); i++ {
		r := xmlStr[i]
		switch r {
		case '<':
			if inText {
				sb.WriteString(textBuf.String())
				textBuf.Reset()
			}
			inTag = true
			currentTag.Reset()
		case '>':
			tagContent := currentTag.String()
			lower := strings.ToLower(tagContent)
			if strings.HasPrefix(lower, "/") {
				inText = false
			} else if !strings.HasPrefix(lower, "/") && !strings.HasPrefix(lower, "?") && !strings.HasPrefix(lower, "!") {
				if strings.HasPrefix(lower, "w:t") || strings.HasPrefix(lower, "t ") {
					inText = true
				}
			}
			inTag = false
		default:
			if inTag {
				currentTag.WriteByte(r)
			} else if inText {
				textBuf.WriteByte(r)
			}
		}
	}
	if inText {
		sb.WriteString(textBuf.String())
	}

	result := strings.Join(strings.Fields(sb.String()), " ")
	return result
}
