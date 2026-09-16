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
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"aiharn/internal/llm"
	"aiharn/internal/logging"
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
	EventUser
	EventReasoningStart
	EventReasoningDelta
	EventTaskQueued
	EventPause
	EventToolCall
	EventToolResult
	EventState
)

// Event is a single streamed event emitted by an agent for live display.
type Event struct {
	AgentID string
	Type    EventType
	Text    string   // text/reasoning delta, or EventToolResult output
	Call    llm.Item // EventToolCall / EventToolResult
	State   State    // EventState
}

// HistoryObserver receives history items as they are appended to an agent's
// conversation. It is invoked after the items are committed and without the
// agent's lock held, so the observer may do its own I/O. The recorder package
// implements it to persist a full, untrimmed conversation.
type HistoryObserver interface {
	ObserveHistory(agentID, agentType string, items []llm.Item)
}

// ActivityObserver optionally records live events, including partial output
// that may never reach completed history when a request is interrupted.
type ActivityObserver interface {
	ObserveEvent(agentID, agentType string, event Event)
}

// Spec is the resolved, immutable configuration for one agent instance.
type Spec struct {
	ID               string
	Type             string
	Model            string
	System           string // system prompt content
	ReasoningEffort  string
	ReasoningSummary string
	Client           llm.Client
	Tools            *tools.Registry
	Depth            int    // 0 = top-level
	CallerID         string // "" for top-level
	AllowSubagents   bool   // whether this agent may spawn subagents
	EventCapacity    int
	InboxCapacity    int
	MaxToolRounds    int           // per-turn cap on tool-call iterations
	RequestTimeout   time.Duration // per-provider-request timeout (0 = none)
	ToolResultBytes  int64         // cap on model-visible tool output (0 = none)
	TranscriptItems  int           // retained completed transcript items (0 = unlimited)
	TranscriptBytes  int64         // retained completed transcript content bytes (0 = unlimited)
	Observer         HistoryObserver
	Cleanup          func() // invoked once at close (e.g. release the session)
}

const (
	defaultEventCapacity = 256
	defaultInboxCapacity = 32
	defaultMaxToolRounds = 64
)

// ErrAgentClosed is returned when a new turn is attempted after Close.
var ErrAgentClosed = errors.New("agent: agent closed")

// Agent runs the turn loop for one agent instance. Turns are serialized; the
// Manager also serializes a subagent's inbox and the TUI drives the top-level
// agent from one event loop.
type Agent struct {
	id               string
	typ              string
	model            string
	system           string
	reasoningEffort  string
	reasoningSummary string
	client           llm.Client
	tools            *tools.Registry
	depth            int
	callerID         string
	allowSubagents   bool

	maxToolRounds   int
	requestTimeout  time.Duration
	toolResultBytes int64
	transcriptItems int
	transcriptBytes int64

	mu      sync.Mutex
	turnMu  sync.Mutex
	active  sync.WaitGroup
	history []llm.Item
	state   State
	events  chan Event

	inbox chan string
	// Pause holds queued tasks at turn boundaries. In-flight work is allowed to
	// complete, avoiding duplicate commands or provider requests on resume.
	paused bool
	resume chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	// turnCancel cancels only the currently executing turn. Unlike cancel, it
	// leaves the agent lifecycle and its execution session open for later work.
	turnCancel context.CancelFunc

	// cleanup is invoked exactly once when the agent is closed; the app layer
	// uses it to release the agent's execution session.
	cleanup     func()
	cleanupOnce sync.Once
	closeOnce   sync.Once
	closeDone   chan struct{}

	// onComplete, when set, is called once per completed subagent task with the
	// task's final result (or its error). It is the Manager's delivery hook.
	onComplete func(result string)

	// onStateChange, when set, is called after every state transition. The
	// Manager uses it to notify roster subscribers that a subagent's status
	// changed.
	onStateChange func()

	// observer, when set, is notified of every appended history item so an
	// external recorder can persist the full conversation (see HistoryObserver).
	observer HistoryObserver
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
		id:               spec.ID,
		typ:              spec.Type,
		model:            spec.Model,
		system:           spec.System,
		reasoningEffort:  spec.ReasoningEffort,
		reasoningSummary: spec.ReasoningSummary,
		client:           spec.Client,
		tools:            spec.Tools,
		depth:            spec.Depth,
		callerID:         spec.CallerID,
		allowSubagents:   spec.AllowSubagents,
		maxToolRounds:    spec.MaxToolRounds,
		requestTimeout:   spec.RequestTimeout,
		toolResultBytes:  spec.ToolResultBytes,
		transcriptItems:  spec.TranscriptItems,
		transcriptBytes:  spec.TranscriptBytes,
		cleanup:          spec.Cleanup,
		observer:         spec.Observer,
		state:            StateStarting,
		events:           make(chan Event, spec.EventCapacity),
		inbox:            make(chan string, spec.InboxCapacity),
		closeDone:        make(chan struct{}),
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == StateClosed || a.state == StateErrored {
		return false
	}
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
	return a.runTurn(ctx, input, true)
}

// turn is the core loop: append input and iterate stream → tools until the model
// stops calling tools. It does not drain the inbox; Turn and the subagent run
// loop manage that.
func (a *Agent) turn(ctx context.Context, input string) error {
	return a.runTurn(ctx, input, false)
}

func (a *Agent) runTurn(ctx context.Context, input string, drainInbox bool) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()

	start := time.Now()
	logging.Debug("agent: turn start",
		slog.String("component", "agent"),
		slog.String("agent_id", a.id),
		slog.String("agent_type", a.typ),
		slog.Int("input_bytes", len(input)),
	)
	fail := func(err error) error {
		logging.Debug("agent: turn end",
			slog.String("component", "agent"),
			slog.String("agent_id", a.id),
			slog.String("agent_type", a.typ),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Any("err", err),
		)
		return a.fail(err)
	}

	a.mu.Lock()
	if a.state == StateClosed {
		a.mu.Unlock()
		logging.Debug("agent: turn end",
			slog.String("component", "agent"),
			slog.String("agent_id", a.id),
			slog.String("agent_type", a.typ),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Any("err", ErrAgentClosed),
		)
		return ErrAgentClosed
	}
	a.active.Add(1)
	lifecycle := a.ctx
	turnCtx, turnCancel := context.WithCancel(ctx)
	a.turnCancel = turnCancel
	a.mu.Unlock()
	var stopLifecycle func() bool
	if lifecycle != nil {
		stopLifecycle = context.AfterFunc(lifecycle, turnCancel)
	}
	defer func() {
		if stopLifecycle != nil {
			stopLifecycle()
		}
		turnCancel()
		a.mu.Lock()
		a.turnCancel = nil
		a.mu.Unlock()
		a.active.Done()
	}()
	if drainInbox {
		for _, m := range a.drainInbox() {
			a.append(llm.Item{Type: llm.ItemMessage, Role: llm.RoleUser, Content: m})
		}
	}

	ctx = turnCtx

	a.setState(StateRunning)
	a.append(llm.Item{Type: llm.ItemMessage, Role: llm.RoleUser, Content: input})
	a.emit(Event{Type: EventUser, Text: input})
	a.trimHistory()

	for round := 0; round < a.maxToolRounds; round++ {
		if a.reasoningSummary != "" || (a.reasoningEffort != "" && a.reasoningEffort != "none") {
			a.emit(Event{Type: EventReasoningStart})
		}
		rctx, cancel := a.requestContext(ctx)
		stream, err := a.client.Stream(rctx, a.buildRequest())
		if err != nil {
			cancel()
			logging.Debug("agent: client.Stream error",
				slog.String("component", "agent"),
				slog.String("agent_id", a.id),
				slog.Int("round", round),
				slog.Any("err", err),
			)
			return fail(err)
		}

		output, err := a.collect(rctx, stream)
		cancel()
		if err != nil {
			logging.Debug("agent: collect error",
				slog.String("component", "agent"),
				slog.String("agent_id", a.id),
				slog.Int("round", round),
				slog.Any("err", err),
			)
			return fail(err)
		}
		a.append(output...)

		calls := functionCalls(output)
		logging.Debug("agent: round collected",
			slog.String("component", "agent"),
			slog.String("agent_id", a.id),
			slog.Int("round", round),
			slog.Int("output_items", len(output)),
			slog.Int("tool_calls", len(calls)),
		)
		if len(calls) == 0 {
			a.trimHistory()
			a.setState(StateIdle)
			logging.Debug("agent: turn end",
				slog.String("component", "agent"),
				slog.String("agent_id", a.id),
				slog.String("agent_type", a.typ),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
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

	return fail(fmt.Errorf("turn exceeded %d tool-call rounds", a.maxToolRounds))
}

// run drives a subagent: it consumes one task at a time from the inbox, runs a
// turn per task, and reports each final result via onComplete until the context
// is cancelled. A completed subagent returns to idle and can be reused; the
// caller decides when to close it.
func (a *Agent) run(ctx context.Context) {
	for {
		if !a.waitUnpaused(ctx) {
			return
		}
		select {
		case task := <-a.inbox:
			if !a.waitUnpaused(ctx) {
				return
			}
			if err := a.turn(ctx, task); err != nil {
				// A user interrupt cancels one task, not the reusable subagent's
				// lifecycle. Keep its run loop alive for future messages.
				if errors.Is(err, context.Canceled) && ctx.Err() == nil {
					continue
				}
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

func (a *Agent) waitUnpaused(ctx context.Context) bool {
	for {
		a.mu.Lock()
		paused, resume := a.paused, a.resume
		a.mu.Unlock()
		if !paused {
			return ctx.Err() == nil
		}
		select {
		case <-resume:
		case <-ctx.Done():
			return false
		}
	}
}

// setPaused controls scheduling of future subagent tasks. It deliberately does
// not interrupt an in-flight turn, which might already have produced side effects.
func (a *Agent) setPaused(paused bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.paused == paused {
		return
	}
	a.paused = paused
	if paused {
		a.resume = make(chan struct{})
	} else {
		close(a.resume)
		a.resume = nil
	}
}

// Paused reports whether new subagent tasks are held at a turn boundary.
func (a *Agent) Paused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.paused
}

// cancelWork cancels the current turn and optionally discards queued inbox
// work. It leaves the agent open and returns whether anything was affected.
func (a *Agent) cancelWork(discardInbox bool) bool {
	a.mu.Lock()
	cancel := a.turnCancel
	affected := cancel != nil
	if discardInbox {
		for {
			select {
			case <-a.inbox:
				affected = true
			default:
				a.mu.Unlock()
				if cancel != nil {
					cancel()
				}
				return affected
			}
		}
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return affected
}

// Close terminates the agent: it marks it closed, cancels its lifecycle context,
// runs its cleanup exactly once, and emits a state event. It is idempotent and
// safe to call concurrently.
func (a *Agent) Close() {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.state = StateClosed
		a.mu.Unlock()

		if a.cancel != nil {
			a.cancel()
		}
		a.active.Wait()
		a.releaseResources()
		a.emit(Event{Type: EventState, State: StateClosed})
		close(a.closeDone)
	})
	<-a.closeDone
}

// releaseResources releases external resources without changing lifecycle
// state. The Manager uses it for errored subagents so diagnostics remain
// visible without leaking their execution sessions.
func (a *Agent) releaseResources() {
	if a.cleanup != nil {
		a.cleanupOnce.Do(a.cleanup)
	}
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
		Model:            a.model,
		System:           a.system,
		Stream:           true,
		Input:            history,
		ReasoningEffort:  a.reasoningEffort,
		ReasoningSummary: a.reasoningSummary,
	}
	if a.tools != nil {
		req.Tools = a.tools.Definitions()
	}
	return req
}

func (a *Agent) collect(ctx context.Context, stream <-chan llm.Event) ([]llm.Item, error) {
	var output []llm.Item
	textDeltas := 0
	for ev := range stream {
		switch ev.Type {
		case llm.EventReasoningDelta:
			a.emit(Event{Type: EventReasoningDelta, Text: ev.Text})
		case llm.EventTextDelta:
			textDeltas++
			a.emit(Event{Type: EventText, Text: ev.Text})
		case llm.EventCompleted:
			if ev.FinishReason != "" && ev.FinishReason != "stop" {
				logging.Debug("agent: collect terminal",
					slog.String("component", "agent"),
					slog.String("agent_id", a.id),
					slog.String("reason", "completed"),
					slog.String("finish_reason", ev.FinishReason),
					slog.Int("text_deltas", textDeltas),
				)
				return nil, fmt.Errorf("llm response incomplete: %s", ev.FinishReason)
			}
			output = ev.Items
			logging.Debug("agent: collect terminal",
				slog.String("component", "agent"),
				slog.String("agent_id", a.id),
				slog.String("reason", "completed"),
				slog.String("finish_reason", ev.FinishReason),
				slog.Int("text_deltas", textDeltas),
				slog.Int("items", len(ev.Items)),
			)
		case llm.EventFailed:
			logging.Debug("agent: collect terminal",
				slog.String("component", "agent"),
				slog.String("agent_id", a.id),
				slog.String("reason", "failed"),
				slog.Int("text_deltas", textDeltas),
				slog.Any("err", ev.Err),
			)
			return nil, ev.Err
		}
	}
	if output == nil {
		if err := ctx.Err(); err != nil {
			logging.Debug("agent: collect terminal",
				slog.String("component", "agent"),
				slog.String("agent_id", a.id),
				slog.String("reason", "context"),
				slog.Int("text_deltas", textDeltas),
				slog.Any("err", err),
			)
			return nil, err
		}
		logging.Debug("agent: collect terminal",
			slog.String("component", "agent"),
			slog.String("agent_id", a.id),
			slog.String("reason", "no_terminal"),
			slog.Int("text_deltas", textDeltas),
		)
		return nil, errors.New("llm stream ended without a terminal event")
	}
	return output, nil
}

func (a *Agent) runTool(ctx context.Context, call llm.Item) string {
	start := time.Now()
	argsLog := call.Args
	if len(argsLog) > 256 {
		argsLog = argsLog[:256] + "..."
	}
	logging.Debug("agent: runTool",
		slog.String("component", "agent"),
		slog.String("agent_id", a.id),
		slog.String("tool", call.Name),
		slog.String("args", argsLog),
		slog.Int("args_bytes", len(call.Args)),
	)
	logDone := func(resultBytes int, err error) {
		attrs := []slog.Attr{
			slog.String("component", "agent"),
			slog.String("agent_id", a.id),
			slog.String("tool", call.Name),
			slog.Int("result_bytes", resultBytes),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		}
		if err != nil {
			attrs = append(attrs, slog.Any("err", err))
		}
		logging.Debug("agent: runTool done", attrs...)
	}
	var result string
	var err error
	if a.tools == nil {
		result = fmt.Sprintf("error: no tools available (cannot call %q)", call.Name)
		err = errors.New("no tools available")
	} else {
		result, err = a.tools.Run(ctx, call.Name, json.RawMessage(call.Args))
		if err != nil {
			result = "error: " + err.Error()
		}
	}
	// Emit the full result before the model-visible truncation so the UI can show
	// the whole command output; the model still sees only the truncated form.
	a.emit(Event{Type: EventToolResult, Call: call, Text: result})
	logDone(len(result), err)
	if err != nil {
		return result
	}
	if a.toolResultBytes > 0 && int64(len(result)) > a.toolResultBytes {
		result = truncateUTF8(result, a.toolResultBytes) + "\n[result truncated]"
	}
	return result
}

// requestContext derives a per-request context with the configured timeout, if
// any, so a single slow provider call cannot stall a turn indefinitely.
func (a *Agent) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	if a.requestTimeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, a.requestTimeout)
}

func (a *Agent) fail(err error) error {
	if errors.Is(err, context.Canceled) {
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
	if a.observer != nil {
		a.observer.ObserveHistory(a.id, a.typ, items)
	}
}

// trimHistory bounds completed transcript retention. It runs only at legal
// turn boundaries, never between a function call and its output. The newest
// suffix is retained; if the newest item alone exceeds the byte limit, its
// dynamic text is truncated so the configured bound still holds.
func (a *Agent) trimHistory() {
	if a.transcriptItems <= 0 && a.transcriptBytes <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	start := len(a.history)
	var bytesUsed int64
	for start > 0 {
		if a.transcriptItems > 0 && len(a.history)-start >= a.transcriptItems {
			break
		}
		n := transcriptItemBytes(a.history[start-1])
		if a.transcriptBytes > 0 && bytesUsed+n > a.transcriptBytes {
			break
		}
		start--
		bytesUsed += n
	}

	if start == len(a.history) && len(a.history) > 0 {
		// Keep a bounded form of the newest item rather than erasing the entire
		// conversation. At a completed boundary this is normally the final
		// assistant message and remains valid provider input on its own.
		last := a.history[len(a.history)-1]
		last = truncateTranscriptItem(last, a.transcriptBytes)
		a.history = []llm.Item{last}
		return
	}
	a.history = append([]llm.Item(nil), a.history[start:]...)
	a.removeOrphanedToolItemsLocked()
}

func (a *Agent) removeOrphanedToolItemsLocked() {
	calls := make(map[string]bool)
	outputs := make(map[string]bool)
	for _, it := range a.history {
		switch it.Type {
		case llm.ItemFunctionCall:
			calls[it.CallID] = true
		case llm.ItemFunctionCallOutput:
			outputs[it.CallID] = true
		}
	}
	filtered := a.history[:0]
	for _, it := range a.history {
		if it.Type == llm.ItemFunctionCall && !outputs[it.CallID] {
			continue
		}
		if it.Type == llm.ItemFunctionCallOutput && !calls[it.CallID] {
			continue
		}
		filtered = append(filtered, it)
	}
	a.history = filtered
}

func transcriptItemBytes(it llm.Item) int64 {
	return int64(len(it.Content) + len(it.Args))
}

func truncateTranscriptItem(it llm.Item, max int64) llm.Item {
	if max <= 0 {
		return it
	}
	it.Content = truncateUTF8(it.Content, max)
	remaining := max - int64(len(it.Content))
	it.Args = truncateUTF8(it.Args, remaining)
	return it
}

func (a *Agent) setState(s State) {
	a.mu.Lock()
	// Closed is terminal: a cancellation racing with Close (whose fail maps to
	// idle) must not resurrect a closed agent.
	if a.state == StateClosed {
		a.mu.Unlock()
		return
	}
	a.state = s
	a.mu.Unlock()
	a.emit(Event{Type: EventState, State: s})
	if a.onStateChange != nil {
		a.onStateChange()
	}
}

func (a *Agent) emit(e Event) {
	e.AgentID = a.id
	if observer, ok := a.observer.(ActivityObserver); ok {
		observer.ObserveEvent(a.id, a.typ, e)
	}
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

// truncateUTF8 truncates s to at most n bytes without splitting a UTF-8 rune.
func truncateUTF8(s string, n int64) string {
	if int64(len(s)) <= n {
		return s
	}
	cut := int(n)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
