package execution

import (
	"fmt"
	"regexp"
)

// Remote-home tokens that resolve on the remote host. These are the literal
// spellings accepted in a working_dir template; they are replaced here (after
// the remote home is known), not by shell tilde expansion.
const (
	remoteHomeTilde = "~"
	remoteHomeVar   = "$HOME"
)

var runtimePlaceholder = regexp.MustCompile(`\$\{([^}]+)\}`)

// RenderWorkingDir substitutes runtime values into a validated working-directory
// template. agentType and agentID replace ${agent.type} and ${agent.id};
// remoteHome replaces the "~" and "$HOME" tokens at path-segment boundaries.
// It returns an error for any unknown ${...} placeholder (defense in depth; the
// config package already rejects these at load time).
func RenderWorkingDir(template, agentType, agentID, remoteHome string) (string, error) {
	var firstErr error
	out := runtimePlaceholder.ReplaceAllStringFunc(template, func(m string) string {
		name := m[2 : len(m)-1]
		switch name {
		case "agent.type":
			return agentType
		case "agent.id":
			return agentID
		default:
			if firstErr == nil {
				firstErr = fmt.Errorf("working_dir: unknown placeholder ${%s}", name)
			}
			return m
		}
	})
	if firstErr != nil {
		return "", firstErr
	}
	out = replaceRemoteHome(out, remoteHome)
	return out, nil
}

// replaceRemoteHome replaces "~" and "$HOME" only where they form a whole path
// segment (start, or after a "/"), so a stray "~" inside a segment is left
// alone.
func replaceRemoteHome(s, home string) string {
	s = regexp.MustCompile(`(^|/)~($|/)`).ReplaceAllString(s, `${1}`+home+`${2}`)
	s = regexp.MustCompile(`(^|/)\$HOME($|/)`).ReplaceAllString(s, `${1}`+home+`${2}`)
	return s
}
