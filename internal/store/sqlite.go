package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS documents (
	id        INTEGER PRIMARY KEY,
	path      TEXT NOT NULL UNIQUE,
	mtime     INTEGER NOT NULL,
	size      INTEGER NOT NULL,
	indexed_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS chunks (
	id        INTEGER PRIMARY KEY,
	doc_id    INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	ord       INTEGER NOT NULL,
	text      TEXT NOT NULL,
	embedding BLOB NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(doc_id);

CREATE TABLE IF NOT EXISTS known_roots (
	root          TEXT PRIMARY KEY,
	last_indexed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

type Store struct {
	db *sql.DB
}

type ChunkRow struct {
	ID        int64
	DocID     int64
	Path      string
	Ord       int
	Text      string
	Embedding []float32
}

// ChunkToInsert holds data for a single chunk insertion.
type ChunkToInsert struct {
	DocID     int64
	Ord       int
	Text      string
	Embedding []float32
}

func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("store open %s: %w", dbPath, err)
	}
	// Use a single connection: regenerate runs UpsertDocument from the main
	// goroutine while BatchInsertChunks runs from the writer goroutine; with
	// multiple connections SQLite returns SQLITE_BUSY immediately when both
	// try to write at once. One connection serializes all access at the
	// database/sql layer instead.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store WAL pragma: %w", err)
	}
	// Required for ON DELETE CASCADE on chunks(doc_id). Without this the
	// foreign key clause is silently ignored and orphan chunks accumulate.
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store foreign_keys pragma: %w", err)
	}
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store synchronous pragma: %w", err)
	}
	// Belt and suspenders: if any concurrent writer ever appears, wait up
	// to 10s for the lock instead of failing the whole regenerate.
	if _, err := db.Exec("PRAGMA busy_timeout=10000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store busy_timeout pragma: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// RegisterRoot records a root path in known_roots so that orphan purge
// respects it as an indexed tree.
func (s *Store) RegisterRoot(root string) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO known_roots (root, last_indexed_at) VALUES (?, datetime('now'))",
		root,
	)
	if err != nil {
		return fmt.Errorf("store register root %s: %w", root, err)
	}
	return nil
}

// KnownRoots returns all registered root paths.
func (s *Store) KnownRoots() ([]string, error) {
	rows, err := s.db.Query("SELECT root FROM known_roots")
	if err != nil {
		return nil, fmt.Errorf("store known roots: %w", err)
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("store scan root: %w", err)
		}
		roots = append(roots, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store roots iteration: %w", err)
	}
	return roots, nil
}

// RootsForPath returns the set of known_roots that the given path falls under.
func (s *Store) RootsForPath(path string) (map[string]bool, error) {
	roots, err := s.KnownRoots()
	if err != nil {
		return nil, err
	}
	found := make(map[string]bool)
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		pathAbs = path
	}
	for _, r := range roots {
		rAbs, err := filepath.Abs(r)
		if err != nil {
			rAbs = r
		}
		if pathAbs == rAbs || strings.HasPrefix(pathAbs, rAbs+string(os.PathSeparator)) {
			found[rAbs] = true
		}
	}
	return found, nil
}

func (s *Store) Reset() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store reset begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DROP TABLE IF EXISTS chunks"); err != nil {
		return fmt.Errorf("store reset drop chunks: %w", err)
	}
	if _, err := tx.Exec("DROP TABLE IF EXISTS documents"); err != nil {
		return fmt.Errorf("store reset drop documents: %w", err)
	}
	// Do NOT drop known_roots -- it must persist across resets.
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("store reset recreate: %w", err)
	}
	return tx.Commit()
}

// GetDocument returns id, mtime (unix seconds) and size of an existing
// document by path. ok=false when the document is not present.
func (s *Store) GetDocument(path string) (id int64, mtime int64, size int64, ok bool, err error) {
	err = s.db.QueryRow(
		"SELECT id, mtime, size FROM documents WHERE path = ?",
		path,
	).Scan(&id, &mtime, &size)
	if err == sql.ErrNoRows {
		return 0, 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("store get document %s: %w", path, err)
	}
	return id, mtime, size, true, nil
}

// DeleteDocument removes a document and (via ON DELETE CASCADE) all of its
// chunks. No-op when the path is not present.
func (s *Store) DeleteDocument(path string) error {
	if _, err := s.db.Exec("DELETE FROM documents WHERE path = ?", path); err != nil {
		return fmt.Errorf("store delete document %s: %w", path, err)
	}
	return nil
}

// AllDocumentPaths returns every indexed document path.
func (s *Store) AllDocumentPaths() ([]string, error) {
	rows, err := s.db.Query("SELECT path FROM documents")
	if err != nil {
		return nil, fmt.Errorf("store all document paths: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store scan document path: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store document paths iteration: %w", err)
	}
	return paths, nil
}

func (s *Store) UpsertDocument(path string, mtime time.Time, size int64) (int64, error) {
	now := time.Now().Unix()
	mtimeUnix := mtime.Unix()

	var id int64
	err := s.db.QueryRow(
		"INSERT INTO documents (path, mtime, size, indexed_at) VALUES (?, ?, ?, ?) ON CONFLICT(path) DO UPDATE SET mtime=?, size=?, indexed_at=? RETURNING id",
		path, mtimeUnix, size, now, mtimeUnix, size, now,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store upsert document %s: %w", path, err)
	}
	return id, nil
}

// pendingStamp is the (mtime, size) of an OPEN document that is not finished
// yet. No file on disk can hold a negative size, so a pending document never
// matches what is on disk and never gives a cache hit: if the pass dies halfway
// through, the next pass reindexes it in full.
const pendingStamp = -1

// UpsertDocumentPending opens (or reopens) a document WITHOUT sealing it. The
// stamp with the real mtime and size is set by SealDocument, and only once all
// of its chunks are in.
func (s *Store) UpsertDocumentPending(path string) (int64, error) {
	now := time.Now().Unix()
	var id int64
	err := s.db.QueryRow(
		"INSERT INTO documents (path, mtime, size, indexed_at) VALUES (?, ?, ?, ?) ON CONFLICT(path) DO UPDATE SET mtime=?, size=?, indexed_at=? RETURNING id",
		path, pendingStamp, pendingStamp, now, pendingStamp, pendingStamp, now,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store upsert pending document %s: %w", path, err)
	}
	return id, nil
}

// SealDocument marks a document as finished with the mtime and size the file
// had when it was read. It is the only thing that makes a later pass skip it
// through the cache.
func (s *Store) SealDocument(id int64, mtime time.Time, size int64) error {
	_, err := s.db.Exec(
		"UPDATE documents SET mtime = ?, size = ?, indexed_at = ? WHERE id = ?",
		mtime.Unix(), size, time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("store seal document %d: %w", id, err)
	}
	return nil
}

// DocumentHasChunks reports whether a document has at least one chunk inside. A
// sealed document without chunks is not indexed: it does not count as a cache hit.
func (s *Store) DocumentHasChunks(id int64) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM chunks WHERE doc_id = ? LIMIT 1", id).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store document has chunks %d: %w", id, err)
	}
	return true, nil
}

func (s *Store) InsertChunk(docID int64, ord int, text string, embedding []float32) error {
	embBlob := encodeVec(embedding)
	_, err := s.db.Exec(
		"INSERT INTO chunks (doc_id, ord, text, embedding) VALUES (?, ?, ?, ?)",
		docID, ord, text, embBlob,
	)
	if err != nil {
		return fmt.Errorf("store insert chunk doc=%d ord=%d: %w", docID, ord, err)
	}
	return nil
}

// BatchInsertChunks inserts multiple chunks in a single transaction.
// Each transaction holds up to maxChunks inserts.
func (s *Store) BatchInsertChunks(chunks []ChunkToInsert, maxChunks int) error {
	if len(chunks) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store batch begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO chunks (doc_id, ord, text, embedding) VALUES (?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("store batch prepare: %w", err)
	}
	defer stmt.Close()

	for i, c := range chunks {
		if i%maxChunks == 0 && i > 0 {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("store batch commit: %w", err)
			}
			tx, err = s.db.Begin()
			if err != nil {
				return fmt.Errorf("store batch rebegin: %w", err)
			}
			stmt, err = tx.Prepare("INSERT INTO chunks (doc_id, ord, text, embedding) VALUES (?, ?, ?, ?)")
			if err != nil {
				return fmt.Errorf("store batch reprepare: %w", err)
			}
			defer stmt.Close()
		}
		_, err := stmt.Exec(c.DocID, c.Ord, c.Text, encodeVec(c.Embedding))
		if err != nil {
			return fmt.Errorf("store batch insert chunk %d: %w", i, err)
		}
	}
	return tx.Commit()
}

func (s *Store) AllChunks() ([]ChunkRow, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.doc_id, d.path, c.ord, c.text, c.embedding
		FROM chunks c JOIN documents d ON c.doc_id = d.id
		ORDER BY c.doc_id, c.ord
	`)
	if err != nil {
		return nil, fmt.Errorf("store all chunks: %w", err)
	}
	defer rows.Close()

	var result []ChunkRow
	for rows.Next() {
		var cr ChunkRow
		var embBlob []byte
		if err := rows.Scan(&cr.ID, &cr.DocID, &cr.Path, &cr.Ord, &cr.Text, &embBlob); err != nil {
			return nil, fmt.Errorf("store scan chunk: %w", err)
		}
		cr.Embedding = decodeVec(embBlob)
		result = append(result, cr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store chunks iteration: %w", err)
	}
	return result, nil
}

// EachEmbedding walks the index reading only the id and the embedding of each
// chunk, holding nothing back: the slice fn receives is reused on the next row,
// so fn must not keep it. This is what allows searching an index of hundreds of
// thousands of chunks without loading it into memory.
func (s *Store) EachEmbedding(fn func(id int64, emb []float32)) error {
	rows, err := s.db.Query(`SELECT id, embedding FROM chunks`)
	if err != nil {
		return fmt.Errorf("store each embedding: %w", err)
	}
	defer rows.Close()

	var buf []float32
	for rows.Next() {
		var id int64
		var blob sql.RawBytes
		if err := rows.Scan(&id, &blob); err != nil {
			return fmt.Errorf("store scan embedding: %w", err)
		}
		n := len(blob) / 4
		if cap(buf) < n {
			buf = make([]float32, n)
		}
		buf = buf[:n]
		for i := 0; i < n; i++ {
			buf[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4 : i*4+4]))
		}
		fn(id, buf)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store embeddings iteration: %w", err)
	}
	return nil
}

// ChunksByID returns path, ord and text of the requested chunks, without the embedding.
func (s *Store) ChunksByID(ids []int64) (map[int64]ChunkRow, error) {
	out := make(map[int64]ChunkRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.Query(`
		SELECT c.id, c.doc_id, d.path, c.ord, c.text
		FROM chunks c JOIN documents d ON c.doc_id = d.id
		WHERE c.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store chunks by id: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cr ChunkRow
		if err := rows.Scan(&cr.ID, &cr.DocID, &cr.Path, &cr.Ord, &cr.Text); err != nil {
			return nil, fmt.Errorf("store scan chunk by id: %w", err)
		}
		out[cr.ID] = cr
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store chunks by id iteration: %w", err)
	}
	return out, nil
}

func (s *Store) Stats() (docs, chunks int64, err error) {
	err = s.db.QueryRow("SELECT COUNT(*) FROM documents").Scan(&docs)
	if err != nil {
		return 0, 0, fmt.Errorf("store stats docs: %w", err)
	}
	err = s.db.QueryRow("SELECT COUNT(*) FROM chunks").Scan(&chunks)
	if err != nil {
		return 0, 0, fmt.Errorf("store stats chunks: %w", err)
	}
	return docs, chunks, nil
}

func encodeVec(v []float32) []byte {
	buf := make([]byte, 0, len(v)*4)
	for _, f := range v {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(f))
	}
	return buf
}

func decodeVec(b []byte) []float32 {
	if len(b) == 0 {
		return nil
	}
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4 : i*4+4]))
	}
	return out
}
