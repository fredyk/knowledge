package config

import (
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	APIBase      string
	APIKey       string
	Model        string
	DBPath       string
	ChunkSize    int
	ChunkOverlap int
	Workers      int
}

const (
	defaultAPIBase   = "http://localhost:11434/v1"
	defaultModel     = "bge-m3"
	defaultChunkSize = 1200
	defaultOverlap   = 200
	defaultWorkers   = 4
)

// Load builds the configuration from the environment. The defaults point at a
// local Ollama instance, so a fresh clone works with no credentials: the API key
// is optional and no credentials file is read from disk.
func Load() (*Config, error) {
	c := &Config{
		APIBase:      envOr("KNOWLEDGE_API_BASE", defaultAPIBase),
		APIKey:       os.Getenv("KNOWLEDGE_API_KEY"),
		Model:        envOr("KNOWLEDGE_MODEL", defaultModel),
		ChunkSize:    envInt("KNOWLEDGE_CHUNK_SIZE", defaultChunkSize),
		ChunkOverlap: envInt("KNOWLEDGE_CHUNK_OVERLAP", defaultOverlap),
		Workers:      envInt("KNOWLEDGE_WORKERS", defaultWorkers),
	}

	if db := os.Getenv("KNOWLEDGE_DB"); db != "" {
		c.DBPath = db
	} else {
		home, _ := os.UserHomeDir()
		c.DBPath = filepath.Join(home, ".local", "share", "knowledge-cli", "index.db")
	}
	if err := os.MkdirAll(filepath.Dir(c.DBPath), 0o755); err != nil {
		return nil, err
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	_, err := fmt.Sscanf(v, "%d", &n)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
