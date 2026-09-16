// Package webapi exposes the active Aiharn session as a versioned JSON API.
// It deliberately contains no frontend assets: browser clients can be hosted
// independently and can connect to one or many API instances.
package webapi

import (
	"context"
	"crypto/subtle"
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

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/llm"
	"aiharn/internal/logging"
)

const (
	maxMessageBytes  = 64 << 10
	defaultQueueSize = 32
)

// Agent is the portion of the active agent needed by the remote API.
type Agent interface {
	ID() string
	Type() string
	State() agent.State
	History() []llm.Item
	Turn(context.Context, string) error
}

// SessionInfo contains non-secret labels describing the active session.
type SessionInfo struct {
	Model   string
	Channel string
}

// Config configures one API server.
type Config struct {
	Listen         string
	Token          string
	AllowedOrigins []string
	QueueSize      int
	Agent          Agent
	Manager        *agent.Manager
	Gate           *approval.Gate
	Session        SessionInfo
}

// Server owns the HTTP listener and the bounded remote-message worker.
type Server struct {
	cfg      Config
	handler  http.Handler
	server   *http.Server
	listener net.Listener

	ctx    context.Context
	cancel context.CancelFunc
	queue  chan string
	wg     sync.WaitGroup

	mu        sync.Mutex
	lastError string
	closed    bool
}

// New validates cfg and constructs a server. Start opens the configured
// listener; Handler can be used independently by another HTTP host.
func New(cfg Config) (*Server, error) {
	if cfg.Agent == nil {
		return nil, errors.New("webapi: agent is required")
	}
	if cfg.Gate == nil {
		return nil, errors.New("webapi: approval gate is required")
	}
	if cfg.Listen == "" {
		return nil, errors.New("webapi: listen address is required")
	}
	if err := validateListen(cfg.Listen, cfg.Token); err != nil {
		return nil, err
	}
	if err := validateOrigins(cfg.AllowedOrigins); err != nil {
		return nil, err
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg: cfg, ctx: ctx, cancel: cancel,
		queue: make(chan string, cfg.QueueSize),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/session", s.handleSession)
	mux.HandleFunc("/api/v1/messages", s.handleMessages)
	mux.HandleFunc("/api/v1/approvals/", s.handleApproval)
	s.handler = s.requestLog(s.securityHeaders(s.cors(s.authenticate(mux))))

	go s.runMessages()
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
			s.setLastError("api server stopped: " + err.Error())
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

// Close stops requests and cancels work initiated by the remote queue.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv := s.server
	s.cancel()
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

type sessionResponse struct {
	APIVersion       int               `json:"api_version"`
	Session          session           `json:"session"`
	Agents           []agentSummary    `json:"agents"`
	Messages         []message         `json:"messages"`
	PendingApprovals []approvalRequest `json:"pending_approvals"`
	QueuedMessages   int               `json:"queued_messages"`
	LastError        string            `json:"last_error,omitempty"`
}

type agentSummary struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	State  string `json:"state"`
	Depth  int    `json:"depth"`
	Paused bool   `json:"paused"`
}

type session struct {
	AgentID      string `json:"agent_id"`
	AgentType    string `json:"agent_type"`
	Model        string `json:"model"`
	Channel      string `json:"channel"`
	State        string `json:"state"`
	ApprovalMode string `json:"approval_mode"`
}

type message struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
}

type approvalRequest struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	ToolName  string `json:"tool_name"`
	Command   string `json:"command,omitempty"`
	Args      any    `json:"arguments,omitempty"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	agents := []agentSummary{{
		ID: s.cfg.Agent.ID(), Type: s.cfg.Agent.Type(),
		State: s.cfg.Agent.State().String(),
	}}
	if s.cfg.Manager != nil {
		subs, err := s.cfg.Manager.ListSubagents(r.Context(), s.cfg.Agent.ID())
		if err != nil {
			writeError(w, http.StatusConflict, "cannot list subagents")
			return
		}
		for _, sub := range subs {
			agents = append(agents, agentSummary{
				ID: sub.ID, Type: sub.Type, State: sub.State,
				Depth: sub.Depth, Paused: sub.Paused,
			})
		}
	}
	selected := s.cfg.Agent
	selectedID := r.URL.Query().Get("agent_id")
	if selectedID != "" && selectedID != selected.ID() {
		found := false
		for _, entry := range agents[1:] {
			if entry.ID == selectedID {
				found = true
				break
			}
		}
		if !found || s.cfg.Manager == nil {
			writeError(w, http.StatusNotFound, "agent not found")
			return
		}
		sub := s.cfg.Manager.Agent(selectedID)
		if sub == nil {
			writeError(w, http.StatusNotFound, "agent not found")
			return
		}
		selected = sub
	}
	model, channel, queued, lastError := "", "", 0, ""
	if selected.ID() == s.cfg.Agent.ID() {
		model, channel = s.cfg.Session.Model, s.cfg.Session.Channel
		queued, lastError = len(s.queue), s.getLastError()
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		APIVersion: 1,
		Session: session{
			AgentID: selected.ID(), AgentType: selected.Type(),
			Model: model, Channel: channel,
			State:        selected.State().String(),
			ApprovalMode: s.cfg.Gate.Mode().String(),
		},
		Agents:           agents,
		Messages:         messagesFromHistory(selected.History()),
		PendingApprovals: approvalsFromGate(s.cfg.Gate.PendingRequests()),
		QueuedMessages:   queued, LastError: lastError,
	})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var body struct {
		Content string `json:"content"`
		AgentID string `json:"agent_id"`
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
	if body.AgentID != "" && body.AgentID != s.cfg.Agent.ID() {
		if s.cfg.Manager == nil {
			writeError(w, http.StatusNotFound, "agent not found")
			return
		}
		err := s.cfg.Manager.SendSubagentMessage(r.Context(), s.cfg.Agent.ID(), body.AgentID, body.Content)
		switch {
		case errors.Is(err, agent.ErrSubagentNotFound), errors.Is(err, agent.ErrSubagentNotOwned):
			writeError(w, http.StatusNotFound, "agent not found")
		case err != nil:
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "queued_messages": 0})
		}
		return
	}
	if s.cfg.Agent.State() == agent.StateClosed {
		writeError(w, http.StatusConflict, "session is closed")
		return
	}
	select {
	case s.queue <- body.Content:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted": true, "queued_messages": len(s.queue),
		})
	default:
		writeError(w, http.StatusConflict, "message queue is full")
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
		Decision string `json:"decision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	var err error
	switch body.Decision {
	case "approve":
		err = s.cfg.Gate.Decide(id, approval.DecisionApproved)
	case "deny":
		err = s.cfg.Gate.Decide(id, approval.DecisionDenied)
	case "approve_all":
		err = s.cfg.Gate.ApproveAll(id)
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

func (s *Server) runMessages() {
	for {
		select {
		case content := <-s.queue:
			s.setLastError("")
			if err := s.cfg.Agent.Turn(s.ctx, content); err != nil && s.ctx.Err() == nil {
				s.setLastError(err.Error())
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func messagesFromHistory(history []llm.Item) []message {
	messages := make([]message, 0, len(history))
	for _, item := range history {
		m := message{CallID: item.CallID, Name: item.Name, Content: item.Content}
		switch item.Type {
		case llm.ItemMessage:
			m.Type, m.Role = "message", string(item.Role)
		case llm.ItemFunctionCall:
			m.Type, m.Arguments = "tool_call", parseJSON(item.Args)
		case llm.ItemFunctionCallOutput:
			m.Type = "tool_result"
		default:
			continue
		}
		messages = append(messages, m)
	}
	return messages
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
		if s.cfg.Token != "" {
			authorization := r.Header.Get("Authorization")
			provided := strings.TrimPrefix(authorization, "Bearer ")
			if !strings.HasPrefix(authorization, "Bearer ") || len(provided) != len(s.cfg.Token) ||
				subtle.ConstantTimeCompare([]byte(provided), []byte(s.cfg.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
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
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
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

func validateListen(address, token string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("webapi: invalid listen address %q: %w", address, err)
	}
	loopback := host == "localhost" || strings.HasSuffix(host, ".localhost")
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	if !loopback && token == "" {
		return errors.New("webapi: a token is required when listening beyond loopback")
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

func (s *Server) setLastError(value string) {
	s.mu.Lock()
	s.lastError = value
	s.mu.Unlock()
}

func (s *Server) getLastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastError
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
