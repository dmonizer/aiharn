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
	"sync"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/app"
	"aiharn/internal/config"
	"aiharn/internal/logging"
	"aiharn/internal/recorder"
	"aiharn/internal/sessions"
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
	if len(args) > 0 && args[0] == "add-user" {
		return runAddUser(args[1:])
	}
	fs := flag.NewFlagSet("aiharn", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		configPath  = fs.String("config", "", "path to the TOML configuration file (default $HOME/.aiharn/config.toml)")
		agentName   = fs.String("agent", "", "top-level agent type (default \"main\")")
		promptFile  = fs.String("prompt", "", "override the system prompt file")
		channel     = fs.String("channel", "", "override the execution channel")
		modelName   = fs.String("model", "", "override the model config")
		approval    = fs.String("approval", "", "override approval mode (ask | allow-all)")
		showVer     = fs.Bool("version", false, "print version and exit")
		debug       = fs.Bool("debug", false, "enable debug logging to stderr")
		logPath     = fs.String("log", "", "override the session transcript path (JSON Lines); empty disables logging")
		apiListen   = fs.String("api-listen", "", "serve the current-session API on this address (for example 127.0.0.1:7331)")
		apiAuthFile = fs.String("api-auth-file", "", "path to the API password file")
		apiOrigins  = fs.String("api-allow-origin", "", "comma-separated frontend origins allowed by CORS")
		apiOnly     = fs.Bool("api-only", false, "run without the terminal UI (requires --api-listen)")
	)

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
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
	if !specified["config"] {
		var err error
		*configPath, err = config.DefaultConfigPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "aiharn: resolve default config path: %v\n", err)
			return 1
		}
		created, err := config.EnsureDefaultConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aiharn: initialize default config: %v\n", err)
			return 1
		}
		if created {
			fmt.Fprintf(os.Stderr, "aiharn: created starter config at %s\n", *configPath)
		}
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
		return 1
	}
	apiCfg := cfg.API
	if path := os.Getenv("AIHARN_AUTH_FILE"); path != "" {
		apiCfg.AuthFile = path
	}
	if specified["api-listen"] {
		apiCfg.Listen = *apiListen
	}
	if specified["api-auth-file"] {
		apiCfg.AuthFile = *apiAuthFile
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

	transcripts, closeTranscripts, err := transcriptFactory(cfg.AiharnHome, *logPath, specified["log"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: open session transcript: %v\n", err)
		return 1
	}
	// Only the shared --log recorder needs closing here; a per-session
	// transcript is owned and closed by its session.
	defer closeTranscripts()

	buildOpts := app.Options{
		Agent:         *agentName,
		PromptFile:    *promptFile,
		Channel:       *channel,
		Model:         *modelName,
		Approval:      *approval,
		NewTranscript: transcripts,
	}
	sessionsMgr, err := app.NewSessionManager(context.Background(), app.SessionManagerOptions{
		Config: cfg, Build: buildOpts, MaxSessions: apiCfg.MaxSessions,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
		return 1
	}
	defer sessionsMgr.Shutdown()

	rt := sessionsMgr.DefaultRuntime()
	if rt == nil {
		fmt.Fprintf(os.Stderr, "aiharn: default session is unavailable\n")
		return 1
	}

	var api *webapi.Server
	if apiCfg.Listen != "" {
		api, err = webapi.New(webapi.Config{
			Listen: apiCfg.Listen, AuthFile: apiCfg.AuthFile,
			AllowedOrigins: apiCfg.AllowOrigins,
			Sessions:       sessionsMgr,
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
		fmt.Fprintf(os.Stderr, "aiharn: session API listening on %s (up to %d sessions)\n", api.Addr(), sessionLimit(apiCfg.MaxSessions))
	}
	if apiCfg.Only {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		return 0
	}

	m := tui.New(rt.Manager, rt.Agent, rt.Gate, tui.Status{
		Model:      rt.Summary.Model,
		AgentType:  rt.Summary.AgentType,
		Channel:    rt.Summary.Channel,
		Approval:   rt.Summary.Approval,
		Shortcuts:  cfg.Shortcuts,
		AiharnHome: cfg.AiharnHome,
	})
	m.SetClearFunc(func(ctx context.Context) (tui.ClearResult, error) {
		handle, err := sessionsMgr.Clear(ctx, "")
		if err != nil {
			return tui.ClearResult{}, err
		}
		top, ok := handle.Agent().(*agent.Agent)
		if !ok {
			return tui.ClearResult{}, fmt.Errorf("aiharn: cleared session returned unexpected agent type %T", handle.Agent())
		}
		return tui.ClearResult{
			Manager:   handle.Manager(),
			Agent:     top,
			Gate:      handle.Gate(),
			Model:     handle.Model(),
			AgentType: top.Type(),
			Channel:   handle.Channel(),
			Approval:  handle.Gate().Mode().String(),
		}, nil
	})
	p := tea.NewProgram(m, tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
		return 1
	}
	return 0
}

// transcriptFactory returns the per-session transcript factory and a function
// that closes whatever the factory owns. An unset (nil) transcript means the
// session keeps no transcript.
//
// By default every session gets its own private, timestamped file under
// <aiharn_home>/transcripts. An explicit --log PATH shares one file between all
// sessions, so their records interleave and the header line describes whichever
// session wrote first; that is the cost of pinning the path. An explicit
// --log "" disables transcripts entirely.
func transcriptFactory(aiharnHome, logPath string, logOverridden bool) (func(string, string) (sessions.Transcript, error), func(), error) {
	if logOverridden && logPath == "" {
		// A nil factory leaves Options.NewTranscript unset, which means "no
		// transcript" for every session.
		return nil, func() {}, nil
	}
	if logOverridden {
		// One shared recorder for every session, created on first use.
		var (
			once sync.Once
			rec  *recorder.Recorder
			err  error
		)
		factory := func(string, string) (sessions.Transcript, error) {
			once.Do(func() { rec, err = recorder.NewFile(logPath) })
			if err != nil {
				return nil, err
			}
			return rec, nil
		}
		return factory, func() {
			if rec != nil {
				_ = rec.Close()
			}
		}, nil
	}
	return func(string, string) (sessions.Transcript, error) {
		rec, _, err := recorder.NewSessionFile(aiharnHome)
		if err != nil {
			return nil, err
		}
		return rec, nil
	}, func() {}, nil
}

// sessionLimit reports the session bound the API will enforce.
func sessionLimit(configured int) int {
	if configured <= 0 {
		return 8
	}
	return configured
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
