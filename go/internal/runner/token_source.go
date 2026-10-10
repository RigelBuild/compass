//go:build unix

package runner

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenSource returns the bearer token used for a Runner RPC.
type TokenSource interface {
	Token() (string, error)
}

// StaticToken is a fixed bearer token.
type StaticToken string

func (t StaticToken) Token() (string, error) { return string(t), nil }

// FileToken re-reads a projected token file and retains a still-valid token
// across transient read failures.
type FileToken struct {
	path string
	log  *slog.Logger
	now  func() time.Time

	mu        sync.Mutex
	lastToken string
	lastExp   time.Time
}

// NewFileToken constructs a file-backed token source. A nil logger or clock
// uses the process defaults.
func NewFileToken(path string, log *slog.Logger, now func() time.Time) *FileToken {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &FileToken{path: path, log: log, now: now}
}

func (t *FileToken) Token() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	contents, err := os.ReadFile(t.path)
	if err != nil {
		return t.cachedOrError(fmt.Errorf("reading runner token file: %w", err))
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return t.cachedOrError(errors.New("runner token file is empty"))
	}
	exp, err := tokenExpiry(token)
	if err != nil {
		t.log.Warn("runner token file contains a token without a usable expiry", slog.Any("error", err))
		return token, nil
	}
	if !t.now().Before(exp) {
		return "", errors.New("runner token file contains an expired token")
	}
	t.lastToken = token
	t.lastExp = exp
	return token, nil
}

func (t *FileToken) cachedOrError(err error) (string, error) {
	t.log.Warn("reading runner token file failed", slog.String("path", t.path), slog.Any("error", err))
	if t.lastToken != "" && t.now().Before(t.lastExp) {
		return t.lastToken, nil
	}
	return "", err
}

func tokenExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("runner token is not a compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding runner token claims: %w", err)
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil {
		return time.Time{}, fmt.Errorf("decoding runner token claims: %w", err)
	}
	seconds, err := claims.Exp.Int64()
	if err != nil {
		return time.Time{}, fmt.Errorf("reading runner token expiry: %w", err)
	}
	return time.Unix(seconds, 0), nil
}
