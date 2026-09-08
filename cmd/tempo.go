package cmd

import "github.com/spf13/cobra"

func newTempoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tempo",
		Short: "Interact with Grafana Tempo",
	}
	cmd.AddCommand(newTempoSearchCmd())
	cmd.AddCommand(newTempoTailCmd())
	return cmd
}
