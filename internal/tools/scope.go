package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveScoped resolves path (absolute or relative to cwd) and rejects any
// result that escapes cwd, whether via ".." traversal or an absolute path
// pointing outside the task's working directory.
func resolveScoped(cwd, path string) (string, error) {
	var full string
	if filepath.IsAbs(path) {
		full = filepath.Clean(path)
	} else {
		full = filepath.Clean(filepath.Join(cwd, path))
	}

	cleanCwd := filepath.Clean(cwd)
	if full != cleanCwd && !strings.HasPrefix(full, cleanCwd+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside the working directory", path)
	}
	return full, nil
}

// withinScope reports whether the given already-resolved path lies inside cwd.
// Used to filter results (e.g. glob matches) that can escape scope via pattern
// tricks even when the base search directory was itself validated.
func withinScope(cwd, path string) bool {
	cleanCwd := filepath.Clean(cwd)
	clean := filepath.Clean(path)
	return clean == cleanCwd || strings.HasPrefix(clean, cleanCwd+string(filepath.Separator))
}
