// Package cli exposes the token-router command and configuration flags.
package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"token-router/src/internal/config"
)

// NewCommand builds a command whose server lifecycle is supplied by run.
func NewCommand(run func(context.Context, config.Config) error) *cobra.Command {
	var path string
	flags := config.Flags()
	values := make(map[string]*string, len(flags))
	root := &cobra.Command{
		Use: "token-router", Short: "Route OpenAI API requests through a configurable proxy",
		SilenceUsage: true, SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error {
		return errors.New("invalid command-line flags; use --help for available options")
	})
	root.PersistentFlags().StringVar(&path, "config", "", "YAML configuration file (TOKEN_ROUTER_CONFIG; defaults to ./config.yaml when present)")
	for _, flag := range flags {
		values[flag.Key] = root.PersistentFlags().String(flag.Name, flag.Default, flag.Usage)
		if flag.Boolean {
			root.PersistentFlags().Lookup(flag.Name).NoOptDefVal = "true"
		}
	}
	load := func() (config.Config, error) {
		overrides := make(map[string]string)
		for _, flag := range flags {
			if root.PersistentFlags().Changed(flag.Name) {
				overrides[flag.Key] = *values[flag.Key]
			}
		}
		return config.Load(config.Options{
			Path: path, PathExplicit: root.PersistentFlags().Changed("config"), Overrides: overrides,
		})
	}
	noArgs := func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return errors.New("unexpected positional arguments; use --help for usage")
		}
		return nil
	}
	serve := func(cmd *cobra.Command, _ []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		return run(cmd.Context(), cfg)
	}
	root.Args, root.RunE = noArgs, serve
	root.AddCommand(&cobra.Command{Use: "serve", Short: "Start the proxy, metrics and optional admin listeners", Args: noArgs, RunE: serve})
	root.AddCommand(&cobra.Command{
		Use: "validate", Short: "Validate configuration without starting listeners", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := load(); err != nil {
				return err
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "configuration is valid")
			return err
		},
	})
	return root
}
