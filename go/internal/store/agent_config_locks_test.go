package store

import (
	"testing"
)

func TestParseConfigLocks(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantPaths []string
		wantErr   bool
	}{
		{name: "empty paths", body: "paths: []\n", wantPaths: []string{}},
		{name: "exact skill file rejected", body: "paths:\n  - skills/web/SKILL.md\n", wantErr: true},
		{name: "exact nested extension source rejected", body: "paths:\n  - extensions/ext/src/main.ts\n", wantErr: true},
		{name: "nested locks file rejected", body: "paths:\n  - skills/x/locks.yml\n", wantErr: true},
		{name: "members and prefixes", body: "paths:\n  - settings/config.yml\n  - skills/web/\n", wantPaths: []string{"settings/config.yml", "skills/web/"}},
		{name: "unknown key", body: "paths: []\nextra: true\n", wantErr: true},
		{name: "lock path inside directory member rejected", body: "paths:\n  - skills/x/locks.yml\n", wantErr: true},
		{name: "invalid yaml", body: "paths: [\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			locks, err := parseConfigLocks([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseConfigLocks error = %v, wantErr %t", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if len(locks.paths) != len(tt.wantPaths) {
				t.Fatalf("paths = %v, want %v", locks.paths, tt.wantPaths)
			}
			for i, path := range locks.paths {
				if path != tt.wantPaths[i] {
					t.Errorf("paths[%d] = %q, want %q", i, path, tt.wantPaths[i])
				}
			}
		})
	}
}

func TestConfigLocksCovers(t *testing.T) {
	locks, err := parseConfigLocks([]byte("paths:\n  - settings/config.yml\n  - skills/web/\n  - mcp/fetch.json\n"))
	if err != nil {
		t.Fatalf("parseConfigLocks: %v", err)
	}
	tests := []struct {
		name   string
		member string
		want   bool
	}{
		{name: "exact file", member: "settings/config.yml", want: true},
		{name: "directory member", member: "skills/web/SKILL.md", want: true},
		{name: "mcp exact member", member: "mcp/fetch.json", want: true},
		{name: "prefix boundary", member: "skills/webster/SKILL.md", want: false},
		{name: "unlocked", member: "skills/other/SKILL.md", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := locks.Covers(tt.member); got != tt.want {
				t.Errorf("Covers(%q) = %t, want %t", tt.member, got, tt.want)
			}
		})
	}
}
