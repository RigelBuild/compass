// Package forgecred defines the on-disk contract shared by the secret
// materializer and the forge credential helper.
package forgecred

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var hostGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)
var ownerGrammar = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// ValidOwner reports whether owner is a GitHub account login accepted by the credential layout.
func ValidOwner(owner string) bool {
	return ownerGrammar.MatchString(owner)
}

// TokenPath returns the path for an owner's credential on a forge host.
func TokenPath(home, host, owner string) string {
	return filepath.Join(home, ".compass", "forge", safeHost(host), safeOwner(owner))
}

// DefaultOwnerPath returns the marker path for the default forge installation.
func DefaultOwnerPath(home, host string) string {
	return filepath.Join(home, ".compass", "forge", safeHost(host), ".default-owner")
}

// LookupToken reads an owner's token. Missing files are a normal lookup miss.
func LookupToken(home, host, owner string) (string, bool, error) {
	b, err := os.ReadFile(TokenPath(home, host, owner))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading forge credential: %w", err)
	}
	return strings.TrimSuffix(string(b), "\n"), true, nil
}

// DefaultOwner reads the login named by the host's default-owner marker.
func DefaultOwner(home, host string) (string, bool, error) {
	b, err := os.ReadFile(DefaultOwnerPath(home, host))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading forge default owner: %w", err)
	}
	owner := strings.ToLower(strings.TrimSpace(string(b)))
	if !ownerGrammar.MatchString(owner) {
		return "", false, errors.New("invalid forge default owner")
	}
	return owner, true, nil
}

// OwnerFromCredentialPath extracts the owner from Git's owner/repository path.
func OwnerFromCredentialPath(path string) (string, bool) {
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || !ownerGrammar.MatchString(owner) || repo == "" || strings.Contains(repo, "/") {
		return "", false
	}
	repo = strings.TrimSuffix(repo, ".git")
	if repo == "" {
		return "", false
	}
	return strings.ToLower(owner), true
}

func safeHost(host string) string {
	if !hostGrammar.MatchString(host) || strings.Contains(host, "..") {
		return "invalid-host"
	}
	return host
}

func safeOwner(owner string) string {
	if !ownerGrammar.MatchString(owner) {
		return "invalid-owner"
	}
	return strings.ToLower(owner)
}
