package store

import (
	"archive/tar"
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// ConfigLocks is the tenant's set of config paths that user and role overlays cannot replace.
type ConfigLocks struct {
	paths []string
}

func parseConfigLocks(body []byte) (ConfigLocks, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return ConfigLocks{}, fmt.Errorf("%w: locks.yml is not valid YAML: %w", ErrInvalidArgument, err)
	}
	if len(doc) != 1 {
		return ConfigLocks{}, fmt.Errorf("%w: locks.yml must contain only paths", ErrInvalidArgument)
	}
	raw, ok := doc["paths"]
	if !ok {
		return ConfigLocks{}, fmt.Errorf("%w: locks.yml must contain paths", ErrInvalidArgument)
	}
	values, ok := raw.([]any)
	if !ok {
		return ConfigLocks{}, fmt.Errorf("%w: locks.yml paths must be a list", ErrInvalidArgument)
	}
	locks := ConfigLocks{paths: make([]string, 0, len(values))}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		path, ok := value.(string)
		if !ok || path == "" {
			return ConfigLocks{}, fmt.Errorf("%w: locks.yml paths entries must be non-empty strings", ErrInvalidArgument)
		}
		if _, ok := seen[path]; ok {
			return ConfigLocks{}, fmt.Errorf("%w: locks.yml repeats path %q", ErrInvalidArgument, path)
		}
		isPrefix := strings.HasSuffix(path, "/")
		trimmed := strings.TrimSuffix(path, "/")
		parts, err := configMemberParts(trimmed)
		if err != nil {
			return ConfigLocks{}, fmt.Errorf("%w: locks.yml path %q is not a config member", ErrInvalidArgument, path)
		}
		if isPrefix {
			if len(parts) > 2 || validateConfigMemberPath(parts, tar.TypeDir, LayerTenant) != nil || (parts[0] != topDirPrompts && parts[0] != topDirSkills && parts[0] != topDirExtensions) {
				return ConfigLocks{}, fmt.Errorf("%w: locks.yml prefix %q must name an admitted directory", ErrInvalidArgument, path)
			}
		} else if !validLockMemberPath(parts) {
			return ConfigLocks{}, fmt.Errorf("%w: locks.yml path %q must name an admitted member", ErrInvalidArgument, path)
		}
		seen[path] = struct{}{}
		locks.paths = append(locks.paths, path)
	}
	return locks, nil
}

func validLockMemberPath(parts []string) bool {
	if len(parts) == 1 {
		return parts[0] == memberAgentsMD || parts[0] == memberModels
	}
	if len(parts) >= 3 && (parts[0] == topDirSkills || parts[0] == topDirExtensions) {
		return false
	}
	return validateConfigMemberPath(parts, tar.TypeReg, LayerTenant) == nil
}

// Covers reports whether a canonical config member matches an exact lock or directory prefix.
func (l ConfigLocks) Covers(member string) bool {
	member = canonicalConfigPath(member)
	for _, path := range l.paths {
		if strings.HasSuffix(path, "/") {
			if strings.HasPrefix(member, path) {
				return true
			}
			continue
		}
		if member == path {
			return true
		}
	}
	return false
}
