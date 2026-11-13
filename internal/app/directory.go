package app

import (
	"context"
	"fmt"
	"path"
	"strings"

	"aiharn/internal/execution"
	"aiharn/internal/sessions"
)

// Directory settings are literal POSIX paths, never URL- or shell-decoded.
func boundedDirectory(root, current, dir string) (string, error) {
	if dir == "" || strings.ContainsAny(dir, "%\\\x00\r\n") {
		return "", fmt.Errorf("working directory must be a literal path without percent escapes, backslashes or control characters")
	}
	if !path.IsAbs(dir) {
		dir = path.Join(current, dir)
	}
	dir = path.Clean(dir)
	if root != "" && (!path.IsAbs(root) || (dir != root && !strings.HasPrefix(dir, strings.TrimSuffix(root, "/")+"/"))) {
		return "", fmt.Errorf("working directory must be within root %q", root)
	}
	return dir, nil
}

func physicalDir(ctx context.Context, s execution.Session, dir string) (string, error) {
	if !path.IsAbs(dir) {
		return "", fmt.Errorf("directory must be absolute: %q", dir)
	}
	r, err := s.Exec(ctx, "cd -- "+shellQuote(dir)+" && pwd -P", execution.ExecOptions{MaxOutputBytes: 64 << 10})
	if err != nil {
		return "", err
	}
	if r.ExitCode != 0 || r.Truncated {
		return "", fmt.Errorf("cannot resolve directory %q: %s", dir, strings.TrimSpace(r.Stderr))
	}
	return strings.TrimSuffix(r.Stdout, "\n"), nil
}

func (s *Session) WorkingDirectory() string { return s.rt.channel.Cwd() }
func (s *Session) SetWorkingDirectory(ctx context.Context, dir string) error {
	if s.isClosed() {
		return sessions.ErrClosed
	}
	c := s.rt.channel
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setDirectoryLocked(ctx, dir)
}

func (c *channelController) setDirectoryLocked(ctx context.Context, dir string) error {
	if c.session == nil {
		return sessions.ErrClosed
	}
	ch, err := selectChannel(c.cfg, "", c.name)
	if err != nil {
		return err
	}
	root := ""
	if ch.Root != "" {
		root, err = physicalDir(ctx, c.session, ch.Root)
		if err != nil {
			return err
		}
	}
	current := c.cwd
	if current == "" {
		r, e := c.session.Exec(ctx, "pwd -P", execution.ExecOptions{MaxOutputBytes: 64 << 10})
		if e != nil {
			return e
		}
		if r.ExitCode != 0 || r.Truncated {
			return fmt.Errorf("cannot resolve current directory")
		}
		current = strings.TrimSuffix(r.Stdout, "\n")
	}
	candidate, err := boundedDirectory(root, current, dir)
	if err != nil {
		return err
	}
	candidate, err = physicalDir(ctx, c.session, candidate)
	if err != nil {
		return err
	}
	candidate, err = boundedDirectory(root, current, candidate)
	if err != nil {
		return err
	}
	if c.directories == nil {
		c.directories = make(map[string]string)
	}
	c.directories[c.name] = candidate
	c.cwd = candidate
	return nil
}

// ChannelWorkingDirectory reads or updates one channel without changing the active channel.
// A nil directory reads the current setting.
func (s *Session) ChannelWorkingDirectory(ctx context.Context, name string, dir *string) (string, error) {
	if s.isClosed() {
		return "", sessions.ErrClosed
	}
	c := s.rt.channel
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return "", sessions.ErrClosed
	}
	if name == "" || name == c.name {
		if dir != nil {
			if err := c.setDirectoryLocked(ctx, *dir); err != nil {
				return "", err
			}
		}
		return c.cwd, nil
	}
	ch, err := selectChannel(c.cfg, "", name)
	if err != nil {
		return "", fmt.Errorf("%w: %v", sessions.ErrChannelNotFound, err)
	}
	// Open a separate shell for validation on the selected execution host.
	target := &channelController{cfg: c.cfg, tc: c.tc, agentCfg: c.agentCfg, agentType: c.agentType, directories: c.directories}
	if err := target.openLocked(ctx, ch); err != nil {
		return "", err
	}
	defer target.Close()
	if dir != nil {
		if err := target.setDirectoryLocked(ctx, *dir); err != nil {
			return "", err
		}
		c.directories = target.directories
	}
	return target.cwd, nil
}
