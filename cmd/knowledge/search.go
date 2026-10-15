package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/fredyk/knowledge/internal/config"
	"github.com/fredyk/knowledge/internal/index"
	"github.com/spf13/cobra"
)

var searchK int
var searchJSON bool

var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Find the chunks most similar to the query",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		query := strings.Join(args, " ")

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
		hits, err := index.Search(ctx, cfg, query, searchK)
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}

		if searchJSON {
			// Emit valid JSON even with no matches so `... --json | jq`
			// never chokes on plain text or a `null` body.
			if len(hits) == 0 {
				fmt.Println("[]")
				return nil
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(hits)
		}

		if len(hits) == 0 {
			fmt.Println("No results found.")
			return nil
		}

		for i, h := range hits {
			fmt.Printf("%d. [%.3f] %s (#%d)\n   Snippet: %q\n",
				i+1, h.Score, h.Path, h.Ord, h.Snippet)
			if i < len(hits)-1 {
				fmt.Println()
			}
		}
		return nil
	},
}

func init() {
	searchCmd.Flags().IntVarP(&searchK, "k", "k", 8, "number of results")
	searchCmd.Flags().BoolVar(&searchJSON, "json", false, "output results as JSON")
	rootCmd.AddCommand(searchCmd)
}
