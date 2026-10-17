package store

import (
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

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
		parts, err := configMemberParts(strings.TrimSuffix(path, "/"))
		if err != nil {
			return ConfigLocks{}, fmt.Errorf("%w: locks.yml path %q is not a config member", ErrInvalidArgument, path)
		}
		isPrefix := strings.HasSuffix(path, "/")
		if isPrefix {
			if len(parts) != 2 || (parts[0] != topDirSkills && parts[0] != topDirExtensions) {
				return ConfigLocks{}, fmt.Errorf("%w: locks.yml prefix %q must name a skills or extensions directory", ErrInvalidArgument, path)
			}
			if !configNamePattern.MatchString(parts[1]) {
				return ConfigLocks{}, fmt.Errorf("%w: locks.yml prefix %q has an invalid directory name", ErrInvalidArgument, path)
			}
		} else if !validLockMemberPath(parts) {
			return ConfigLocks{}, fmt.Errorf("%w: locks.yml path %q must name a member, not a directory member", ErrInvalidArgument, path)
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
	switch parts[0] {
	case topDirSettings:
		return strings.Join(parts, "/") == settingsMember
	case topDirRules:
		return len(parts) == 2 && validateFlatNamedMember(topDirRules, parts[1], strings.Join(parts, "/"), ".md", ".mdc") == nil
	case topDirAgents:
		return len(parts) == 2 && validateFlatNamedMember(topDirAgents, parts[1], strings.Join(parts, "/"), ".md") == nil
	case topDirPrompts:
		return len(parts) == 3 && parts[1] != "" && parts[2] == memberSystemMD && configNamePattern.MatchString(parts[1])
	case topDirSkills, topDirExtensions:
		return len(parts) >= 3 && configNamePattern.MatchString(parts[1]) && validLockRelativePath(parts[2:])
	case topDirProfiles:
		return len(parts) == 3 && parts[1] != "" && parts[2] == memberProfileYML && configNamePattern.MatchString(parts[1])
	default:
		return false
	}
}

func validLockRelativePath(parts []string) bool {
	for _, part := range parts {
		if part == memberLocksYML || part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\\`) {
			return false
		}
	}
	return len(parts) > 0
}

func (l ConfigLocks) Covers(member string) bool {
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
