// Package agent implements the runtime for a single LLM-driven agent: the turn
// loop that streams a response, executes tool calls, and feeds results back to
// the model. It is channel-agnostic; execution, approval, and subagent
// management are injected as tool dependencies, never as agent knowledge.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"aiharn/internal/llm"
	"aiharn/internal/tools"
)

// State is the lifecycle state of an agent.
type State int

const (
	StateStarting State = iota
	StateRunning
	StateIdle
	StateClosed
	StateErrored
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateIdle:
		return "idle"
	case StateClosed:
		return "closed"
	case StateErrored:
		return "errored"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// EventType discriminates events emitted during a turn.
type EventType int

const (
	EventText EventType = iota
	EventToolCall
	EventState
)

// Event is a single streamed event emitted by an agent for live display.
type Event struct {
	AgentID string
	Type    EventType
	Text    string   // EventText
	Call    llm.Item // EventToolCall
	State   State    // EventState
}

// Spec is the resolved, immutable configuration for one agent instance.
type Spec struct {
	ID             string
	Type           string
	Model          string
	System         string // system prompt content
	Client         llm.Client
	Tools          *tools.Registry
	Depth          int    // 0 = top-level
	CallerID       string // "" for top-level
	AllowSubagents bool   // whether this agent may spawn subagents
	EventCapacity  int
	InboxCapacity  int
	MaxToolRounds  int // per-turn cap on tool-call iterations
	Cleanup        func() // invoked once at close (e.g. release the session)
}

const (
	defaultEventCapacity = 256
	defaultInboxCapacity = 32
	defaultMaxToolRounds = 64
)

// Agent runs the turn loop for one agent instance. Turn is not safe for
// concurrent use; the Manager serializes access to a subagent's inbox and the
// top-level agent is driven by a single TUI event loop.
type Agent struct {
	id             string
	typ            string
	model          string
	system         string
	client         llm.Client
	tools          *tools.Registry
	depth          int
	callerID       string
	allowSubagents bool

	maxToolRounds int

	mu      sync.Mutex
	history []llm.Item
	state   State
	events  chan Event

	inbox chan string

	ctx    context.Context
	cancel context.CancelFunc

	// cleanup is invoked exactly once when the agent is closed; the app layer
	// uses it to release the agent's execution session.
	cleanup     func()
	cleanupOnce sync.Once

	// onComplete, when set, is called once per completed subagent task with the
	// task's final result (or its error). It is the Manager's delivery hook.
	onComplete func(result string)

	// onStateChange, when set, is called after every state transition. The
	// Manager uses it to notify roster subscribers that a subagent's status
	// changed.
	onStateChange func()
}

// New constructs an Agent from a resolved Spec.
func New(spec Spec) *Agent {
	if spec.EventCapacity <= 0 {
		spec.EventCapacity = defaultEventCapacity
	}
	if spec.InboxCapacity <= 0 {
		spec.InboxCapacity = defaultInboxCapacity
	}
	if spec.MaxToolRounds <= 0 {
		spec.MaxToolRounds = defaultMaxToolRounds
	}
	return &Agent{
		id:             spec.ID,
		typ:            spec.Type,
		model:          spec.Model,
		system:         spec.System,
		client:         spec.Client,
		tools:          spec.Tools,
		depth:          spec.Depth,
		callerID:       spec.CallerID,
		allowSubagents: spec.AllowSubagents,
		maxToolRounds:  spec.MaxToolRounds,
		cleanup:        spec.Cleanup,
		state:          StateStarting,
		events:         make(chan Event, spec.EventCapacity),
		inbox:          make(chan string, spec.InboxCapacity),
	}
}

// ID returns the agent's unique id.
func (a *Agent) ID() string { return a.id }

// Type returns the agent's configured type name.
func (a *Agent) Type() string { return a.typ }

// Depth returns the agent's nesting depth (0 = top-level).
func (a *Agent) Depth() int { return a.depth }

// AllowSubagents reports whether this agent may spawn subagents.
func (a *Agent) AllowSubagents() bool { return a.allowSubagents }

// CallerID returns the id of the agent that spawned this one ("" for top-level).
func (a *Agent) CallerID() string { return a.callerID }

// State returns the current lifecycle state.
func (a *Agent) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// Events returns the stream of live events. The channel is never closed.
func (a *Agent) Events() <-chan Event { return a.events }

// History returns a copy of the current conversation history.
func (a *Agent) History() []llm.Item {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Item(nil), a.history...)
}

// Send enqueues text into the agent's inbox. It never blocks; on overflow the
// message is dropped and false is returned.
func (a *Agent) Send(text string) bool {
	select {
	case a.inbox <- text:
		return true
	default:
		return false
	}
}

// drainInbox returns and removes all currently queued inbox messages.
func (a *Agent) drainInbox() []string {
	var msgs []string
	for {
		select {
		case m := <-a.inbox:
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

// setContext installs the agent's lifecycle context (Manager-internal, called
// before the agent is published or its run loop starts).
func (a *Agent) setContext(ctx context.Context, cancel context.CancelFunc) {
	a.ctx, a.cancel = ctx, cancel
}

// setOnComplete installs the delivery hook (Manager-internal).
func (a *Agent) setOnComplete(fn func(string)) { a.onComplete = fn }

// setOnStateChange installs the state-change hook (Manager-internal). It is set
// before the subagent's run loop starts and read only from that goroutine.
func (a *Agent) setOnStateChange(fn func()) { a.onStateChange = fn }

// Turn runs one full turn on behalf of a caller: it drains the inbox (subagent
// results and other inbound messages, injected as synthetic user messages),
// appends input, then streams and executes tool calls until the model responds
// with no tool calls. On error it returns the error and sets the state to
// errored (or idle for cancellation).
func (a *Agent) Turn(ctx context.Context, input string) error {
	for _, m := range a.drainInbox() {
		a.append(llm.Item{Type: llm.ItemMessage, Role: llm.RoleUser, Content: m})
	}
	return a.turn(ctx, input)
}

// turn is the core loop: append input and iterate stream → tools until the model
// stops calling tools. It does not drain the inbox; Turn and the subagent run
// loop manage that.
func (a *Agent) turn(ctx context.Context, input string) error {
	a.setState(StateRunning)
	a.append(llm.Item{Type: llm.ItemMessage, Role: llm.RoleUser, Content: input})

	for round := 0; round < a.maxToolRounds; round++ {
		stream, err := a.client.Stream(ctx, a.buildRequest())
		if err != nil {
			return a.fail(err)
		}

		output, err := a.collect(ctx, stream)
		if err != nil {
			return a.fail(err)
		}
		a.append(output...)

		calls := functionCalls(output)
		if len(calls) == 0 {
			a.setState(StateIdle)
			return nil
		}

		for _, call := range calls {
			a.emit(Event{Type: EventToolCall, Call: call})
			result := a.runTool(ctx, call)
			a.append(llm.Item{
				Type:    llm.ItemFunctionCallOutput,
				CallID:  call.CallID,
				Content: result,
			})
		}
	}

	return a.fail(fmt.Errorf("turn exceeded %d tool-call rounds", a.maxToolRounds))
}

// run drives a subagent: it consumes one task at a time from the inbox, runs a
// turn per task, and reports each final result via onComplete until the context
// is cancelled. A completed subagent returns to idle and can be reused; the
// caller decides when to close it.
func (a *Agent) run(ctx context.Context) {
	for {
		select {
		case task := <-a.inbox:
			if err := a.turn(ctx, task); err != nil {
				// A cancellation is a normal shutdown, not a task failure.
				if ctx.Err() == nil && a.onComplete != nil {
					a.onComplete("error: " + err.Error())
				}
				return
			}
			if a.onComplete != nil {
				a.onComplete(a.lastAssistant())
			}
		case <-ctx.Done():
			return
		}
	}
}

// Close terminates the agent: it marks it closed, cancels its lifecycle context,
// runs its cleanup exactly once, and emits a state event. It is idempotent and
// safe to call concurrently.
func (a *Agent) Close() {
	a.mu.Lock()
	if a.state == StateClosed {
		a.mu.Unlock()
		return
	}
	a.state = StateClosed
	a.mu.Unlock()

	if a.cancel != nil {
		a.cancel()
	}
	if a.cleanup != nil {
		a.cleanupOnce.Do(a.cleanup)
	}
	a.emit(Event{Type: EventState, State: StateClosed})
}

// lastAssistant returns the content of the most recent assistant message.
func (a *Agent) lastAssistant() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.history) - 1; i >= 0; i-- {
		it := a.history[i]
		if it.Type == llm.ItemMessage && it.Role == llm.RoleAssistant {
			return it.Content
		}
	}
	return ""
}

func (a *Agent) buildRequest() llm.Request {
	a.mu.Lock()
	history := append([]llm.Item(nil), a.history...)
	a.mu.Unlock()

	req := llm.Request{
		Model:  a.model,
		System: a.system,
		Stream: true,
		Input:  history,
	}
	if a.tools != nil {
		req.Tools = a.tools.Definitions()
	}
	return req
}

func (a *Agent) collect(ctx context.Context, stream <-chan llm.Event) ([]llm.Item, error) {
	var output []llm.Item
	for ev := range stream {
		switch ev.Type {
		case llm.EventTextDelta:
			a.emit(Event{Type: EventText, Text: ev.Text})
		case llm.EventCompleted:
			output = ev.Items
		case llm.EventFailed:
			return nil, ev.Err
		}
	}
	if output == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("llm stream ended without a terminal event")
	}
	return output, nil
}

func (a *Agent) runTool(ctx context.Context, call llm.Item) string {
	if a.tools == nil {
		return fmt.Sprintf("error: no tools available (cannot call %q)", call.Name)
	}
	result, err := a.tools.Run(ctx, call.Name, json.RawMessage(call.Args))
	if err != nil {
		return "error: " + err.Error()
	}
	return result
}

func (a *Agent) fail(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		a.setState(StateIdle)
	} else {
		a.setState(StateErrored)
	}
	return err
}

func (a *Agent) append(items ...llm.Item) {
	a.mu.Lock()
	a.history = append(a.history, items...)
	a.mu.Unlock()
}

func (a *Agent) setState(s State) {
	a.mu.Lock()
	a.state = s
	a.mu.Unlock()
	a.emit(Event{Type: EventState, State: s})
	if a.onStateChange != nil {
		a.onStateChange()
	}
}

func (a *Agent) emit(e Event) {
	e.AgentID = a.id
	select {
	case a.events <- e:
	default:
	}
}

func functionCalls(items []llm.Item) []llm.Item {
	var calls []llm.Item
	for _, it := range items {
		if it.Type == llm.ItemFunctionCall {
			calls = append(calls, it)
		}
	}
	return calls
}
