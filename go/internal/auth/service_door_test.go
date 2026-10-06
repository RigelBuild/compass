package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestServiceBearerInterceptor(t *testing.T) {
	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	validToken := "opaque-service-token"
	resolveErr := errors.New("resolver failure")
	tests := []struct {
		name       string
		header     string
		resolveErr error
		subject    store.Subject
		allow      []string
		wantReason string
		wantCall   bool
	}{
		{name: "missing header", allow: []string{LLMGatewayServiceID}, wantReason: "missing authorization"},
		{name: "malformed header", header: "Basic secret-value", allow: []string{LLMGatewayServiceID}, wantReason: "malformed authorization"},
		{name: "unknown token", header: "Bearer " + validToken, resolveErr: ErrTokenNotFound, allow: []string{LLMGatewayServiceID}, wantReason: "token not found", wantCall: true},
		{name: "revoked token", header: "Bearer " + validToken, resolveErr: ErrTokenRevoked, allow: []string{LLMGatewayServiceID}, wantReason: "token revoked", wantCall: true},
		{name: "resolver failure", header: "Bearer " + validToken, resolveErr: resolveErr, allow: []string{LLMGatewayServiceID}, wantReason: "token resolution failed", wantCall: true},
		{name: "wrong kind resolver", header: "Bearer " + validToken, resolveErr: ErrWrongKind, allow: []string{LLMGatewayServiceID}, wantReason: "wrong subject kind", wantCall: true},
		{name: "wrong kind subject", header: "Bearer " + validToken, subject: store.Subject{Kind: store.SubjectAccount, ID: "account"}, allow: []string{LLMGatewayServiceID}, wantReason: "wrong subject kind", wantCall: true},
		{name: "not allowlisted", header: "Bearer " + validToken, subject: store.Subject{Kind: store.SubjectService, ID: "other-service"}, allow: []string{LLMGatewayServiceID}, wantReason: "service not allowlisted", wantCall: true},
		{name: "accepted", header: "Bearer " + validToken, subject: store.Subject{Kind: store.SubjectService, ID: LLMGatewayServiceID}, allow: []string{LLMGatewayServiceID}, wantCall: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()
			ctx := t.Context()
			called := false
			resolvedToken := ""
			wantKind := store.SubjectKind(-1)
			resolve := func(_ context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
				called = true
				resolvedToken = presented
				wantKind = want
				return tt.subject, tt.resolveErr
			}
			next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
				subj, ok := ServiceSubjectFrom(ctx)
				if !ok || subj != tt.subject {
					t.Fatalf("ServiceSubjectFrom() = (%+v, %v), want (%+v, true)", subj, ok, tt.subject)
				}
				return connect.NewResponse(&compassv1.GetServerInfoResponse{}), nil
			}
			interceptor := ServiceBearerInterceptor(resolve, tt.allow...)
			request := connect.NewRequest(&compassv1.GetServerInfoRequest{})
			if tt.header != "" {
				request.Header().Set("Authorization", tt.header)
			}
			_, err := interceptor(next)(ctx, request)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("valid service token rejected: %v", err)
				}
			} else {
				if connect.CodeOf(err) != connect.CodeUnauthenticated {
					t.Fatalf("error code = %v, want Unauthenticated (err %v)", connect.CodeOf(err), err)
				}
				if got := err.Error(); got != "unauthenticated: unauthenticated" {
					t.Fatalf("client error = %q, want opaque unauthenticated", got)
				}
				if !strings.Contains(logs.String(), "reason=\""+tt.wantReason+"\"") {
					t.Fatalf("debug log missing reason %q: %s", tt.wantReason, logs.String())
				}
				if strings.Contains(logs.String(), validToken) || strings.Contains(logs.String(), "secret-value") {
					t.Fatalf("debug log contains presented credential: %s", logs.String())
				}
			}
			if called != tt.wantCall {
				t.Fatalf("resolver called = %v, want %v", called, tt.wantCall)
			}
			if tt.wantCall && (resolvedToken != validToken || wantKind != store.SubjectService) {
				t.Fatalf("resolver args = (%q, %v), want (%q, SubjectService)", resolvedToken, wantKind, validToken)
			}
		})
	}
}
