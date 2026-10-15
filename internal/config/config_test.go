package config

import "testing"

func TestDefaultsToLocalOllamaWithoutKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KNOWLEDGE_API_BASE", "")
	t.Setenv("KNOWLEDGE_MODEL", "")
	t.Setenv("KNOWLEDGE_API_KEY", "")
	t.Setenv("KNOWLEDGE_DB", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("a missing key must not fail: %v", err)
	}
	if c.APIBase != "http://localhost:11434/v1" || c.Model != "bge-m3" {
		t.Fatalf("defaults must be the local Ollama endpoint with bge-m3, got %s %s", c.APIBase, c.Model)
	}
	if c.APIKey != "" {
		t.Fatalf("without KNOWLEDGE_API_KEY the key must stay empty, got %q", c.APIKey)
	}
}

func TestEnvAPIKeyIsUsed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KNOWLEDGE_API_KEY", "secret-key")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "secret-key" {
		t.Fatalf("KNOWLEDGE_API_KEY must be used, got %q", c.APIKey)
	}
}

func TestNoKeyNeededForAnyAPIBase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KNOWLEDGE_API_BASE", "https://example.com/v1")
	t.Setenv("KNOWLEDGE_API_KEY", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("a remote base without a key must still load: %v", err)
	}
	if c.APIKey != "" {
		t.Fatalf("no fallback key may be read from disk, got %q", c.APIKey)
	}
}
