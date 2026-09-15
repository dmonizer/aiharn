package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/llm"
)

type fakeAgent struct {
	mu      sync.Mutex
	history []llm.Item
	turns   chan string
}

func newFakeAgent() *fakeAgent          { return &fakeAgent{turns: make(chan string, 8)} }
func (a *fakeAgent) ID() string         { return "main" }
func (a *fakeAgent) Type() string       { return "main" }
func (a *fakeAgent) State() agent.State { return agent.StateIdle }
func (a *fakeAgent) History() []llm.Item {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Item(nil), a.history...)
}
func (a *fakeAgent) Turn(_ context.Context, input string) error {
	a.mu.Lock()
	a.history = append(a.history,
		llm.Item{Type: llm.ItemMessage, Role: llm.RoleUser, Content: input},
		llm.Item{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "reply"},
	)
	a.mu.Unlock()
	a.turns <- input
	return nil
}

func testServer(t *testing.T, token string, origins ...string) (*Server, *fakeAgent, *approval.Gate) {
	t.Helper()
	a := newFakeAgent()
	g := approval.NewGate(approval.ModeAsk)
	s, err := New(Config{
		Listen: "127.0.0.1:0", Token: token, AllowedOrigins: origins,
		Agent: a, Gate: g, Session: SessionInfo{Model: "model", Channel: "channel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = g.Close() })
	return s, a, g
}

func request(t *testing.T, s *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestSessionSnapshotAndAuthentication(t *testing.T) {
	s, a, _ := testServer(t, "secret")
	a.history = []llm.Item{
		{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "hello"},
		{Type: llm.ItemFunctionCall, CallID: "c1", Name: "execute_command", Args: `{"command":"pwd"}`},
	}
	if got := request(t, s, http.MethodGet, "/api/v1/session", "", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", got)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	r.Header.Set("Authorization", "secret")
	malformed := httptest.NewRecorder()
	s.Handler().ServeHTTP(malformed, r)
	if malformed.Code != http.StatusUnauthorized {
		t.Fatalf("malformed authorization status = %d", malformed.Code)
	}
	w := request(t, s, http.MethodGet, "/api/v1/session", "secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var response sessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.APIVersion != 1 || response.Session.AgentID != "main" || response.Session.Model != "model" {
		t.Fatalf("session = %#v", response)
	}
	if len(response.Messages) != 2 || response.Messages[1].Name != "execute_command" {
		t.Fatalf("messages = %#v", response.Messages)
	}
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

func TestApprovalCanBeResolvedRemotely(t *testing.T) {
	s, _, g := testServer(t, "")
	decision := make(chan approval.Decision, 1)
	go func() {
		d, _ := g.Check(context.Background(), approval.Request{
			ToolName: "execute_command", Command: "whoami", Args: `{"command":"whoami"}`,
		})
		decision <- d
	}()
	req := <-g.Pending()

	w := request(t, s, http.MethodGet, "/api/v1/session", "", nil)
	var snapshot sessionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &snapshot)
	if len(snapshot.PendingApprovals) != 1 || snapshot.PendingApprovals[0].ID != req.ID {
		t.Fatalf("approvals = %#v", snapshot.PendingApprovals)
	}

	w = request(t, s, http.MethodPost, "/api/v1/approvals/"+req.ID, "", map[string]string{"decision": "approve"})
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

	r = httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disallowed origin status = %d", w.Code)
	}
}

func TestRemoteListenRequiresToken(t *testing.T) {
	g := approval.NewGate(approval.ModeAsk)
	defer g.Close()
	s, err := New(Config{Listen: "0.0.0.0:7331", Agent: newFakeAgent(), Gate: g})
	if s != nil {
		_ = s.Close()
	}
	if err == nil {
		t.Fatal("expected token requirement")
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
