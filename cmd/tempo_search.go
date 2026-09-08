package cmd

import (
	"fmt"
	"os"

	"github.com/pathcl/notrace/internal/config"
	"github.com/pathcl/notrace/internal/render"
	"github.com/pathcl/notrace/internal/tempo"
	"github.com/spf13/cobra"
)

func newTempoSearchCmd() *cobra.Command {
	var (
		startStr string
		endStr   string
		query    string
		limit    int
		output   string
		details  bool
	)

	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search traces by TraceQL and time range",
		Example: `  notrace tempo search --start 1h
  notrace tempo search --start 30m --query '{status=error}'
  notrace tempo search --start 2026-09-08T10:00:00Z --end 2026-09-08T11:00:00Z
  notrace tempo search --start 15m --query '{resource.service.name="checkout"}' --output json | jq .`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			start, err := tempo.ParseTime(startStr)
			if err != nil {
				return fmt.Errorf("--start: %w", err)
			}
			end, err := tempo.ParseTime(endStr)
			if err != nil {
				return fmt.Errorf("--end: %w", err)
			}

			client := tempo.NewClient(cfg.Tempo.URL, cfg.Tempo.Token, cfg.Tempo.OrgID, cfg.Tempo.Timeout)
			resp, err := client.Search(cmd.Context(), tempo.SearchQuery{
				Query: query,
				Start: start.Unix(),
				End:   end.Unix(),
				Limit: limit,
			})
			if err != nil {
				return err
			}

			if len(resp.Traces) == 0 {
				fmt.Fprintln(os.Stderr, "no traces found")
				return nil
			}
			if !details {
				return render.Traces(os.Stdout, resp.Traces, output)
			}
			traceDetails := make([]*tempo.TraceDetail, len(resp.Traces))
			for i, tr := range resp.Traces {
				d, err := client.GetTrace(cmd.Context(), tr.TraceID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warn: get trace %s: %v\n", tr.TraceID, err)
				}
				traceDetails[i] = d
			}
			return render.TracesDetailed(os.Stdout, resp.Traces, traceDetails, output)
		},
	}

	cmd.Flags().StringVar(&startStr, "start", "1h", "Start time: relative (1h, 30m, 2d) or RFC3339")
	cmd.Flags().StringVar(&endStr, "end", "now", "End time: relative or RFC3339 (default: now)")
	cmd.Flags().StringVarP(&query, "query", "q", "{}", "TraceQL query")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max traces to return")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "Output format: table, json")
	cmd.Flags().BoolVarP(&details, "details", "d", false, "Fetch and display resource and span attributes for each trace")

	return cmd
}
