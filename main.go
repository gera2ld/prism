package main

import (
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/spf13/cobra"

	"github.com/gera2ld/prism/internal/gateway"
	"github.com/gera2ld/prism/internal/store"
)

func main() {
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: "./pb_data",
	})

	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		Automigrate: true,
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var config gateway.Config
	var logs gateway.LogSink

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Func: func(e *core.ServeEvent) error {
			gwStore, err := store.Open(app, logger)
			if err != nil {
				return err
			}
			config = gwStore
			logStore := store.NewLogSink(app)
			logs = logStore
			proxy := gateway.New(config, logs, logger)
			proxy.Capture = gwStore.CaptureEnabled
			if err := logStore.RegisterRetention(app, gwStore.RetentionCron(), gwStore.Retention); err != nil {
				return err
			}
			gwStore.OnSettingsChange(func() {
				if err := logStore.UpdateSchedule(gwStore.RetentionCron()); err != nil {
					logger.Error("invalid retention_cron, keeping previous schedule", "error", err)
				}
			})

			e.Router.Any("/v1/{path...}", func(e *core.RequestEvent) error {
				proxy.ServeHTTP(e.Response, e.Request)
				return nil
			})
			prismMux := newPrismMux(app, gwStore)
			registerPrismAPI(e, prismMux)
			return e.Next()
		},
		Priority: -99,
	})

	// withStore bootstraps the app, runs migrations, and opens the store
	// before invoking fn. It centralizes the setup every CLI subcommand needs.
	withStore := func(fn func(*Commands, []string) error) func(*cobra.Command, []string) error {
		return func(_ *cobra.Command, args []string) error {
			if err := app.Bootstrap(); err != nil {
				return err
			}
			defer app.ClearBootstrap()
			if err := app.RunAllMigrations(); err != nil {
				return err
			}
			gwStore, err := store.Open(app, logger)
			if err != nil {
				return err
			}
			return fn(&Commands{App: app, Store: gwStore}, args)
		}
	}

	keyCmd := &cobra.Command{
		Use:   "key",
		Short: "Manage client API keys",
	}
	keyCmd.AddCommand(&cobra.Command{
		Use:   "generate <name>",
		Short: "Create an API key and print it once",
		Args:  cobra.ExactArgs(1),
		RunE: withStore(func(cmds *Commands, args []string) error {
			key, err := cmds.GenerateKey(args[0])
			if err != nil {
				return err
			}
			fmt.Println(key)
			return nil
		}),
	})
	keyCmd.AddCommand(&cobra.Command{
		Use:   "reveal <name>",
		Short: "Decrypt and print a client API key for copying",
		Args:  cobra.ExactArgs(1),
		RunE: withStore(func(cmds *Commands, args []string) error {
			key, err := cmds.RevealKey(args[0])
			if err != nil {
				return err
			}
			fmt.Println(key)
			return nil
		}),
	})
	keyCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List client API keys (names and status, never secrets)",
		Args:  cobra.NoArgs,
		RunE: withStore(func(cmds *Commands, _ []string) error {
			keys, err := cmds.ListKeys()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tENABLED")
			for _, k := range keys {
				fmt.Fprintf(w, "%s\t%t\n", k.Name, k.Enabled)
			}
			return w.Flush()
		}),
	})
	app.RootCmd.AddCommand(keyCmd)

	providerCmd := &cobra.Command{
		Use:   "provider",
		Short: "Manage upstream providers",
	}
	providerCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List providers (names, URLs and status, never tokens)",
		Args:  cobra.NoArgs,
		RunE: withStore(func(cmds *Commands, _ []string) error {
			providers, err := cmds.ListProviders()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tBASE_URL\tENABLED")
			for _, p := range providers {
				fmt.Fprintf(w, "%s\t%s\t%t\n", p.Name, p.BaseURL, p.Enabled)
			}
			return w.Flush()
		}),
	})
	providerCmd.AddCommand(&cobra.Command{
		Use:   "reveal <name>",
		Short: "Decrypt and print a provider token for copying",
		Args:  cobra.ExactArgs(1),
		RunE: withStore(func(cmds *Commands, args []string) error {
			token, err := cmds.RevealProviderToken(args[0])
			if err != nil {
				return err
			}
			fmt.Println(token)
			return nil
		}),
	})
	app.RootCmd.AddCommand(providerCmd)

	routeCmd := &cobra.Command{Use: "route", Short: "Import and export routes"}
	routeCmd.AddCommand(&cobra.Command{
		Use: "export <file>", Args: cobra.ExactArgs(1),
		Short: "Export all routes to CSV",
		RunE: withStore(func(cmds *Commands, args []string) error {
			return cmds.ExportRoutesToFile(args[0])
		}),
	})
	routeImport := &cobra.Command{
		Use: "import <file>", Args: cobra.ExactArgs(1),
		Short: "Merge routes from CSV",
	}
	routeImport.Flags().Bool("prune", false, "Delete routes absent from the CSV")
	routeImport.RunE = withStore(func(cmds *Commands, args []string) error {
		prune, err := routeImport.Flags().GetBool("prune")
		if err != nil {
			return err
		}
		return cmds.ImportRoutesFromFile(args[0], prune)
	})
	routeCmd.AddCommand(routeImport)
	app.RootCmd.AddCommand(routeCmd)

	if err := app.Start(); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}
