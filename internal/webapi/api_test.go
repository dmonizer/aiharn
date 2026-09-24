package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/authfile"
	"aiharn/internal/llm"
	"aiharn/internal/sessions"
	"aiharn/internal/tools"

	testllm "aiharn/internal/testutil/llm"
)

// fakeAgent is the top-level agent of a fake session.
type fakeAgent struct {
	id   string
	name string
	typ  string
	mu   sync.Mutex
	// history, turns, state and turnErr are mutable and guarded by mu.
	history []llm.Item
	turns   chan string
	state   agent.State
	turnErr error
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{id: "main", typ: "main", turns: make(chan string, 8), state: agent.StateIdle}
}

func (a *fakeAgent) ID() string { return a.id }

// Name mirrors *agent.Agent: the display name falls back to the id so it is
// never empty.
func (a *fakeAgent) Name() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.name != "" {
		return a.name
	}
	return a.id
}

func (a *fakeAgent) setName(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.name = name
}

func (a *fakeAgent) Type() string {
	return a.typ
}

func (a *fakeAgent) State() agent.State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

func (a *fakeAgent) History() []llm.Item {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Item(nil), a.history...)
}

func (a *fakeAgent) Turn(_ context.Context, input string) error {
	a.mu.Lock()
	err := a.turnErr
	if err == nil {
		a.history = append(a.history,
			llm.Item{Type: llm.ItemMessage, Role: llm.RoleUser, Content: input},
			llm.Item{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "reply"},
		)
	}
	a.mu.Unlock()
	a.turns <- input
	return err
}

func (a *fakeAgent) setState(state agent.State) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = state
}

// fakeHandle is one in-memory session. Its top-level agent is an interface so a
// test can back it with either fakeAgent or a real *agent.Agent.
type fakeHandle struct {
	id        string
	name      string
	createdAt time.Time
	agent     sessions.Agent
	manager   *agent.Manager
	gate      *approval.Gate
	model     string
	channel   string
	channels  []sessions.Channel

	mu        sync.Mutex
	queued    int
	lastError string
	closed    bool
	queueFull bool
}

func (h *fakeHandle) ID() string           { return h.id }
func (h *fakeHandle) CreatedAt() time.Time { return h.createdAt }
func (h *fakeHandle) Model() string        { return h.model }
func (h *fakeHandle) Channel() string      { return h.channel }

func (h *fakeHandle) SetChannel(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return sessions.ErrClosed
	}
	for _, c := range h.channels {
		if c.Name == name {
			h.channel = name
			return nil
		}
	}
	return sessions.ErrChannelNotFound
}
func (h *fakeHandle) Agent() sessions.Agent   { return h.agent }
func (h *fakeHandle) Manager() *agent.Manager { return h.manager }
func (h *fakeHandle) Gate() *approval.Gate    { return h.gate }

func (h *fakeHandle) Name() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.name
}

func (h *fakeHandle) setName(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.name = name
}

func (h *fakeHandle) Queued() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.queued
}

func (h *fakeHandle) LastError() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastError
}

func (h *fakeHandle) Cancel(_ context.Context) (int, error) {
	h.mu.Lock()
	closed := h.closed
	affected := h.queued
	h.queued = 0
	h.mu.Unlock()
	if closed {
		return 0, sessions.ErrClosed
	}
	return affected, nil
}

func (h *fakeHandle) Submit(ctx context.Context, agentID, content string) error {
	h.mu.Lock()
	closed, full := h.closed, h.queueFull
	h.mu.Unlock()
	if closed {
		return sessions.ErrClosed
	}
	if full {
		return sessions.ErrQueueFull
	}
	if agentID != "" && agentID != h.agent.ID() {
		if h.manager == nil {
			return sessions.ErrAgentNotFound
		}
		if err := h.manager.SendSubagentMessage(ctx, h.agent.ID(), agentID, content, llm.OriginHuman); err != nil {
			return sessions.ErrAgentNotFound
		}
		return nil
	}
	return h.agent.Turn(ctx, content)
}

// fakeStore is the sessions.Store the API is tested against.
type fakeStore struct {
	mu        sync.Mutex
	list      []*fakeHandle
	channels  []sessions.Channel
	createErr error
	max       int
	seq       int
}

func (s *fakeStore) Default() sessions.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) == 0 {
		return nil
	}
	return s.list[0]
}

func (s *fakeStore) List() []sessions.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sessions.Handle, 0, len(s.list))
	for _, handle := range s.list {
		out = append(out, handle)
	}
	return out
}

func (s *fakeStore) Channels() []sessions.Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sessions.Channel(nil), s.channels...)
}

func (s *fakeStore) Lookup(id string) (sessions.Handle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" && len(s.list) > 0 {
		return s.list[0], true
	}
	for _, handle := range s.list {
		if handle.id == id {
			return handle, true
		}
	}
	return nil, false
}

func (s *fakeStore) Create(ctx context.Context, name string) (sessions.Handle, error) {
	s.mu.Lock()
	if s.createErr != nil {
		err := s.createErr
		s.mu.Unlock()
		return nil, err
	}
	// app.SessionManager validates the name before the cap, so a bad name is
	// reported as such even when every slot is taken.
	if name != "" {
		trimmed, err := sessions.ValidateName(name)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		name = trimmed
	}
	if s.max > 0 && len(s.list) >= s.max {
		s.mu.Unlock()
		return nil, sessions.ErrLimitReached
	}
	if name == "" {
		s.seq++
		name = fmt.Sprintf("Session %d", s.seq+1)
	}
	s.seq++
	source := s.list[0]
	created := &fakeHandle{
		id: fmt.Sprintf("session-%d", s.seq), name: name, createdAt: time.Now(),
		agent: newFakeAgent(), manager: source.manager, gate: approval.NewGate(approval.ModeAsk),
		model: source.model, channel: source.channel, channels: s.channels,
	}
	s.list = append(s.list, created)
	s.mu.Unlock()
	return created, nil
}

func (s *fakeStore) Rename(id, name string) (sessions.Handle, error) {
	trimmed, err := sessions.ValidateName(name)
	if err != nil {
		return nil, err
	}
	handle, ok := s.Lookup(id)
	if !ok {
		return nil, sessions.ErrNotFound
	}
	handle.(*fakeHandle).setName(trimmed)
	return handle, nil
}

func (s *fakeStore) Close(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || (len(s.list) > 0 && s.list[0].id == id) {
		return sessions.ErrDefault
	}
	for i, handle := range s.list {
		if handle.id == id {
			s.list = append(s.list[:i], s.list[i+1:]...)
			handle.mu.Lock()
			handle.closed = true
			handle.mu.Unlock()
			return nil
		}
	}
	return sessions.ErrNotFound
}

type harness struct {
	server *Server
	store  *fakeStore
	agent  *fakeAgent
	gate   *approval.Gate
}

func newHarness(t *testing.T, _ string, origins ...string) *harness {
	t.Helper()
	authPath := testAuthFile(t)
	a := newFakeAgent()
	g := approval.NewGate(approval.ModeAsk)
	store := &fakeStore{
		channels: []sessions.Channel{{Name: "channel", Type: "ssh"}, {Name: "local", Type: "local"}},
		list: []*fakeHandle{{
			id: "default", name: "Default", createdAt: time.Now(),
			agent: a, gate: g, model: "model", channel: "channel",
			channels: []sessions.Channel{{Name: "channel", Type: "ssh"}, {Name: "local", Type: "local"}},
		}},
	}
	s, err := New(Config{
		Listen: "127.0.0.1:0", AuthFile: authPath, AllowedOrigins: origins, Sessions: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = g.Close() })
	return &harness{server: s, store: store, agent: a, gate: g}
}

func testAuthFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users")
	if err := authfile.Set(path, "alice", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	return path
}

// addSession appends a second session with its own agent, manager, and gate.
func (h *harness) addSession(t *testing.T, id, name string) *fakeHandle {
	t.Helper()
	gate := approval.NewGate(approval.ModeAsk)
	t.Cleanup(func() { _ = gate.Close() })
	handle := &fakeHandle{
		id: id, name: name, createdAt: time.Now(),
		agent: newFakeAgent(), gate: gate, model: "other-model", channel: "channel",
		channels: h.store.channels,
	}
	h.store.mu.Lock()
	h.store.list = append(h.store.list, handle)
	h.store.mu.Unlock()
	return handle
}

func testServer(t *testing.T, token string, origins ...string) (*Server, *fakeAgent, *approval.Gate) {
	t.Helper()
	h := newHarness(t, token, origins...)
	return h.server, h.agent, h.gate
}

func request(t *testing.T, s *Server, method, path, password string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if password != "unauthenticated" {
		if password == "" {
			password = "secret"
		}
		r.SetBasicAuth("alice", password)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func decodeSession(t *testing.T, w *httptest.ResponseRecorder) sessionResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var response sessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeList(t *testing.T, w *httptest.ResponseRecorder) sessionListResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var response sessionListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestSessionSnapshotAndAuthentication(t *testing.T) {
	s, a, _ := testServer(t, "secret")
	a.history = []llm.Item{
		{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "hello"},
		{Type: llm.ItemFunctionCall, CallID: "c1", Name: "execute_command", Args: `{"command":"pwd"}`},
	}
	if got := request(t, s, http.MethodGet, "/api/v1/session", "unauthenticated", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", got)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	r.Header.Set("Authorization", "secret")
	malformed := httptest.NewRecorder()
	s.Handler().ServeHTTP(malformed, r)
	if malformed.Code != http.StatusUnauthorized {
		t.Fatalf("malformed authorization status = %d", malformed.Code)
	}
	if got := request(t, s, http.MethodGet, "/api/v1/session", "wrong", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	r.SetBasicAuth("unknown", "secret")
	missingUser := httptest.NewRecorder()
	s.Handler().ServeHTTP(missingUser, r)
	if missingUser.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user status = %d", missingUser.Code)
	}
	response := decodeSession(t, request(t, s, http.MethodGet, "/api/v1/session", "secret", nil))
	if response.APIVersion != 1 || response.Session.AgentID != "main" || response.Session.Model != "model" {
		t.Fatalf("session = %#v", response)
	}
	// The single-session defaults of v1 are preserved: no session_id means the
	// default session.
	if response.Session.ID != "default" || response.Session.Name != "Default" ||
		response.Session.CreatedAt.IsZero() {
		t.Fatalf("session identity = %#v", response.Session)
	}
	if response.Session.Channel != "channel" {
		t.Fatalf("channel = %q", response.Session.Channel)
	}
	if len(response.Messages) != 2 || response.Messages[1].Name != "execute_command" {
		t.Fatalf("messages = %#v", response.Messages)
	}
}

func TestSessionMessagesCarryOrigin(t *testing.T) {
	s, a, _ := testServer(t, "")
	a.history = []llm.Item{
		{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "typed by a person", Origin: llm.OriginHuman},
		{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "[subagent coder (7)] done", Origin: llm.OriginAgent},
		{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "reply"},
		{Type: llm.ItemFunctionCall, CallID: "c1", Name: "execute_command", Args: `{"command":"pwd"}`},
		{Type: llm.ItemFunctionCallOutput, CallID: "c1", Content: "out"},
	}
	w := request(t, s, http.MethodGet, "/api/v1/session", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	// Inspect the raw JSON so an omitted `origin` is distinguishable from an
	// explicit value; decoding into message would hide the difference.
	var raw struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Messages) != 5 {
		t.Fatalf("messages = %+v", raw.Messages)
	}
	if got := raw.Messages[0]["origin"]; got != "human" {
		t.Fatalf("human message origin = %v, want human: %+v", got, raw.Messages[0])
	}
	if got := raw.Messages[1]["origin"]; got != "agent" {
		t.Fatalf("agent message origin = %v, want agent: %+v", got, raw.Messages[1])
	}
	for i := 2; i < len(raw.Messages); i++ {
		if _, ok := raw.Messages[i]["origin"]; ok {
			t.Fatalf("message %d must omit origin: %+v", i, raw.Messages[i])
		}
	}
	// None of these items carried a delivery, so the field must be omitted on
	// every one of them.
	for i := range raw.Messages {
		if _, ok := raw.Messages[i]["delivery"]; ok {
			t.Fatalf("message %d must omit delivery: %+v", i, raw.Messages[i])
		}
	}
}

func TestMessagesIncludeSafeMarkdownHTMLForAgentText(t *testing.T) {
	messages := messagesFromHistory([]llm.Item{
		{Type: llm.ItemMessage, Role: llm.RoleUser, Origin: llm.OriginHuman, Content: "**literal human input**"},
		{Type: llm.ItemMessage, Role: llm.RoleUser, Origin: llm.OriginAgent, Content: "## Report\n\n- done"},
		{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "**bold** <script>alert(1)</script> [bad](javascript:alert(1))"},
	})
	if len(messages) != 3 {
		t.Fatalf("messages = %+v", messages)
	}
	if messages[0].HTML != "" {
		t.Fatalf("human-authored text must remain literal, html = %q", messages[0].HTML)
	}
	if !strings.Contains(messages[1].HTML, "<h2>Report</h2>") ||
		!strings.Contains(messages[1].HTML, "<li>done</li>") {
		t.Fatalf("agent Markdown was not rendered: %q", messages[1].HTML)
	}
	if !strings.Contains(messages[2].HTML, "<strong>bold</strong>") {
		t.Fatalf("assistant Markdown was not rendered: %q", messages[2].HTML)
	}
	if strings.Contains(messages[2].HTML, "<script>") ||
		!strings.Contains(messages[2].HTML, "&lt;script&gt;") {
		t.Fatalf("raw HTML was not escaped in safe mode: %q", messages[2].HTML)
	}
	if strings.Contains(strings.ToLower(messages[2].HTML), "javascript:") {
		t.Fatalf("active link scheme survived sanitization: %q", messages[2].HTML)
	}
}

// blockingStreamClient blocks a provider request until its context is
// cancelled, so a subagent stays running and holds no pending inbox work.
type blockingStreamClient struct{}

func (blockingStreamClient) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	out := make(chan llm.Event)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

// TestSessionMessagesCarryDelivery checks the wire shape of the delivery object
// and that agent-to-agent queued messages are always reported.
func TestSessionMessagesCarryDelivery(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{
		MaxAgents: 8,
		Builder: func(_ context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
			return agent.New(agent.Spec{
				ID: spec.ID, Type: spec.Type, Depth: spec.Depth, CallerID: spec.CallerID,
				Client: blockingStreamClient{},
			}), nil
		},
	})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	top := agent.New(agent.Spec{ID: "main", Type: "main", AllowSubagents: true, Client: blockingStreamClient{}})
	if err := mgr.RegisterTop(top); err != nil {
		t.Fatal(err)
	}
	subID, err := mgr.SpawnSubagent(context.Background(), "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for mgr.Agent(subID).State() != agent.StateRunning && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if mgr.Agent(subID).State() != agent.StateRunning {
		t.Fatalf("subagent state = %v, want running", mgr.Agent(subID).State())
	}
	// A message from the descendant to its caller stays pending in main's inbox.
	if err := mgr.SendAgentMessage(context.Background(), subID, "main", "status?", llm.KindMessage); err != nil {
		t.Fatal(err)
	}

	a := newFakeAgent()
	a.history = []llm.Item{
		{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "to sub", Origin: llm.OriginAgent,
			Delivery: &llm.Delivery{From: "main", To: "coder-1", Direction: llm.DirectionDown, Kind: llm.KindMessage}},
		{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "reply"},
		{Type: llm.ItemFunctionCall, CallID: "c1", Name: "execute_command", Args: `{"command":"pwd"}`},
	}
	g := approval.NewGate(approval.ModeAsk)
	store := &fakeStore{list: []*fakeHandle{{
		id: "default", name: "Default", createdAt: time.Now(),
		agent: a, manager: mgr, gate: g, model: "model", channel: "channel",
	}}}
	srv, err := New(Config{Listen: "127.0.0.1:0", AuthFile: testAuthFile(t), Sessions: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(); _ = g.Close() })

	w := request(t, srv, http.MethodGet, "/api/v1/session", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var raw struct {
		Messages             []map[string]any `json:"messages"`
		PendingAgentMessages []map[string]any `json:"pending_agent_messages"`
		Capabilities         []any            `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}

	// The message item carries the nested delivery object.
	if len(raw.Messages) != 3 {
		t.Fatalf("messages = %+v", raw.Messages)
	}
	d, ok := raw.Messages[0]["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("message 0 missing delivery: %+v", raw.Messages[0])
	}
	if d["from"] != "main" || d["to"] != "coder-1" || d["direction"] != "down" || d["kind"] != "message" {
		t.Fatalf("delivery = %+v", d)
	}
	// Assistant and tool items omit both origin and delivery.
	for i := 1; i < len(raw.Messages); i++ {
		if _, ok := raw.Messages[i]["delivery"]; ok {
			t.Fatalf("message %d must omit delivery: %+v", i, raw.Messages[i])
		}
		if _, ok := raw.Messages[i]["origin"]; ok {
			t.Fatalf("message %d must omit origin: %+v", i, raw.Messages[i])
		}
	}

	// The queued agent message is reported.
	if len(raw.PendingAgentMessages) != 1 {
		t.Fatalf("pending_agent_messages = %+v", raw.PendingAgentMessages)
	}
	pm := raw.PendingAgentMessages[0]
	if pm["from"] != subID || pm["to"] != "main" || pm["direction"] != "up" || pm["kind"] != "message" || pm["content"] != "status?" {
		t.Fatalf("pending message = %+v", pm)
	}

	assertCapability(t, raw.Capabilities, "agent_messages")
}

// TestPendingAgentMessagesAlwaysPresent asserts the field is an empty array, not
// null and not omitted, when nothing is queued.
func TestPendingAgentMessagesAlwaysPresent(t *testing.T) {
	s, _, _ := testServer(t, "")
	w := request(t, s, http.MethodGet, "/api/v1/session", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	value, ok := raw["pending_agent_messages"]
	if !ok {
		t.Fatalf("pending_agent_messages key missing: %v", raw)
	}
	if value == nil {
		t.Fatalf("pending_agent_messages is null, want []")
	}
	list, ok := value.([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("pending_agent_messages = %v, want []", value)
	}
}

func TestSessionIncludesCompletedReasoning(t *testing.T) {
	s, a, _ := testServer(t, "")
	a.mu.Lock()
	a.history = append(a.history,
		llm.Item{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "question"},
		llm.Item{Type: llm.ItemReasoning, Content: "checking the answer"},
		llm.Item{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "answer"},
	)
	a.mu.Unlock()
	w := request(t, s, http.MethodGet, "/api/v1/session", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Messages []struct{ Type, Content string } `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 || body.Messages[1].Type != "reasoning" || body.Messages[1].Content != "checking the answer" {
		t.Fatalf("messages = %+v", body.Messages)
	}
}

type limitTestTool struct{}

func (limitTestTool) Definition() llm.ToolDefinition                       { return llm.ToolDefinition{Name: "limit_test"} }
func (limitTestTool) Run(context.Context, json.RawMessage) (string, error) { return "ok", nil }

func TestWebToolLimitDecision(t *testing.T) {
	h := newHarness(t, "")
	reg := tools.New()
	if err := reg.Register(limitTestTool{}); err != nil {
		t.Fatal(err)
	}
	client := &testllm.FakeClient{Script: [][]llm.Event{
		{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemFunctionCall, CallID: "c1", Name: "limit_test", Args: "{}"}}}},
		{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemFunctionCall, CallID: "c2", Name: "limit_test", Args: "{}"}}}},
	}}
	a := agent.New(agent.Spec{ID: "main", Type: "main", Client: client, Tools: reg, ToolcallsPerTurn: 1})
	h.store.list[0].agent = a
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Turn(ctx, "go") }()
	var pending *agent.ToolLimitRequest
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		pending = a.PendingToolLimit()
		if pending != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if pending == nil {
		t.Fatal("tool-limit prompt was not created")
	}
	w := request(t, h.server, http.MethodGet, "/api/v1/session", "", nil)
	var snapshot struct {
		Pending []agent.ToolLimitRequest `json:"pending_tool_limits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil || len(snapshot.Pending) != 1 || snapshot.Pending[0].ID != pending.ID {
		t.Fatalf("snapshot = %+v, err=%v", snapshot, err)
	}
	w = request(t, h.server, http.MethodPost, "/api/v1/tool-limits/"+pending.ID, "", map[string]any{
		"agent_id": "main", "decision": "stop",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("decision status = %d: %s", w.Code, w.Body.String())
	}
	if err := <-done; err != nil || a.State() != agent.StateIdle {
		t.Fatalf("turn err=%v state=%v", err, a.State())
	}
}

type liveReasoningClient struct{}

func (liveReasoningClient) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	out := make(chan llm.Event, 1)
	go func() {
		defer close(out)
		out <- llm.Event{Type: llm.EventReasoningDelta, Text: "working through it"}
		<-ctx.Done()
	}()
	return out, nil
}

func TestSessionIncludesLiveReasoning(t *testing.T) {
	h := newHarness(t, "")
	a := agent.New(agent.Spec{ID: "main", Type: "main", Client: liveReasoningClient{},
		ThinkingTimeout: time.Second, RequestTimeout: time.Second})
	h.store.list[0].agent = a
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Turn(ctx, "go") }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if status := a.LiveReasoning(); status != nil && status.Text != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	w := request(t, h.server, http.MethodGet, "/api/v1/session", "", nil)
	var snapshot struct {
		Live *agent.ReasoningStatus `json:"live_reasoning"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil || snapshot.Live == nil ||
		!snapshot.Live.Active || snapshot.Live.Text != "working through it" {
		t.Fatalf("live reasoning = %+v, err=%v", snapshot.Live, err)
	}
}

func assertCapability(t *testing.T, caps []any, want string) {
	t.Helper()
	for _, c := range caps {
		if c == want {
			return
		}
	}
	t.Fatalf("capabilities = %v, want %q", caps, want)
}

func TestMessageIsQueuedAndRun(t *testing.T) {
	s, a, _ := testServer(t, "")
	w := request(t, s, http.MethodPost, "/api/v1/messages", "", map[string]string{"content": "from web"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	select {
	case got := <-a.turns:
		if got != "from web" {
			t.Fatalf("turn input = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("queued turn was not run")
	}
}

func TestSubagentSessionAndMessage(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{
		SubagentTypes: []tools.SubagentType{{Name: "coder"}},
		Builder: func(_ context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
			return agent.New(agent.Spec{
				ID: spec.ID, Name: spec.Name, Type: spec.Type, Depth: spec.Depth, CallerID: spec.CallerID,
				Client: &testllm.FakeClient{Script: [][]llm.Event{
					{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "first reply"}}}},
					{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "second reply"}}}},
				}},
			}), nil
		},
	})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	top := agent.New(agent.Spec{ID: "main", Type: "main", AllowSubagents: true})
	if err := mgr.RegisterTop(top); err != nil {
		t.Fatal(err)
	}
	subID, err := mgr.SpawnSubagent(context.Background(), top.ID(), "coder", "researcher", "initial task")
	if err != nil {
		t.Fatal(err)
	}

	gate := approval.NewGate(approval.ModeAsk)
	t.Cleanup(func() { _ = gate.Close() })
	store := &fakeStore{list: []*fakeHandle{{
		id: "default", name: "Default", createdAt: time.Now(),
		agent: top, manager: mgr, gate: gate, model: "main-model", channel: "local",
	}}}
	s, err := New(Config{Listen: "127.0.0.1:0", AuthFile: testAuthFile(t), Sessions: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	response := request(t, s, http.MethodGet, "/api/v1/session", "", nil)
	mainSnapshot := decodeSession(t, response)
	if len(mainSnapshot.Agents) != 2 || mainSnapshot.Agents[1].ID != subID ||
		mainSnapshot.Agents[1].Name != "researcher" || mainSnapshot.Agents[0].Name == "" ||
		mainSnapshot.Agents[1].Type != "coder" || mainSnapshot.Agents[1].State == "" ||
		mainSnapshot.Session.AgentID != "main" {
		t.Fatalf("main snapshot = %+v", mainSnapshot)
	}
	// Every roster entry carries a display name on the wire, the spawned
	// subagent keeps its custom name, and an unnamed top-level agent falls back
	// to its id. The capability that promises this is advertised.
	var rawRoster struct {
		Agents       []map[string]any `json:"agents"`
		Capabilities []any            `json:"capabilities"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rawRoster); err != nil {
		t.Fatal(err)
	}
	if len(rawRoster.Agents) != 2 {
		t.Fatalf("raw agents = %+v", rawRoster.Agents)
	}
	for i, entry := range rawRoster.Agents {
		if name, ok := entry["name"].(string); !ok || name == "" {
			t.Fatalf("agents[%d] missing a non-empty name on the wire: %+v", i, entry)
		}
	}
	if rawRoster.Agents[0]["name"] != "main" {
		t.Fatalf("top-level name = %v, want main", rawRoster.Agents[0]["name"])
	}
	if rawRoster.Agents[1]["name"] != "researcher" {
		t.Fatalf("subagent name = %v, want researcher", rawRoster.Agents[1]["name"])
	}
	assertCapability(t, rawRoster.Capabilities, "agent_names")
	if len(mainSnapshot.Messages) != 0 {
		t.Fatalf("main history contains subagent activity: %+v", mainSnapshot.Messages)
	}

	url := "/api/v1/session?agent_id=" + subID
	waitForSubagent := func(want string) sessionResponse {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if got := request(t, s, http.MethodGet, url, "", nil); got.Code != http.StatusOK {
				t.Fatalf("subagent status = %d: %s", got.Code, got.Body.String())
			} else {
				var snapshot sessionResponse
				if err := json.Unmarshal(got.Body.Bytes(), &snapshot); err != nil {
					t.Fatal(err)
				}
				for _, message := range snapshot.Messages {
					if message.Content == want {
						return snapshot
					}
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("subagent history never contained %q", want)
		return sessionResponse{}
	}
	initial := waitForSubagent("first reply")
	if initial.Session.AgentID != subID || initial.Session.AgentType != "coder" {
		t.Fatalf("selected session = %+v", initial.Session)
	}

	w := request(t, s, http.MethodPost, "/api/v1/messages", "", map[string]string{
		"agent_id": subID, "content": "from web",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("message status = %d: %s", w.Code, w.Body.String())
	}
	_ = waitForSubagent("second reply")
	mainSnapshot = decodeSession(t, request(t, s, http.MethodGet, "/api/v1/session", "", nil))
	if len(mainSnapshot.Messages) != 0 {
		t.Fatalf("main history contains subagent activity: %+v", mainSnapshot.Messages)
	}
	if got := request(t, s, http.MethodGet, "/api/v1/session?agent_id=missing", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown session status = %d", got.Code)
	}
	if got := request(t, s, http.MethodPost, "/api/v1/messages", "", map[string]string{
		"agent_id": "missing", "content": "no",
	}); got.Code != http.StatusNotFound {
		t.Fatalf("unknown message status = %d", got.Code)
	}
}

// TestTopLevelAgentNameIsReported asserts the top-level roster entry is built
// from the agent's display name, not its id: an unnamed agent falls back to the
// id, and a named one reports the name.
func TestTopLevelAgentNameIsReported(t *testing.T) {
	h := newHarness(t, "")

	// Unnamed: like *agent.Agent, the fake agent reports its id.
	snapshot := decodeSession(t, request(t, h.server, http.MethodGet, "/api/v1/session", "", nil))
	if len(snapshot.Agents) != 1 {
		t.Fatalf("agents = %+v", snapshot.Agents)
	}
	if snapshot.Agents[0].Name != "main" {
		t.Fatalf("unnamed top-level name = %q, want main", snapshot.Agents[0].Name)
	}

	// Named: the display name wins over the id, which only holds if the entry is
	// built from Name() rather than ID().
	h.agent.setName("Main Agent")
	snapshot = decodeSession(t, request(t, h.server, http.MethodGet, "/api/v1/session", "", nil))
	if len(snapshot.Agents) != 1 {
		t.Fatalf("agents = %+v", snapshot.Agents)
	}
	if snapshot.Agents[0].ID != "main" || snapshot.Agents[0].Name != "Main Agent" {
		t.Fatalf("named top-level entry = %+v, want name Main Agent for id main", snapshot.Agents[0])
	}
}

// TestSubagentNameFallsBackToID asserts a subagent spawned without a name still
// reports a non-empty display name equal to its id, so a UI never has to handle
// an empty name.
func TestSubagentNameFallsBackToID(t *testing.T) {
	mgr := agent.NewManager(agent.ManagerOptions{
		SubagentTypes: []tools.SubagentType{{Name: "coder"}},
		Builder: func(_ context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
			// Name is copied through, but the manager defaulted an empty one to
			// the id, so agent.Name() is non-empty either way.
			return agent.New(agent.Spec{
				ID: spec.ID, Name: spec.Name, Type: spec.Type, Depth: spec.Depth, CallerID: spec.CallerID,
				Client: &testllm.FakeClient{},
			}), nil
		},
	})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	top := agent.New(agent.Spec{ID: "main", Type: "main", AllowSubagents: true})
	if err := mgr.RegisterTop(top); err != nil {
		t.Fatal(err)
	}
	subID, err := mgr.SpawnSubagent(context.Background(), top.ID(), "coder", "", "unnamed task")
	if err != nil {
		t.Fatal(err)
	}
	if subID != "coder-1" {
		t.Fatalf("subagent id = %q, want coder-1", subID)
	}

	gate := approval.NewGate(approval.ModeAsk)
	t.Cleanup(func() { _ = gate.Close() })
	store := &fakeStore{list: []*fakeHandle{{
		id: "default", name: "Default", createdAt: time.Now(),
		agent: top, manager: mgr, gate: gate, model: "main-model", channel: "local",
	}}}
	s, err := New(Config{Listen: "127.0.0.1:0", AuthFile: testAuthFile(t), Sessions: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	w := request(t, s, http.MethodGet, "/api/v1/session", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var raw struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Agents) != 2 {
		t.Fatalf("agents = %+v", raw.Agents)
	}
	name, ok := raw.Agents[1]["name"].(string)
	if !ok || name == "" {
		t.Fatalf("unnamed subagent name = %v, want %q", raw.Agents[1]["name"], subID)
	}
	if name != subID {
		t.Fatalf("unnamed subagent name = %q, want its id %q", name, subID)
	}
	// The top-level agent is unnamed too, so it also reports its id.
	if raw.Agents[0]["name"] != "main" {
		t.Fatalf("top-level name = %v, want main", raw.Agents[0]["name"])
	}
}

func TestApprovalCanBeResolvedRemotely(t *testing.T) {
	s, _, g := testServer(t, "")
	decision := make(chan approval.Decision, 1)
	go func() {
		d, _ := g.Check(context.Background(), approval.Request{
			AgentID: "coder-7", AgentType: "coder",
			ToolName: "execute_command", Command: "whoami", Args: `{"command":"whoami"}`,
		})
		decision <- d
	}()
	req := <-g.Pending()

	snapshot := decodeSession(t, request(t, s, http.MethodGet, "/api/v1/session", "", nil))
	if len(snapshot.PendingApprovals) != 1 || snapshot.PendingApprovals[0].ID != req.ID {
		t.Fatalf("approvals = %#v", snapshot.PendingApprovals)
	}
	if got := snapshot.PendingApprovals[0]; got.AgentID != "coder-7" || got.AgentType != "coder" {
		t.Fatalf("approval agent = %+v", got)
	}

	w := request(t, s, http.MethodPost, "/api/v1/approvals/"+req.ID, "", map[string]string{"decision": "approve"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	select {
	case got := <-decision:
		if got != approval.DecisionApproved {
			t.Fatalf("decision = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("approval was not resolved")
	}
}

func TestApproveAllDoesNotLoosenForUnknownRequest(t *testing.T) {
	s, _, g := testServer(t, "")
	w := request(t, s, http.MethodPost, "/api/v1/approvals/missing", "", map[string]string{"decision": "approve_all"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if g.Mode() != approval.ModeAsk {
		t.Fatal("unknown approval loosened gate")
	}
}

func TestCORSIsExplicit(t *testing.T) {
	s, _, _ := testServer(t, "secret", "https://console.example")
	r := httptest.NewRequest(http.MethodOptions, "/api/v1/session", nil)
	r.Header.Set("Origin", "https://console.example")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent ||
		w.Header().Get("Access-Control-Allow-Origin") != "https://console.example" {
		t.Fatalf("allowed preflight = %d, headers %#v", w.Code, w.Header())
	}
	// Session management needs the new verbs on the preflight response.
	methods := w.Header().Get("Access-Control-Allow-Methods")
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		if !bytes.Contains([]byte(methods), []byte(method)) {
			t.Fatalf("preflight methods %q missing %s", methods, method)
		}
	}

	r = httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	r.Header.Set("Origin", "https://evil.example")
	r.SetBasicAuth("alice", "secret")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disallowed origin status = %d", w.Code)
	}
}

func TestAPIRequiresAuthFile(t *testing.T) {
	s, err := New(Config{Listen: "0.0.0.0:7331", Sessions: &fakeStore{}})
	if s != nil {
		_ = s.Close()
	}
	if err == nil {
		t.Fatal("expected auth file requirement")
	}
	if _, err := New(Config{Listen: "127.0.0.1:0"}); err == nil {
		t.Fatal("expected session store requirement")
	}
}

func TestInputValidation(t *testing.T) {
	s, _, _ := testServer(t, "")
	tests := []struct {
		name string
		body any
	}{
		{"empty", map[string]string{"content": "  "}},
		{"unknown field", map[string]string{"content": "hello", "extra": "no"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, s, http.MethodPost, "/api/v1/messages", "", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestStartAndClose(t *testing.T) {
	s, _, _ := testServer(t, "")
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if s.Addr() == "" {
		t.Fatal("empty bound address")
	}
}

func TestSessionsListAndDescriptors(t *testing.T) {
	h := newHarness(t, "")
	h.addSession(t, "ab12cd34", "Research")
	w := request(t, h.server, http.MethodGet, "/api/v1/sessions", "", nil)
	list := decodeList(t, w)
	if list.APIVersion != 1 || list.DefaultSessionID != "default" {
		t.Fatalf("list header = %+v", list)
	}
	if len(list.Sessions) != 2 {
		t.Fatalf("sessions = %+v", list.Sessions)
	}
	if !list.Sessions[0].Default || list.Sessions[1].Default {
		t.Fatalf("default flags = %+v", list.Sessions)
	}
	if list.Sessions[1].ID != "ab12cd34" || list.Sessions[1].Name != "Research" ||
		list.Sessions[1].Model != "other-model" {
		t.Fatalf("second descriptor = %+v", list.Sessions[1])
	}
	for _, descriptor := range list.Sessions {
		if len(descriptor.Agents) != 1 || descriptor.Agents[0].ID != "main" {
			t.Fatalf("descriptor roster = %+v", descriptor.Agents)
		}
	}
	// The descriptor roster must match GET /session's roster exactly, or the
	// sidebar and the agent panel would disagree.
	snapshot := decodeSession(t, request(t, h.server, http.MethodGet,
		"/api/v1/session?session_id=ab12cd34", "", nil))
	if len(snapshot.Agents) != len(list.Sessions[1].Agents) ||
		snapshot.Agents[0].ID != list.Sessions[1].Agents[0].ID {
		t.Fatalf("roster drift: %+v vs %+v", snapshot.Agents, list.Sessions[1].Agents)
	}
	if snapshot.Session.ID != "ab12cd34" || snapshot.Session.Name != "Research" {
		t.Fatalf("selected session = %+v", snapshot.Session)
	}

	// GET one descriptor.
	single := request(t, h.server, http.MethodGet, "/api/v1/sessions/ab12cd34", "", nil)
	var descriptor sessionDescriptor
	if err := json.Unmarshal(single.Body.Bytes(), &descriptor); err != nil {
		t.Fatal(err)
	}
	if single.Code != http.StatusOK || descriptor.ID != "ab12cd34" {
		t.Fatalf("descriptor = %d %+v", single.Code, descriptor)
	}
	if got := request(t, h.server, http.MethodGet, "/api/v1/sessions/nope", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown descriptor status = %d", got.Code)
	}
	if got := request(t, h.server, http.MethodGet, "/api/v1/session?session_id=nope", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown session_id status = %d", got.Code)
	}
}

func TestSessionCreateRenameClose(t *testing.T) {
	h := newHarness(t, "")
	h.store.max = 3 // the default session occupies one slot

	named := request(t, h.server, http.MethodPost, "/api/v1/sessions", "", map[string]string{"name": "Research"})
	if named.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", named.Code, named.Body.String())
	}
	var created sessionDescriptor
	if err := json.Unmarshal(named.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "Research" || created.Default || created.ID == "" || created.ID == "default" {
		t.Fatalf("created = %+v", created)
	}

	// No body at all: the store auto-names the session.
	unnamed := request(t, h.server, http.MethodPost, "/api/v1/sessions", "", nil)
	if unnamed.Code != http.StatusCreated {
		t.Fatalf("unnamed create status = %d: %s", unnamed.Code, unnamed.Body.String())
	}

	// The cap counts the default session.
	capped := request(t, h.server, http.MethodPost, "/api/v1/sessions", "", map[string]string{"name": "Too many"})
	if capped.Code != http.StatusConflict {
		t.Fatalf("cap status = %d: %s", capped.Code, capped.Body.String())
	}

	invalid := request(t, h.server, http.MethodPost, "/api/v1/sessions", "", map[string]string{"name": "  "})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid name status = %d: %s", invalid.Code, invalid.Body.String())
	}

	h.store.createErr = sessions.ErrClosed
	failed := request(t, h.server, http.MethodPost, "/api/v1/sessions", "", map[string]string{"name": "Nope"})
	if failed.Code != http.StatusConflict {
		t.Fatalf("store error status = %d: %s", failed.Code, failed.Body.String())
	}
	h.store.createErr = context.DeadlineExceeded
	failed = request(t, h.server, http.MethodPost, "/api/v1/sessions", "", map[string]string{"name": "Nope"})
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected error status = %d: %s", failed.Code, failed.Body.String())
	}
	h.store.createErr = nil

	renamed := request(t, h.server, http.MethodPatch, "/api/v1/sessions/"+created.ID, "", map[string]string{"name": "Renamed"})
	if renamed.Code != http.StatusOK {
		t.Fatalf("rename status = %d: %s", renamed.Code, renamed.Body.String())
	}
	var afterRename sessionDescriptor
	if err := json.Unmarshal(renamed.Body.Bytes(), &afterRename); err != nil {
		t.Fatal(err)
	}
	if afterRename.Name != "Renamed" {
		t.Fatalf("renamed = %+v", afterRename)
	}
	if got := request(t, h.server, http.MethodPatch, "/api/v1/sessions/nope", "", map[string]string{"name": "x"}); got.Code != http.StatusNotFound {
		t.Fatalf("unknown rename status = %d", got.Code)
	}
	if got := request(t, h.server, http.MethodPatch, "/api/v1/sessions/"+created.ID, "", map[string]string{"name": "   "}); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid rename status = %d", got.Code)
	}

	if got := request(t, h.server, http.MethodDelete, "/api/v1/sessions/default", "", nil); got.Code != http.StatusConflict {
		t.Fatalf("default close status = %d: %s", got.Code, got.Body.String())
	}
	closed := request(t, h.server, http.MethodDelete, "/api/v1/sessions/"+created.ID, "", nil)
	if closed.Code != http.StatusOK {
		t.Fatalf("close status = %d: %s", closed.Code, closed.Body.String())
	}
	if got := request(t, h.server, http.MethodGet, "/api/v1/session?session_id="+created.ID, "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("closed session status = %d", got.Code)
	}
	if got := request(t, h.server, http.MethodDelete, "/api/v1/sessions/nope", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown close status = %d", got.Code)
	}
	if got := request(t, h.server, http.MethodPut, "/api/v1/sessions", "", nil); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", got.Code)
	}
	if got := request(t, h.server, http.MethodDelete, "/api/v1/sessions", "", nil); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("collection delete status = %d", got.Code)
	}
}

func TestSessionsAreIsolatedFromEachOther(t *testing.T) {
	h := newHarness(t, "")
	other := h.addSession(t, "other", "Other")

	// A message addressed to the second session runs only that session's agent.
	w := request(t, h.server, http.MethodPost, "/api/v1/messages", "",
		map[string]string{"content": "to the other session", "session_id": "other"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	select {
	case got := <-other.agent.(*fakeAgent).turns:
		if got != "to the other session" {
			t.Fatalf("turn = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("message was not delivered to the addressed session")
	}
	select {
	case got := <-h.agent.turns:
		t.Fatalf("default session ran the other session's message: %q", got)
	default:
	}

	// Transcripts stay separate.
	first := decodeSession(t, request(t, h.server, http.MethodGet, "/api/v1/session", "", nil))
	second := decodeSession(t, request(t, h.server, http.MethodGet, "/api/v1/session?session_id=other", "", nil))
	if len(first.Messages) != 0 {
		t.Fatalf("default transcript = %+v", first.Messages)
	}
	if len(second.Messages) != 2 {
		t.Fatalf("other transcript = %+v", second.Messages)
	}

	// Unknown session ids are rejected rather than silently served by the
	// default session.
	if got := request(t, h.server, http.MethodPost, "/api/v1/messages", "",
		map[string]string{"content": "x", "session_id": "ghost"}); got.Code != http.StatusNotFound {
		t.Fatalf("unknown session message status = %d", got.Code)
	}

	// Approval ids are sequential per gate, so the same id exists in both
	// sessions and each decision must reach only the addressed gate.
	gates := []*approval.Gate{h.gate, other.gate}
	pending := make([]string, len(gates))
	for i, gate := range gates {
		gate := gate
		done := make(chan approval.Decision, 1)
		go func() {
			decision, _ := gate.Check(context.Background(), approval.Request{
				AgentID: "main", AgentType: "main", ToolName: "execute_command", Command: "whoami",
			})
			done <- decision
		}()
		pending[i] = (<-gate.Pending()).ID
	}
	if pending[0] != pending[1] {
		t.Fatalf("expected the same per-gate approval id, got %v", pending)
	}
	w = request(t, h.server, http.MethodPost, "/api/v1/approvals/"+pending[0], "",
		map[string]string{"decision": "approve_all", "session_id": "other"})
	if w.Code != http.StatusOK {
		t.Fatalf("approval status = %d: %s", w.Code, w.Body.String())
	}
	if other.gate.Mode() != approval.ModeAllowAll {
		t.Fatalf("addressed gate mode = %v", other.gate.Mode())
	}
	if h.gate.Mode() != approval.ModeAsk {
		t.Fatal("approve_all leaked into the other session's gate")
	}
	if got := len(h.gate.PendingRequests()); got != 1 {
		t.Fatalf("default session approvals = %d, want 1 still pending", got)
	}
	if got := len(other.gate.PendingRequests()); got != 0 {
		t.Fatalf("other session approvals = %d, want 0", got)
	}
}

func TestAgentsKeyIsAlwaysPresent(t *testing.T) {
	h := newHarness(t, "")
	rawKeys := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var payload map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	// A session without subagents still reports its top-level agent.
	payload := rawKeys(request(t, h.server, http.MethodGet, "/api/v1/session", "", nil))
	agents, ok := payload["agents"].([]any)
	if !ok {
		t.Fatalf("agents key missing from the session payload: %v", payload)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %v", agents)
	}
	if _, present := payload["agents_error"]; present {
		t.Fatalf("unexpected agents_error: %v", payload["agents_error"])
	}

	// A roster failure degrades the payload instead of discarding it: the
	// transcript, approvals and top-level agent survive. A client must be able
	// to tell this apart from "no subagents".
	broken := &fakeHandle{
		id: "broken", name: "Broken", createdAt: time.Now(),
		// The manager knows only "main", so listing this agent's subagents fails.
		agent: &fakeAgent{id: "ghost", typ: "main", state: agent.StateIdle, turns: make(chan string, 1)},
		manager: agent.NewManager(agent.ManagerOptions{
			Builder: func(context.Context, agent.SpawnSpec) (*agent.Agent, error) {
				return agent.New(agent.Spec{Client: &testllm.FakeClient{}}), nil
			},
		}),
		gate: approval.NewGate(approval.ModeAsk), model: "model", channel: "channel",
	}
	h.store.mu.Lock()
	h.store.list = append(h.store.list, broken)
	h.store.mu.Unlock()

	payload = rawKeys(request(t, h.server, http.MethodGet, "/api/v1/session?session_id=broken", "", nil))
	agents, ok = payload["agents"].([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("agents = %v", payload["agents"])
	}
	if message, _ := payload["agents_error"].(string); message == "" {
		t.Fatalf("agents_error missing: %v", payload)
	}
	if _, present := payload["messages"]; !present {
		t.Fatal("transcript dropped with the roster")
	}

	// The list route carries the same guarantee.
	listRaw := rawKeys(request(t, h.server, http.MethodGet, "/api/v1/sessions", "", nil))
	sessionsRaw, _ := listRaw["sessions"].([]any)
	if len(sessionsRaw) != 2 {
		t.Fatalf("sessions = %v", listRaw["sessions"])
	}
	for _, entry := range sessionsRaw {
		descriptor := entry.(map[string]any)
		roster, ok := descriptor["agents"].([]any)
		if !ok || len(roster) == 0 {
			t.Fatalf("descriptor without agents: %v", descriptor)
		}
	}
}

func TestMessagesReflectClosedAndFullSessions(t *testing.T) {
	h := newHarness(t, "")
	handle := h.addSession(t, "full", "Full")

	handle.mu.Lock()
	handle.queueFull = true
	handle.mu.Unlock()
	if got := request(t, h.server, http.MethodPost, "/api/v1/messages", "",
		map[string]string{"content": "x", "session_id": "full"}); got.Code != http.StatusConflict {
		t.Fatalf("full queue status = %d: %s", got.Code, got.Body.String())
	}

	handle.mu.Lock()
	handle.queueFull, handle.closed = false, true
	handle.mu.Unlock()
	if got := request(t, h.server, http.MethodPost, "/api/v1/messages", "",
		map[string]string{"content": "x", "session_id": "full"}); got.Code != http.StatusConflict {
		t.Fatalf("closed session status = %d: %s", got.Code, got.Body.String())
	}

	// A closed top-level agent is reported as a closed session.
	closed := h.addSession(t, "terminated", "Terminated")
	closed.agent.(*fakeAgent).setState(agent.StateClosed)
	snapshot := decodeSession(t, request(t, h.server, http.MethodGet,
		"/api/v1/session?session_id=terminated", "", nil))
	if snapshot.Session.State != "closed" {
		t.Fatalf("state = %q", snapshot.Session.State)
	}
}

func TestSessionChannelSwitchAndValidation(t *testing.T) {
	h := newHarness(t, "")

	// 1. GET /api/v1/session reports the configured channels, the channel_switch
	// capability, and the active channel.
	snapshot := decodeSession(t, request(t, h.server, http.MethodGet, "/api/v1/session", "", nil))
	if snapshot.Session.Channel != "channel" {
		t.Fatalf("channel = %q, want channel", snapshot.Session.Channel)
	}
	if len(snapshot.Channels) != 2 ||
		snapshot.Channels[0] != (channelDescriptor{Name: "channel", Type: "ssh"}) ||
		snapshot.Channels[1] != (channelDescriptor{Name: "local", Type: "local"}) {
		t.Fatalf("channels = %+v", snapshot.Channels)
	}
	var rawCaps struct {
		Capabilities []any `json:"capabilities"`
	}
	w := request(t, h.server, http.MethodGet, "/api/v1/session", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &rawCaps); err != nil {
		t.Fatal(err)
	}
	assertCapability(t, rawCaps.Capabilities, "channel_switch")

	// 2. POST /api/v1/session/channel switches the active channel and is echoed
	// by the handle and by the next session snapshot.
	w = request(t, h.server, http.MethodPost, "/api/v1/session/channel", "", map[string]string{"channel": "local"})
	if w.Code != http.StatusOK {
		t.Fatalf("switch status = %d: %s", w.Code, w.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["channel"] != "local" {
		t.Fatalf("response channel = %q, want local", payload["channel"])
	}
	if got := h.store.list[0].Channel(); got != "local" {
		t.Fatalf("handle channel = %q, want local", got)
	}
	snapshot = decodeSession(t, request(t, h.server, http.MethodGet, "/api/v1/session", "", nil))
	if snapshot.Session.Channel != "local" {
		t.Fatalf("follow-up channel = %q, want local", snapshot.Session.Channel)
	}

	// 3. Validation and method/body handling.
	validation := []struct {
		name   string
		method string
		body   any
		status int
		errMsg string
	}{
		{"empty channel", http.MethodPost, map[string]string{"channel": ""},
			http.StatusBadRequest, "channel must not be empty"},
		{"unknown channel", http.MethodPost, map[string]string{"channel": "ghost"},
			http.StatusNotFound, "channel not found"},
		{"unknown session", http.MethodPost, map[string]string{"channel": "local", "session_id": "ghost"},
			http.StatusNotFound, "session not found"},
		{"get", http.MethodGet, nil,
			http.StatusMethodNotAllowed, "method not allowed"},
		{"unknown field", http.MethodPost, map[string]string{"channel": "local", "extra": "no"},
			http.StatusBadRequest, "invalid JSON body"},
	}
	for _, tc := range validation {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, h.server, tc.method, "/api/v1/session/channel", "", tc.body)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			var payload map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["error"] != tc.errMsg {
				t.Fatalf("error = %q, want %q", payload["error"], tc.errMsg)
			}
		})
	}

	// 4. A closed session refuses the switch.
	closed := h.addSession(t, "closed", "Closed")
	closed.mu.Lock()
	closed.closed = true
	closed.mu.Unlock()
	w = request(t, h.server, http.MethodPost, "/api/v1/session/channel", "",
		map[string]string{"channel": "local", "session_id": "closed"})
	if w.Code != http.StatusConflict {
		t.Fatalf("closed session status = %d: %s", w.Code, w.Body.String())
	}
	var closedPayload map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &closedPayload); err != nil {
		t.Fatal(err)
	}
	if closedPayload["error"] != "session is closed" {
		t.Fatalf("error = %q, want session is closed", closedPayload["error"])
	}
}

func TestSessionCancelStopsWork(t *testing.T) {
	h := newHarness(t, "")

	// The session-cancel capability tells the console it can offer a Stop
	// button; the route must advertise it alongside the other capabilities.
	w := request(t, h.server, http.MethodGet, "/api/v1/session", "", nil)
	var rawCaps struct {
		Capabilities []any `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rawCaps); err != nil {
		t.Fatal(err)
	}
	assertCapability(t, rawCaps.Capabilities, "session_cancel")

	// A queued prompt is discarded and the response reports it as affected.
	h.store.list[0].queued = 3
	w = request(t, h.server, http.MethodPost, "/api/v1/session/cancel", "", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["stopped"] != true || payload["affected"] != float64(3) {
		t.Fatalf("payload = %#v, want stopped=true affected=3", payload)
	}
	if got := h.store.list[0].Queued(); got != 0 {
		t.Fatalf("queued after cancel = %d, want 0", got)
	}

	// Unknown session, wrong method, and closed session follow the same
	// validation rules as the other session routes.
	unknown := request(t, h.server, http.MethodPost, "/api/v1/session/cancel", "",
		map[string]string{"session_id": "ghost"})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown session status = %d: %s", unknown.Code, unknown.Body.String())
	}
	get := request(t, h.server, http.MethodGet, "/api/v1/session/cancel", "", nil)
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d: %s", get.Code, get.Body.String())
	}
	closed := h.addSession(t, "closed", "Closed")
	closed.mu.Lock()
	closed.closed = true
	closed.mu.Unlock()
	w = request(t, h.server, http.MethodPost, "/api/v1/session/cancel", "",
		map[string]string{"session_id": "closed"})
	if w.Code != http.StatusConflict {
		t.Fatalf("closed session status = %d: %s", w.Code, w.Body.String())
	}
}

func TestSessionApprovalModeSwitchesBothWays(t *testing.T) {
	h := newHarness(t, "")
	reportedMode := func() string {
		t.Helper()
		return decodeSession(t, request(t, h.server, http.MethodGet,
			"/api/v1/session", "", nil)).Session.ApprovalMode
	}
	toggle := func(mode string) {
		t.Helper()
		w := request(t, h.server, http.MethodPost, "/api/v1/session/approval", "",
			map[string]string{"mode": mode})
		if w.Code != http.StatusOK {
			t.Fatalf("toggle %q status = %d: %s", mode, w.Code, w.Body.String())
		}
		var payload map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		// The response echoes the gate's real state, not the request.
		if payload["mode"] != mode {
			t.Fatalf("response mode = %q, want %q", payload["mode"], mode)
		}
	}

	if got := reportedMode(); got != "ask" {
		t.Fatalf("initial approval_mode = %q, want ask", got)
	}
	// The human path may loosen: ask -> allow-all.
	toggle("allow-all")
	if got := h.gate.Mode(); got != approval.ModeAllowAll {
		t.Fatalf("gate mode after loosening = %v", got)
	}
	if got := reportedMode(); got != "allow-all" {
		t.Fatalf("approval_mode after loosening = %q, want allow-all", got)
	}
	// And tighten: allow-all -> ask. ApplyModelMode would refuse this, so a
	// passing toggle proves the handler uses the user-facing SetMode.
	toggle("ask")
	if got := h.gate.Mode(); got != approval.ModeAsk {
		t.Fatalf("gate mode after tightening = %v", got)
	}
	if got := reportedMode(); got != "ask" {
		t.Fatalf("approval_mode after tightening = %q, want ask", got)
	}
}

func TestSessionApprovalAppliesOnlyToAddressedSession(t *testing.T) {
	h := newHarness(t, "")
	other := h.addSession(t, "other", "Other")

	w := request(t, h.server, http.MethodPost, "/api/v1/session/approval", "",
		map[string]string{"mode": "allow-all", "session_id": "other"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := other.gate.Mode(); got != approval.ModeAllowAll {
		t.Fatalf("addressed gate mode = %v", got)
	}
	// The scope is the addressed session only.
	if got := h.gate.Mode(); got != approval.ModeAsk {
		t.Fatal("approval toggle leaked into the default session's gate")
	}
	untouched := decodeSession(t, request(t, h.server, http.MethodGet, "/api/v1/session", "", nil))
	if untouched.Session.ApprovalMode != "ask" {
		t.Fatalf("untouched approval_mode = %q, want ask", untouched.Session.ApprovalMode)
	}
	// The addressed session reports its own new mode.
	toggled := decodeSession(t, request(t, h.server, http.MethodGet,
		"/api/v1/session?session_id=other", "", nil))
	if toggled.Session.ApprovalMode != "allow-all" {
		t.Fatalf("addressed approval_mode = %q, want allow-all", toggled.Session.ApprovalMode)
	}
}

func TestSessionApprovalValidation(t *testing.T) {
	h := newHarness(t, "")
	h.addSession(t, "other", "Other")
	tests := []struct {
		name   string
		method string
		body   any
		status int
		errMsg string
	}{
		{"unknown session", http.MethodPost,
			map[string]string{"mode": "allow-all", "session_id": "ghost"},
			http.StatusNotFound, "session not found"},
		{"unknown mode", http.MethodPost, map[string]string{"mode": "banana"},
			http.StatusBadRequest, "mode must be ask or allow-all"},
		{"empty mode", http.MethodPost, map[string]string{"mode": ""},
			http.StatusBadRequest, "mode must be ask or allow-all"},
		{"unknown field", http.MethodPost, map[string]string{"mode": "ask", "extra": "no"},
			http.StatusBadRequest, "invalid JSON body"},
		{"get", http.MethodGet, nil,
			http.StatusMethodNotAllowed, "method not allowed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, h.server, tc.method, "/api/v1/session/approval", "", tc.body)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			var payload map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["error"] != tc.errMsg {
				t.Fatalf("error = %q, want %q", payload["error"], tc.errMsg)
			}
		})
	}
	// A rejected request must never move a gate.
	if got := h.gate.Mode(); got != approval.ModeAsk {
		t.Fatalf("default gate mode = %v after rejected requests", got)
	}
}

func TestCapabilitiesKeyIsAlwaysPresent(t *testing.T) {
	h := newHarness(t, "")
	h.addSession(t, "other", "Other")

	rawPayload := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var payload map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	assertCapabilities := func(t *testing.T, payload map[string]any) {
		t.Helper()
		caps, ok := payload["capabilities"].([]any)
		if !ok {
			t.Fatalf("capabilities key missing: %v", payload)
		}
		// Every response form must keep advertising the approval-mode route,
		// agent-name guarantee, and Markdown message representation.
		assertCapability(t, caps, "approval_mode")
		assertCapability(t, caps, "agent_names")
		assertCapability(t, caps, "markdown_html")
	}

	// GET /api/v1/session advertises the approval-mode route and agent names.
	assertCapabilities(t, rawPayload(
		request(t, h.server, http.MethodGet, "/api/v1/session", "", nil)))

	// So does every descriptor of GET /api/v1/sessions.
	list := rawPayload(request(t, h.server, http.MethodGet, "/api/v1/sessions", "", nil))
	descriptors, ok := list["sessions"].([]any)
	if !ok || len(descriptors) != 2 {
		t.Fatalf("sessions = %v", list["sessions"])
	}
	for _, entry := range descriptors {
		assertCapabilities(t, entry.(map[string]any))
	}

	// Create and rename reuse describe, so their payloads carry it too.
	assertCapabilities(t, rawPayload(
		request(t, h.server, http.MethodPost, "/api/v1/sessions", "", nil)))
	assertCapabilities(t, rawPayload(
		request(t, h.server, http.MethodPatch, "/api/v1/sessions/default", "",
			map[string]string{"name": "Renamed"})))
}
