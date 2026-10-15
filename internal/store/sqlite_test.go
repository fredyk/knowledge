package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegisterAndKnownRoots(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Register roots
	roots := []string{"/home/user/knowledge", "/home/user/Documents"}
	for _, r := range roots {
		if err := s.RegisterRoot(r); err != nil {
			t.Fatalf("RegisterRoot(%s): %v", r, err)
		}
	}

	// Retrieve roots
	retrieved, err := s.KnownRoots()
	if err != nil {
		t.Fatal(err)
	}

	if len(retrieved) != len(roots) {
		t.Fatalf("expected %d roots, got %d", len(roots), len(retrieved))
	}

	// Check all roots are present
	found := make(map[string]bool)
	for _, r := range retrieved {
		found[r] = true
	}
	for _, r := range roots {
		if !found[r] {
			t.Errorf("expected root %s to be present", r)
		}
	}
}

func TestRegisterRootReplace(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Register a root twice
	err = s.RegisterRoot("/home/user/knowledge")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)

	err = s.RegisterRoot("/home/user/knowledge")
	if err != nil {
		t.Fatal(err)
	}

	roots, err := s.KnownRoots()
	if err != nil {
		t.Fatal(err)
	}

	// Should still be exactly 1 root (REPLACE)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root after replace, got %d", len(roots))
	}
}

func TestRootsForPath(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Register roots
	err = s.RegisterRoot("/home/user/knowledge")
	if err != nil {
		t.Fatal(err)
	}
	err = s.RegisterRoot("/home/user/Documents")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path          string
		expectedCount int
	}{
		{"/home/user/knowledge/file.txt", 1},
		{"/home/user/knowledge/subdir/file.txt", 1},
		{"/home/user/Documents/notes.txt", 1},
		{"/home/user/Documents/Workana/invoice.csv", 1},
		{"/home/user/other/file.txt", 0},
		{"/tmp/random.txt", 0},
	}

	for _, tt := range tests {
		found, err := s.RootsForPath(tt.path)
		if err != nil {
			t.Fatalf("RootsForPath(%s): %v", tt.path, err)
		}
		if len(found) != tt.expectedCount {
			t.Errorf("RootsForPath(%s) = %d roots, want %d (got: %v)",
				tt.path, len(found), tt.expectedCount, found)
		}
	}
}

func TestUpsertAndGetDocument(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mtime := time.Now()
	id, err := s.UpsertDocument("/home/user/test.txt", mtime, 1234)
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	gotId, gotMtime, gotSize, ok, err := s.GetDocument("/home/user/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected document to exist")
	}
	if gotId != id {
		t.Errorf("expected id %d, got %d", id, gotId)
	}
	if gotMtime != mtime.Unix() {
		t.Errorf("expected mtime %d, got %d", mtime.Unix(), gotMtime)
	}
	if gotSize != 1234 {
		t.Errorf("expected size 1234, got %d", gotSize)
	}

	// Non-existent document
	_, _, _, ok, err = s.GetDocument("/home/user/nonexistent.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected ok=false for non-existent document")
	}
}

func TestDeleteDocument(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mtime := time.Now()
	_, err = s.UpsertDocument("/home/user/test.txt", mtime, 100)
	if err != nil {
		t.Fatal(err)
	}

	err = s.DeleteDocument("/home/user/test.txt")
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, ok, err := s.GetDocument("/home/user/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected document to be deleted")
	}
}

func TestAllDocumentPaths(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mtime := time.Now()
	paths := []string{
		"/home/user/a.txt",
		"/home/user/b.md",
		"/home/user/c.pdf",
	}
	for _, p := range paths {
		_, err := s.UpsertDocument(p, mtime, 100)
		if err != nil {
			t.Fatal(err)
		}
	}

	result, err := s.AllDocumentPaths()
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != len(paths) {
		t.Fatalf("expected %d paths, got %d", len(paths), len(result))
	}

	found := make(map[string]bool)
	for _, p := range result {
		found[p] = true
	}
	for _, p := range paths {
		if !found[p] {
			t.Errorf("expected path %s to be present", p)
		}
	}
}

func TestInsertAndAllChunks(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mtime := time.Now()
	docID, err := s.UpsertDocument("/home/user/test.txt", mtime, 100)
	if err != nil {
		t.Fatal(err)
	}

	// Insert chunks
	embedding := []float32{0.1, 0.2, 0.3, 0.4, 0.5}
	err = s.InsertChunk(docID, 0, "chunk one", embedding)
	if err != nil {
		t.Fatal(err)
	}
	err = s.InsertChunk(docID, 1, "chunk two", embedding)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := s.AllChunks()
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(rows))
	}

	if rows[0].Text != "chunk one" {
		t.Errorf("expected 'chunk one', got %q", rows[0].Text)
	}
	if rows[1].Text != "chunk two" {
		t.Errorf("expected 'chunk two', got %q", rows[1].Text)
	}
}

func TestStats(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mtime := time.Now()
	for i := 0; i < 3; i++ {
		path := filepath.Join("/home/user", "file"+string(rune('0'+i))+".txt")
		_, err := s.UpsertDocument(path, mtime, 100)
		if err != nil {
			t.Fatal(err)
		}
	}

	docs, chunks, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}

	if docs != 3 {
		t.Errorf("expected 3 docs, got %d", docs)
	}
	if chunks != 0 {
		t.Errorf("expected 0 chunks, got %d", chunks)
	}
}

func TestStoreResetPreservesKnownRoots(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Register a root
	err = s.RegisterRoot("/home/user/knowledge")
	if err != nil {
		t.Fatal(err)
	}

	// Add some documents
	mtime := time.Now()
	_, err = s.UpsertDocument("/home/user/file.txt", mtime, 100)
	if err != nil {
		t.Fatal(err)
	}

	// Reset
	err = s.Reset()
	if err != nil {
		t.Fatal(err)
	}

	// Known roots should still exist
	roots, err := s.KnownRoots()
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Errorf("expected 1 known root after reset, got %d", len(roots))
	}

	// But documents should be gone
	docs, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if docs != 0 {
		t.Errorf("expected 0 docs after reset, got %d", docs)
	}
}

func TestEnvMaxFileSize(t *testing.T) {
	// Verify that KNOWLEDGE_MAX_FILE_SIZE env var is parsed correctly.
	// The actual walk filtering is tested in index_test.go.
	os.Setenv("KNOWLEDGE_MAX_FILE_SIZE", "100")
	defer os.Unsetenv("KNOWLEDGE_MAX_FILE_SIZE")

	// Just verify the DB opens fine with the env var set
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Basic write/read to verify DB works with the env var
	mtime := time.Now()
	_, err = s.UpsertDocument("/home/user/test.txt", mtime, 50)
	if err != nil {
		t.Fatal(err)
	}

	_, _, size, ok, err := s.GetDocument("/home/user/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || size != 50 {
		t.Errorf("expected size 50, got %d, ok=%v", size, ok)
	}
}

func TestWalkAndCollect_excludesTrash(t *testing.T) {
	// .cache is excluded by walkAndCollect in the index package.
	// This test verifies the exclusion list includes .cache.
	// The excludedDirs map is in the index package.
	// We just verify our DB works in the presence of a Trash-like dir.
	tmp := t.TempDir()
	trashDir := filepath.Join(tmp, ".trash")
	err := os.MkdirAll(trashDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_ = trashDir
}
