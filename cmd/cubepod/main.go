// Package main implements the cubepod command-line interface.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cubeplexai/cubepod/pkg/client"
	"github.com/cubeplexai/cubepod/pkg/version"
)

func printUsage() {
	fmt.Fprintf(os.Stderr, `cubepod — CubePod Agent Sandbox CLI (%s)

Usage:
  cubepod [global-flags] <command> [subcommand] [arguments...]

Global Flags:
  --gateway-url   Gateway base URL (default: http://127.0.0.1:8080 or $CUBEPOD_GATEWAY_URL)
  --api-key       API Bearer token (default: $CUBEPOD_API_KEY)
  --json          Output results as JSON

Commands:
  health                                      Check Gateway health status
  mcp                                         Start native Model Context Protocol (MCP) JSON-RPC stdio server
  template list                               List available templates
  template get <id-or-alias> [--alias]        Get template by ID or alias
  template create --from-image <img> --alias <alias> [--cpu <n>] [--memory-mb <n>] [--disk-mb <n>]
                                              Register template from existing OCI image
  template update <id> --public=true|false    Update template public visibility
  template delete <id> [--force]              Delete a template
  sandbox create [flags]                      Provision a new sandbox
  sandbox list [--template <id>]              List managed sandboxes
  sandbox get <sandbox-id>                    Get status for a sandbox
  sandbox exec <sandbox-id> <cmd...>          Execute a command inside a sandbox
  sandbox write-file <id> <path> <file|text>  Write content or local file to sandbox
  sandbox read-file <id> <path>               Read a file from sandbox to stdout
  sandbox ls <id> [path]                      List files in sandbox directory
  sandbox rm <id> <path>                      Delete a file inside sandbox
  sandbox archive-sync <id> <dest> <localdir> Sync local directory to sandbox via tar.gz
  sandbox pause <id>                          Pause sandbox compute (retain PVCs)
  sandbox connect <id>                        Resume and connect to sandbox
  sandbox reset-rootfs <id>                   Reset RootFS Upper (preserve /workspace)
  sandbox fork <id>                           Create a 1-second CoW clone of sandbox
  sandbox delete <id>                         Permanently terminate and delete sandbox
  warmpool create <name> [flags]              Create or scale a PodWarmPool
  warmpool list                               List all active PodWarmPools
`, version.Version)
}

func main() {
	var (
		gatewayURL = os.Getenv("CUBEPOD_GATEWAY_URL")
		apiKey     = os.Getenv("CUBEPOD_API_KEY")
		jsonOutput bool
	)
	if gatewayURL == "" {
		gatewayURL = "http://127.0.0.1:8080"
	}

	// Parse custom leading global flags
	args := os.Args[1:]
	for len(args) > 0 {
		if strings.HasPrefix(args[0], "--gateway-url=") {
			gatewayURL = strings.TrimPrefix(args[0], "--gateway-url=")
			args = args[1:]
		} else if args[0] == "--gateway-url" && len(args) > 1 {
			gatewayURL = args[1]
			args = args[2:]
		} else if strings.HasPrefix(args[0], "--api-key=") {
			apiKey = strings.TrimPrefix(args[0], "--api-key=")
			args = args[1:]
		} else if args[0] == "--api-key" && len(args) > 1 {
			apiKey = args[1]
			args = args[2:]
		} else if args[0] == "--json" {
			jsonOutput = true
			args = args[1:]
		} else {
			break
		}
	}

	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printUsage()
		os.Exit(0)
	}

	c := client.New(
		gatewayURL,
		client.WithAPIKey(apiKey),
		client.WithTimeout(60*time.Second),
	)
	ctx := context.Background()

	cmd := args[0]
	subArgs := args[1:]

	switch cmd {
	case "health":
		res, err := c.Health(ctx)
		if err != nil {
			fatal(err)
		}
		output(res, jsonOutput, func() {
			fmt.Printf("Status: %s\n", res["status"])
		})

	case "mcp":
		if err := ServeMCP(ctx, c, os.Stdin, os.Stdout); err != nil {
			fatal(err)
		}

	case "template":
		if len(subArgs) == 0 {
			printUsage()
			os.Exit(1)
		}
		handleTemplate(ctx, c, subArgs, jsonOutput)

	case "sandbox":
		if len(subArgs) == 0 {
			printUsage()
			os.Exit(1)
		}
		handleSandbox(ctx, c, subArgs, jsonOutput)

	case "warmpool":
		if len(subArgs) == 0 {
			printUsage()
			os.Exit(1)
		}
		handleWarmpool(ctx, c, subArgs, jsonOutput)

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func handleSandbox(ctx context.Context, c *client.Client, args []string, jsonOutput bool) {
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "create":
		fs := flag.NewFlagSet("sandbox create", flag.ExitOnError)
		tpl := fs.String("template", "cubeplex-ubuntu24-desktop", "Template ID")
		volKey := fs.String("volume-key", "", "Deterministic workspace volume key")
		_ = fs.Parse(rest)

		sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
			TemplateID: *tpl,
			VolumeKey:  *volKey,
		})
		if err != nil {
			fatal(err)
		}
		output(sb, jsonOutput, func() {
			fmt.Printf("Created sandbox: %s (state: %s, gen: %d)\n", sb.SandboxID, sb.State, sb.RootfsGeneration)
		})

	case "list":
		fs := flag.NewFlagSet("sandbox list", flag.ExitOnError)
		tpl := fs.String("template", "", "Filter by template ID")
		_ = fs.Parse(rest)

		list, err := c.ListSandboxes(ctx, *tpl)
		if err != nil {
			fatal(err)
		}
		output(list, jsonOutput, func() {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "SANDBOX ID\tTEMPLATE\tSTATE\tROOTFS GEN")
			for _, sb := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", sb.SandboxID, sb.TemplateID, sb.State, sb.RootfsGeneration)
			}
			_ = w.Flush()
		})

	case "get":
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: cubepod sandbox get <sandbox-id>"))
		}
		sb, err := c.GetSandbox(ctx, rest[0])
		if err != nil {
			fatal(err)
		}
		output(sb, jsonOutput, func() {
			fmt.Printf("ID:         %s\n", sb.SandboxID)
			fmt.Printf("Template:   %s\n", sb.TemplateID)
			fmt.Printf("State:      %s\n", sb.State)
			fmt.Printf("Rootfs Gen: %d\n", sb.RootfsGeneration)
			if sb.VolumeKey != "" {
				fmt.Printf("Volume Key: %s\n", sb.VolumeKey)
			}
		})

	case "exec":
		if len(rest) < 2 {
			fatal(fmt.Errorf("usage: cubepod sandbox exec <sandbox-id> <command...>"))
		}
		sandboxID := rest[0]
		cmdStr := strings.Join(rest[1:], " ")

		res, err := c.RunCommand(ctx, sandboxID, client.RunCommandRequest{
			Command: cmdStr,
		})
		if err != nil {
			fatal(err)
		}
		if jsonOutput {
			buf, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(buf))
		} else {
			if res.Output != "" {
				fmt.Print(res.Output)
				if !strings.HasSuffix(res.Output, "\n") {
					fmt.Println()
				}
			}
		}
		if res.ExitCode != nil && *res.ExitCode != 0 {
			os.Exit(*res.ExitCode)
		}

	case "write-file":
		if len(rest) < 3 {
			fatal(fmt.Errorf("usage: cubepod sandbox write-file <sandbox-id> <path> <file-or-string>"))
		}
		sandboxID := rest[0]
		targetPath := rest[1]
		source := rest[2]

		var content []byte
		if data, err := os.ReadFile(source); err == nil {
			content = data
		} else {
			content = []byte(source)
		}

		if err := c.WriteFile(ctx, sandboxID, targetPath, content, "0644"); err != nil {
			fatal(err)
		}
		output(map[string]string{"status": "ok", "path": targetPath}, jsonOutput, func() {
			fmt.Printf("Successfully wrote %d bytes to %s\n", len(content), targetPath)
		})

	case "read-file":
		if len(rest) < 2 {
			fatal(fmt.Errorf("usage: cubepod sandbox read-file <sandbox-id> <path>"))
		}
		data, err := c.ReadFile(ctx, rest[0], rest[1])
		if err != nil {
			fatal(err)
		}
		_, _ = os.Stdout.Write(data)

	case "ls":
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: cubepod sandbox ls <sandbox-id> [path]"))
		}
		sandboxID := rest[0]
		path := "/workspace"
		if len(rest) > 1 {
			path = rest[1]
		}
		entries, err := c.ListFiles(ctx, sandboxID, path)
		if err != nil {
			fatal(err)
		}
		output(entries, jsonOutput, func() {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "MODE\tSIZE\tNAME")
			for _, e := range entries {
				typeIndicator := "-"
				if e.IsDir {
					typeIndicator = "d"
				}
				fmt.Fprintf(w, "%s%s\t%d\t%s\n", typeIndicator, e.Mode, e.Size, e.Name)
			}
			_ = w.Flush()
		})

	case "rm":
		if len(rest) < 2 {
			fatal(fmt.Errorf("usage: cubepod sandbox rm <sandbox-id> <path>"))
		}
		if err := c.DeleteFile(ctx, rest[0], rest[1]); err != nil {
			fatal(err)
		}
		output(map[string]string{"status": "deleted", "path": rest[1]}, jsonOutput, func() {
			fmt.Printf("Deleted %s\n", rest[1])
		})

	case "archive-sync":
		if len(rest) < 3 {
			fatal(fmt.Errorf("usage: cubepod sandbox archive-sync <sandbox-id> <dest> <local-dir>"))
		}
		if err := c.ArchiveSyncDir(ctx, rest[0], rest[1], rest[2]); err != nil {
			fatal(err)
		}
		output(map[string]string{"status": "synced", "dest": rest[1]}, jsonOutput, func() {
			fmt.Printf("Synced %s to %s\n", rest[2], rest[1])
		})

	case "pause":
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: cubepod sandbox pause <sandbox-id>"))
		}
		sb, err := c.PauseSandbox(ctx, rest[0])
		if err != nil {
			fatal(err)
		}
		output(sb, jsonOutput, func() {
			fmt.Printf("Paused sandbox: %s\n", sb.SandboxID)
		})

	case "connect":
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: cubepod sandbox connect <sandbox-id>"))
		}
		sb, err := c.ConnectSandbox(ctx, rest[0])
		if err != nil {
			fatal(err)
		}
		output(sb, jsonOutput, func() {
			fmt.Printf("Connected/Resumed sandbox: %s (state: %s)\n", sb.SandboxID, sb.State)
		})

	case "reset-rootfs":
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: cubepod sandbox reset-rootfs <sandbox-id>"))
		}
		sb, err := c.ResetRootFS(ctx, rest[0])
		if err != nil {
			fatal(err)
		}
		output(sb, jsonOutput, func() {
			fmt.Printf("Reset RootFS for %s (new gen: %d)\n", sb.SandboxID, sb.RootfsGeneration)
		})

	case "fork":
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: cubepod sandbox fork <sandbox-id>"))
		}
		sb, err := c.ForkSandbox(ctx, rest[0])
		if err != nil {
			fatal(err)
		}
		output(sb, jsonOutput, func() {
			fmt.Printf("Forked sandbox: %s\n", sb.SandboxID)
		})

	case "delete":
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: cubepod sandbox delete <sandbox-id>"))
		}
		if err := c.DeleteSandbox(ctx, rest[0]); err != nil {
			fatal(err)
		}
		output(map[string]string{"status": "deleted", "id": rest[0]}, jsonOutput, func() {
			fmt.Printf("Deleted sandbox: %s\n", rest[0])
		})

	default:
		fatal(fmt.Errorf("unknown sandbox subcommand: %s", sub))
	}
}

func handleTemplate(ctx context.Context, c *client.Client, args []string, jsonOutput bool) {
	if err := runTemplate(ctx, c, args, jsonOutput, os.Stdout); err != nil {
		fatal(err)
	}
}

func runTemplate(ctx context.Context, c *client.Client, args []string, jsonOutput bool, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cubepod template <list|get|create|update|delete> [arguments...]")
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "list":
		return runTemplateList(ctx, c, rest, jsonOutput, out)
	case "get":
		return runTemplateGet(ctx, c, rest, jsonOutput, out)
	case "create":
		return runTemplateCreate(ctx, c, rest, jsonOutput, out)
	case "update":
		return runTemplateUpdate(ctx, c, rest, jsonOutput, out)
	case "delete":
		return runTemplateDelete(ctx, c, rest, jsonOutput, out)
	default:
		return fmt.Errorf("unknown template subcommand: %s", sub)
	}
}

func normalizeBoolFlagArgs(args []string) []string {
	var flags, pos []string
	for i, a := range args {
		if a == "--" {
			pos = append(pos, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func runTemplateList(ctx context.Context, c *client.Client, rest []string, jsonOutput bool, out io.Writer) error {
	fs := flag.NewFlagSet("template list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: cubepod template list")
	}

	list, err := c.ListTemplates(ctx)
	if err != nil {
		return err
	}
	if jsonOutput {
		buf, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(buf))
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "TEMPLATE ID\tALIASES\tSTATUS\tCPU\tMEMORY")
	for _, tpl := range list {
		id := tpl.TemplateID
		if id == "" {
			id = tpl.ID
		}
		aliases := strings.Join(tpl.Aliases, ", ")
		if aliases == "" && len(tpl.Names) > 0 {
			aliases = strings.Join(tpl.Names, ", ")
		}
		if aliases == "" && tpl.Image != "" {
			aliases = tpl.Image
		}
		status := tpl.BuildStatus
		if status == "" {
			status = "ready"
		}
		memStr := "-"
		if tpl.MemoryMB > 0 {
			memStr = fmt.Sprintf("%d MB", tpl.MemoryMB)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", id, aliases, status, tpl.CPUCount, memStr)
	}
	return w.Flush()
}

func runTemplateGet(ctx context.Context, c *client.Client, rest []string, jsonOutput bool, out io.Writer) error {
	fs := flag.NewFlagSet("template get", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	isAlias := fs.Bool("alias", false, "Lookup template by alias")
	if err := fs.Parse(normalizeBoolFlagArgs(rest)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: cubepod template get <id-or-alias> [--alias]")
	}
	target := fs.Arg(0)

	var (
		tpl *client.Template
		err error
	)
	if *isAlias {
		tpl, err = c.GetTemplateByAlias(ctx, target)
	} else {
		tpl, err = c.GetTemplate(ctx, target)
		if err != nil {
			var apiErr *client.APIError
			is404 := errors.As(err, &apiErr) && apiErr.StatusCode == 404
			if is404 || strings.ContainsAny(target, ":/") {
				if aliasTpl, aliasErr := c.GetTemplateByAlias(ctx, target); aliasErr == nil {
					tpl = aliasTpl
					err = nil
				} else if is404 {
					err = aliasErr
				}
			}
		}
	}
	if err != nil {
		return err
	}

	if jsonOutput {
		buf, err := json.MarshalIndent(tpl, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(buf))
		return nil
	}
	fmt.Fprintf(out, "Template ID: %s\n", tpl.TemplateID)
	if tpl.BuildID != "" {
		fmt.Fprintf(out, "Build ID:    %s\n", tpl.BuildID)
	}
	if len(tpl.Aliases) > 0 {
		fmt.Fprintf(out, "Aliases:     %s\n", strings.Join(tpl.Aliases, ", "))
	}
	if len(tpl.Names) > 0 {
		fmt.Fprintf(out, "Names:       %s\n", strings.Join(tpl.Names, ", "))
	}
	if tpl.BuildStatus != "" {
		fmt.Fprintf(out, "Status:      %s\n", tpl.BuildStatus)
	}
	if tpl.CPUCount > 0 {
		fmt.Fprintf(out, "CPU:         %d\n", tpl.CPUCount)
	}
	if tpl.MemoryMB > 0 {
		fmt.Fprintf(out, "Memory:      %d MB\n", tpl.MemoryMB)
	}
	if tpl.DiskSizeMB > 0 {
		fmt.Fprintf(out, "Disk:        %d MB\n", tpl.DiskSizeMB)
	}
	fmt.Fprintf(out, "Public:      %t\n", tpl.Public)
	return nil
}

func runTemplateCreate(ctx context.Context, c *client.Client, rest []string, jsonOutput bool, out io.Writer) error {
	fs := flag.NewFlagSet("template create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fromImage := fs.String("from-image", "", "Source OCI image reference")
	alias := fs.String("alias", "", "Template alias")
	name := fs.String("name", "", "Template name")
	cpu := fs.Int("cpu", 0, "CPU count")
	memoryMB := fs.Int("memory-mb", 0, "Memory in MB")
	diskMB := fs.Int("disk-mb", 0, "Minimum free disk in MB")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 || *fromImage == "" || *alias == "" {
		return fmt.Errorf("usage: cubepod template create --from-image <image> --alias <alias> [--cpu <n>] [--memory-mb <n>] [--disk-mb <n>]")
	}

	req := client.CreateTemplateFromImageRequest{
		FromImage:     *fromImage,
		Alias:         *alias,
		Name:          *name,
		CPUCount:      *cpu,
		MemoryMB:      *memoryMB,
		MinFreeDiskMB: *diskMB,
	}
	res, err := c.CreateTemplateFromImage(ctx, req)
	if err != nil {
		return err
	}

	if jsonOutput {
		buf, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(buf))
		return nil
	}
	fmt.Fprintf(out, "Created template: %s (build: %s)\n", res.TemplateID, res.BuildID)
	if len(res.Aliases) > 0 {
		fmt.Fprintf(out, "Aliases: %s\n", strings.Join(res.Aliases, ", "))
	}
	return nil
}

func runTemplateUpdate(ctx context.Context, c *client.Client, rest []string, jsonOutput bool, out io.Writer) error {
	fs := flag.NewFlagSet("template update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	public := fs.Bool("public", false, "Set template public visibility (true|false)")
	if err := fs.Parse(normalizeBoolFlagArgs(rest)); err != nil {
		return err
	}
	publicSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "public" {
			publicSet = true
		}
	})
	if fs.NArg() != 1 || !publicSet {
		return fmt.Errorf("usage: cubepod template update <id> --public=true|false")
	}
	target := fs.Arg(0)

	if err := c.UpdateTemplateVisibility(ctx, target, *public); err != nil {
		return err
	}

	if jsonOutput {
		buf, err := json.MarshalIndent(map[string]interface{}{
			"status":     "updated",
			"templateID": target,
			"public":     *public,
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(buf))
		return nil
	}
	fmt.Fprintf(out, "Updated template %s (public: %t)\n", target, *public)
	return nil
}

func runTemplateDelete(ctx context.Context, c *client.Client, rest []string, jsonOutput bool, out io.Writer) error {
	fs := flag.NewFlagSet("template delete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "Force deletion even if referenced")
	if err := fs.Parse(normalizeBoolFlagArgs(rest)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: cubepod template delete <id> [--force]")
	}
	target := fs.Arg(0)

	if err := c.DeleteTemplate(ctx, target, *force); err != nil {
		return err
	}

	if jsonOutput {
		buf, err := json.MarshalIndent(map[string]string{"status": "deleted", "templateID": target}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(buf))
		return nil
	}
	fmt.Fprintf(out, "Deleted template: %s\n", target)
	return nil
}

func handleWarmpool(ctx context.Context, c *client.Client, args []string, jsonOutput bool) {
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "create":
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: cubepod warmpool create <name> [--template <id>] [--replicas <n>]"))
		}
		name := rest[0]
		fs := flag.NewFlagSet("warmpool create", flag.ExitOnError)
		tpl := fs.String("template", "cubeplex-ubuntu24-desktop", "Template ID")
		replicas := fs.Int("replicas", 2, "Standby replicas")
		_ = fs.Parse(rest[1:])

		wp, err := c.CreateWarmPool(ctx, name, *tpl, int32(*replicas))
		if err != nil {
			fatal(err)
		}
		output(wp, jsonOutput, func() {
			fmt.Printf("Created warmpool: %s (replicas: %d)\n", wp.Name, wp.Replicas)
		})

	case "list":
		list, err := c.ListWarmPools(ctx)
		if err != nil {
			fatal(err)
		}
		output(list, jsonOutput, func() {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "WARM POOL\tTEMPLATE\tREPLICAS\tREADY")
			for _, wp := range list {
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", wp.Name, wp.TemplateID, wp.Replicas, wp.ReadyReplicas)
			}
			_ = w.Flush()
		})

	default:
		fatal(fmt.Errorf("unknown warmpool subcommand: %s", sub))
	}
}

func output(v interface{}, asJSON bool, textPrinter func()) {
	if asJSON {
		buf, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fatal(err)
		}
		fmt.Println(string(buf))
		return
	}
	textPrinter()
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
