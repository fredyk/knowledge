package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/fredyk/knowledge/internal/config"
	"github.com/fredyk/knowledge/internal/fetch"
	"github.com/spf13/cobra"
)

var (
	fetchSince     string
	fetchThreshold float32
	fetchDryRun    bool
	fetchBOEOnly   bool
	fetchDOGOnly   bool
)

var fetchCmd = &cobra.Command{
	Use:   "fetch",
	Short: "Download the new, relevant BOE/DOG provisions and leave them in ~/knowledge/",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		level := slog.LevelInfo
		if os.Getenv("KNOWLEDGE_DEBUG") == "1" {
			level = slog.LevelDebug
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

		opts := fetch.Options{
			Threshold: fetchThreshold,
			DryRun:    fetchDryRun,
			BOEOnly:   fetchBOEOnly,
			DOGOnly:   fetchDOGOnly,
		}
		if fetchSince != "" {
			t, err := time.Parse("2006-01-02", fetchSince)
			if err != nil {
				return fmt.Errorf("invalid --since: %w", err)
			}
			opts.Since = t
		}
		if fetchBOEOnly && fetchDOGOnly {
			return fmt.Errorf("--boe-only and --dog-only are mutually exclusive")
		}

		rep, err := fetch.Run(cmd.Context(), cfg, opts)
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}

		fmt.Printf("done · BOE: %d new / %d skipped · DOG: %d new / %d skipped · %.1fs\n",
			rep.BOENew, rep.BOESkipped, rep.DOGNew, rep.DOGSkipped, rep.Elapsed.Seconds())
		return nil
	},
}

func init() {
	fetchCmd.Flags().StringVar(&fetchSince, "since", "", "date to download from (YYYY-MM-DD); defaults to the persisted state or 7 days ago")
	fetchCmd.Flags().Float32Var(&fetchThreshold, "threshold", 0.55, "cosine cutoff for the fine-grained semantic filter")
	fetchCmd.Flags().BoolVar(&fetchDryRun, "dry-run", false, "download nothing; print what would be downloaded")
	fetchCmd.Flags().BoolVar(&fetchBOEOnly, "boe-only", false, "limit to BOE")
	fetchCmd.Flags().BoolVar(&fetchDOGOnly, "dog-only", false, "limit to DOG")
	rootCmd.AddCommand(fetchCmd)
}
