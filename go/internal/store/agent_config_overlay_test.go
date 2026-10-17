package store

import (
	"compress/gzip"
	"errors"
	"strings"
	"testing"
	"time"
)

func validateBundleErrorForLayer(bundle []byte, layer ConfigLayer) error {
	_, _, err := validateAndHashConfigBundle(bundle, layer)
	return err
}
func TestValidateConfigBundleLayers(t *testing.T) {
	tests := []struct {
		name    string
		layer   ConfigLayer
		entries []tarEntry
		wantErr bool
	}{
		{name: "default admits profiles", layer: LayerDefault, entries: []tarEntry{{name: "profiles/x/profile.yml", content: "models: {}\n"}}},
		{name: "tenant admits locks", layer: LayerTenant, entries: []tarEntry{{name: "locks.yml", content: "paths: []\n"}}},
		{name: "default rejects locks", layer: LayerDefault, entries: []tarEntry{{name: "locks.yml", content: "paths: []\n"}}, wantErr: true},
		{name: "overlay rejects profiles", layer: LayerUser, entries: []tarEntry{{name: "profiles/x/profile.yml", content: "models: {}\n"}}, wantErr: true},
		{name: "locks reject nested directory member", layer: LayerTenant, entries: []tarEntry{{name: "skills/x/locks.yml", content: "paths: []\n"}}, wantErr: true},
		{name: "extension directory path rejected", layer: LayerDefault, entries: []tarEntry{{name: "extensions/foo", content: "x"}}, wantErr: true},
		{name: "default cap accepts default size", layer: LayerDefault, entries: []tarEntry{{name: "skills/x/f", content: "#" + strings.Repeat("x", int(defaultLayerCaps.bytes)-4096)}}},
		{name: "overlay cap rejects default size", layer: LayerUser, entries: []tarEntry{{name: "skills/x/f", content: "#" + strings.Repeat("x", int(overlayLayerCaps.bytes)+1)}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tt.entries...)
			err := validateBundleErrorForLayer(bundle, tt.layer)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAndHashConfigBundle error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestValidateStoredConfigBundleChecksPathsAndCapsOnly(t *testing.T) {
	validMCP := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tarEntry{
		name:    "mcp/fetch.json",
		content: `{"mcpServers":{"fetch":{"url":"https://user:pass@example.com?token=secret"}}}`,
	})
	if err := validateStoredConfigBundle(validMCP, LayerUser); err != nil {
		t.Fatalf("stored path validator ran per-value MCP lint: %v", err)
	}
	badPath := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tarEntry{name: "unknown/a.txt", content: "x"})
	if err := validateStoredConfigBundle(badPath, LayerUser); err == nil {
		t.Fatal("stored path validator accepted unknown top-level path")
	}
	overCap := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tarEntry{
		name:    "skills/x/SKILL.md",
		content: strings.Repeat("x", int(maxDecompressedBytes)+1),
	})
	if err := validateStoredConfigBundle(overCap, LayerUser); err == nil {
		t.Fatal("stored path validator accepted over-effective-cap content")
	}
}

func TestOverlayLayerProfileAgentLintIsLayerLocal(t *testing.T) {
	bundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "profiles/x/profile.yml", content: "models: {}\n"},
	)
	if _, _, err := validateAndHashConfigBundle(bundle, LayerUser); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("overlay profile error = %v, want ErrInvalidArgument", err)
	}
}

func TestValidateMCPMemberOverlayShapeAndCredentials(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		body    string
		wantErr bool
	}{
		{name: "only matching server accepted", path: "mcp/a.json", body: `{"mcpServers":{"a":{"command":"srv"}}}`},
		{name: "empty server map accepted", path: "mcp/a.json", body: `{"mcpServers":{}}`},
		{name: "missing mcpServers rejected", path: "mcp/a.json", body: `{}`, wantErr: true},
		{name: "two server keys rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{},"b":{}}}`, wantErr: true},
		{name: "mis-keyed server rejected", path: "mcp/a.json", body: `{"mcpServers":{"b":{}}}`, wantErr: true},
		{name: "client secret rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"oauth":{"clientSecret":"x"}}}}`, wantErr: true},
		{name: "auth secret rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"auth":{"clientSecret":"x"}}}}`, wantErr: true},
		{name: "env policy rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"envPolicy":"allow"}}}`, wantErr: true},
		{name: "header policy rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"headerPolicy":"allow"}}}`, wantErr: true},
		{name: "literal env rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"env":{"TOKEN":"literal value"}}}}`, wantErr: true},
		{name: "env name accepted", path: "mcp/a.json", body: `{"mcpServers":{"a":{"env":{"TOKEN":"API_TOKEN"}}}}`},
		{name: "command indirection accepted", path: "mcp/a.json", body: `{"mcpServers":{"a":{"env":{"TOKEN":"!command"}}}}`},
		{name: "userinfo in token URL rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"auth":{"tokenUrl":"https://user:pass@host/token"}}}}`, wantErr: true},
		{name: "token query in URL rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host/?token=x"}}}`, wantErr: true},
		{name: "exa key in args rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"args":["exaApiKey=x"]}}}`, wantErr: true},
		{name: "credential free URL accepted", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host/mcp"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := strings.Split(tt.path, "/")
			_, err := validateMCPMember(path, tt.path, strings.NewReader(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateMCPMember error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestExtensionManifestIdentity(t *testing.T) {
	tests := []struct {
		name     string
		manifest map[string][]byte
		members  map[string]bool
		wantErr  bool
	}{
		{
			name:     "root index derives extension directory",
			manifest: map[string][]byte{"foo": []byte(`{"omp":{"extensions":["index.ts"]}}`)},
			members:  map[string]bool{"extensions/foo/package.json": true, "extensions/foo/index.ts": true},
		},
		{
			name:     "nested src index derives src and mismatches extension ID",
			manifest: map[string][]byte{"foo": []byte(`{"omp":{"extensions":["src/index.ts"]}}`)},
			members:  map[string]bool{"extensions/foo/package.json": true, "extensions/foo/src/index.ts": true},
			wantErr:  true,
		},
		{
			name:     "extension ID src matches nested src index",
			manifest: map[string][]byte{"src": []byte(`{"omp":{"extensions":["src/index.ts"]}}`)},
			members:  map[string]bool{"extensions/src/package.json": true, "extensions/src/src/index.ts": true},
		},
		{
			name:     "named nested file derives basename",
			manifest: map[string][]byte{"foo": []byte(`{"omp":{"extensions":["src/foo.ts"]}}`)},
			members:  map[string]bool{"extensions/foo/package.json": true, "extensions/foo/src/foo.ts": true},
		},
		{
			name:     "traversal rejected",
			manifest: map[string][]byte{"foo": []byte(`{"omp":{"extensions":["../bar/index.ts"]}}`)},
			members:  map[string]bool{"extensions/foo/package.json": true},
			wantErr:  true,
		},
		{
			name:    "dotted root extension rejected",
			members: map[string]bool{"extensions/foo.ts": true},
			wantErr: true,
		},
		{
			name:     "unparseable manifest skipped",
			manifest: map[string][]byte{"foo": []byte("{")},
			members:  map[string]bool{"extensions/foo/package.json": true, "extensions/foo/index.ts": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := lintExtensionManifests(tt.manifest, tt.members)
			if (err != nil) != tt.wantErr {
				t.Fatalf("lintExtensionManifests error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestOverlayLayerCapsFitEffectiveCap(t *testing.T) {
	entries := make([]tarEntry, 0, 4)
	for i := range 4 {
		entries = append(entries, tarEntry{name: "skills/part" + strings.Repeat("x", i) + "/f", content: strings.Repeat("x", (int(overlayLayerCaps.bytes)-4096)/4)})
	}
	bundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), entries...)
	if _, _, err := validateAndHashConfigBundle(bundle, LayerUser); err != nil {
		t.Fatalf("four overlay-size members in one layer should fit its cap: %v", err)
	}
}

func TestConfigBundleRejectsNestedLocksYML(t *testing.T) {
	bundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/x/locks.yml", content: "paths: []\n"},
	)
	if _, _, err := validateAndHashConfigBundle(bundle, LayerTenant); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nested locks.yml error = %v, want ErrInvalidArgument", err)
	}
}
