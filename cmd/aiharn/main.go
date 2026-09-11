// Command aiharn is the Aiharn AI agent harness entrypoint: it loads and
// validates configuration, assembles the runtime (LLM client, execution
// channel, approval gate, tools, agent), and starts the TUI.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/app"
	"aiharn/internal/config"
	"aiharn/internal/tools"
	"aiharn/internal/tui"
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
	)

	if err := fs.Parse(args); err != nil {
		return 2
	}
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
	if err := config.Validate(cfg, config.ValidateOptions{
		KnownProviders:    map[string]bool{config.ProviderOpenAIResponses: true},
		KnownChannelTypes: map[string]bool{config.ChannelTypeSSH: true},
		KnownTools:        toolSet(tools.Names()),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
		return 1
	}

	rt, err := app.Build(context.Background(), cfg, app.Options{
		Agent:      *agentName,
		PromptFile: *promptFile,
		Channel:    *channel,
		Model:      *modelName,
		Approval:   *approval,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: %v\n", err)
		return 1
	}
	defer rt.Close()

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
