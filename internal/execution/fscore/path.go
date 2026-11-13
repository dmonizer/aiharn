package fscore

import (
	"path"
	"strings"
)

// pathSeparator is the only separator understood by confinement checks.
const pathSeparator = "/"

// canonicalName validates name against the canonical root and returns the
// canonical slash-separated root-relative name together with its components.
//
// An absolute name is accepted only when it is lexically inside root. The root
// itself is reported as a non-regular file. "." components and repeated
// separators are canonicalized away, while ".." is refused instead of resolved,
// so a name can never climb above the root handle.
func canonicalName(root, name string) (string, []string, error) {
	if name == "" {
		return "", nil, outsideRootError(name)
	}
	relative, err := relativeTo(root, name)
	if err != nil {
		return "", nil, err
	}
	if path.Clean(relative) == "." {
		// The name denotes the root directory itself, which is a directory and
		// not a file; that is more specific than the general "no traversal
		// component" rule applied below.
		return "", nil, notRegularFileError(name)
	}
	components, err := splitComponents(relative, name)
	if err != nil {
		return "", nil, err
	}
	return strings.Join(components, pathSeparator), components, nil
}

// relativeTo returns name relative to root, rejecting absolute names that are
// not lexically inside root.
func relativeTo(root, name string) (string, error) {
	if !strings.HasPrefix(name, pathSeparator) {
		return name, nil
	}
	cleaned := path.Clean(name)
	if root == pathSeparator {
		return strings.TrimPrefix(cleaned, pathSeparator), nil
	}
	if cleaned == root {
		return "", nil
	}
	prefix := root + pathSeparator
	if !strings.HasPrefix(cleaned, prefix) {
		return "", outsideRootError(name)
	}
	return strings.TrimPrefix(cleaned, prefix), nil
}

// splitComponents validates every component of relative. Empty components and
// "." are dropped while canonicalizing; ".." is refused because resolving it
// would mean walking out of the directory the traversal already holds.
func splitComponents(relative, name string) ([]string, error) {
	raw := strings.Split(relative, pathSeparator)
	components := make([]string, 0, len(raw))
	for _, component := range raw {
		switch component {
		case "", ".":
			continue
		case "..":
			return nil, outsideRootError(name)
		}
		components = append(components, component)
	}
	if len(components) == 0 {
		return nil, notRegularFileError(name)
	}
	return components, nil
}
