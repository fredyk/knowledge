package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fredyk/knowledge/internal/config"
	"github.com/fredyk/knowledge/internal/store"
)

// The bug this test exists to prevent (2026-09-14): six of the 2,051 ".pdf"
// files under ~/knowledge are not PDF, they are HTML -a <meta refresh> to the
// search engine of the Judicial Power, the Incapsula anti-bot page-. pdftotext
// cannot read them, the loop skips them with a slog.Warn and moves on, and the
// final line of the cron says "0 failed". That is: three Supreme Court rulings
// and a deontological circular are not in the index the lawyer agent queries,
// and the summary claims nothing failed. Searching for them returns empty
// without saying something is missing.
//
// The Failed counter only counted chunks the embedder could not process. A
// document that cannot even be READ was counted nowhere.
func TestUnreadableDocumentIsCountedInTheSummary(t *testing.T) {
	tmp := t.TempDir()

	// A .pdf that is actually HTML: exactly what is in knowledge.
	fake := filepath.Join(tmp, "STS-2025-242.pdf")
	if err := os.WriteFile(fake, []byte(
		"<html>\n<head>\n<meta http-equiv=\"refresh\" content=\"0;url=https://www.poderjudicial.es/search/openDocument/abc\">\n</head>\n</html>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cfg := &config.Config{ChunkSize: 1200, ChunkOverlap: 200, Workers: 1}

	// The embeddings client is nil on purpose: if an unreadable document ever
	// asked for embeddings, the test would blow up instead of passing quietly.
	rep, err := processFiles(context.Background(), s, nil, []string{fake}, false, time.Now(), cfg)
	if err != nil {
		t.Fatalf("processFiles: %v", err)
	}

	if rep.Unreadable != 1 {
		t.Errorf("a document that cannot be read has to be counted: Unreadable = %d, expected 1", rep.Unreadable)
	}
	if rep.Processed != 0 {
		t.Errorf("nothing was processed, Processed = %d", rep.Processed)
	}
}
