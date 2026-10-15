package fetch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRelevanceQueriesComeFromTheUserFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relevance-queries.txt")
	content := "# my topics\nfirst query\n\n  second query  \n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRelevanceQueries(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first query", "second query"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMissingUserFileFallsBackToTheExamples(t *testing.T) {
	got, err := LoadRelevanceQueries(filepath.Join(t.TempDir(), "absent.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, DefaultRelevanceQueries) || len(got) == 0 {
		t.Fatalf("a missing file must fall back to DefaultRelevanceQueries, got %q", got)
	}
}

func TestUserFileWithoutQueriesIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relevance-queries.txt")
	if err := os.WriteFile(path, []byte("# only comments\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRelevanceQueries(path); err == nil {
		t.Fatal("a file with no queries must fail loudly, not rank everything against nothing")
	}
}

func TestRelevanceQueriesPathHonoursTheEnvironment(t *testing.T) {
	t.Setenv("KNOWLEDGE_RELEVANCE_QUERIES", "/some/where.txt")
	if got := RelevanceQueriesPath(); got != "/some/where.txt" {
		t.Fatalf("got %s", got)
	}
	t.Setenv("KNOWLEDGE_RELEVANCE_QUERIES", "")
	t.Setenv("HOME", "/home/someone")
	if got := RelevanceQueriesPath(); got != "/home/someone/.config/knowledge/relevance-queries.txt" {
		t.Fatalf("got %s", got)
	}
}
