// Package tools defines the tool contract and built-in tools exposed to the
// model. A Tool declares a JSON Schema for its arguments; the Registry validates
// incoming arguments against that schema before dispatching.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"aiharn/internal/llm"
)

// Built-in tool names.
const (
	NameExecuteCommand = "execute_command"
	NameSetApproval    = "set_approval"
)

// Names returns all built-in tool names, for config validation.
func Names() []string {
	return []string{
		NameExecuteCommand,
		NameSpawnSubagent,
		NameSendSubagentMessage,
		NameCheckSubagent,
		NameListSubagents,
		NameCloseSubagent,
		NameSetApproval,
	}
}

// Tool is a callable tool. Run receives already schema-validated raw JSON
// arguments and returns the textual result to feed back to the model.
type Tool interface {
	Definition() llm.ToolDefinition
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry holds a set of tools, keyed by name, and validates arguments before
// running.
type Registry struct {
	tools   map[string]Tool
	schemas map[string]*jsonschema.Schema
	order   []string
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		tools:   map[string]Tool{},
		schemas: map[string]*jsonschema.Schema{},
	}
}

// Register compiles the tool's argument schema and adds it to the registry.
func (r *Registry) Register(t Tool) error {
	def := t.Definition()
	if def.Name == "" {
		return errors.New("tools: tool has empty name")
	}
	if _, ok := r.tools[def.Name]; ok {
		return fmt.Errorf("tools: duplicate tool %q", def.Name)
	}
	schema, err := compileSchema(def.Name, def.Parameters)
	if err != nil {
		return fmt.Errorf("tools: %q: %w", def.Name, err)
	}
	r.tools[def.Name] = t
	r.schemas[def.Name] = schema
	r.order = append(r.order, def.Name)
	return nil
}

// Run validates args against the tool's schema, then dispatches to it.
func (r *Registry) Run(ctx context.Context, name string, args json.RawMessage) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	inst, err := decodeJSON(args)
	if err != nil {
		return "", fmt.Errorf("invalid arguments for %q: %w", name, err)
	}
	if err := r.schemas[name].Validate(inst); err != nil {
		return "", fmt.Errorf("invalid arguments for %q: %w", name, err)
	}
	return t.Run(ctx, args)
}

// Has reports whether name is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.tools[name]
	return ok
}

// Names returns the registered tool names in registration order.
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// Definitions returns the tool definitions in registration order.
func (r *Registry) Definitions() []llm.ToolDefinition {
	defs := make([]llm.ToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.tools[name].Definition())
	}
	return defs
}

// compileSchema compiles a JSON Schema document under a per-tool URL.
func compileSchema(name string, params json.RawMessage) (*jsonschema.Schema, error) {
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object"}`)
	}
	compiler := jsonschema.NewCompiler()
	url := "tool://" + name
	if err := compiler.AddResource(url, bytes.NewReader(params)); err != nil {
		return nil, err
	}
	return compiler.Compile(url)
}

// decodeJSON decodes raw JSON into a value suitable for schema.Validate.
func decodeJSON(raw json.RawMessage) (interface{}, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra interface{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return v, nil
}
