package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
	"time"
)

func TestMergeConfigBundles(t *testing.T) {
	bundle := func(files map[string]string) []byte {
		entries := make([]tarEntry, 0, len(files))
		for name, content := range files {
			entries = append(entries, tarEntry{name: name, content: content})
		}
		return buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), entries...)
	}
	tests := []struct {
		name       string
		in         LayerBundles
		want       map[string]string
		wantLayers map[string]ConfigLayer
		wantDrop   []string
	}{
		{
			name: "same-layer skill files are all retained",
			in: LayerBundles{
				Default: bundle(map[string]string{"skills/x/a": "a", "skills/x/b": "b", "skills/y/a": "y"}),
				User:    bundle(map[string]string{"skills/x/c": "c", "skills/x/d": "d"}),
			},
			want:       map[string]string{"skills/x/c": "c", "skills/x/d": "d", "skills/y/a": "y"},
			wantLayers: map[string]ConfigLayer{"skills/x/c": LayerUser, "skills/x/d": LayerUser, "skills/y/a": LayerDefault},
		},
		{
			name: "user file replaces whole tenant file",
			in: LayerBundles{
				Default: bundle(map[string]string{"settings/config.yml": "default: yes\n"}),
				Tenant:  bundle(map[string]string{"settings/config.yml": "tenant: yes\n"}),
				User:    bundle(map[string]string{"settings/config.yml": "user: yes\n"}),
			},
			want:       map[string]string{"settings/config.yml": "user: yes\n"},
			wantLayers: map[string]ConfigLayer{"settings/config.yml": LayerUser},
		},
		{
			name: "user skill replaces entire default directory",
			in: LayerBundles{
				Default: bundle(map[string]string{"skills/x/a": "a", "skills/x/b": "b", "skills/y/a": "y"}),
				User:    bundle(map[string]string{"skills/x/c": "c"}),
			},
			want:       map[string]string{"skills/x/c": "c", "skills/y/a": "y"},
			wantLayers: map[string]ConfigLayer{"skills/x/c": LayerUser, "skills/y/a": LayerDefault},
		},

		{
			name: "user settings replaces tenant-only keys",
			in: LayerBundles{
				Default: bundle(map[string]string{"settings/config.yml": "base: true\ntenantOnly: false\n"}),
				Tenant:  bundle(map[string]string{"settings/config.yml": "base: true\ntenantOnly: true\n"}),
				User:    bundle(map[string]string{"settings/config.yml": "base: false\n"}),
			},
			want:       map[string]string{"settings/config.yml": "base: false\n"},
			wantLayers: map[string]ConfigLayer{"settings/config.yml": LayerUser},
		},
		{
			name: "role wins and empty MCP server removes fetch",
			in: LayerBundles{
				Default: bundle(map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"a"}}}`}),
				Tenant:  bundle(map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"b"}}}`}),
				User:    bundle(map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"c"}}}`}),
				Role:    bundle(map[string]string{"mcp/fetch.json": `{"mcpServers":{}}`}),
			},
			want:       map[string]string{"mcp/fetch.json": `{"mcpServers":{}}`},
			wantLayers: map[string]ConfigLayer{"mcp/fetch.json": LayerRole},
		},
		{
			name: "locks drop user and role members and never ship locks",
			in: LayerBundles{
				Default: bundle(map[string]string{"settings/config.yml": "default: base\n"}),
				Tenant:  bundle(map[string]string{"locks.yml": "paths:\n  - skills/web/\n  - settings/config.yml\n", "settings/config.yml": "tenant: yes\n"}),
				User:    bundle(map[string]string{"skills/web/b": "b", "settings/config.yml": "user: yes\n"}),
				Role:    bundle(map[string]string{"skills/web/c": "c"}),
			},
			want:       map[string]string{"settings/config.yml": "tenant: yes\n"},
			wantLayers: map[string]ConfigLayer{"settings/config.yml": LayerTenant},
			wantDrop:   []string{"settings/config.yml", "skills/web/b", "skills/web/c"},
		},
		{
			name: "four layer member resolves role",
			in: LayerBundles{
				Default: bundle(map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"default"}}}`}),
				Tenant:  bundle(map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"tenant"}}}`}),
				User:    bundle(map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"user"}}}`}),
				Role:    bundle(map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"role"}}}`}),
			},
			want:       map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"role"}}}`},
			wantLayers: map[string]ConfigLayer{"mcp/fetch.json": LayerRole},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, members, dropped, err := MergeConfigBundles(tt.in)
			if err != nil {
				t.Fatalf("MergeConfigBundles: %v", err)
			}
			gotFiles := bundleFiles(t, got)
			if len(gotFiles) != len(tt.want) {
				t.Fatalf("members = %v, want %v", gotFiles, tt.want)
			}
			for path, content := range tt.want {
				if gotFiles[path] != content {
					t.Errorf("%s = %q, want %q", path, gotFiles[path], content)
				}
			}
			if len(members) != len(tt.wantLayers) {
				t.Fatalf("effective members = %v, want %v", members, tt.wantLayers)
			}
			for _, member := range members {
				if tt.wantLayers[member.Path] != member.Layer {
					t.Errorf("member %q layer = %d, want %d", member.Path, member.Layer, tt.wantLayers[member.Path])
				}
			}
			gotDropped := make([]string, len(dropped))
			for i, member := range dropped {
				gotDropped[i] = member.Path
			}
			if strings.Join(gotDropped, ",") != strings.Join(tt.wantDrop, ",") {
				t.Errorf("dropped = %v, want %v", gotDropped, tt.wantDrop)
			}
		})
	}
}

func TestEffectiveConfigVersion(t *testing.T) {
	if got := EffectiveConfigVersion("default", "", "", ""); got != "default" {
		t.Fatalf("default-only version = %q, want default", got)
	}
	a := EffectiveConfigVersion("d", "t", "u", "r")
	if len(a) != 64 || strings.ToLower(a) != a {
		t.Fatalf("merged version = %q, want lowercase SHA-256 hex", a)
	}
	for _, versions := range [][4]string{{"x", "t", "u", "r"}, {"d", "x", "u", "r"}, {"d", "t", "x", "r"}, {"d", "t", "u", "x"}} {
		if EffectiveConfigVersion(versions[0], versions[1], versions[2], versions[3]) == a {
			t.Errorf("changed layer version %v did not change effective version", versions)
		}
	}
	if EffectiveConfigVersion("ab", "c", "", "") == EffectiveConfigVersion("a", "bc", "", "") {
		t.Fatal("length framing allowed distinct layer versions to collide")
	}
}

func bundleFiles(t *testing.T, bundle []byte) map[string]string {
	t.Helper()
	files := map[string]string{}
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() {
		if err := gz.Close(); err != nil {
			t.Errorf("gzip close: %v", err)
		}
	}()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %q: %v", hdr.Name, err)
		}
		files[hdr.Name] = string(data)
	}
	return files
}
