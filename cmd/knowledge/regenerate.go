package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/fredyk/knowledge/internal/config"
	"github.com/fredyk/knowledge/internal/index"
	"github.com/spf13/cobra"
)

var regenerateForce bool

var regenerateCmd = &cobra.Command{
	Use:   "regenerate [paths...]",
	Short: "Reindex documents under the given paths using an incremental cache keyed by (path,mtime,size)",
	Args:  cobra.MaximumNArgs(10),
	RunE: func(cmd *cobra.Command, args []string) error {
		var roots []string
		if len(args) == 0 {
			roots = []string{"~/knowledge"}
		} else {
			roots = args
		}

		// Expand ~ in each root
		for i, root := range roots {
			if len(root) >= 2 && root[0] == '~' {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("could not expand ~: %w", err)
				}
				roots[i] = filepath.Join(home, root[2:])
			}

			// Check that root exists
			if _, err := os.Stat(roots[i]); os.IsNotExist(err) {
				return fmt.Errorf("path does not exist: %s", roots[i])
			}
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		// Set up logging
		level := slog.LevelInfo
		if os.Getenv("KNOWLEDGE_DEBUG") == "1" {
			level = slog.LevelDebug
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

		ctx := cmd.Context()
		report, err := index.RegenerateRoots(ctx, cfg, roots, regenerateForce)
		if err != nil {
			return fmt.Errorf("regenerate: %w", err)
		}

		fmt.Printf("done · %d docs (%d cached, %d processed) · %d chunks new · %d tokens · %d failed · %d unreadable · %d incomplete · %d orphans · %.1fs\n",
			report.Docs, report.Skipped, report.Processed,
			report.Chunks, report.Tokens, report.Failed, report.Unreadable, report.Incomplete, report.Orphans,
			report.Elapsed.Seconds())
		return nil
	},
}

func init() {
	regenerateCmd.Flags().BoolVarP(&regenerateForce, "force", "f", false, "drop the index and re-embed everything from scratch (ignores the incremental cache)")
	rootCmd.AddCommand(regenerateCmd)
}
