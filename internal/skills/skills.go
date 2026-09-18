// Package skills expands ${TAG} placeholders in a system prompt using content
// found under the aiharn home directory. It is a leaf package (standard
// library only) so that callers can substitute skill content without pulling
// in the config or agent layers.
package skills

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxTagFileBytes caps how much a single tag may read. A file larger than this
// is reported as an error rather than substituted in a silently truncated form.
const maxTagFileBytes = 4 << 20

// tagPattern matches ${NAME} placeholders. A name starts with a letter or
// underscore and continues with letters, digits, or underscores.
var tagPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Tag names understood by Expand. Only SKILLS_INDEX is known today; the slice
// is also used to render the "supported tags" list in error messages.
const (
	skillsIndexTag = "SKILLS_INDEX"
)

var supportedTags = []string{skillsIndexTag}

// Expand replaces every supported ${TAG} placeholder in prompt with content
// read from under aiharnHome and returns the result. ${SKILLS_INDEX} is
// replaced with the contents of <aiharnHome>/skills/index.md (capped at 4
// MiB). A prompt that contains no supported tag is returned unchanged and the
// filesystem is not touched, so a missing skills/ directory is harmless unless
// a tag actually references it. Unknown tags matching the ${NAME} pattern and
// unreadable referenced files return an error.
func Expand(prompt, aiharnHome string) (string, error) {
	matches := tagPattern.FindAllStringSubmatchIndex(prompt, -1)
	if len(matches) == 0 {
		return prompt, nil
	}

	var b strings.Builder
	last := 0
	for _, m := range matches {
		name := prompt[m[2]:m[3]]
		var replacement string
		switch name {
		case skillsIndexTag:
			content, err := readTagFile(filepath.Join(aiharnHome, "skills", "index.md"))
			if err != nil {
				return "", err
			}
			replacement = content
		default:
			return "", fmt.Errorf("unknown tag ${%s}; supported tags: %s", name, strings.Join(supportedTags, ", "))
		}
		b.WriteString(prompt[last:m[0]])
		b.WriteString(replacement)
		last = m[1]
	}
	b.WriteString(prompt[last:])
	return b.String(), nil
}

// readTagFile reads path in full, refusing files larger than maxTagFileBytes.
func readTagFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("skills: read %s: %w", path, err)
	}
	defer f.Close()

	b, err := io.ReadAll(io.LimitReader(f, maxTagFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("skills: read %s: %w", path, err)
	}
	if len(b) > maxTagFileBytes {
		return "", fmt.Errorf("skills: %s exceeds %d-byte limit", path, maxTagFileBytes)
	}
	return string(b), nil
}
