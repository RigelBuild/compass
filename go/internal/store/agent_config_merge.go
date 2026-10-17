package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"
)

const configMergeEpoch = "1"

// ConfigLayer identifies the precedence layer that owns a config member.
type ConfigLayer int8

// LayerDefault is the base fleet bundle layer.
// LayerTenant is the tenant-owned overlay layer.
// LayerUser is the user's overlay layer.
// LayerRole is the user's role-specific overlay layer.
const (
	LayerDefault ConfigLayer = iota + 1
	LayerTenant
	LayerUser
	LayerRole
)

// EffectiveMember reports a config path and the layer that supplies it.
type EffectiveMember struct {
	Path  string
	Layer ConfigLayer
}

// LayerBundles holds the optional bundles in merge precedence order.
type LayerBundles struct {
	Default []byte
	Tenant  []byte
	User    []byte
	Role    []byte
}

type configLayerBundle struct {
	layer  ConfigLayer
	bundle []byte
}

type configMember struct {
	content []byte
	layer   ConfigLayer
}

// MergeConfigBundles resolves the bundles in precedence order and returns winning members and tenant-lock drops.
func MergeConfigBundles(in LayerBundles) ([]byte, []EffectiveMember, []EffectiveMember, error) {
	layers := []configLayerBundle{
		{layer: LayerDefault, bundle: in.Default},
		{layer: LayerTenant, bundle: in.Tenant},
		{layer: LayerUser, bundle: in.User},
		{layer: LayerRole, bundle: in.Role},
	}
	selected := make(map[string]configMember)
	var locks ConfigLocks
	var dropped []EffectiveMember
	for _, layer := range layers {
		if layer.bundle == nil {
			continue
		}
		files, err := readValidatedLayer(layer)
		if err != nil {
			return nil, nil, nil, err
		}
		if layer.layer == LayerTenant {
			lockBody, ok := files[memberLocksYML]
			if ok {
				locks, err = parseConfigLocks(lockBody)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("invalid stored tenant locks: %s", err.Error())
				}
				delete(files, memberLocksYML)
			}
		}
		applyConfigLayer(selected, files, layer.layer, locks, &dropped)
	}

	members := effectiveConfigMembers(selected)
	if in.Tenant == nil && in.User == nil && in.Role == nil {
		if in.Default != nil {
			return in.Default, members, dropped, nil
		}
		bundle, err := writeConfigBundle(nil)
		return bundle, members, dropped, err
	}
	bundle, err := writeEffectiveConfigBundle(selected, dropped)
	if err != nil {
		return nil, nil, nil, err
	}
	return bundle, members, dropped, nil
}

func readValidatedLayer(layer configLayerBundle) (map[string][]byte, error) {
	if err := validateStoredConfigBundle(layer.bundle, layer.layer); err != nil {
		return nil, fmt.Errorf("invalid stored config layer %d bundle: %s", layer.layer, err.Error())
	}
	files, err := readConfigBundleMembers(layer.bundle)
	if err != nil {
		return nil, fmt.Errorf("read stored config layer %d bundle: %s", layer.layer, err.Error())
	}
	return files, nil
}

func applyConfigLayer(selected map[string]configMember, files map[string][]byte, layer ConfigLayer, locks ConfigLocks, dropped *[]EffectiveMember) {
	memberDirectories := make(map[string]struct{})
	for path := range files {
		path = canonicalConfigPath(path)
		if (layer == LayerUser || layer == LayerRole) && locks.Covers(path) {
			*dropped = append(*dropped, EffectiveMember{Path: path, Layer: layer})
			continue
		}
		if dir := configDirectoryMember(path); dir != "" {
			memberDirectories[dir] = struct{}{}
		}
	}
	for dir := range memberDirectories {
		for selectedPath := range selected {
			if configDirectoryMember(selectedPath) == dir {
				delete(selected, selectedPath)
			}
		}
	}
	for path, content := range files {
		path = canonicalConfigPath(path)
		if (layer == LayerUser || layer == LayerRole) && locks.Covers(path) {
			continue
		}
		selected[path] = configMember{content: content, layer: layer}
	}

}

func canonicalConfigPath(path string) string {
	return strings.TrimSuffix(path, "/")
}

func effectiveConfigMembers(selected map[string]configMember) []EffectiveMember {
	members := make([]EffectiveMember, 0, len(selected))
	for path, member := range selected {
		members = append(members, EffectiveMember{Path: path, Layer: member.layer})
	}
	sortEffectiveMembers(members)
	return members
}

func writeEffectiveConfigBundle(selected map[string]configMember, dropped []EffectiveMember) ([]byte, error) {
	if len(selected) > maxFileCount {
		return nil, fmt.Errorf("%w: effective bundle exceeds file-count cap of %d files", ErrInvalidArgument, maxFileCount)
	}
	paths := make([]string, 0, len(selected))
	for path := range selected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]configBundleEntry, 0, len(paths))
	var contentBytes int64
	for _, path := range paths {
		content := selected[path].content
		if int64(len(content)) > maxDecompressedBytes-contentBytes {
			return nil, fmt.Errorf("%w: effective bundle exceeds decompressed size cap of %d bytes", ErrInvalidArgument, maxDecompressedBytes)
		}
		contentBytes += int64(len(content))
		entries = append(entries, configBundleEntry{name: path, content: content})
	}
	sortEffectiveMembers(dropped)
	bundle, err := writeConfigBundle(entries)
	if err != nil {
		return nil, fmt.Errorf("write merged config bundle: %w", err)
	}
	return bundle, nil
}

// EffectiveConfigVersion hashes the framed layer versions, or returns the default version when no overlays exist.
func EffectiveConfigVersion(def, tenant, user, role string) string {
	if tenant == "" && user == "" && role == "" {
		return def
	}
	h := sha256.New()
	var size [8]byte
	for _, part := range []string{configMergeEpoch, def, tenant, user, role} {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = io.WriteString(h, part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func configDirectoryMember(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 2 || (parts[0] != topDirSkills && parts[0] != topDirExtensions) {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func sortEffectiveMembers(members []EffectiveMember) {
	sort.Slice(members, func(i, j int) bool {
		return members[i].Path < members[j].Path
	})
}

type configBundleEntry struct {
	name    string
	content []byte
}

func writeConfigBundle(entries []configBundleEntry) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o644,
			Size:     int64(len(entry.content)),
			ModTime:  time.Unix(0, 0),
			Typeflag: tar.TypeReg,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := tarWriter.Write(entry.content); err != nil {
			return nil, err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func readConfigBundleMembers(bundle []byte) (map[string][]byte, error) {
	members := make(map[string][]byte)
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(&cappedReader{r: gz})
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return members, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		members[canonicalConfigPath(header.Name)] = content
	}
}

func warnDroppedConfigMembers(ctx context.Context, dropped []EffectiveMember) {
	for _, member := range dropped {
		slog.WarnContext(ctx, "tenant config lock dropped stored overlay member", "layer", member.Layer, "path", member.Path)
	}
}
func warnEffectiveProfileAgentOrphans(ctx context.Context, bundle []byte) {
	files, err := readConfigBundleMembers(bundle)
	if err != nil {
		return
	}
	agentNames := make(map[string]struct{})
	for path, body := range files {
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[0] == topDirAgents {
			if name := agentDefFrontmatterName(body); name != "" {
				agentNames[name] = struct{}{}
			}
		}
	}
	for path, body := range files {
		parts := strings.Split(path, "/")
		if len(parts) != 3 || parts[0] != topDirProfiles || parts[2] != memberProfileYML {
			continue
		}
		mapping, err := parseYAMLMapping(body, path)
		if err != nil {
			continue
		}
		models, ok := mapping["models"].(map[string]any)
		if !ok {
			continue
		}
		agents, ok := models["agents"].(map[string]any)
		if !ok {
			continue
		}
		for name := range agents {
			if _, ok := agentNames[name]; !ok {
				slog.WarnContext(ctx, "effective config profile references missing agent", "profile", parts[1], "agent", name)
			}
		}
	}
}
