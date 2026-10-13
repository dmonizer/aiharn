package recorder

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aiharn/internal/llm"
	"aiharn/internal/sessions"
)

// ErrArchivedNotFound reports that an archived transcript does not exist or
// that its key is invalid.
var ErrArchivedNotFound = errors.New("recorder: archived session not found")

// archiveHeaderMaxBytes bounds the first line read from each transcript, so
// listing and previewing never slurp a whole (potentially large) file just to
// read its header.
const archiveHeaderMaxBytes = 1 << 20

// archiveFilenameLayout is the timestamp prefix of per-session transcript file
// names, used as a fallback when a header has no parseable started_at.
const archiveFilenameLayout = "2006-01-02T15-04-05.000000000Z"

var errHeaderTooLong = errors.New("recorder: transcript header line too long")

// archiveExt is the file extension of archived transcript files.
const archiveExt = ".jsonl"

// ArchivedSession is the archive-listing summary of one finished transcript.
type ArchivedSession struct {
	Key       string    `json:"key"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Model     string    `json:"model,omitempty"`
	AgentType string    `json:"agent_type,omitempty"`
	Channel   string    `json:"channel,omitempty"`
	Approval  string    `json:"approval,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// ArchivedMessage is one completed top-level conversation message in an
// archived transcript.
type ArchivedMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// ArchivedDetail is a bounded preview of one archived transcript.
type ArchivedDetail struct {
	ArchivedSession
	Messages []ArchivedMessage `json:"messages"`
}

// archiveHeader is the JSON object on a transcript's first line. started_at is
// kept as raw JSON so an unparseable timestamp degrades to the filename
// fallback instead of discarding an otherwise valid header.
type archiveHeader struct {
	Type        string          `json:"type"`
	Version     int             `json:"version"`
	StartedAt   json.RawMessage `json:"started_at"`
	Model       string          `json:"model"`
	Agent       string          `json:"agent"`
	Channel     string          `json:"channel"`
	Approval    string          `json:"approval"`
	SessionID   string          `json:"session_id"`
	SessionName string          `json:"session_name"`
}

type archiveEntry struct {
	Type    string `json:"type"`
	AgentID string `json:"agent_id"`
	Content string `json:"content"`
}

// archiveHistoryEntry is one completed-history line in an archived transcript.
// It carries the tool-call fields archiveEntry lacks so a finished conversation
// can be reconstructed into llm.Item history.
type archiveHistoryEntry struct {
	Type      string          `json:"type"`
	AgentID   string          `json:"agent_id"`
	Content   string          `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ArchivedSessions returns every archived transcript below
// <aiharnHome>/transcripts, newest first. A missing transcripts directory is
// not an error: it simply means there is nothing archived yet. Files whose
// header is missing, malformed, or lacks a session_id are skipped (a shared
// --log transcript has no session identity).
func ArchivedSessions(aiharnHome string) ([]ArchivedSession, error) {
	dir := filepath.Join(aiharnHome, "transcripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	sessions := make([]ArchivedSession, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if session, ok := listArchivedSession(dir, entry.Name()); ok {
			sessions = append(sessions, session)
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt.After(sessions[j].StartedAt)
	})
	return sessions, nil
}

// listArchivedSession reads one transcript's header and returns its summary.
// ok is false when the file should be skipped (not a transcript, missing, or
// malformed).
func listArchivedSession(dir, name string) (ArchivedSession, bool) {
	if filepath.Ext(name) != archiveExt {
		return ArchivedSession{}, false
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return ArchivedSession{}, false
	}
	defer f.Close()

	line, err := readFirstLine(bufio.NewReader(f))
	if err != nil {
		return ArchivedSession{}, false
	}
	h, err := parseArchiveHeader(line)
	if err != nil || h.Type != "session" || h.SessionID == "" {
		return ArchivedSession{}, false
	}
	return archivedSession(name, h), true
}

// isArchivedKey reports whether key is a bare .jsonl file name. Anything else
// is rejected before it reaches the filesystem.
func isArchivedKey(key string) bool {
	return key != "" && filepath.Base(key) == key && filepath.Ext(key) == archiveExt
}

// openArchived opens the transcript named by key and returns its parsed header.
// The file is positioned just after the header line; callers scan from br to
// read the remaining entries.
func openArchived(aiharnHome, key string) (*os.File, *bufio.Reader, archiveHeader, error) {
	f, err := os.Open(filepath.Join(aiharnHome, "transcripts", key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, archiveHeader{}, ErrArchivedNotFound
		}
		return nil, nil, archiveHeader{}, err
	}
	br := bufio.NewReader(f)
	line, err := readFirstLine(br)
	if err != nil {
		f.Close()
		return nil, nil, archiveHeader{}, fmt.Errorf("recorder: read archived header: %w", err)
	}
	h, err := parseArchiveHeader(line)
	if err != nil || h.Type != "session" {
		f.Close()
		return nil, nil, archiveHeader{}, fmt.Errorf("recorder: parse archived header: %w", err)
	}
	return f, br, h, nil
}

// ArchivedSessionDetail returns up to limit completed top-level conversation
// messages from the archived transcript named by key. limit <= 0 means no
// bound. key must be a bare .jsonl file name; anything else is
// ErrArchivedNotFound.
func ArchivedSessionDetail(aiharnHome, key string, limit int) (ArchivedDetail, error) {
	if !isArchivedKey(key) {
		return ArchivedDetail{}, ErrArchivedNotFound
	}
	f, br, h, err := openArchived(aiharnHome, key)
	if err != nil {
		return ArchivedDetail{}, err
	}
	defer f.Close()

	detail := ArchivedDetail{
		ArchivedSession: archivedSession(key, h),
		Messages:        make([]ArchivedMessage, 0),
	}

	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 0, 64*1024), archiveHeaderMaxBytes)
	for scanner.Scan() && (limit <= 0 || len(detail.Messages) < limit) {
		var entry archiveEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if !topLevelMessage(entry.Type, entry.AgentID, h.Agent) {
			continue
		}
		detail.Messages = append(detail.Messages, archivedMessage(entry))
	}
	return detail, nil
}

// topLevelMessage reports whether a transcript entry is a completed user or
// assistant message belonging to the top-level agent.
func topLevelMessage(entryType, agentID, agent string) bool {
	if entryType != "user" && entryType != "assistant" {
		return false
	}
	return agent == "" || agentID == "" || agentID == agent
}

// archivedMessage maps one user/assistant transcript entry onto a preview
// message.
func archivedMessage(entry archiveEntry) ArchivedMessage {
	return ArchivedMessage{Type: "message", Role: entry.Type, Content: entry.Content}
}

// ArchivedHistory returns the reconstructed top-level conversation history of an
// archived transcript, plus its session metadata. Only the top-level agent's
// completed items are included, in transcript order. Streaming/delta and
// lifecycle entries are ignored.
func ArchivedHistory(aiharnHome, key string) ([]llm.Item, sessions.TranscriptMeta, error) {
	if !isArchivedKey(key) {
		return nil, sessions.TranscriptMeta{}, ErrArchivedNotFound
	}
	f, br, h, err := openArchived(aiharnHome, key)
	if err != nil {
		return nil, sessions.TranscriptMeta{}, err
	}
	defer f.Close()

	if h.Agent == "" {
		h.Agent = "main"
	}
	meta := sessions.TranscriptMeta{
		ID:        h.SessionID,
		Name:      h.SessionName,
		Model:     h.Model,
		AgentType: h.Agent,
		Channel:   h.Channel,
		Approval:  h.Approval,
	}

	history := make([]llm.Item, 0)
	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 0, 64*1024), archiveHeaderMaxBytes)
	for scanner.Scan() {
		var entry archiveHistoryEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.AgentID != "" && entry.AgentID != h.Agent {
			continue
		}
		if item, ok := archiveHistoryItem(entry); ok {
			history = append(history, item)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, sessions.TranscriptMeta{}, err
	}
	return history, meta, nil
}

// archiveHistoryItem maps one completed transcript entry onto an llm.Item.
// Display-only and lifecycle entries return ok == false.
func archiveHistoryItem(entry archiveHistoryEntry) (llm.Item, bool) {
	switch entry.Type {
	case "user":
		return llm.Item{Type: llm.ItemMessage, Role: llm.RoleUser, Content: entry.Content}, true
	case "assistant":
		return llm.Item{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: entry.Content}, true
	case "tool_call":
		return llm.Item{Type: llm.ItemFunctionCall, CallID: entry.CallID, Name: entry.Name, Args: string(entry.Arguments)}, true
	case "tool_result":
		return llm.Item{Type: llm.ItemFunctionCallOutput, CallID: entry.CallID, Content: entry.Content}, true
	default:
		return llm.Item{}, false
	}
}

// readFirstLine reads at most archiveHeaderMaxBytes of the first line of r,
// returning it without its trailing newline. It leaves r positioned at the
// start of the following line, so callers can continue scanning the file.
func readFirstLine(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err == io.EOF {
			if len(buf) == 0 {
				return "", io.EOF
			}
			return strings.TrimRight(string(buf), "\r"), nil
		}
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return strings.TrimRight(string(buf), "\r"), nil
		}
		if len(buf) >= archiveHeaderMaxBytes {
			return "", errHeaderTooLong
		}
		buf = append(buf, b)
	}
}

func parseArchiveHeader(line string) (archiveHeader, error) {
	var h archiveHeader
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		return archiveHeader{}, err
	}
	return h, nil
}

func archivedSession(key string, h archiveHeader) ArchivedSession {
	started := archiveStartedAt(key, h.StartedAt)
	return ArchivedSession{
		Key:       key,
		ID:        h.SessionID,
		Name:      h.SessionName,
		Model:     h.Model,
		AgentType: h.Agent,
		Channel:   h.Channel,
		Approval:  h.Approval,
		StartedAt: started,
	}
}

func archiveStartedAt(key string, raw json.RawMessage) time.Time {
	var started time.Time
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &started)
	}
	if started.IsZero() {
		if parsed, err := time.Parse(archiveFilenameLayout, strings.TrimSuffix(key, archiveExt)); err == nil {
			started = parsed
		}
	}
	return started
}
