package auth

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/sync/singleflight"

	"github.com/RigelBuild/compass/go/internal/store"
)

// ErrKeysUnavailable means the token names a registered cluster whose key set is
// unusable (never fetched, discarded as stale, or sharing a key with another
// cluster). It is the one verifier failure the door reports as retryable.
var ErrKeysUnavailable = errors.New("auth: runner cluster keys unavailable")

const (
	jwksFetchTimeout = 3 * time.Second
	jwksMaxBody      = 1 << 20
	jwksRefreshEvery = time.Hour
	// kidRefetchCooldown bounds how often forged kids can make the Server fetch.
	kidRefetchCooldown = 5 * time.Minute
	// staleKeysAfter is how long the last good keys survive a failing issuer.
	staleKeysAfter = 24 * time.Hour
)

var runnerTokenAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}

// LooksLikeJWT reports whether token has the shape of a compact JWS. Minted
// Runner tokens are base64url and never contain a dot.
func LooksLikeJWT(token string) bool {
	return strings.Count(token, ".") == 2
}

// WorkloadRunnerID derives the Runner ID for a node. RFC 1123 node names never
// contain "_" or "/", so the mapping is injective and NATS-subject safe.
func WorkloadRunnerID(cluster, node string) string {
	return cluster + "/" + strings.ReplaceAll(node, ".", "_")
}

// RunnerVerifier verifies Kubernetes projected ServiceAccount tokens offline
// against each registered cluster's JWKS.
type RunnerVerifier struct {
	clusters []*clusterKeys
	byIssuer map[string]*clusterKeys
	tenant   store.TenantID
	now      func() time.Time
	refetch  singleflight.Group
	// onLoad, when set, runs after each fetch result is applied; tests gate on it.
	onLoad func()

	// mu guards every clusterKeys' mutable fields; the overlap check spans all clusters.
	mu sync.Mutex
}

// clusterKeys is one cluster's key-set state. Mutable fields are guarded by
// RunnerVerifier.mu.
type clusterKeys struct {
	cluster RunnerCluster
	client  *http.Client

	keys         *keySet
	failingSince time.Time // first fetch error since the last good load
	overlap      bool      // a key here is also in another cluster's set
	lastRefetch  time.Time // last unknown-kid re-fetch
}

// NewRunnerVerifier builds a verifier over clusters. It reads every jwksFile and
// caFile now, so a bad path refuses start; fetched key sets load on Start.
func NewRunnerVerifier(clusters []RunnerCluster, tenant store.TenantID, client *http.Client, now func() time.Time) (*RunnerVerifier, error) {
	if err := validateRunnerClusters(clusters); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	v := &RunnerVerifier{
		byIssuer: make(map[string]*clusterKeys, len(clusters)),
		tenant:   tenant,
		now:      now,
	}
	for _, c := range clusters {
		hc, err := clusterHTTPClient(client, c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("runner cluster %q: %w", c.Name, err)
		}
		ck := &clusterKeys{cluster: c, client: hc}
		v.clusters = append(v.clusters, ck)
		v.byIssuer[c.Issuer] = ck
	}
	for _, ck := range v.clusters {
		if ck.cluster.JWKSFile == "" {
			continue
		}
		data, err := os.ReadFile(ck.cluster.JWKSFile)
		if err != nil {
			return nil, fmt.Errorf("runner cluster %q: reading jwksFile: %w", ck.cluster.Name, err)
		}
		set, err := parseJWKS(data)
		if err != nil {
			return nil, fmt.Errorf("runner cluster %q: jwksFile: %w", ck.cluster.Name, err)
		}
		v.mu.Lock()
		ck.keys = set
		v.recomputeOverlapLocked()
		v.mu.Unlock()
	}
	return v, nil
}

// clusterHTTPClient copies base so a private CA and the https-only redirect rule
// apply to this cluster alone.
func clusterHTTPClient(base *http.Client, caFile string) (*http.Client, error) {
	hc := *base
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != httpsScheme {
			return fmt.Errorf("refusing non-https redirect to %s", req.URL.Redacted())
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	if caFile == "" {
		return &hc, nil
	}
	pem, err := os.ReadFile(caFile) //nolint:gosec // caFile is the operator's cluster file setting, the whole point of caFile
	if err != nil {
		return nil, fmt.Errorf("reading caFile: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("caFile %s holds no PEM certificate", caFile)
	}
	var tr *http.Transport
	switch t := base.Transport.(type) {
	case *http.Transport:
		tr = t.Clone()
	case nil:
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	default:
		return nil, fmt.Errorf("caFile needs an *http.Transport, client has %T", base.Transport)
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.RootCAs = pool
	hc.Transport = tr
	return &hc, nil
}

// Start begins one background fetch per fetched cluster without waiting for it,
// then refreshes hourly until ctx ends.
func (v *RunnerVerifier) Start(ctx context.Context) {
	for _, ck := range v.clusters {
		if ck.cluster.JWKSFile != "" {
			continue
		}
		go func() {
			ticker := time.NewTicker(jwksRefreshEvery)
			defer ticker.Stop()
			for {
				v.refresh(ctx, ck)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
}

// k8sClaims is the "kubernetes.io" object of a bound ServiceAccount token.
type k8sClaims struct {
	K8s struct {
		Namespace      string `json:"namespace"`
		Node           k8sRef `json:"node"`
		Pod            k8sRef `json:"pod"`
		ServiceAccount k8sRef `json:"serviceaccount"`
	} `json:"kubernetes.io"`
}

type k8sRef struct {
	Name string `json:"name"`
}

// Verify authenticates a projected ServiceAccount token to its node's Runner
// subject. Every failure wraps errInvalidToken except an unusable key set for a
// registered cluster, which wraps ErrKeysUnavailable. The token is never logged.
func (v *RunnerVerifier) Verify(ctx context.Context, token string) (store.Subject, error) {
	tok, err := jwt.ParseSigned(token, runnerTokenAlgs)
	if err != nil {
		return store.Subject{}, fmt.Errorf("%w: parsing runner token: %w", errInvalidToken, err)
	}
	var unverified jwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&unverified); err != nil {
		return store.Subject{}, fmt.Errorf("%w: reading runner token claims: %w", errInvalidToken, err)
	}
	ck, ok := v.byIssuer[unverified.Issuer]
	if !ok {
		return store.Subject{}, fmt.Errorf("%w: runner token issuer is not a registered cluster", errInvalidToken)
	}
	c := ck.cluster
	kid := tok.Headers[0].KeyID
	if kid == "" {
		return store.Subject{}, fmt.Errorf("%w: runner cluster %q: token has no kid", errInvalidToken, c.Name)
	}
	keys, err := v.keysFor(ck, kid)
	if err != nil {
		return store.Subject{}, err
	}
	// A jwksFile cluster's keys change only on rollout; never fetch for it.
	if len(keys) == 0 && ck.cluster.JWKSFile == "" {
		v.refetchForKid(ctx, ck)
		if keys, err = v.keysFor(ck, kid); err != nil {
			return store.Subject{}, err
		}
	}
	if len(keys) == 0 {
		return store.Subject{}, fmt.Errorf("%w: runner cluster %q: unknown kid", errInvalidToken, c.Name)
	}

	var claims jwt.Claims
	var kc k8sClaims
	var sigErr error
	for i := range keys {
		if sigErr = tok.Claims(keys[i].Key, &claims, &kc); sigErr == nil {
			break
		}
	}
	if sigErr != nil {
		return store.Subject{}, fmt.Errorf("%w: runner cluster %q: bad signature: %w", errInvalidToken, c.Name, sigErr)
	}

	expected := jwt.Expected{Issuer: c.Issuer, AnyAudience: jwt.Audience{c.Audience}, Time: v.now()}
	if err := claims.ValidateWithLeeway(expected, jwt.DefaultLeeway); err != nil {
		return store.Subject{}, fmt.Errorf("%w: runner cluster %q: %w", errInvalidToken, c.Name, err)
	}
	// go-jose skips an absent time claim, so require both explicitly.
	if claims.Expiry == nil || claims.IssuedAt == nil {
		return store.Subject{}, fmt.Errorf("%w: runner cluster %q: token lacks exp or iat", errInvalidToken, c.Name)
	}
	if claims.Expiry.Time().Sub(claims.IssuedAt.Time()) > c.MaxTokenLifetime {
		return store.Subject{}, fmt.Errorf("%w: runner cluster %q: token lifetime exceeds %s", errInvalidToken, c.Name, c.MaxTokenLifetime)
	}
	wantSub := "system:serviceaccount:" + c.Namespace + ":" + c.ServiceAccount
	if claims.Subject != wantSub || kc.K8s.Namespace != c.Namespace || kc.K8s.ServiceAccount.Name != c.ServiceAccount {
		return store.Subject{}, fmt.Errorf("%w: runner cluster %q: token is for another ServiceAccount", errInvalidToken, c.Name)
	}
	if kc.K8s.Pod.Name == "" || kc.K8s.Node.Name == "" {
		return store.Subject{}, fmt.Errorf("%w: runner cluster %q: token lacks pod or node", errInvalidToken, c.Name)
	}
	return store.Subject{
		Kind:   store.SubjectRunner,
		ID:     WorkloadRunnerID(c.Name, kc.K8s.Node.Name),
		Tenant: v.tenant,
	}, nil
}

// refresh fetches ck's key set and applies the result.
func (v *RunnerVerifier) refresh(ctx context.Context, ck *clusterKeys) {
	set, err := fetchJWKS(ctx, ck.client, ck.cluster)
	v.applyFetch(ctx, ck, set, err)
	if v.onLoad != nil {
		v.onLoad()
	}
}

// applyFetch installs a fetched set or records a fetch failure, then rechecks
// cross-cluster key sharing.
func (v *RunnerVerifier) applyFetch(ctx context.Context, ck *clusterKeys, set *keySet, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err == nil {
		ck.keys = set
		ck.failingSince = time.Time{}
	} else {
		now := v.now()
		if ck.failingSince.IsZero() {
			ck.failingSince = now
		}
		slog.WarnContext(ctx, "runner cluster key fetch failed", "cluster", ck.cluster.Name, "error", err)
		if ck.keys != nil && now.Sub(ck.failingSince) >= staleKeysAfter {
			ck.keys = nil
			slog.ErrorContext(ctx, "runner cluster keys discarded as stale", "cluster", ck.cluster.Name)
		}
	}
	if names := v.recomputeOverlapLocked(); len(names) > 0 {
		slog.ErrorContext(ctx, "runner clusters share a signing key; failing them closed", "clusters", names)
	}
}

// recomputeOverlapLocked marks every cluster holding a key (by RFC 7638
// thumbprint) that another cluster also holds, and returns their names. Every
// holder fails, not only the later loader, or the earlier one would accept
// tokens the other cluster signs.
func (v *RunnerVerifier) recomputeOverlapLocked() []string {
	holders := make(map[string][]*clusterKeys)
	for _, ck := range v.clusters {
		ck.overlap = false
		if ck.keys == nil {
			continue
		}
		seen := make(map[string]struct{}, len(ck.keys.thumbprints))
		for _, tp := range ck.keys.thumbprints {
			if _, dup := seen[tp]; dup {
				continue
			}
			seen[tp] = struct{}{}
			holders[tp] = append(holders[tp], ck)
		}
	}
	var names []string
	for _, hs := range holders {
		if len(hs) < 2 {
			continue
		}
		for _, ck := range hs {
			if !ck.overlap {
				ck.overlap = true
				names = append(names, ck.cluster.Name)
			}
		}
	}
	slices.Sort(names)
	return names
}

// usableKeysLocked returns ck's key set, or nil when the cluster must fail closed.
func (v *RunnerVerifier) usableKeysLocked(ck *clusterKeys) *keySet {
	if ck.keys == nil || ck.overlap {
		return nil
	}
	if !ck.failingSince.IsZero() && v.now().Sub(ck.failingSince) >= staleKeysAfter {
		return nil
	}
	return ck.keys
}

// keysFor returns the usable keys matching kid, or ErrKeysUnavailable.
func (v *RunnerVerifier) keysFor(ck *clusterKeys, kid string) ([]jose.JSONWebKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	set := v.usableKeysLocked(ck)
	if set == nil {
		return nil, fmt.Errorf("runner cluster %q: %w", ck.cluster.Name, ErrKeysUnavailable)
	}
	return set.jwks.Key(kid), nil
}

// refetchForKid re-fetches ck's keys at most once per cooldown; concurrent
// callers share one fetch.
func (v *RunnerVerifier) refetchForKid(ctx context.Context, ck *clusterKeys) {
	// The shared fetch must not die with whichever caller started it.
	fetchCtx := context.WithoutCancel(ctx)
	// Waiting on the channel is the dedupe; refresh logs and records its own failure.
	<-v.refetch.DoChan(ck.cluster.Name, func() (any, error) {
		v.mu.Lock()
		now := v.now()
		if !ck.lastRefetch.IsZero() && now.Sub(ck.lastRefetch) < kidRefetchCooldown {
			v.mu.Unlock()
			return struct{}{}, nil
		}
		ck.lastRefetch = now
		v.mu.Unlock()
		v.refresh(fetchCtx, ck)
		return struct{}{}, nil
	})
}

// fetchJWKS loads c's key set via OIDC discovery, or from c.JWKSURI when set.
func fetchJWKS(ctx context.Context, client *http.Client, c RunnerCluster) (*keySet, error) {
	jwksURI := c.JWKSURI
	if jwksURI == "" {
		body, err := fetchHTTPS(ctx, client, strings.TrimSuffix(c.Issuer, "/")+"/.well-known/openid-configuration")
		if err != nil {
			return nil, fmt.Errorf("oidc discovery: %w", err)
		}
		var disc struct {
			JWKSURI string `json:"jwks_uri"`
		}
		if err := json.Unmarshal(body, &disc); err != nil {
			return nil, fmt.Errorf("oidc discovery: decoding: %w", err)
		}
		if disc.JWKSURI == "" {
			return nil, errors.New("oidc discovery: no jwks_uri")
		}
		jwksURI = disc.JWKSURI
	}
	body, err := fetchHTTPS(ctx, client, jwksURI)
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	return parseJWKS(body)
}

// fetchHTTPS GETs an https URL with the fetch timeout and body cap.
func fetchHTTPS(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing url: %w", err)
	}
	if u.Scheme != httpsScheme {
		return nil, fmt.Errorf("refusing non-https url %s", u.Redacted())
	}
	ctx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", u.Redacted(), err)
	}
	// A close error on a fully read response body changes nothing we act on.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: status %d", u.Redacted(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", u.Redacted(), err)
	}
	if len(body) > jwksMaxBody {
		return nil, fmt.Errorf("reading %s: body exceeds %d bytes", u.Redacted(), jwksMaxBody)
	}
	return body, nil
}

// keySet is a loaded JWKS with each key's RFC 7638 thumbprint, computed once at
// load for the cross-cluster overlap check.
type keySet struct {
	jwks        jose.JSONWebKeySet
	thumbprints []string
}

// parseJWKS decodes a JWKS and keeps only valid public keys, so a private or
// symmetric key published by mistake can never verify a token.
func parseJWKS(data []byte) (*keySet, error) {
	var raw jose.JSONWebKeySet
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decoding jwks: %w", err)
	}
	set := &keySet{}
	for i := range raw.Keys {
		k := &raw.Keys[i]
		if !k.Valid() || !k.IsPublic() {
			continue
		}
		tp, err := k.Thumbprint(crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("thumbprinting key %q: %w", k.KeyID, err)
		}
		set.jwks.Keys = append(set.jwks.Keys, *k)
		set.thumbprints = append(set.thumbprints, string(tp))
	}
	if len(set.jwks.Keys) == 0 {
		return nil, errors.New("jwks holds no usable public key")
	}
	return set, nil
}
