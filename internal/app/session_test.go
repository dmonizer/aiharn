package app

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/config"
	"aiharn/internal/llm"
	"aiharn/internal/sessions"
	testllm "aiharn/internal/testutil/llm"
	"aiharn/internal/tools"
)

// fakeBuild builds one fake Runtime per session through the BuildRuntime seam:
// a real agent.Manager with a real (scripted) top-level agent, but no
// transports, model providers, or tools.
type fakeBuild struct {
	mu        sync.Mutex
	built     int
	closed    int
	failNext  int
	client    func() llm.Client
	observers []agent.HistoryObserver
}

func (b *fakeBuild) build(_ context.Context, _ *config.Config, opts Options) (*Runtime, error) {
	b.mu.Lock()
	b.built++
	b.observers = append(b.observers, opts.Observer)
	client := b.client
	fail := b.failNext > 0
	if fail {
		b.failNext--
	}
	b.mu.Unlock()
	if fail {
		return nil, errors.New("fake: build failed")
	}
	if client == nil {
		client = func() llm.Client { return &testllm.FakeClient{Script: replies(4)} }
	}
	mgr := agent.NewManager(agent.ManagerOptions{
		SubagentTypes: []tools.SubagentType{{Name: "coder"}},
		Builder: func(_ context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
			return agent.New(agent.Spec{
				ID: spec.ID, Type: spec.Type, Depth: spec.Depth, CallerID: spec.CallerID,
				Client:   &testllm.FakeClient{Script: replies(4)},
				Observer: opts.Observer,
			}), nil
		},
	})
	top := agent.New(agent.Spec{
		ID: "main", Type: "main", AllowSubagents: true, Client: client(),
		Observer: opts.Observer,
		Cleanup: func() {
			b.mu.Lock()
			b.closed++
			b.mu.Unlock()
		},
	})
	if err := mgr.RegisterTop(top); err != nil {
		return nil, err
	}
	return &Runtime{
		Manager: mgr,
		Agent:   top,
		Gate:    approval.NewGate(approval.ModeAsk),
		Summary: Summary{AgentType: "main", Model: "model", ModelName: "m", Channel: "local", Approval: "ask"},
	}, nil
}

func (b *fakeBuild) closedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *fakeBuild) observerList() []agent.HistoryObserver {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]agent.HistoryObserver(nil), b.observers...)
}

// replies returns n scripted completed turns.
func replies(n int) [][]llm.Event {
	out := make([][]llm.Event, 0, n)
	for i := range n {
		out = append(out, []llm.Event{{Type: llm.EventCompleted, Items: []llm.Item{
			{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: fmt.Sprintf("reply %d", i+1)},
		}}})
	}
	return out
}

// counterIDs is a deterministic session id generator for tests.
func counterIDs() func() string {
	var n int64
	return func() string { return fmt.Sprintf("s%d", atomic.AddInt64(&n, 1)) }
}

func newTestManager(t *testing.T, opts SessionManagerOptions) *SessionManager {
	t.Helper()
	if opts.Config == nil {
		opts.Config = &config.Config{}
	}
	if opts.BuildRuntime == nil {
		opts.BuildRuntime = (&fakeBuild{}).build
	}
	m, err := NewSessionManager(context.Background(), opts)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown() })
	return m
}

func mustCreate(t *testing.T, m *SessionManager, name string) sessions.Handle {
	t.Helper()
	h, err := m.Create(context.Background(), name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return h
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func historyContains(items []llm.Item, content string) bool {
	for _, it := range items {
		if it.Content == content {
			return true
		}
	}
	return false
}

func TestSessionManagerDefault(t *testing.T) {
	m := newTestManager(t, SessionManagerOptions{})

	d := m.Default()
	if d == nil {
		t.Fatal("no default session")
	}
	if d.ID() != "default" || d.Name() != "Default" {
		t.Fatalf("default = id %q name %q", d.ID(), d.Name())
	}
	if d.Model() != "model" || d.Channel() != "local" {
		t.Fatalf("default model/channel = %q/%q", d.Model(), d.Channel())
	}
	if d.Agent() == nil || d.Agent().ID() != "main" {
		t.Fatalf("default agent = %v", d.Agent())
	}
	if d.Manager() == nil || d.Gate() == nil {
		t.Fatal("default session is missing its manager or gate")
	}
	if d.CreatedAt().IsZero() {
		t.Fatal("default session has no creation time")
	}
	if d.Queued() != 0 || d.LastError() != "" {
		t.Fatalf("fresh default session: queued=%d lastError=%q", d.Queued(), d.LastError())
	}
	for _, id := range []string{"", "default"} {
		h, ok := m.Lookup(id)
		if !ok || h.ID() != "default" {
			t.Fatalf("Lookup(%q) = %v, %v", id, h, ok)
		}
	}
	if h, ok := m.Lookup("missing"); ok || h != nil {
		t.Fatalf("Lookup(missing) = %v, %v", h, ok)
	}
	list := m.List()
	if len(list) != 1 || list[0].ID() != "default" {
		t.Fatalf("List() = %v", list)
	}
	if rt := m.DefaultRuntime(); rt == nil || rt.Agent.ID() != "main" {
		t.Fatalf("DefaultRuntime() = %v", rt)
	}
}

func TestSessionManagerCreateAutoNamesAndOrder(t *testing.T) {
	m := newTestManager(t, SessionManagerOptions{ID: counterIDs()})
	ctx := context.Background()

	first, err := m.Create(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Name() != "Session 1" || first.ID() != "s1" {
		t.Fatalf("first = id %q name %q", first.ID(), first.Name())
	}
	named := mustCreate(t, m, "  Research  ")
	if named.Name() != "Research" {
		t.Fatalf("named session = %q", named.Name())
	}
	second, err := m.Create(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Name() != "Session 2" {
		t.Fatalf("second auto name = %q", second.Name())
	}

	list := m.List()
	wantIDs := []string{"default", first.ID(), named.ID(), second.ID()}
	wantNames := []string{"Default", "Session 1", "Research", "Session 2"}
	if len(list) != len(wantIDs) {
		t.Fatalf("List() = %v", list)
	}
	for i, h := range list {
		if h.ID() != wantIDs[i] || h.Name() != wantNames[i] {
			t.Fatalf("List()[%d] = id %q name %q, want id %q name %q", i, h.ID(), h.Name(), wantIDs[i], wantNames[i])
		}
	}
}

func TestSessionManagerLimit(t *testing.T) {
	ctx := context.Background()

	t.Run("sequential", func(t *testing.T) {
		m := newTestManager(t, SessionManagerOptions{MaxSessions: 3, ID: counterIDs()})
		mustCreate(t, m, "one")
		mustCreate(t, m, "two")
		if _, err := m.Create(ctx, "three"); !errors.Is(err, sessions.ErrLimitReached) {
			t.Fatalf("third Create err = %v, want ErrLimitReached", err)
		}
		if got := len(m.List()); got != 3 {
			t.Fatalf("List() length = %d, want 3 (default counts)", got)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		const max = 5
		m := newTestManager(t, SessionManagerOptions{MaxSessions: max, ID: counterIDs()})
		var wg sync.WaitGroup
		var created int64
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := m.Create(context.Background(), "concurrent")
				switch {
				case err == nil:
					atomic.AddInt64(&created, 1)
				case !errors.Is(err, sessions.ErrLimitReached):
					t.Errorf("Create err = %v", err)
				}
			}()
		}
		wg.Wait()
		if got := int(atomic.LoadInt64(&created)); got != max-1 {
			t.Fatalf("created %d sessions, want %d", got, max-1)
		}
		if got := len(m.List()); got != max {
			t.Fatalf("List() length = %d, want %d", got, max)
		}
	})
}

func TestSessionManagerInvalidNames(t *testing.T) {
	m := newTestManager(t, SessionManagerOptions{ID: counterIDs()})
	ctx := context.Background()

	invalid := []string{"", "   ", strings.Repeat("a", sessions.NameLimitBytes+1), "bad\nname", "bad\tname"}
	for _, name := range invalid {
		if _, err := sessions.ValidateName(name); !errors.Is(err, sessions.ErrNameInvalid) {
			t.Errorf("ValidateName(%q) err = %v, want ErrNameInvalid", name, err)
		}
	}
	// A non-empty invalid name is rejected by Create as well; only the empty
	// string triggers auto-naming.
	for _, name := range []string{"   ", strings.Repeat("a", sessions.NameLimitBytes+1), "bad\nname"} {
		if _, err := m.Create(ctx, name); !errors.Is(err, sessions.ErrNameInvalid) {
			t.Errorf("Create(%q) err = %v, want ErrNameInvalid", name, err)
		}
	}
	if got := len(m.List()); got != 1 {
		t.Fatalf("a rejected Create registered a session: %d sessions", got)
	}

	if got, err := sessions.ValidateName("  Research  "); err != nil || got != "Research" {
		t.Fatalf("ValidateName = %q, %v", got, err)
	}
	limit := strings.Repeat("a", sessions.NameLimitBytes)
	if got, err := sessions.ValidateName(limit); err != nil || got != limit {
		t.Fatalf("ValidateName at limit = %q, %v", got, err)
	}
	if h := mustCreate(t, m, limit); h.Name() != limit {
		t.Fatalf("Create at limit name = %q", h.Name())
	}
}

func TestSessionManagerFailedBuildReleasesSlot(t *testing.T) {
	b := &fakeBuild{}
	m := newTestManager(t, SessionManagerOptions{MaxSessions: 2, BuildRuntime: b.build, ID: counterIDs()})
	ctx := context.Background()

	b.mu.Lock()
	b.failNext = 1
	b.mu.Unlock()
	if _, err := m.Create(ctx, "boom"); err == nil {
		t.Fatal("Create with a failing BuildRuntime returned no error")
	}
	if got := len(m.List()); got != 1 {
		t.Fatalf("a failed build registered a session: %d sessions", got)
	}
	if _, ok := m.Lookup("s1"); ok {
		t.Fatal("failed build registered its session id")
	}
	// The reserved slot is released, so the next Create fits under the cap.
	h := mustCreate(t, m, "ok")
	if h.Name() != "ok" {
		t.Fatalf("name = %q", h.Name())
	}
	if _, err := m.Create(ctx, "third"); !errors.Is(err, sessions.ErrLimitReached) {
		t.Fatalf("third Create err = %v, want ErrLimitReached", err)
	}
}

func TestSessionManagerRename(t *testing.T) {
	m := newTestManager(t, SessionManagerOptions{ID: counterIDs()})
	h := mustCreate(t, m, "Original")

	renamed, err := m.Rename(h.ID(), "  New name  ")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID() != h.ID() || renamed.Name() != "New name" || h.Name() != "New name" {
		t.Fatalf("renamed = id %q name %q (handle %q)", renamed.ID(), renamed.Name(), h.Name())
	}
	if _, err := m.Rename("missing", "x"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Rename(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := m.Rename(h.ID(), "   "); !errors.Is(err, sessions.ErrNameInvalid) {
		t.Fatalf("Rename invalid err = %v, want ErrNameInvalid", err)
	}
	if h.Name() != "New name" {
		t.Fatalf("rejected rename changed the name to %q", h.Name())
	}
	if got, err := m.Rename("default", "Home"); err != nil || got.Name() != "Home" {
		t.Fatalf("Rename(default) = %v, %v", got, err)
	}
}

func TestSessionManagerClose(t *testing.T) {
	ctx := context.Background()
	b := &fakeBuild{}
	m := newTestManager(t, SessionManagerOptions{BuildRuntime: b.build, ID: counterIDs()})
	h := mustCreate(t, m, "Work")

	for _, id := range []string{"default", ""} {
		if err := m.Close(ctx, id); !errors.Is(err, sessions.ErrDefault) {
			t.Fatalf("Close(%q) err = %v, want ErrDefault", id, err)
		}
	}
	if err := m.Close(ctx, h.ID()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := m.Lookup(h.ID()); ok {
		t.Fatal("closed session is still registered")
	}
	if list := m.List(); len(list) != 1 || list[0].ID() != "default" {
		t.Fatalf("List() after Close = %v", list)
	}
	if got := b.closedCount(); got != 1 {
		t.Fatalf("runtime closes = %d, want 1", got)
	}
	if err := m.Close(ctx, h.ID()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := m.Close(ctx, "missing"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Close(missing) err = %v, want ErrNotFound", err)
	}
	if err := h.Submit(ctx, "", "late"); !errors.Is(err, sessions.ErrClosed) {
		t.Fatalf("Submit on a closed session err = %v, want ErrClosed", err)
	}
	if m.Default() == nil {
		t.Fatal("default session disappeared")
	}
}

func TestSessionSubmitRunsTurn(t *testing.T) {
	m := newTestManager(t, SessionManagerOptions{ID: counterIDs()})
	ctx := context.Background()
	d := m.Default()

	if err := d.Submit(ctx, "", "hello"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, "the queued turn to run", func() bool {
		return historyContains(d.Agent().History(), "hello") && historyContains(d.Agent().History(), "reply 1")
	})
	if d.LastError() != "" {
		t.Fatalf("LastError = %q, want empty", d.LastError())
	}
	// The top-level agent's own id also addresses the queue.
	if err := d.Submit(ctx, d.Agent().ID(), "again"); err != nil {
		t.Fatalf("Submit(top id): %v", err)
	}
	waitFor(t, "the second turn to run", func() bool {
		return historyContains(d.Agent().History(), "again")
	})
}

func TestSessionSubmitRecordsLastError(t *testing.T) {
	b := &fakeBuild{client: func() llm.Client { return &testllm.FakeClient{SetupErr: errors.New("boom")} }}
	m := newTestManager(t, SessionManagerOptions{BuildRuntime: b.build, ID: counterIDs()})
	d := m.Default()

	if err := d.Submit(context.Background(), "", "hi"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, "the failing turn to be recorded", func() bool { return d.LastError() != "" })
	if !strings.Contains(d.LastError(), "boom") {
		t.Fatalf("LastError = %q, want it to mention boom", d.LastError())
	}
}

func TestSessionSubmitQueueFull(t *testing.T) {
	bc := newBlockingClient()
	b := &fakeBuild{client: func() llm.Client { return bc }}
	m := newTestManager(t, SessionManagerOptions{BuildRuntime: b.build, ID: counterIDs()})
	ctx := context.Background()
	d := m.Default()

	if err := d.Submit(ctx, "", "first"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	select {
	case <-bc.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first turn never started")
	}
	for i := range defaultQueueSize {
		if err := d.Submit(ctx, "", "fill"); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	if got := d.Queued(); got != defaultQueueSize {
		t.Fatalf("Queued = %d, want %d", got, defaultQueueSize)
	}
	if err := d.Submit(ctx, "", "overflow"); !errors.Is(err, sessions.ErrQueueFull) {
		t.Fatalf("Submit on a full queue err = %v, want ErrQueueFull", err)
	}
	close(bc.release)
}

func TestSessionSubmitRoutesToSubagent(t *testing.T) {
	m := newTestManager(t, SessionManagerOptions{ID: counterIDs()})
	ctx := context.Background()
	h := mustCreate(t, m, "Chat")

	subID, err := h.Manager().SpawnSubagent(ctx, h.Agent().ID(), "coder", "", "initial task")
	if err != nil {
		t.Fatalf("SpawnSubagent: %v", err)
	}
	if err := h.Submit(ctx, subID, "hello sub"); err != nil {
		t.Fatalf("Submit(subagent): %v", err)
	}
	if err := h.Submit(ctx, "missing", "x"); !errors.Is(err, sessions.ErrAgentNotFound) {
		t.Fatalf("Submit(unknown agent) err = %v, want ErrAgentNotFound", err)
	}
	waitFor(t, "the subagent to receive its message", func() bool {
		sub := h.Manager().Agent(subID)
		return sub != nil && historyContains(sub.History(), "hello sub")
	})

	// Session.Submit is the HTTP console's path: a person typed this, so the
	// subagent must record it as human, while the model's spawn prompt stays
	// agent-authored.
	hist := h.Manager().Agent(subID).History()
	var human *llm.Item
	for i := range hist {
		if hist[i].Content == "hello sub" {
			human = &hist[i]
		}
	}
	if human == nil || human.Origin != llm.OriginHuman {
		t.Fatalf("submit-delivered message = %+v, want human origin: %+v", human, hist)
	}
	for _, it := range hist {
		if it.Content == "initial task" && it.Origin != llm.OriginAgent {
			t.Fatalf("spawn prompt origin = %q, want %q", it.Origin, llm.OriginAgent)
		}
	}
}

func TestSessionManagerShutdown(t *testing.T) {
	base := runtime.NumGoroutine()
	b := &fakeBuild{}
	m := newTestManager(t, SessionManagerOptions{BuildRuntime: b.build, ID: counterIDs()})
	for i := range 3 {
		mustCreate(t, m, fmt.Sprintf("s%d", i))
	}

	if err := m.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := m.Shutdown(); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if got := b.closedCount(); got != 4 {
		t.Fatalf("closed runtimes = %d, want 4 (default plus three)", got)
	}
	if _, err := m.Create(context.Background(), "after"); !errors.Is(err, sessions.ErrClosed) {
		t.Fatalf("Create after Shutdown err = %v, want ErrClosed", err)
	}
	waitForGoroutines(t, base)
}

func waitForGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n := runtime.NumGoroutine(); n <= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d, want <= %d", runtime.NumGoroutine(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockingClient blocks a turn until release is closed, so tests can fill a
// session's queue deterministically.
type blockingClient struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingClient() *blockingClient {
	return &blockingClient{started: make(chan struct{}, 1), release: make(chan struct{})}
}

func (c *blockingClient) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	out := make(chan llm.Event, 1)
	go func() {
		defer close(out)
		select {
		case <-c.release:
			out <- llm.Event{Type: llm.EventCompleted, Items: []llm.Item{
				{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "reply"},
			}}
		case <-ctx.Done():
		}
	}()
	return out, nil
}

// fakeTranscript records the header and closure of a per-session transcript.
type fakeTranscript struct {
	mu     sync.Mutex
	meta   sessions.TranscriptMeta
	closed bool
	items  int
}

func (f *fakeTranscript) ObserveHistory(_, _ string, items []llm.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items += len(items)
}

func (f *fakeTranscript) SetMeta(m sessions.TranscriptMeta) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meta = m
}

func (f *fakeTranscript) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeTranscript) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakeObserver stands in for a shared Options.Observer.
type fakeObserver struct{}

func (fakeObserver) ObserveHistory(string, string, []llm.Item) {}

func TestSessionManagerTranscriptFactory(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	made := map[string]*fakeTranscript{}
	b := &fakeBuild{}
	m := newTestManager(t, SessionManagerOptions{
		BuildRuntime: b.build,
		ID:           counterIDs(),
		Build: Options{NewTranscript: func(id, name string) (sessions.Transcript, error) {
			tr := &fakeTranscript{}
			mu.Lock()
			made[id] = tr
			mu.Unlock()
			return tr, nil
		}},
	})

	h := mustCreate(t, m, "Chat")
	mu.Lock()
	def, chat := made["default"], made[h.ID()]
	mu.Unlock()
	if def == nil || chat == nil {
		t.Fatalf("transcripts created for %v", made)
	}

	// The factory's transcript is handed to Build as the session observer.
	observers := b.observerList()
	if len(observers) != 2 {
		t.Fatalf("observers = %d, want 2", len(observers))
	}
	for _, o := range observers {
		if o != sessions.Transcript(def) && o != sessions.Transcript(chat) {
			t.Fatalf("Build received observer %v, want a session transcript", o)
		}
	}

	if got := def.meta; got.ID != "default" || got.Name != "Default" || got.Model != "model" ||
		got.AgentType != "main" || got.Channel != "local" || got.Approval != "ask" {
		t.Fatalf("default transcript meta = %+v", got)
	}
	if got := chat.meta; got.ID != h.ID() || got.Name != "Chat" || got.Model != "model" {
		t.Fatalf("chat transcript meta = %+v", got)
	}

	if err := m.Close(ctx, h.ID()); err != nil {
		t.Fatal(err)
	}
	if !chat.isClosed() {
		t.Fatal("closing a session left its transcript open")
	}
	if def.isClosed() {
		t.Fatal("closing one session closed another session's transcript")
	}
}

func TestSessionManagerSharedObserverFallback(t *testing.T) {
	shared := fakeObserver{}
	b := &fakeBuild{}
	m := newTestManager(t, SessionManagerOptions{
		BuildRuntime: b.build,
		ID:           counterIDs(),
		Build:        Options{Observer: shared},
	})
	mustCreate(t, m, "Chat")

	for i, o := range b.observerList() {
		if o != agent.HistoryObserver(shared) {
			t.Fatalf("Build %d received observer %v, want the shared observer", i, o)
		}
	}
	if err := m.Shutdown(); err != nil {
		t.Fatal(err)
	}
}
