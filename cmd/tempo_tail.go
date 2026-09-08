package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pathcl/notrace/internal/config"
	"github.com/pathcl/notrace/internal/render"
	"github.com/pathcl/notrace/internal/tempo"
	"github.com/spf13/cobra"
)

func newTempoTailCmd() *cobra.Command {
	var (
		query    string
		interval time.Duration
		output   string
		details  bool
		limit    int
	)

	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Stream new traces from Tempo in real time",
		Example: `  notrace tempo tail
  notrace tempo tail --query '{resource.service.name="checkout"}'
  notrace tempo tail --query '{status=error}' --interval 3s
  notrace tempo tail --output json | jq .rootServiceName`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			client := tempo.NewClient(cfg.Tempo.URL, cfg.Tempo.Token, cfg.Tempo.OrgID, cfg.Tempo.Timeout)
			client.SetVerbose(verbose)
			tailer := tempo.NewTailer(client, tempo.TailOptions{
				Query:    query,
				Interval: interval,
				Lookback: 30 * time.Second,
				Limit:    limit,
			})

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			fmt.Fprintf(os.Stderr, "tailing %s  query=%s  interval=%s\n", cfg.Tempo.URL, query, interval)

			return tailer.Run(ctx, func(traces []tempo.TraceSearchMetadata) error {
				if !details {
					return render.Traces(os.Stdout, traces, output)
				}
				traceDetails := make([]*tempo.TraceDetail, len(traces))
				for i, tr := range traces {
					d, err := client.GetTrace(ctx, tr.TraceID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warn: get trace %s: %v\n", tr.TraceID, err)
					}
					traceDetails[i] = d
				}
				return render.TracesDetailed(os.Stdout, traces, traceDetails, output)
			})
		},
	}

	cmd.Flags().StringVarP(&query, "query", "q", "{}", "TraceQL query")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "Poll interval")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "Output format: table, json")
	cmd.Flags().BoolVarP(&details, "details", "d", false, "Fetch and display resource and span attributes for each trace")
	cmd.Flags().IntVar(&limit, "limit", 100, "Max traces to fetch per poll")

	return cmd
}
