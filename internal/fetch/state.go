package fetch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State persists where fetch left off across runs so it does not redownload.
type State struct {
	LastBOEDate string    `json:"last_boe_date"` // YYYY-MM-DD of latest day already processed
	LastDOGDate string    `json:"last_dog_date"` // YYYY-MM-DD of latest day already processed
	Stats       StateStat `json:"stats"`
}

type StateStat struct {
	TotalFetched           int       `json:"total_fetched"`
	TotalSkippedIrrelevant int       `json:"total_skipped_irrelevant"`
	LastRun                time.Time `json:"last_run"`
}

// StatePath returns ~/.local/share/knowledge-cli/fetch.state.
func StatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "knowledge-cli", "fetch.state"), nil
}

// LoadState reads the persisted state. If the file does not exist a zero State
// is returned (no error). Callers should use the default "7 days ago" if
// LastBOEDate / LastDOGDate are empty.
func LoadState() (*State, error) {
	p, err := StatePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	return &s, nil
}

// SaveState atomically writes the state to disk.
func SaveState(s *State) error {
	p, err := StatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
