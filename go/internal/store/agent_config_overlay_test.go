package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
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
		{name: "credential password query rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host/?password=x"}}}`, wantErr: true},
		{name: "credential sig query rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host/?sig=x"}}}`, wantErr: true},
		{name: "credential pwd query rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host/?pwd=x"}}}`, wantErr: true},
		{name: "special scheme URL token query rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https:host/?token=x"}}}`, wantErr: true},
		{name: "special scheme auth tokenUrl password query rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"auth":{"tokenUrl":"HTTPS:host/?password=x"}}}}`, wantErr: true},
		{name: "special scheme args sig query rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"args":["https:host/?sig=x"]}}}`, wantErr: true},
		{name: "token query in URL rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host/?token=x"}}}`, wantErr: true},
		{name: "key suffix query rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host/?monkey=x"}}}`, wantErr: true},
		{name: "secret suffix query rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host/?xSecretHint=x"}}}`, wantErr: true},
		{name: "benign version query accepted", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host/?api-version=2024-10-01"}}}`},
		{name: "userinfo authority URL rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https://host@evil/?x=v"}}}`, wantErr: true},
		{name: "backslash authority URL rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https:\\user@host"}}}`, wantErr: true},
		{name: "double backslash authority URL rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"url":"https:\\\\user@host"}}}`, wantErr: true},
		{name: "mixed authority tokenUrl rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"auth":{"tokenUrl":"https://a\\b@c"}}}}`, wantErr: true},
		{name: "userinfo authority URL argument rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"args":["https://host@evil/?x=v"]}}}`, wantErr: true},
		{name: "backslash authority URL argument rejected", path: "mcp/a.json", body: `{"mcpServers":{"a":{"args":["https://a\\b@c"]}}}`, wantErr: true},
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
			name:     "dot slash root entry resolves",
			manifest: map[string][]byte{"foo": []byte(`{"omp":{"extensions":["./entry.js"]}}`)},
			members:  map[string]bool{"extensions/foo/package.json": true, "extensions/foo/entry.js": true},
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

func TestTenantLockBundleIsValidatedAtDoor(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed YAML", body: "paths: [\n"},
		{name: "invalid member path", body: "paths:\n  - unknown/file\n"},
		{name: "exact path inside skills directory", body: "paths:\n  - skills/web/SKILL.md\n"},
		{name: "exact path inside extension directory", body: "paths:\n  - extensions/web/main.ts\n"},
		{name: "trailing slash regular-file lock", body: "paths:\n  - settings/config.yml/\n"},
		{name: "directory prefix must end in slash", body: "paths:\n  - skills/web\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tarEntry{name: "locks.yml", content: tt.body})
			if err := validateBundleErrorForLayer(bundle, LayerTenant); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("tenant lock bundle error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestConfigBundleDoorAndStoredPathGrammarParity(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		typeflag byte
		layer    ConfigLayer
		content  string
		wantErr  bool
	}{
		{name: "nested skill directory", path: "skills/x/references/", typeflag: tar.TypeDir, layer: LayerUser},
		{name: "nested extension directory", path: "extensions/x/src/", typeflag: tar.TypeDir, layer: LayerDefault},
		{name: "nested prompt directory rejected", path: "prompts/manager/extra/", typeflag: tar.TypeDir, layer: LayerDefault, wantErr: true},
		{name: "stored invalid settings path", path: "settings/other.yml", layer: LayerUser, content: "key: value\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tarEntry{name: tt.path, typeflag: tt.typeflag, content: tt.content})
			doorErr := validateBundleErrorForLayer(bundle, tt.layer)
			storedErr := validateStoredConfigBundle(bundle, tt.layer)
			if (doorErr != nil) != tt.wantErr || (storedErr != nil) != tt.wantErr {
				t.Fatalf("door error = %v, stored error = %v, wantErr %t", doorErr, storedErr, tt.wantErr)
			}
		})
	}
}
func TestConfigBundleAcceptsRegularHeaderWithTrailingSlash(t *testing.T) {
	name := "settings/config.yml/"
	var tarBytes bytes.Buffer
	header := make([]byte, 512)
	copy(header[:100], name)
	copy(header[100:108], "0000644\x00")
	copy(header[108:116], "0000000\x00")
	copy(header[116:124], "0000000\x00")
	copy(header[124:136], "00000000006\x00")
	copy(header[136:148], "00000000000\x00")
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	header[156] = tar.TypeReg
	copy(header[257:263], "ustar\x00")
	copy(header[263:265], "00")
	var checksum int64
	for _, value := range header {
		checksum += int64(value)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", checksum))
	tarBytes.Write(header)
	tarBytes.WriteString("key: x\n")
	tarBytes.Write(make([]byte, 512-7))
	tarBytes.Write(make([]byte, 1024))
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	if _, err := gz.Write(tarBytes.Bytes()); err != nil {
		t.Fatalf("gzip raw tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close raw tar gzip: %v", err)
	}
	if err := validateBundleErrorForLayer(raw.Bytes(), LayerUser); err != nil {
		t.Fatalf("regular header with trailing slash: %v", err)
	}
	if err := validateStoredConfigBundle(raw.Bytes(), LayerUser); err != nil {
		t.Fatalf("stored regular header with trailing slash: %v", err)
	}
}

func TestConfigLocksAcceptMCPMembersAndPromptPrefixes(t *testing.T) {
	locks, err := parseConfigLocks([]byte("paths:\n  - mcp/fetch.json\n  - prompts/\n  - prompts/manager/\n"))
	if err != nil {
		t.Fatalf("parseConfigLocks: %v", err)
	}
	for _, path := range []string{"mcp/fetch.json", "prompts/manager/SYSTEM.md"} {
		if !locks.Covers(path) {
			t.Errorf("lock does not cover %q", path)
		}
	}
	if _, err := parseConfigLocks([]byte("paths:\n  - prompts/manager\n")); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("non-slash directory lock error = %v, want ErrInvalidArgument", err)
	}
}
func TestMergeConfigBundlesEnforcesSerializedCap(t *testing.T) {
	content := strings.Repeat("x", 32<<20)
	defaultBundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tarEntry{name: "skills/default/file", content: content})
	tenantBundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tarEntry{name: "skills/tenant/file", content: content})
	if err := validateStoredConfigBundle(defaultBundle, LayerDefault); err != nil {
		t.Fatalf("validate stored default bundle: %v", err)
	}
	if err := validateStoredConfigBundle(tenantBundle, LayerTenant); err != nil {
		t.Fatalf("validate stored tenant bundle: %v", err)
	}
	if _, _, _, _, err := MergeConfigBundles(LayerBundles{Default: defaultBundle, Tenant: tenantBundle}); err == nil {
		t.Fatal("merged tar stream over decompressed cap was accepted")
	}
}

func TestMergeConfigBundlesEnforcesEffectiveCaps(t *testing.T) {
	t.Run("merged content size", func(t *testing.T) {
		bundle := func(name, content string) []byte {
			return buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tarEntry{name: name, content: content})
		}
		_, _, _, _, err := MergeConfigBundles(LayerBundles{
			Default: bundle("skills/default/file", strings.Repeat("d", 40<<20)),
			Tenant:  bundle("skills/tenant/file", strings.Repeat("t", 30<<20)),
		})
		if err == nil {
			t.Fatal("merged content above the effective cap was accepted")
		}
	})
	t.Run("merged file count", func(t *testing.T) {
		makeBundle := func(prefix string, count int) []byte {
			entries := make([]tarEntry, 0, count)
			for i := range count {
				entries = append(entries, tarEntry{name: fmt.Sprintf("skills/%s%d/file", prefix, i), content: "x"})
			}
			return buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), entries...)
		}
		_, _, _, _, err := MergeConfigBundles(LayerBundles{
			Default: makeBundle("d", 3000),
			Tenant:  makeBundle("t", 3000),
		})
		if err == nil {
			t.Fatal("merged file count above the effective cap was accepted")
		}
	})
}
