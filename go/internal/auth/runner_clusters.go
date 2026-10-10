package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// RunnerCluster is one Kubernetes cluster whose projected ServiceAccount tokens
// the Runner door trusts. The cluster's Name is the first segment of every Runner
// ID it authenticates.
type RunnerCluster struct {
	Name, Issuer, Audience, Namespace, ServiceAccount string
	JWKSURI, JWKSFile, CAFile                         string
	// MaxTokenLifetime bounds exp - iat. It defaults to, and may not be below,
	// the Kubernetes TokenRequest minimum of 600s.
	MaxTokenLifetime time.Duration
}

const (
	defaultRunnerAudience = "compass-runner"
	// httpsScheme is the only scheme an issuer or key fetch may use.
	httpsScheme = "https"
	// minRunnerTokenLifetime is the Kubernetes TokenRequest minimum; a shorter
	// bound would reject every projected token.
	minRunnerTokenLifetime = 600 * time.Second
)

// validRunnerClusterName reports whether name matches [a-z0-9-]{1,40}.
func validRunnerClusterName(name string) bool {
	if len(name) == 0 || len(name) > 40 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

type runnerClustersFile struct {
	Clusters []runnerClusterEntry `yaml:"clusters"`
}

type runnerClusterEntry struct {
	Name             string        `yaml:"name"`
	Issuer           string        `yaml:"issuer"`
	Audience         string        `yaml:"audience"`
	Namespace        string        `yaml:"namespace"`
	ServiceAccount   string        `yaml:"serviceAccount"`
	MaxTokenLifetime time.Duration `yaml:"maxTokenLifetime"`
	JWKSURI          string        `yaml:"jwksURI"`
	JWKSFile         string        `yaml:"jwksFile"`
	CAFile           string        `yaml:"caFile"`
}

// ParseRunnerClusters parses the Runner cluster file, applies defaults, and
// rejects any file the Server must refuse to start with.
func ParseRunnerClusters(data []byte) ([]RunnerCluster, error) {
	var file runnerClustersFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// A misspelt key in the trust root must fail, not silently drop a setting.
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing runner clusters: %w", err)
	}
	// A later document would otherwise be ignored without validation.
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("parsing runner clusters: the file must hold one YAML document")
	}
	clusters := make([]RunnerCluster, 0, len(file.Clusters))
	for _, e := range file.Clusters {
		c := RunnerCluster{
			Name:             e.Name,
			Issuer:           e.Issuer,
			Audience:         e.Audience,
			Namespace:        e.Namespace,
			ServiceAccount:   e.ServiceAccount,
			JWKSURI:          e.JWKSURI,
			JWKSFile:         e.JWKSFile,
			CAFile:           e.CAFile,
			MaxTokenLifetime: e.MaxTokenLifetime,
		}
		if c.Audience == "" {
			c.Audience = defaultRunnerAudience
		}
		if c.MaxTokenLifetime == 0 {
			c.MaxTokenLifetime = minRunnerTokenLifetime
		}
		clusters = append(clusters, c)
	}
	if err := validateRunnerClusters(clusters); err != nil {
		return nil, err
	}
	return clusters, nil
}

// validateRunnerClusters enforces the start-up invariants. Unique issuers are
// load-bearing: the verifier selects a cluster by iss, so a shared issuer would
// let one cluster mint another's node identities.
func validateRunnerClusters(clusters []RunnerCluster) error {
	names := make(map[string]struct{}, len(clusters))
	issuers := make(map[string]string, len(clusters))
	for _, c := range clusters {
		if !validRunnerClusterName(c.Name) {
			return fmt.Errorf("runner cluster %q: name must match [a-z0-9-]{1,40}", c.Name)
		}
		if _, dup := names[c.Name]; dup {
			return fmt.Errorf("runner cluster %q: duplicate name", c.Name)
		}
		names[c.Name] = struct{}{}
		if !isHTTPSURL(c.Issuer) {
			return fmt.Errorf("runner cluster %q: issuer must be an https:// URL", c.Name)
		}
		key := strings.TrimSuffix(c.Issuer, "/")
		if other, dup := issuers[key]; dup {
			return fmt.Errorf("runner cluster %q: issuer already used by cluster %q", c.Name, other)
		}
		issuers[key] = c.Name
		if c.Namespace == "" || c.ServiceAccount == "" {
			return fmt.Errorf("runner cluster %q: namespace and serviceAccount are required", c.Name)
		}
		if c.JWKSURI != "" && c.JWKSFile != "" {
			return fmt.Errorf("runner cluster %q: jwksURI and jwksFile are mutually exclusive", c.Name)
		}
		if c.JWKSURI != "" && !isHTTPSURL(c.JWKSURI) {
			return fmt.Errorf("runner cluster %q: jwksURI must be an https:// URL", c.Name)
		}
		if c.MaxTokenLifetime < minRunnerTokenLifetime {
			return fmt.Errorf("runner cluster %q: maxTokenLifetime %s is below the %s minimum",
				c.Name, c.MaxTokenLifetime, minRunnerTokenLifetime)
		}
	}
	return nil
}

// isHTTPSURL reports whether raw parses as an https URL with a host.
func isHTTPSURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == httpsScheme && u.Host != ""
}
