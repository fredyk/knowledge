package store

import (
	"path/filepath"
	"testing"
	"time"
)

// A document opens PENDING and is only sealed once all of its chunks are in.
// While it is pending it carries an (mtime, size) that no file on disk can
// have, so it never gives a cache hit.
func TestAPendingDocumentNeverHitsTheCache(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	docPath := "/home/user/manual.md"
	id, err := s.UpsertDocumentPending(docPath)
	if err != nil {
		t.Fatalf("UpsertDocumentPending: %v", err)
	}
	if id == 0 {
		t.Fatal("UpsertDocumentPending returned id 0")
	}

	gotID, mtime, size, ok, err := s.GetDocument(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || gotID != id {
		t.Fatalf("the pending document must exist: ok = %v, id = %d", ok, gotID)
	}
	if mtime >= 0 || size >= 0 {
		t.Errorf("a pending document cannot carry a real file mtime/size: mtime = %d, size = %d", mtime, size)
	}

	// Seal it: from here on it does give a cache hit, without losing the id or the chunks.
	if err := s.InsertChunk(id, 0, "text", []float32{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	sealedAt := time.Unix(1757800000, 0)
	if err := s.SealDocument(id, sealedAt, 4096); err != nil {
		t.Fatalf("SealDocument: %v", err)
	}
	gotID, mtime, size, ok, err = s.GetDocument(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || gotID != id {
		t.Fatalf("sealing moved the document: ok = %v, id = %d, want %d", ok, gotID, id)
	}
	if mtime != sealedAt.Unix() || size != 4096 {
		t.Errorf("the seal did not stick: mtime = %d, size = %d", mtime, size)
	}
	chunks, err := s.AllChunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Errorf("sealing must not touch the chunks: got %d", len(chunks))
	}
}

// Reopening an already sealed document returns it to pending: while it is being
// reindexed, a run that crosses paths must not take it as good.
func TestReopeningASealedDocumentReturnsItToPending(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	docPath := "/home/user/manual.md"
	id, err := s.UpsertDocument(docPath, time.Unix(1757800000, 0), 4096)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.UpsertDocumentPending(docPath)
	if err != nil {
		t.Fatalf("UpsertDocumentPending: %v", err)
	}
	if id2 != id {
		t.Errorf("reopening must keep the id: %d vs %d", id2, id)
	}
	_, mtime, size, _, err := s.GetDocument(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if mtime >= 0 || size >= 0 {
		t.Errorf("still sealed after reopening: mtime = %d, size = %d", mtime, size)
	}
}
