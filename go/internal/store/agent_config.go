package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// The fleet CONFIG-BUNDLE store (SEA-1624 T1). One fleet-wide singleton bundle
// row (agent_config_bundle, 0008_agent_config) holds the gzip-tarball of the
// skills/, extensions/, and mcp/ material every agent materializes into its
// scoped config dir (T3/T4). Unlike the secrets NAMES registry (secrets.go, a
// set of named rows), config is CURRENT-ONLY: PutAgentConfig replaces the one
// row in place, and version is the canonical CONTENT hash so a re-put of
// identical content is version-stable. The bundle is credential-free by MVP
// rule (CD-3) — secrets ride the separate resolve path, never this bundle.

// configBundleTopDirs is the whitelist of allowed top-level directories in a
// config bundle. Every tar member must live under one of these; each becomes a
// host directory when the bundle is materialized into an agent's config dir
// (T4), so the set is closed at the store door, not filtered downstream.
var configBundleTopDirs = map[string]bool{
	"skills":     true,
	"extensions": true,
	"mcp":        true,
}

// configNamePattern is the grammar for a config entry's <name> segment —
// skills/<name>, extensions/<name>, mcp/<name>.json. A declared name is
// validated at the store door (PutAgentConfig) before it can reach a row
// because it later becomes a host path segment under the agent's config dir
// (T4): constrained at the door, not escaped downstream (mirrors secrets.go's
// secretNamePattern posture).
var configNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const (
	// maxDecompressedBytes caps the total DECOMPRESSED size of a config bundle,
	// enforced DURING gunzip (cappedReader) so a gzip bomb — a few KiB that
	// inflates to gigabytes — is aborted mid-stream rather than after a full
	// decompress. 64 MiB is generous headroom over the skills/extensions/mcp
	// material a fleet realistically ships while still bounding a single Put's
	// memory and the BYTEA row.
	maxDecompressedBytes int64 = 64 << 20 // 64 MiB
	// maxFileCount caps the number of regular-file members, also enforced during
	// the streamed read, so a bundle of millions of tiny files (each under the
	// byte cap) cannot exhaust memory/handles. 4096 files comfortably covers a
	// realistic skill/extension/mcp set.
	maxFileCount = 4096
)

// errBundleTooLarge is returned through the tar/gzip read stack the moment the
// decompressed byte cap is exceeded. It wraps ErrInvalidArgument so the door's
// caller sees a field error, and is a distinct sentinel so the read loop can
// recognize it via errors.Is after tar propagates it.
var errBundleTooLarge = fmt.Errorf("%w: bundle exceeds decompressed size cap of %d bytes", ErrInvalidArgument, maxDecompressedBytes)

// cappedReader bounds the total bytes read from an underlying reader, returning
// errBundleTooLarge once the cap is crossed. Wrapping the gunzip stream (the
// DECOMPRESSED side) with it is the gzip-bomb defense: the tar reader can never
// pull more than maxDecompressedBytes of inflated content regardless of how
// small the compressed input is.
type cappedReader struct {
	r io.Reader
	n int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	nn, err := c.r.Read(p)
	c.n += int64(nn)
	if c.n > maxDecompressedBytes {
		return nn, errBundleTooLarge
	}
	return nn, err
}

// PutAgentConfig validates a fleet config bundle at the store door and, if it
// passes, upserts it as the single current bundle (SEA-1624 T1). It returns the
// bundle's canonical content version (canonicalConfigVersion): the sha256 over
// the DECOMPRESSED, metadata-zeroed (path, bytes) content, so tar member
// ordering, mtimes/uid/gid, and gzip framing never perturb it — a re-put of
// byte-identical CONTENT yields the same version and replaces the row in place
// (current-only retention via the singleton PK upsert).
//
// Validation runs BEFORE any row write: a bundle that is not a gzip tarball,
// whose members escape the whitelisted skills/|extensions/|mcp/ top dirs, use
// absolute or ".." paths, carry a symlink or hardlink member, violate the
// <name> grammar, exceed the decompressed-size or file-count cap, or contain an
// mcp/*.json that is not valid JSON, is rejected as a %w-wrapped
// ErrInvalidArgument and NO row is written. actor is the operator-scoped writer
// (empty → ErrInvalidArgument); the singleton has no per-actor column, but the
// param documents the operator-scoped write and matches the record.
func (s *Store) PutAgentConfig(ctx context.Context, actor AccountID, bundle []byte) (version string, err error) {
	if actor == "" {
		return "", fmt.Errorf("%w: config-bundle writer account id is required", ErrInvalidArgument)
	}
	version, err = validateAndHashConfigBundle(bundle)
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO agent_config_bundle (singleton, version, bundle)
		 VALUES (TRUE, $1, $2)
		 ON CONFLICT (singleton)
		 DO UPDATE SET version = EXCLUDED.version, bundle = EXCLUDED.bundle, created_at = now()`,
		version, bundle,
	); err != nil {
		return "", fmt.Errorf("store: put agent config: %w", err)
	}
	return version, nil
}

// CurrentAgentConfig returns the single current config bundle and its canonical
// content version. ErrNotFound when no bundle has been declared — a valid state
// downstream (the fetch path then materializes an empty config dir), but the
// store still reports the absence; the caller decides empty-is-ok.
func (s *Store) CurrentAgentConfig(ctx context.Context) (version string, bundle []byte, err error) {
	if err := s.pool.QueryRow(ctx,
		`SELECT version, bundle FROM agent_config_bundle WHERE singleton = TRUE`,
	).Scan(&version, &bundle); err != nil {
		if noRows(err) {
			return "", nil, fmt.Errorf("%w: no agent config bundle declared", ErrNotFound)
		}
		return "", nil, fmt.Errorf("store: read agent config: %w", err)
	}
	return version, bundle, nil
}

// validateAndHashConfigBundle is the security-critical store door: a SINGLE
// streamed pass over the gzip tarball that both validates every member and
// accumulates the canonical content hash, returning a %w-wrapped
// ErrInvalidArgument on the first violation (so nothing is decompressed twice).
//
// Canonical version serialization (metadata-zeroed, order-independent): collect
// (name, content) for every regular file, sort by name, then hash a
// LENGTH-PREFIXED framing so two distinct member sets cannot collide —
//
//	uint64(len(members))
//	for each member in name order:
//	    uint64(len(name)) name
//	    uint64(len(content)) content
//
// all big-endian. Only the member NAME and file CONTENT feed the hash; tar
// ordering, mtimes, uid/gid, and gzip mtime/level are excluded, so identical
// content re-packed any which way hashes identically.
func validateAndHashConfigBundle(bundle []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return "", fmt.Errorf("%w: bundle is not a valid gzip stream: %w", ErrInvalidArgument, err)
	}
	// Read-only gunzip: Close only releases the decompressor, so its error is
	// not actionable here (nothing was written to flush).
	defer func() { _ = gz.Close() }()

	type member struct {
		name    string
		content []byte
	}
	var members []member
	fileCount := 0

	tr := tar.NewReader(&cappedReader{r: gz})
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, errBundleTooLarge) {
				return "", errBundleTooLarge
			}
			return "", fmt.Errorf("%w: bundle is not a valid tar stream: %w", ErrInvalidArgument, err)
		}

		// Reject the escape-vector typeflags outright. A symlink and a hardlink
		// are DISTINCT escapes (TypeSymlink vs TypeLink); both can point outside
		// the materialized config dir, so both are refused before any path
		// analysis.
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			return "", fmt.Errorf("%w: bundle member %q is a symlink (not allowed)", ErrInvalidArgument, hdr.Name)
		case tar.TypeLink:
			return "", fmt.Errorf("%w: bundle member %q is a hardlink (not allowed)", ErrInvalidArgument, hdr.Name)
		}

		parts, err := configMemberParts(hdr.Name)
		if err != nil {
			return "", err
		}

		if !hdr.FileInfo().Mode().IsRegular() {
			// A directory (or other non-escape typeflag) contributes no content
			// and no host file; its path has already passed the escape +
			// whitelist checks in configMemberParts, which is all that matters.
			continue
		}

		content, err := validateRegularMember(parts, tr)
		if err != nil {
			if errors.Is(err, errBundleTooLarge) {
				return "", errBundleTooLarge
			}
			return "", err
		}

		fileCount++
		if fileCount > maxFileCount {
			return "", fmt.Errorf("%w: bundle exceeds file-count cap of %d files", ErrInvalidArgument, maxFileCount)
		}
		members = append(members, member{name: hdr.Name, content: content})
	}

	sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })

	h := sha256.New()
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(members)))
	h.Write(lenBuf[:])
	for _, m := range members {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(m.name)))
		h.Write(lenBuf[:])
		h.Write([]byte(m.name))
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(m.content)))
		h.Write(lenBuf[:])
		h.Write(m.content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// configMemberParts validates a member path for escapes and the top-dir
// whitelist, returning its cleaned path components. It rejects an absolute
// path, the empty/root path, and any "", ".", or ".." component (the ".."
// traversal escape), then requires the top-level directory to be one of
// skills/|extensions/|mcp/.
func configMemberParts(name string) ([]string, error) {
	if strings.HasPrefix(name, "/") {
		return nil, fmt.Errorf("%w: bundle member %q is an absolute path", ErrInvalidArgument, name)
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" {
		return nil, fmt.Errorf("%w: bundle member has an empty path", ErrInvalidArgument)
	}
	parts := strings.Split(trimmed, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("%w: bundle member %q has an illegal path component %q", ErrInvalidArgument, name, p)
		}
	}
	if !configBundleTopDirs[parts[0]] {
		return nil, fmt.Errorf("%w: bundle member %q is not under skills/, extensions/, or mcp/", ErrInvalidArgument, name)
	}
	return parts, nil
}

// validateRegularMember enforces the per-file grammar and reads the member's
// content (bounded by the enclosing cappedReader). For skills/ and extensions/
// the second path component (the <name>) must match the config name grammar;
// for mcp/ the member must be exactly mcp/<name>.json with a grammar-valid
// <name> and content that parses as JSON. The read propagates errBundleTooLarge
// if the decompressed cap is crossed mid-file.
func validateRegularMember(parts []string, r io.Reader) ([]byte, error) {
	joined := strings.Join(parts, "/")
	if parts[0] == "mcp" {
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: mcp member %q must be mcp/<name>.json", ErrInvalidArgument, joined)
		}
		fn := parts[1]
		if !strings.HasSuffix(fn, ".json") {
			return nil, fmt.Errorf("%w: mcp member %q must be a .json file", ErrInvalidArgument, joined)
		}
		base := strings.TrimSuffix(fn, ".json")
		if !configNamePattern.MatchString(base) {
			return nil, fmt.Errorf("%w: mcp member name %q must match %s", ErrInvalidArgument, base, configNamePattern.String())
		}
		content, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		if !json.Valid(content) {
			return nil, fmt.Errorf("%w: mcp member %q is not valid JSON", ErrInvalidArgument, joined)
		}
		return content, nil
	}

	// skills/ or extensions/: the <name> segment must match the grammar; deeper
	// components have already passed the escape check in configMemberParts.
	if len(parts) < 2 {
		return nil, fmt.Errorf("%w: bundle member %q must live under %s/<name>", ErrInvalidArgument, joined, parts[0])
	}
	if !configNamePattern.MatchString(parts[1]) {
		return nil, fmt.Errorf("%w: bundle member name %q must match %s", ErrInvalidArgument, parts[1], configNamePattern.String())
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return content, nil
}
