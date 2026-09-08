package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var verbose bool

var rootCmd = &cobra.Command{
	Use:   "notrace",
	Short: "CLI for querying Grafana Tempo and Prometheus/Mimir",
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().String("tempo-url", "", "Tempo base URL (env: NOTRACE_TEMPO_URL)")
	rootCmd.PersistentFlags().String("token", "", "Bearer token (env: NOTRACE_TOKEN)")
	rootCmd.PersistentFlags().String("org-id", "", "Org ID for multi-tenant Tempo (env: NOTRACE_ORG_ID)")
	rootCmd.PersistentFlags().Duration("timeout", 0, "HTTP request timeout, e.g. 5s, 30s (env: NOTRACE_TIMEOUT, default 10s)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Log HTTP requests and responses to stderr")

	viper.BindPFlag("tempo.url", rootCmd.PersistentFlags().Lookup("tempo-url"))      //nolint:errcheck
	viper.BindPFlag("tempo.token", rootCmd.PersistentFlags().Lookup("token"))        //nolint:errcheck
	viper.BindPFlag("tempo.org_id", rootCmd.PersistentFlags().Lookup("org-id"))     //nolint:errcheck
	viper.BindPFlag("tempo.timeout", rootCmd.PersistentFlags().Lookup("timeout"))   //nolint:errcheck

	rootCmd.AddCommand(newTempoCmd())
}

func initConfig() {
	// Use "config" as the filename to avoid viper finding and trying to parse
	// the ./notrace binary when searching the current directory.
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("$HOME/.config/notrace")
	viper.AddConfigPath(".")

	// Explicit env bindings for nested keys (AutomaticEnv doesn't traverse dots).
	viper.BindEnv("tempo.url", "NOTRACE_TEMPO_URL")      //nolint:errcheck
	viper.BindEnv("tempo.token", "NOTRACE_TOKEN")        //nolint:errcheck
	viper.BindEnv("tempo.org_id", "NOTRACE_ORG_ID")     //nolint:errcheck
	viper.BindEnv("tempo.timeout", "NOTRACE_TIMEOUT")   //nolint:errcheck

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			fmt.Fprintf(os.Stderr, "warning: config: %v\n", err)
		}
	}
}
