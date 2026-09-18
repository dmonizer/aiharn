// Package skills expands ${TAG} placeholders in a system prompt using content
// found under the aiharn home directory. It is a leaf package (standard
// library only) so that callers can substitute skill content without pulling
// in the config or agent layers.
package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxTagFileBytes caps how much a single tag may read. A file larger than this
// is reported as an error rather than substituted in a silently truncated form.
const maxTagFileBytes = 4 << 20

// tagPattern matches ${NAME} placeholders. A name starts with a letter or
// underscore and continues with letters, digits, or underscores.
var tagPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Tag names understood by the package.
const (
	skillsIndexTag = "SKILLS_INDEX"
	skillsDirTag   = "SKILLS_DIR"
	installURLTag  = "URL"
)

var errUnknownTag = errors.New("unknown tag")

// expandTags replaces ${TAG} placeholders in prompt using resolve. It returns
// an error naming supported when resolve returns errUnknownTag; any other
// resolve error is propagated.
func expandTags(prompt string, supported []string, resolve func(name string) (string, error)) (string, error) {
	matches := tagPattern.FindAllStringSubmatchIndex(prompt, -1)
	if len(matches) == 0 {
		return prompt, nil
	}

	var b strings.Builder
	last := 0
	for _, m := range matches {
		name := prompt[m[2]:m[3]]
		replacement, err := resolve(name)
		if err != nil {
			if errors.Is(err, errUnknownTag) {
				return "", fmt.Errorf("unknown tag ${%s}; supported tags: %s", name, strings.Join(supported, ", "))
			}
			return "", err
		}
		b.WriteString(prompt[last:m[0]])
		b.WriteString(replacement)
		last = m[1]
	}
	b.WriteString(prompt[last:])
	return b.String(), nil
}

// Expand replaces every supported ${TAG} placeholder in prompt with content
// read from under aiharnHome and returns the result. ${SKILLS_INDEX} is
// replaced with the contents of <aiharnHome>/skills/index.md (capped at 4
// MiB). A prompt that contains no supported tag is returned unchanged and the
// filesystem is not touched, so a missing skills/ directory is harmless unless
// a tag actually references it. Unknown tags matching the ${NAME} pattern and
// unreadable referenced files return an error.
func Expand(prompt, aiharnHome string) (string, error) {
	return expandTags(prompt, []string{skillsIndexTag}, func(name string) (string, error) {
		switch name {
		case skillsIndexTag:
			return readTagFile(filepath.Join(Dir(aiharnHome), "index.md"))
		default:
			return "", errUnknownTag
		}
	})
}

// Dir returns the skills directory under aiharnHome.
func Dir(aiharnHome string) string { return filepath.Join(aiharnHome, "skills") }

// Skill is one installed skill directory.
type Skill struct {
	Name string // directory name under <aiharn_home>/skills
	Path string // absolute path to the skill directory
}

// List returns installed skills under aiharnHome, sorted by Name. A skill is
// a subdirectory of <aiharn_home>/skills containing a non-directory SKILL.md
// file. A missing skills directory returns an empty slice and nil error.
func List(aiharnHome string) ([]Skill, error) {
	dir := Dir(aiharnHome)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var skills []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, entry.Name(), "SKILL.md"))
		if err != nil || info.IsDir() {
			continue
		}
		skills = append(skills, Skill{
			Name: entry.Name(),
			Path: filepath.Join(dir, entry.Name()),
		})
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})
	return skills, nil
}

// InstallPrompt reads <aiharn_home>/prompts/skill-install.md (capped at 4 MiB,
// reusing readTagFile) and substitutes ${URL} and ${SKILLS_DIR}.
func InstallPrompt(aiharnHome, url string) (string, error) {
	raw, err := readTagFile(filepath.Join(aiharnHome, "prompts", "skill-install.md"))
	if err != nil {
		return "", err
	}
	return expandTags(raw, []string{installURLTag, skillsDirTag}, func(name string) (string, error) {
		switch name {
		case installURLTag:
			return url, nil
		case skillsDirTag:
			return Dir(aiharnHome), nil
		default:
			return "", errUnknownTag
		}
	})
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
