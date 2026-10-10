package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

type recordingGranter struct {
	granted []store.ForgeScope
	err     error
}

func (r *recordingGranter) GrantForgeScope(_ context.Context, s store.ForgeScope) error {
	if r.err != nil {
		return r.err
	}
	r.granted = append(r.granted, s)
	return nil
}

func TestReconcileForgeScopeGrants(t *testing.T) {
	grant := store.ForgeScope{AccountID: "acct-u", Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "o/r"}

	t.Run("seeds every declared grant without warning", func(t *testing.T) {
		var logs bytes.Buffer
		g := &recordingGranter{}
		fc := ForgeConfig{ScopeGrants: []store.ForgeScope{grant}}
		if err := reconcileForgeScopeGrants(context.Background(), g, fc, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if len(g.granted) != 1 || g.granted[0] != grant || logs.Len() != 0 {
			t.Fatalf("granted=%+v logs=%q", g.granted, logs.String())
		}
	})
	t.Run("warns by default when no grants are declared", func(t *testing.T) {
		var logs bytes.Buffer
		err := reconcileForgeScopeGrants(context.Background(), &recordingGranter{}, ForgeConfig{}, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil || !strings.Contains(logs.String(), "no grants declared at boot") || !strings.Contains(logs.String(), "writes need a matching store grant") {
			t.Fatalf("err=%v logs=%q", err, logs.String())

		}
	})
	t.Run("does not warn when single-trust-domain enforcement is disabled", func(t *testing.T) {
		var logs bytes.Buffer
		err := reconcileForgeScopeGrants(context.Background(), &recordingGranter{}, ForgeConfig{ScopeEnforcementDisabled: true}, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil || logs.Len() != 0 {
			t.Fatalf("err=%v logs=%q", err, logs.String())
		}
	})
	t.Run("store failure fails boot", func(t *testing.T) {
		g := &recordingGranter{err: errors.New("db down")}
		fc := ForgeConfig{ScopeGrants: []store.ForgeScope{grant}}
		if err := reconcileForgeScopeGrants(context.Background(), g, fc, slog.Default()); err == nil {
			t.Fatal("reconcile = nil, want error")
		}
	})
}
