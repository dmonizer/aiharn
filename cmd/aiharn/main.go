// Command aiharn is the Aiharn AI agent harness entrypoint: it loads and
// validates configuration, assembles the runtime (LLM client, execution
// channel, approval gate, tools, agent), and starts the TUI.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/app"
	"aiharn/internal/config"
	"aiharn/internal/logging"
	"aiharn/internal/recorder"
	"aiharn/internal/tools"
	"aiharn/internal/tui"
	"aiharn/internal/webapi"
)

// version is the harness version, overridable at build time via
// -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("aiharn", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		configPath = fs.String("config", "config.toml", "path to the TOML configuration file")
		agentName  = fs.String("agent", "", "top-level agent type (default \"main\")")
		promptFile = fs.String("prompt", "", "override the system prompt file")
		channel    = fs.String("channel", "", "override the execution channel")
		modelName  = fs.String("model", "", "override the model config")
		approval   = fs.String("approval", "", "override approval mode (ask | allow-all)")
		showVer    = fs.Bool("version", false, "print version and exit")
		debug      = fs.Bool("debug", false, "enable debug logging to stderr")
		logPath    = fs.String("log", "aiharn.log.jsonl", "path to the conversation log (JSON Lines); empty disables logging")
		apiListen  = fs.String("api-listen", "", "serve the current-session API on this address (for example 127.0.0.1:7331)")
		apiToken   = fs.String("api-token", "", "API bearer token (prefer AIHARN_API_TOKEN)")
		apiOrigins = fs.String("api-allow-origin", "", "comma-separated frontend origins allowed by CORS")
		apiOnly    = fs.Bool("api-only", false, "run without the terminal UI (requires --api-listen)")
	)

	if err := fs.Parse(args); err != nil {
		return 2
	}
	specified := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		specified[f.Name] = true
	})
	logging.SetDebug(*debug)
	if *showVer {
		fmt.Fprintf(os.Stdout, "aiharn %s\n", version)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "aiharn: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
		return 1
	}
	apiCfg := cfg.API
	if token := os.Getenv("AIHARN_API_TOKEN"); token != "" {
		apiCfg.Token = token
	}
	if specified["api-listen"] {
		apiCfg.Listen = *apiListen
	}
	if specified["api-token"] {
		apiCfg.Token = *apiToken
	}
	if specified["api-allow-origin"] {
		apiCfg.AllowOrigins = commaList(*apiOrigins)
	}
	if specified["api-only"] {
		apiCfg.Only = *apiOnly
	}
	if apiCfg.Only && apiCfg.Listen == "" {
		fmt.Fprintln(os.Stderr, "aiharn: API-only mode requires an API listen address")
		return 2
	}
	cfg.API = apiCfg
	if err := config.Validate(cfg, config.ValidateOptions{
		KnownProviders: map[string]bool{
			config.ProviderOpenAIResponses:       true,
			config.ProviderOpenAIChatCompletions: true,
		},
		KnownChannelTypes: map[string]bool{config.ChannelTypeSSH: true, config.ChannelTypeLocal: true},
		KnownTools:        toolSet(tools.Names()),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
		return 1
	}

	var rec *recorder.Recorder
	if *logPath != "" {
		rec, err = recorder.NewFile(*logPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aiharn: open conversation log: %v\n", err)
			return 1
		}
		defer rec.Close()
	}

	buildOpts := app.Options{
		Agent:      *agentName,
		PromptFile: *promptFile,
		Channel:    *channel,
		Model:      *modelName,
		Approval:   *approval,
	}
	if rec != nil {
		buildOpts.Observer = rec
	}
	rt, err := app.Build(context.Background(), cfg, buildOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
		return 1
	}
	defer rt.Close()

	if rec != nil {
		rec.SetSession(recorder.Meta{
			Model:     rt.Summary.Model,
			AgentType: rt.Summary.AgentType,
			Channel:   rt.Summary.Channel,
			Approval:  rt.Summary.Approval,
		})
	}

	var api *webapi.Server
	if apiCfg.Listen != "" {
		api, err = webapi.New(webapi.Config{
			Listen: apiCfg.Listen, Token: apiCfg.Token,
			AllowedOrigins: apiCfg.AllowOrigins,
			Agent:          rt.Agent, Gate: rt.Gate,
			Session: webapi.SessionInfo{Model: rt.Summary.Model, Channel: rt.Summary.Channel},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
			return 1
		}
		if err := api.Start(); err != nil {
			api.Close()
			fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
			return 1
		}
		defer api.Close()
		fmt.Fprintf(os.Stderr, "aiharn: current-session API listening on %s\n", api.Addr())
	}
	if apiCfg.Only {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		return 0
	}

	p := tea.NewProgram(tui.New(rt.Manager, rt.Agent, rt.Gate, tui.Status{
		Model:     rt.Summary.Model,
		AgentType: rt.Summary.AgentType,
		Channel:   rt.Summary.Channel,
		Approval:  rt.Summary.Approval,
	}))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
		return 1
	}
	return 0
}

func toolSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func commaList(value string) []string {
	var values []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			values = append(values, item)
		}
	}
	return values
}
