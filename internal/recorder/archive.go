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
		name := entry.Name()
		if filepath.Ext(name) != ".jsonl" {
			continue
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		line, headerErr := readFirstLine(bufio.NewReader(f))
		_ = f.Close()
		if headerErr != nil {
			continue
		}
		h, err := parseArchiveHeader(line)
		if err != nil || h.Type != "session" || h.SessionID == "" {
			continue
		}
		sessions = append(sessions, archivedSession(name, h))
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt.After(sessions[j].StartedAt)
	})
	return sessions, nil
}

// ArchivedSessionDetail returns up to limit completed top-level conversation
// messages from the archived transcript named by key. limit <= 0 means no
// bound. key must be a bare .jsonl file name; anything else is
// ErrArchivedNotFound.
func ArchivedSessionDetail(aiharnHome, key string, limit int) (ArchivedDetail, error) {
	if key == "" || filepath.Base(key) != key || filepath.Ext(key) != ".jsonl" {
		return ArchivedDetail{}, ErrArchivedNotFound
	}

	path := filepath.Join(aiharnHome, "transcripts", key)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ArchivedDetail{}, ErrArchivedNotFound
		}
		return ArchivedDetail{}, err
	}
	defer f.Close()

	br := bufio.NewReader(f)
	line, err := readFirstLine(br)
	if err != nil {
		return ArchivedDetail{}, fmt.Errorf("recorder: read archived header: %w", err)
	}
	h, err := parseArchiveHeader(line)
	if err != nil || h.Type != "session" {
		return ArchivedDetail{}, fmt.Errorf("recorder: parse archived header: %w", err)
	}

	detail := ArchivedDetail{
		ArchivedSession: archivedSession(key, h),
		Messages:        make([]ArchivedMessage, 0),
	}

	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 0, 64*1024), archiveHeaderMaxBytes)
	for {
		if limit > 0 && len(detail.Messages) >= limit {
			break
		}
		if !scanner.Scan() {
			break
		}
		var entry archiveEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		if h.Agent != "" && entry.AgentID != "" && entry.AgentID != h.Agent {
			continue
		}
		detail.Messages = append(detail.Messages, ArchivedMessage{
			Type:    "message",
			Role:    entry.Type,
			Content: entry.Content,
		})
	}
	return detail, nil
}

// ArchivedHistory returns the reconstructed top-level conversation history of an
// archived transcript, plus its session metadata. Only the top-level agent's
// completed items are included, in transcript order. Streaming/delta and
// lifecycle entries are ignored.
func ArchivedHistory(aiharnHome, key string) ([]llm.Item, sessions.TranscriptMeta, error) {
	if key == "" || filepath.Base(key) != key || filepath.Ext(key) != ".jsonl" {
		return nil, sessions.TranscriptMeta{}, ErrArchivedNotFound
	}

	path := filepath.Join(aiharnHome, "transcripts", key)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, sessions.TranscriptMeta{}, ErrArchivedNotFound
		}
		return nil, sessions.TranscriptMeta{}, err
	}
	defer f.Close()

	br := bufio.NewReader(f)
	line, err := readFirstLine(br)
	if err != nil {
		return nil, sessions.TranscriptMeta{}, fmt.Errorf("recorder: read archived header: %w", err)
	}
	h, err := parseArchiveHeader(line)
	if err != nil || h.Type != "session" {
		return nil, sessions.TranscriptMeta{}, fmt.Errorf("recorder: parse archived header: %w", err)
	}
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
		switch entry.Type {
		case "user":
			history = append(history, llm.Item{Type: llm.ItemMessage, Role: llm.RoleUser, Content: entry.Content})
		case "assistant":
			history = append(history, llm.Item{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: entry.Content})
		case "tool_call":
			history = append(history, llm.Item{Type: llm.ItemFunctionCall, CallID: entry.CallID, Name: entry.Name, Args: string(entry.Arguments)})
		case "tool_result":
			history = append(history, llm.Item{Type: llm.ItemFunctionCallOutput, CallID: entry.CallID, Content: entry.Content})
		case "reasoning":
			// Display-only; never part of completed history.
		default:
			// Streaming/delta and lifecycle entries are not completed history.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, sessions.TranscriptMeta{}, err
	}
	return history, meta, nil
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
		if parsed, err := time.Parse(archiveFilenameLayout, strings.TrimSuffix(key, ".jsonl")); err == nil {
			started = parsed
		}
	}
	return started
}
