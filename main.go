package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
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
			proxy.Tools = gwStore.Tools()
			proxy.ToolLogs = store.NewToolLogSink(app)
			if err := logStore.RegisterRetention(app, gwStore.RetentionCron(), gwStore.Retention); err != nil {
				return err
			}
			gwStore.OnSettingsChange(func() {
				if err := logStore.UpdateSchedule(gwStore.RetentionCron()); err != nil {
					logger.Error("invalid retention_cron, keeping previous schedule", "error", err)
				}
			})
			// stdio servers are child processes; without this they would
			// outlive the gateway.
			app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
				gwStore.CloseMCP()
				return e.Next()
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

	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage MCP servers and tool approvals",
	}
	mcpCmd.AddCommand(&cobra.Command{
		Use: "list", Args: cobra.NoArgs,
		Short: "List MCP servers (never env or header values)",
		RunE: withStore(func(cmds *Commands, _ []string) error {
			servers, err := cmds.ListMCPServers()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTRANSPORT\tTARGET\tENABLED\tSECRETS")
			for _, s := range servers {
				target := s.URL
				if s.Transport == "stdio" {
					target = strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%t\n", s.Name, s.Transport, target, s.Enabled, s.HasSecrets)
			}
			return w.Flush()
		}),
	})
	mcpCmd.AddCommand(&cobra.Command{
		Use: "tools <server>", Args: cobra.ExactArgs(1),
		Short: "List a server's tools with approval status and definition hash",
		RunE: withStore(func(cmds *Commands, args []string) error {
			tools, err := cmds.ListMCPTools(context.Background(), args[0])
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TOOL\tSTATUS\tCALLABLE\tDEFINITION_HASH")
			for _, t := range tools {
				fmt.Fprintf(w, "%s\t%s\t%t\t%s\n", t.Tool, t.Status, t.Status == "approved", t.Hash)
			}
			return w.Flush()
		}),
	})
	mcpApprove := &cobra.Command{
		Use: "approve <server> <tool> <hash>", Args: cobra.ExactArgs(3),
		Short: "Approve a tool by pinning the definition hash you reviewed",
		Long: "Approve a tool by pinning the definition hash you reviewed.\n\n" +
			"Read the hash from `mcp tools <server>`. If the server redefines the tool\n" +
			"later the hash stops matching and the tool is withheld from agents until it\n" +
			"is approved again.",
		RunE: withStore(func(cmds *Commands, args []string) error {
			if err := cmds.ApproveMCPTool(context.Background(), args[0], args[1], args[2]); err != nil {
				return err
			}
			fmt.Printf("approved %s/%s\n", args[0], args[1])
			return nil
		}),
	}
	mcpCmd.AddCommand(mcpApprove)
	mcpCmd.AddCommand(&cobra.Command{
		Use: "revoke <server> <tool>", Args: cobra.ExactArgs(2),
		Short: "Revoke a tool approval",
		RunE: withStore(func(cmds *Commands, args []string) error {
			if err := cmds.RevokeMCPTool(args[0], args[1]); err != nil {
				return err
			}
			fmt.Printf("revoked %s/%s\n", args[0], args[1])
			return nil
		}),
	})
	mcpCmd.AddCommand(&cobra.Command{
		Use: "refresh <server>", Args: cobra.ExactArgs(1),
		Short: "Reconnect and relist a server",
		RunE: withStore(func(cmds *Commands, args []string) error {
			if err := cmds.RefreshMCPServer(args[0]); err != nil {
				return err
			}
			fmt.Printf("refreshed %s\n", args[0])
			return nil
		}),
	})
	mcpCmd.AddCommand(&cobra.Command{
		Use: "reveal <server>", Args: cobra.ExactArgs(1),
		Short: "Decrypt and print a server's env and header values",
		RunE: withStore(func(cmds *Commands, args []string) error {
			secrets, err := cmds.RevealMCPSecrets(args[0])
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "FIELD\tVALUE")
			for _, field := range slices.Sorted(maps.Keys(secrets)) {
				fmt.Fprintf(w, "%s\t%s\n", field, secrets[field])
			}
			return w.Flush()
		}),
	})
	app.RootCmd.AddCommand(mcpCmd)

	toolCmd := &cobra.Command{
		Use:   "tool",
		Short: "Manage conduit tools",
	}
	toolCmd.AddCommand(&cobra.Command{
		Use: "list", Args: cobra.NoArgs,
		Short: "List conduit tools and whether they are enabled",
		RunE: withStore(func(cmds *Commands, _ []string) error {
			tools, err := cmds.ListTools()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tENABLED\tVALID\tDESCRIPTION")
			for _, t := range tools {
				description := t.Description
				if len(description) > 60 {
					description = description[:57] + "..."
				}
				fmt.Fprintf(w, "%s\t%t\t%t\t%s\n", t.Name, t.Enabled, !t.Invalid, description)
			}
			return w.Flush()
		}),
	})
	toolCmd.AddCommand(&cobra.Command{
		Use: "validate <file>", Args: cobra.ExactArgs(1),
		Short: "Check a conduit definition without saving it",
		RunE: withStore(func(cmds *Commands, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			if err := cmds.ValidateTool(data); err != nil {
				return err
			}
			fmt.Println("valid")
			return nil
		}),
	})
	app.RootCmd.AddCommand(toolCmd)

	if err := app.Start(); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}
