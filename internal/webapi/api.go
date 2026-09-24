// Package webapi exposes the active Aiharn sessions as a versioned JSON API.
// It deliberately contains no frontend assets: browser clients can be hosted
// independently and can connect to one or many API instances.
//
// The server holds no conversation state of its own. Every session (its agent
// tree, approval gate, transcript, and message queue) belongs to the injected
// sessions.Store, so the same routes serve one session or many.
package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	markdown "github.com/codewandler/markdown"
	markdownhtml "github.com/codewandler/markdown/html"
	"golang.org/x/crypto/bcrypt"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/authfile"
	"aiharn/internal/llm"
	"aiharn/internal/logging"
	"aiharn/internal/sessions"
)

const (
	maxMessageBytes = 64 << 10
	// sessionItemPrefix is the subtree pattern for one session's routes.
	sessionItemPrefix = "/api/v1/sessions/"
)

// Config configures one API server. Sessions is required and owns every
// conversation session; the server keeps no state of its own beyond the
// listener.
type Config struct {
	Listen         string
	AuthFile       string
	AllowedOrigins []string
	Sessions       sessions.Store
}

// Server owns the HTTP listener and exposes the session store over JSON.
type Server struct {
	cfg      Config
	handler  http.Handler
	server   *http.Server
	listener net.Listener
	users    map[string]string

	wg sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

// New validates cfg and constructs a server. Start opens the configured
// listener; Handler can be used independently by another HTTP host.
func New(cfg Config) (*Server, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("webapi: session store is required")
	}
	if cfg.Listen == "" {
		return nil, errors.New("webapi: listen address is required")
	}
	if err := validateListen(cfg.Listen, cfg.AuthFile); err != nil {
		return nil, err
	}
	users, err := authfile.Load(cfg.AuthFile)
	if err != nil {
		return nil, fmt.Errorf("webapi: load API users: %w", err)
	}
	if err := validateOrigins(cfg.AllowedOrigins); err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, users: users}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sessions", s.handleSessions)
	mux.HandleFunc(sessionItemPrefix, s.handleSessionItem)
	mux.HandleFunc("/api/v1/session", s.handleSession)
	mux.HandleFunc("/api/v1/session/approval", s.handleSessionApproval)
	mux.HandleFunc("/api/v1/session/channel", s.handleSessionChannel)
	mux.HandleFunc("/api/v1/session/cancel", s.handleSessionCancel)
	mux.HandleFunc("/api/v1/messages", s.handleMessages)
	mux.HandleFunc("/api/v1/approvals/", s.handleApproval)
	mux.HandleFunc("/api/v1/tool-limits/", s.handleToolLimit)
	s.handler = s.requestLog(s.securityHeaders(s.cors(s.authenticate(mux))))
	return s, nil
}

// Handler returns the JSON-only HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Start opens the configured TCP listener and serves in the background.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("webapi: server is closed")
	}
	if s.listener != nil {
		return errors.New("webapi: server already started")
	}
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("webapi: listen on %s: %w", s.cfg.Listen, err)
	}
	s.listener = ln
	s.server = &http.Server{
		Handler: s.handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Debug("api server stopped", slog.String("error", err.Error()))
		}
	}()
	return nil
}

// Addr returns the bound listener address, or an empty string before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Close stops the listener and waits for in-flight requests. It does not close
// any session: those belong to the injected store.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv := s.server
	s.mu.Unlock()

	var err error
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = srv.Shutdown(ctx)
		cancel()
	}
	s.wg.Wait()
	return err
}

type channelDescriptor struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type sessionResponse struct {
	APIVersion int                 `json:"api_version"`
	Session    session             `json:"session"`
	Channels   []channelDescriptor `json:"channels"`
	// Agents is always present and always holds at least the top-level agent.
	// A client must be able to tell "this session has no subagents" apart from
	// "this server is too old to report agents at all": a stale binary that
	// omitted this field is what made the web console show only the main agent.
	Agents            []agentSummary           `json:"agents"`
	Messages          []message                `json:"messages"`
	LiveReasoning     *agent.ReasoningStatus   `json:"live_reasoning,omitempty"`
	PendingApprovals  []approvalRequest        `json:"pending_approvals"`
	PendingToolLimits []agent.ToolLimitRequest `json:"pending_tool_limits"`
	// PendingAgentMessages is always present (an empty array, never null): it
	// lists agent-to-agent messages queued in inboxes but not yet injected into
	// a history. A polling client has no event stream, so this is how it learns
	// a message is in flight; the empty array still lets it tell "none pending"
	// apart from "this server is too old to report them".
	PendingAgentMessages []pendingMessage `json:"pending_agent_messages"`
	QueuedMessages       int              `json:"queued_messages"`
	LastError            string           `json:"last_error,omitempty"`
	// AgentsError reports a failed subagent roster. The payload stays valid and
	// the transcript, approvals, and top-level agent are still returned.
	AgentsError string `json:"agents_error,omitempty"`
	// Capabilities is always present and lists the optional routes this server
	// implements, so a client can hide a feature a stale binary lacks instead of
	// probing the route and failing. See the capabilities helper.
	Capabilities []string `json:"capabilities"`
}

type agentSummary struct {
	ID string `json:"id"`
	// Name is the agent's display name. It is always non-empty: an agent
	// configured without a name reports its id instead, so a client renders
	// this field unconditionally and never has to fall back to the id itself.
	Name   string `json:"name"`
	Type   string `json:"type"`
	State  string `json:"state"`
	Depth  int    `json:"depth"`
	Paused bool   `json:"paused"`
}

type session struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	AgentID      string    `json:"agent_id"`
	AgentType    string    `json:"agent_type"`
	Model        string    `json:"model"`
	Channel      string    `json:"channel"`
	State        string    `json:"state"`
	ApprovalMode string    `json:"approval_mode"`
}

// sessionListResponse is the payload of GET /api/v1/sessions.
type sessionListResponse struct {
	APIVersion       int                 `json:"api_version"`
	DefaultSessionID string              `json:"default_session_id"`
	Sessions         []sessionDescriptor `json:"sessions"`
}

// sessionDescriptor describes one session without its transcript.
type sessionDescriptor struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	CreatedAt      time.Time      `json:"created_at"`
	Default        bool           `json:"default"`
	Model          string         `json:"model,omitempty"`
	Channel        string         `json:"channel,omitempty"`
	State          string         `json:"state"`
	Agents         []agentSummary `json:"agents"`
	AgentsError    string         `json:"agents_error,omitempty"`
	QueuedMessages int            `json:"queued_messages"`
	LastError      string         `json:"last_error,omitempty"`
	// Capabilities is always present, exactly as on GET /api/v1/session.
	Capabilities []string `json:"capabilities"`
}

type message struct {
	Type string `json:"type"`
	Role string `json:"role,omitempty"`
	// Origin records who authored a message item: "human" or "agent". It is set
	// only on message items, and lets a client tell a person's message apart
	// from agent-injected text such as a delivered subagent report. It is
	// omitted on assistant messages and on tool items.
	Origin string `json:"origin,omitempty"`
	// Delivery is set only on message items another agent injected, describing
	// who sent what to whom, in which direction (relative to the sender), and
	// why. It is omitted on assistant messages and on tool items.
	Delivery *delivery `json:"delivery,omitempty"`
	Content  string    `json:"content,omitempty"`
	// HTML is the sanitized markdown-go rendering of assistant-authored text.
	// Content remains the canonical source and keeps the API backward compatible.
	HTML      string `json:"html,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
}

// delivery mirrors llm.Delivery in the wire payload. direction is relative to
// the SENDER: "down" when an agent messages a descendant it spawned, "up" when
// it messages an ancestor (its caller).
type delivery struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Direction string `json:"direction"`
	Kind      string `json:"kind"`
}

// pendingMessage is one agent-authored message queued in an inbox but not yet
// injected into a history.
type pendingMessage struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Direction string `json:"direction"`
	Kind      string `json:"kind"`
	// Content is the queued text. It is named "content" to match a transcript
	// message item's body: a client renders both, and two names for the same
	// concept in one payload is a trap (the console silently read an empty
	// body when this was "text").
	Content string `json:"content"`
}

type approvalRequest struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	ToolName  string `json:"tool_name"`
	Command   string `json:"command,omitempty"`
	Args      any    `json:"arguments,omitempty"`
}

// approvalModeCapability is advertised in every "capabilities" list. Like the
// agents field, a client must be able to tell "this server supports switching
// the approval mode" apart from "this server is too old", rather than probing
// the route and failing: the web console hides the toggle when it is absent.
const approvalModeCapability = "approval_mode"

// agentMessagesCapability advertises the bidirectional agent-messaging model:
// messages carry delivery metadata, and GET /api/v1/session reports queued
// agent messages. A client hides those affordances when it is absent.
const agentMessagesCapability = "agent_messages"

// agentNamesCapability advertises that every agent entry carries a display
// name, so agents[].name may differ from the id. A client uses it to tell "this
// server reports a name per agent entry" apart from "this server is too old" and
// fall back to rendering ids, rather than assuming the name and id match.
const agentNamesCapability = "agent_names"
const toolLimitsCapability = "tool_limits"
const channelSwitchCapability = "channel_switch"
const markdownHTMLCapability = "markdown_html"
const sessionCancelCapability = "session_cancel"

// capabilities returns a fresh slice for each response. A shared backing array
// would let one caller's mutation leak into another payload, and keeping the
// list in one place stops GET /api/v1/session and the session descriptors from
// drifting apart.
func capabilities() []string {
	return []string{approvalModeCapability, agentMessagesCapability, agentNamesCapability, toolLimitsCapability, channelSwitchCapability, markdownHTMLCapability, sessionCancelCapability}
}

// channelDescriptors renders every configured execution channel for the wire.
func (s *Server) channelDescriptors() []channelDescriptor {
	list := s.cfg.Sessions.Channels()
	out := make([]channelDescriptor, 0, len(list))
	for _, c := range list {
		out = append(out, channelDescriptor{Name: c.Name, Type: c.Type})
	}
	return out
}

// resolve returns the session named by id, or the default session when id is
// empty.
func (s *Server) resolve(id string) (sessions.Handle, bool) {
	handle, ok := s.cfg.Sessions.Lookup(id)
	if !ok || handle == nil {
		return nil, false
	}
	return handle, true
}

// defaultSessionID returns the store's default session id, or "".
func (s *Server) defaultSessionID() string {
	if handle := s.cfg.Sessions.Default(); handle != nil {
		return handle.ID()
	}
	return ""
}

// agentSummaries returns one session's roster: the top-level agent first, then
// every subagent at any depth. A roster failure is reported as a message rather
// than an error, because losing the roster must not hide the transcript, the
// approvals, or the top-level agent.
func (s *Server) agentSummaries(ctx context.Context, handle sessions.Handle) ([]agentSummary, string) {
	top := handle.Agent()
	agents := []agentSummary{{
		ID: top.ID(), Name: top.Name(), Type: top.Type(), State: top.State().String(),
	}}
	manager := handle.Manager()
	if manager == nil {
		return agents, ""
	}
	subs, err := manager.ListSubagents(ctx, top.ID())
	if err != nil {
		return agents, "cannot list subagents"
	}
	for _, sub := range subs {
		agents = append(agents, agentSummary{
			ID: sub.ID, Name: sub.Name, Type: sub.Type, State: sub.State,
			Depth: sub.Depth, Paused: sub.Paused,
		})
	}
	return agents, ""
}

// describe renders one session without its transcript.
func (s *Server) describe(ctx context.Context, handle sessions.Handle, isDefault bool) sessionDescriptor {
	agents, agentsErr := s.agentSummaries(ctx, handle)
	return sessionDescriptor{
		ID: handle.ID(), Name: handle.Name(), CreatedAt: handle.CreatedAt(),
		Default: isDefault, Model: handle.Model(), Channel: handle.Channel(),
		State:          handle.Agent().State().String(),
		Agents:         agents,
		AgentsError:    agentsErr,
		QueuedMessages: handle.Queued(), LastError: handle.LastError(),
		Capabilities: capabilities(),
	}
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	handle, ok := s.resolve(r.URL.Query().Get("session_id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	agents, agentsErr := s.agentSummaries(r.Context(), handle)
	selected := handle.Agent()
	selectedID := r.URL.Query().Get("agent_id")
	if selectedID != "" && selectedID != selected.ID() {
		found := false
		for _, entry := range agents {
			if entry.ID == selectedID {
				found = true
				break
			}
		}
		manager := handle.Manager()
		if !found || manager == nil {
			writeError(w, http.StatusNotFound, "agent not found")
			return
		}
		sub := manager.Agent(selectedID)
		if sub == nil {
			writeError(w, http.StatusNotFound, "agent not found")
			return
		}
		selected = sub
	}
	model, channel, queued, lastError := "", "", 0, ""
	if selected.ID() == handle.Agent().ID() {
		model, channel = handle.Model(), handle.Channel()
		queued, lastError = handle.Queued(), handle.LastError()
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		APIVersion: 1,
		Channels:   s.channelDescriptors(),
		Session: session{
			ID: handle.ID(), Name: handle.Name(), CreatedAt: handle.CreatedAt(),
			AgentID: selected.ID(), AgentType: selected.Type(),
			Model: model, Channel: channel,
			State:        selected.State().String(),
			ApprovalMode: handle.Gate().Mode().String(),
		},
		Agents:               agents,
		AgentsError:          agentsErr,
		Capabilities:         capabilities(),
		Messages:             messagesFromHistory(selected.History()),
		LiveReasoning:        liveReasoning(selected),
		PendingApprovals:     approvalsFromGate(handle.Gate().PendingRequests()),
		PendingToolLimits:    pendingToolLimits(handle, agents),
		PendingAgentMessages: pendingAgentMessages(handle.Manager()),
		QueuedMessages:       queued, LastError: lastError,
	})
}

func pendingToolLimits(handle sessions.Handle, agents []agentSummary) []agent.ToolLimitRequest {
	out := make([]agent.ToolLimitRequest, 0)
	for _, summary := range agents {
		var a interface {
			PendingToolLimit() *agent.ToolLimitRequest
		}
		if summary.ID == handle.Agent().ID() {
			a, _ = handle.Agent().(interface {
				PendingToolLimit() *agent.ToolLimitRequest
			})
		} else if handle.Manager() != nil {
			a = handle.Manager().Agent(summary.ID)
		}
		if a != nil {
			if pending := a.PendingToolLimit(); pending != nil {
				out = append(out, *pending)
			}
		}
	}
	return out
}

func liveReasoning(a sessions.Agent) *agent.ReasoningStatus {
	if source, ok := a.(interface{ LiveReasoning() *agent.ReasoningStatus }); ok {
		return source.LiveReasoning()
	}
	return nil
}

func (s *Server) handleToolLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/tool-limits/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "tool-call limit prompt not found")
		return
	}
	var body struct {
		Decision  string `json:"decision"`
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	handle, ok := s.resolve(body.SessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	var a interface{ DecideToolLimit(string, string) error }
	if handle.Agent().ID() == body.AgentID {
		a, _ = handle.Agent().(interface{ DecideToolLimit(string, string) error })
	} else if handle.Manager() != nil {
		a = handle.Manager().Agent(body.AgentID)
	}
	if a == nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if err := a.DecideToolLimit(id, body.Decision); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"resolved": true})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSessions(w, r)
	case http.MethodPost:
		s.createSession(w, r)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	handles := s.cfg.Sessions.List()
	defaultID := s.defaultSessionID()
	list := make([]sessionDescriptor, 0, len(handles))
	for _, handle := range handles {
		list = append(list, s.describe(r.Context(), handle, handle.ID() == defaultID))
	}
	writeJSON(w, http.StatusOK, sessionListResponse{
		APIVersion:       1,
		DefaultSessionID: defaultID,
		Sessions:         list,
	})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeOptionalJSON(w, r, &body); err != nil {
		return
	}
	handle, err := s.cfg.Sessions.Create(r.Context(), body.Name)
	switch {
	case errors.Is(err, sessions.ErrNameInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sessions.ErrLimitReached):
		writeError(w, http.StatusConflict, "maximum number of sessions reached")
	case errors.Is(err, sessions.ErrClosed):
		writeError(w, http.StatusConflict, "session store is closed")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "cannot create session")
	default:
		// Building a session opens its execution session, so this call can block
		// on a slow channel.
		writeJSON(w, http.StatusCreated, s.describe(r.Context(), handle, false))
	}
}

func (s *Server) handleSessionItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, sessionItemPrefix)
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		handle, ok := s.resolve(id)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeJSON(w, http.StatusOK, s.describe(r.Context(), handle, handle.ID() == s.defaultSessionID()))
	case http.MethodPatch:
		var body struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			return
		}
		handle, err := s.cfg.Sessions.Rename(id, body.Name)
		switch {
		case errors.Is(err, sessions.ErrNameInvalid):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, sessions.ErrNotFound):
			writeError(w, http.StatusNotFound, "session not found")
		case errors.Is(err, sessions.ErrClosed):
			writeError(w, http.StatusConflict, "session store is closed")
		case err != nil:
			writeError(w, http.StatusInternalServerError, "cannot rename session")
		default:
			writeJSON(w, http.StatusOK, s.describe(r.Context(), handle, handle.ID() == s.defaultSessionID()))
		}
	case http.MethodDelete:
		switch err := s.cfg.Sessions.Close(r.Context(), id); {
		case errors.Is(err, sessions.ErrDefault):
			writeError(w, http.StatusConflict, "the default session cannot be closed")
		case errors.Is(err, sessions.ErrNotFound):
			writeError(w, http.StatusNotFound, "session not found")
		case err != nil:
			writeError(w, http.StatusInternalServerError, "cannot close session")
		default:
			writeJSON(w, http.StatusOK, map[string]bool{"closed": true})
		}
	default:
		methodNotAllowed(w, "GET, PATCH, DELETE")
	}
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var body struct {
		Content   string `json:"content"`
		AgentID   string `json:"agent_id"`
		SessionID string `json:"session_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeError(w, http.StatusBadRequest, "content must not be empty")
		return
	}
	if len(body.Content) > maxMessageBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "content exceeds 65536 bytes")
		return
	}
	handle, ok := s.resolve(body.SessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	// A subagent message goes to that agent's inbox, so it never occupies the
	// session queue the count describes.
	queued := handle.Queued()
	if top := handle.Agent(); body.AgentID != "" && body.AgentID != top.ID() {
		queued = 0
	}
	switch err := handle.Submit(r.Context(), body.AgentID, body.Content); {
	case errors.Is(err, sessions.ErrAgentNotFound):
		writeError(w, http.StatusNotFound, "agent not found")
	case errors.Is(err, sessions.ErrNotFound):
		writeError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, sessions.ErrClosed):
		writeError(w, http.StatusConflict, "session is closed")
	case errors.Is(err, sessions.ErrQueueFull):
		writeError(w, http.StatusConflict, "message queue is full")
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted": true, "queued_messages": queued,
		})
	}
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/approvals/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "approval not found")
		return
	}
	var body struct {
		Decision  string `json:"decision"`
		SessionID string `json:"session_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	// Approval ids are sequential per gate, so the same id exists in every
	// session; the decision must be applied to the addressed session's gate.
	handle, ok := s.resolve(body.SessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	gate := handle.Gate()
	var err error
	switch body.Decision {
	case "approve":
		err = gate.Decide(id, approval.DecisionApproved)
	case "deny":
		err = gate.Decide(id, approval.DecisionDenied)
	case "approve_all":
		err = gate.ApproveAll(id)
	default:
		writeError(w, http.StatusBadRequest, "decision must be approve, deny, or approve_all")
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"resolved": true})
}

// handleSessionApproval switches a session's approval mode. This is the
// user-facing path: the human owns the gate, so it may loosen (ask ->
// allow-all) as well as tighten (allow-all -> ask). The model reaches the gate
// through the set_approval tool, which uses Gate.ApplyModelMode and may only
// tighten; the two paths must not be conflated.
func (s *Server) handleSessionApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var body struct {
		Mode      string `json:"mode"`
		SessionID string `json:"session_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	var mode approval.Mode
	switch body.Mode {
	case "ask":
		mode = approval.ModeAsk
	case "allow-all":
		mode = approval.ModeAllowAll
	default:
		writeError(w, http.StatusBadRequest, "mode must be ask or allow-all")
		return
	}
	// Like handleMessages, an empty session_id addresses the default session.
	handle, ok := s.resolve(body.SessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	gate := handle.Gate()
	gate.SetMode(mode)
	writeJSON(w, http.StatusOK, map[string]string{"mode": gate.Mode().String()})
}

// handleSessionChannel switches a session's active execution channel.
func (s *Server) handleSessionChannel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var body struct {
		Channel   string `json:"channel"`
		SessionID string `json:"session_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	if body.Channel == "" {
		writeError(w, http.StatusBadRequest, "channel must not be empty")
		return
	}
	handle, ok := s.resolve(body.SessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err := handle.SetChannel(r.Context(), body.Channel); err != nil {
		switch {
		case errors.Is(err, sessions.ErrChannelNotFound):
			writeError(w, http.StatusNotFound, "channel not found")
		case errors.Is(err, sessions.ErrClosed):
			writeError(w, http.StatusConflict, "session is closed")
		default:
			writeError(w, http.StatusBadGateway, "cannot change channel: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"channel": handle.Channel()})
}

// handleSessionCancel stops every active and queued request in the addressed
// session without closing it. This is the web console's equivalent of the TUI's
// Esc stop-everything behaviour.
func (s *Server) handleSessionCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	handle, ok := s.resolve(body.SessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	affected, err := handle.Cancel(r.Context())
	if err != nil {
		switch {
		case errors.Is(err, sessions.ErrClosed):
			writeError(w, http.StatusConflict, "session is closed")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The client is gone; there is nothing left to write.
			return
		default:
			writeError(w, http.StatusBadGateway, "cannot cancel requests: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "affected": affected})
}

// decodeOptionalJSON decodes an optional single JSON object. An empty body is
// not an error, so POST /sessions needs no body.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return decodeJSON(w, r, dst)
}

func messagesFromHistory(history []llm.Item) []message {
	messages := make([]message, 0, len(history))
	for _, item := range history {
		m := message{CallID: item.CallID, Name: item.Name, Content: item.Content}
		switch item.Type {
		case llm.ItemMessage:
			m.Type, m.Role = "message", string(item.Role)
			m.Origin = string(item.Origin)
			if item.Role == llm.RoleAssistant || item.Origin == llm.OriginAgent {
				// The browser performs a second allow-list pass because API
				// endpoints are user-configurable.
				m.HTML = renderMarkdownHTML(item.Content)
			}
			if item.Delivery != nil {
				m.Delivery = &delivery{
					From: item.Delivery.From, To: item.Delivery.To,
					Direction: item.Delivery.Direction, Kind: item.Delivery.Kind,
				}
			}
		case llm.ItemFunctionCall:
			m.Type, m.Arguments = "tool_call", parseJSON(item.Args)
		case llm.ItemFunctionCallOutput:
			m.Type = "tool_result"
		case llm.ItemReasoning:
			m.Type = "reasoning"
		default:
			continue
		}
		messages = append(messages, m)
	}
	return messages
}

// renderMarkdownHTML renders model-authored Markdown while forcing inline raw
// HTML to remain literal and removing active URL schemes. markdown-go's default
// safe mode escapes HTML blocks, but v0.46.3 intentionally passes inline HTML
// through, so the event stream needs this small hardening pass before render.
func renderMarkdownHTML(source string) string {
	events, err := markdown.ParseBytes([]byte(source))
	if err != nil {
		return ""
	}
	for i := range events {
		events[i].Style.RawHTML = false
		if events[i].Style.LinkData == nil {
			continue
		}
		links := *events[i].Style.LinkData
		if !safeMarkdownURL(links.Link, events[i].Style.Image) {
			links.Link = ""
			links.HasLink = false
		}
		if !safeMarkdownURL(links.ImageLink, false) {
			links.ImageLink = ""
		}
		events[i].Style.LinkData = &links
	}
	html, err := markdownhtml.RenderString(events, markdownhtml.WithHTML5())
	if err != nil {
		return ""
	}
	return html
}

func safeMarkdownURL(raw string, image bool) bool {
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "" || scheme == "http" || scheme == "https" || (!image && scheme == "mailto") {
		return true
	}
	if image && scheme == "data" {
		lower := strings.ToLower(raw)
		for _, prefix := range []string{"data:image/gif;base64,", "data:image/jpeg;base64,", "data:image/png;base64,", "data:image/webp;base64,"} {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}

// pendingAgentMessages renders a session's queued agent messages. It always
// returns a non-nil slice so the JSON field is present as an empty array, not
// null, and tolerates a session without a manager.
func pendingAgentMessages(manager *agent.Manager) []pendingMessage {
	out := make([]pendingMessage, 0)
	if manager == nil {
		return out
	}
	for _, m := range manager.PendingMessages() {
		out = append(out, pendingMessage{
			From: m.From, To: m.To, Direction: m.Direction, Kind: m.Kind, Content: m.Text,
		})
	}
	return out
}

func approvalsFromGate(requests []approval.Request) []approvalRequest {
	result := make([]approvalRequest, 0, len(requests))
	for _, request := range requests {
		result = append(result, approvalRequest{
			ID: request.ID, AgentID: request.AgentID, AgentType: request.AgentType,
			ToolName: request.ToolName, Command: request.Command,
			Args: parseJSON(request.Args),
		})
	}
	return result
}

func parseJSON(raw string) any {
	if raw == "" {
		return nil
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) == nil {
		return value
	}
	return raw
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes+1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "body must contain one JSON object")
		return errors.New("multiple JSON values")
	}
	return nil
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		hash := s.users[username]
		if !ok || hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="Aiharn API", charset="UTF-8"`)
			writeError(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(s.cfg.AllowedOrigins))
	for _, origin := range s.cfg.AllowedOrigins {
		allowed[origin] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if !allowed["*"] && !allowed[origin] {
				writeError(w, http.StatusForbidden, "origin is not allowed")
				return
			}
			if allowed["*"] {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status code for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// requestLog logs each incoming request at debug level. It is the outermost
// middleware so it sees requests rejected by CORS or auth, and the final status.
// It never logs the Authorization value, only whether one was present.
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !logging.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		logging.Debug("api request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr),
			slog.String("origin", r.Header.Get("Origin")),
			slog.Bool("auth", r.Header.Get("Authorization") != ""),
			slog.Int("status", rec.status),
			slog.Duration("dur", time.Since(start)),
		)
	})
}

func validateListen(address, authFile string) error {
	_, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("webapi: invalid listen address %q: %w", address, err)
	}
	if authFile == "" {
		return errors.New("webapi: an auth file is required when the API is enabled")
	}
	return nil
}

func validateOrigins(origins []string) error {
	for _, origin := range origins {
		if origin == "*" {
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
			u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("webapi: invalid allowed origin %q", origin)
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}
