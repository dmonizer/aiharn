package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"aiharn/internal/config"
	"aiharn/internal/execution"
	"aiharn/internal/filetools"
	"aiharn/internal/tools"
)

// defaultFileRegistry coordinates file-tool mutations and read receipts across
// every conversation runtime in the process. Sharing it is what lets an
// untracked change observed by one conversation invalidate another
// conversation's receipts for the same execution target.
var defaultFileRegistry = filetools.NewRegistry()

// fileToolLimits maps the [file_tools] configuration onto the domain limits.
// The read tool additionally refuses to return a result whose JSON envelope
// exceeds the model-visible tool-result budget, so no receipt is ever issued for
// content the model cannot see in full.
func fileToolLimits(cfg *config.Config) filetools.Limits {
	return filetools.Limits{
		MaxReadLines:       cfg.FileTools.MaxReadLines,
		MaxReadBytes:       cfg.FileTools.MaxReadBytes,
		MaxWriteBytes:      cfg.FileTools.MaxWriteBytes,
		MaxFileBytes:       cfg.FileTools.MaxFileBytes,
		MaxEditResultBytes: cfg.FileTools.MaxEditResultBytes,
	}
}

// fileToolDeps carries the conversation-scoped wiring the text-file tools need.
type fileToolDeps struct {
	registry  *filetools.Registry
	runtimeID string
	limits    filetools.Limits
}

// newBackend opens one agent's file-tool session against resolve.
func (d fileToolDeps) newBackend(agentID string, resolve filetools.TargetFunc, maxResultBytes int64) *fileBackend {
	return &fileBackend{
		agentID:        agentID,
		session:        d.registry.NewSession(d.runtimeID, d.limits, resolve),
		maxResultBytes: maxResultBytes,
	}
}

// fileBackend adapts one agent's filetools session to the tools seam and owns
// the session's receipt lifetime.
type fileBackend struct {
	agentID string
	session *filetools.Session
	// maxResultBytes is the model-visible tool-result budget. A read whose
	// encoded result would exceed it is refused before a receipt is minted.
	maxResultBytes int64
}

var _ tools.FileBackend = (*fileBackend)(nil)

func (b *fileBackend) ReadFile(ctx context.Context, req filetools.ReadRequest) (filetools.ReadResult, error) {
	if b.maxResultBytes > 0 {
		req.MaxResultBytes = b.maxResultBytes
	}
	return b.session.Read(ctx, req)
}

func (b *fileBackend) ProposeFileWrite(ctx context.Context, req filetools.WriteRequest) (*filetools.WriteProposal, error) {
	return b.session.ProposeWrite(ctx, req)
}

func (b *fileBackend) ProposeFileEdit(ctx context.Context, req filetools.EditRequest) (*filetools.EditProposal, error) {
	return b.session.ProposeEdit(ctx, req)
}

// Close drops every receipt this agent holds. Other agents in the same
// conversation keep theirs: closing a subagent must not disarm the top-level
// agent's read-before-edit context.
func (b *fileBackend) Close() { b.session.ClearAgent(b.agentID) }

// channelFileTarget resolves the file-tool target of an agent pinned to one
// execution channel (a subagent, which never switches channels).
type channelFileTarget struct {
	tc      *transportCache
	channel config.ChannelConfig

	mu sync.Mutex
	fs execution.Filesystem
}

// target implements filetools.TargetFunc.
func (t *channelFileTarget) target(ctx context.Context) (filetools.Target, error) {
	target := filetools.Target{Channel: t.channel.Name, Root: t.channel.Root}
	if t.channel.Root == "" {
		return target, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fs == nil {
		fs, err := t.tc.filesystem(ctx, t.channel)
		if err != nil {
			return filetools.Target{}, err
		}
		t.fs = fs
	}
	target.FS = t.fs
	return target, nil
}

// Filesystem implements filetools.TargetFunc for the top-level agent. It
// resolves the currently selected channel, so a channel switch redirects later
// file operations and bumps the generation the approval transaction checks.
func (c *channelController) Filesystem(ctx context.Context) (filetools.Target, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return filetools.Target{}, filetools.Errorf(filetools.CodeIOError, "no active execution channel", "")
	}
	channel, err := selectChannel(c.cfg, "", c.name)
	if err != nil {
		return filetools.Target{}, fmt.Errorf("app: %w", err)
	}
	target := filetools.Target{Channel: c.name, Root: channel.Root, Generation: c.gen}
	if channel.Root == "" {
		return target, nil
	}
	if c.filesystem == nil {
		fs, err := c.tc.filesystem(ctx, channel)
		if err != nil {
			return filetools.Target{}, err
		}
		c.filesystem = fs
	}
	target.FS = c.filesystem
	return target, nil
}

// filesystem returns the confined filesystem backend for one channel, creating
// it on first use and caching it for the lifetime of the runtime.
func (tc *transportCache) filesystem(ctx context.Context, c config.ChannelConfig) (execution.Filesystem, error) {
	if fs := tc.cachedFilesystem(c.Name); fs != nil {
		return fs, nil
	}
	transport, err := tc.get(c)
	if err != nil {
		return nil, err
	}
	provider, ok := transport.(execution.FilesystemProvider)
	if !ok {
		return nil, filetools.Errorf(filetools.CodeUnsupported,
			fmt.Sprintf("channel %q cannot provide confined file access", c.Name), "")
	}
	fs, err := provider.NewFilesystem(ctx, c.Root)
	if err != nil {
		return nil, filetools.WrapBackendError(err)
	}
	tc.mu.Lock()
	if existing := tc.filesystems[c.Name]; existing != nil {
		tc.mu.Unlock()
		_ = fs.Close()
		return existing, nil
	}
	if tc.filesystems == nil {
		tc.filesystems = map[string]execution.Filesystem{}
	}
	tc.filesystems[c.Name] = fs
	tc.mu.Unlock()
	return fs, nil
}

// cachedFilesystem returns a backend created earlier for name.
func (tc *transportCache) cachedFilesystem(name string) execution.Filesystem {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.filesystems[name]
}

// closeFilesystems releases every cached filesystem backend.
func (tc *transportCache) closeFilesystems() error {
	tc.mu.Lock()
	backends := make([]execution.Filesystem, 0, len(tc.filesystems))
	for name, fs := range tc.filesystems {
		backends = append(backends, fs)
		delete(tc.filesystems, name)
	}
	tc.mu.Unlock()
	var errs []error
	for _, fs := range backends {
		if err := fs.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// newRuntimeID returns an opaque identifier for one conversation runtime
// instance. Receipts are scoped to it, so they never survive a restart or a
// cleared session even if a public session id is reused.
func newRuntimeID() (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("app: generate conversation runtime id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
